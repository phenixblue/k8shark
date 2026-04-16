package transitions

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// Transition is a single state-change event for a Kubernetes object detected
// from a capture archive.
type Transition struct {
	Time      time.Time       `json:"time"`
	EventType string          `json:"event_type"` // ADDED, MODIFIED, or DELETED
	APIPath   string          `json:"api_path"`
	Group     string          `json:"group,omitempty"`
	Version   string          `json:"version,omitempty"`
	Resource  string          `json:"resource,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Name      string          `json:"name"`
	Before    json.RawMessage `json:"before,omitempty"` // prior body (MODIFIED, DELETED)
	After     json.RawMessage `json:"after,omitempty"`  // new body  (ADDED,   MODIFIED)
}

// FilterOpts narrows which transitions are returned by LoadTransitions.
type FilterOpts struct {
	Resource  string    // substring match against the resource name (e.g. "pods")
	Namespace string    // exact namespace match
	Name      string    // exact object name match
	Since     time.Time // inclusive lower bound on event time (zero = unbounded)
	Until     time.Time // inclusive upper bound on event time (zero = unbounded)
}

// LoadTransitions opens archivePath, discovers all state-change events that
// match opts, and returns them sorted by time.
//
// Detection modes:
//   - Watch-enabled paths: events are read directly from watch-index.json
//     (ADDED/MODIFIED/DELETED labels captured at watch-stream time).
//   - Poll-only paths: consecutive snapshot pairs are diff'd by object identity
//     to detect additions, modifications, and deletions.
func LoadTransitions(archivePath string, opts FilterOpts) ([]Transition, error) {
	ar, err := archive.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	defer ar.Close()

	var idx capture.Index
	if err := ar.ReadIndex(&idx); err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}

	// Watch-index may be absent for older archives.
	var wi capture.WatchIndex
	_, _ = ar.ReadWatchIndex(&wi)

	var all []Transition

	for apiPath, entry := range idx {
		// Skip Table-format query parameters.
		if strings.Contains(apiPath, "?") {
			continue
		}
		g, v, r, ns := parseAPIPath(apiPath)
		if r == "" {
			continue
		}
		if !matchesResourceFilter(r, ns, opts) {
			continue
		}

		if wiEntry, hasWatch := wi[apiPath]; hasWatch && len(wiEntry.Seqs) > 0 {
			ts, err := watchTransitions(ar, apiPath, wiEntry, g, v, r, ns, opts)
			if err != nil {
				return nil, err
			}
			all = append(all, ts...)
		} else {
			ts, err := pollTransitions(ar, apiPath, entry, g, v, r, ns, opts)
			if err != nil {
				return nil, err
			}
			all = append(all, ts...)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Time.Before(all[j].Time)
	})

	return all, nil
}

// watchTransitions emits transitions for a watch-captured API path by walking
// the watch-index entries in chronological order. Per-object state is tracked
// so that Before is populated accurately for MODIFIED and DELETED events.
func watchTransitions(ar *archive.Archive, apiPath string, wi *capture.WatchIndexEntry, g, v, r, ns string, opts FilterOpts) ([]Transition, error) {
	// lastState tracks the most recently seen body per object key so we can
	// populate Before for MODIFIED and DELETED events.
	lastState := make(map[string]json.RawMessage)

	var out []Transition
	for i, seq := range wi.Seqs {
		if i >= len(wi.Times) || i >= len(wi.EventTypes) {
			break
		}
		evTime := wi.Times[i]
		evType := wi.EventTypes[i]

		// Events are stored in chronological order; once we exceed Until we stop.
		if !opts.Until.IsZero() && evTime.After(opts.Until) {
			break
		}

		rec, err := readRecord(ar, apiPath, seq)
		if err != nil {
			// Tolerate individual record read failures.
			continue
		}

		k := objectKey(rec.ResponseBody)
		name := objectName(rec.ResponseBody)

		shouldEmit := inWindow(evTime, opts.Since, opts.Until) &&
			(opts.Name == "" || name == opts.Name)

		if shouldEmit {
			t := Transition{
				Time:      evTime,
				EventType: evType,
				APIPath:   apiPath,
				Group:     g,
				Version:   v,
				Resource:  r,
				Namespace: ns,
				Name:      name,
			}
			switch evType {
			case "ADDED":
				t.After = rec.ResponseBody
			case "MODIFIED":
				t.Before = lastState[k]
				t.After = rec.ResponseBody
			case "DELETED":
				t.Before = lastState[k]
			}
			out = append(out, t)
		}

		// Update per-object state for future Before lookups.
		if k != "" {
			if evType == "DELETED" {
				delete(lastState, k)
			} else {
				lastState[k] = rec.ResponseBody
			}
		}
	}
	return out, nil
}

// pollTransitions emits transitions for a poll-only API path by diffing
// consecutive snapshot pairs. The transition timestamp is that of the newer
// snapshot in each pair.
func pollTransitions(ar *archive.Archive, apiPath string, entry *capture.IndexEntry, g, v, r, ns string, opts FilterOpts) ([]Transition, error) {
	var out []Transition

	var prevItems map[string]json.RawMessage
	var prevOrder []string

	for i, seq := range entry.Seqs {
		rec, err := readRecord(ar, apiPath, seq)
		if err != nil {
			return nil, fmt.Errorf("reading record seq %d: %w", seq, err)
		}

		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if jerr := json.Unmarshal(rec.ResponseBody, &list); jerr != nil || list.Items == nil {
			// Not a list body; reset and skip.
			prevItems = nil
			prevOrder = nil
			continue
		}

		currItems := make(map[string]json.RawMessage, len(list.Items))
		currOrder := make([]string, 0, len(list.Items))
		for _, item := range list.Items {
			k := objectKey(item)
			if k == "" {
				continue
			}
			currItems[k] = item
			currOrder = append(currOrder, k)
		}

		if prevItems != nil {
			var snapTime time.Time
			if i < len(entry.Times) {
				snapTime = entry.Times[i]
			}
			if inWindow(snapTime, opts.Since, opts.Until) {
				ts := diffItems(prevItems, prevOrder, currItems, currOrder,
					snapTime, apiPath, g, v, r, ns, opts.Name)
				out = append(out, ts...)
			}
		}

		prevItems = currItems
		prevOrder = currOrder
	}
	return out, nil
}

// diffItems compares two consecutive item sets and emits ADDED/MODIFIED/DELETED
// transitions at the given timestamp.
func diffItems(
	prev map[string]json.RawMessage, prevOrder []string,
	curr map[string]json.RawMessage, currOrder []string,
	at time.Time, apiPath, g, v, r, ns, nameFilter string,
) []Transition {
	var out []Transition

	// ADDED and MODIFIED: objects present in curr.
	for _, k := range currOrder {
		currBody := curr[k]
		name := objectName(currBody)
		if nameFilter != "" && name != nameFilter {
			continue
		}
		if prevBody, existed := prev[k]; !existed {
			out = append(out, Transition{
				Time:      at,
				EventType: "ADDED",
				APIPath:   apiPath,
				Group:     g,
				Version:   v,
				Resource:  r,
				Namespace: ns,
				Name:      name,
				After:     currBody,
			})
		} else if !jsonEqual(prevBody, currBody) {
			out = append(out, Transition{
				Time:      at,
				EventType: "MODIFIED",
				APIPath:   apiPath,
				Group:     g,
				Version:   v,
				Resource:  r,
				Namespace: ns,
				Name:      name,
				Before:    prevBody,
				After:     currBody,
			})
		}
	}

	// DELETED: objects present in prev but absent from curr.
	for _, k := range prevOrder {
		if _, exists := curr[k]; !exists {
			name := objectName(prev[k])
			if nameFilter != "" && name != nameFilter {
				continue
			}
			out = append(out, Transition{
				Time:      at,
				EventType: "DELETED",
				APIPath:   apiPath,
				Group:     g,
				Version:   v,
				Resource:  r,
				Namespace: ns,
				Name:      name,
				Before:    prev[k],
			})
		}
	}
	return out
}

// readRecord reads and parses a single capture.Record by seq from the archive.
func readRecord(ar *archive.Archive, apiPath string, seq int) (capture.Record, error) {
	data, err := ar.ReadRecord(apiPath, seq)
	if err != nil {
		return capture.Record{}, fmt.Errorf("reading record seq %d: %w", seq, err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return capture.Record{}, fmt.Errorf("parsing record seq %d: %w", seq, err)
	}
	return rec, nil
}

// objectKey returns "namespace/name" for namespaced objects and "name" for
// cluster-scoped objects. Returns "" if the body cannot be parsed.
func objectKey(raw json.RawMessage) string {
	var meta struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	if meta.Metadata.Namespace != "" {
		return meta.Metadata.Namespace + "/" + meta.Metadata.Name
	}
	return meta.Metadata.Name
}

// objectName returns only the name field from a Kubernetes object body.
func objectName(raw json.RawMessage) string {
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &meta)
	return meta.Metadata.Name
}

// matchesResourceFilter returns true when the resource and namespace satisfy
// the resource and namespace filters in opts.
func matchesResourceFilter(resource, namespace string, opts FilterOpts) bool {
	if opts.Resource != "" && !strings.Contains(resource, opts.Resource) {
		return false
	}
	if opts.Namespace != "" && namespace != opts.Namespace {
		return false
	}
	return true
}

// inWindow returns true when t is within the [since, until] range.
// A zero value means "no bound".
func inWindow(t, since, until time.Time) bool {
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !until.IsZero() && t.After(until) {
		return false
	}
	return true
}

// parseAPIPath extracts group, version, resource, and namespace from a
// canonical Kubernetes API path (/api/... or /apis/...).
func parseAPIPath(path string) (group, version, resource, namespace string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "api":
		version = parts[1]
		if len(parts) == 3 {
			resource = parts[2]
		} else if len(parts) == 5 && parts[2] == "namespaces" {
			namespace = parts[3]
			resource = parts[4]
		}
	case len(parts) >= 4 && parts[0] == "apis":
		group = parts[1]
		version = parts[2]
		if len(parts) == 4 {
			resource = parts[3]
		} else if len(parts) == 6 && parts[3] == "namespaces" {
			namespace = parts[4]
			resource = parts[5]
		}
	}
	return
}

// jsonEqual reports whether two JSON blobs are semantically equivalent.
func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return string(a) == string(b)
	}
	aj, _ := json.Marshal(av)
	bj, _ := json.Marshal(bv)
	return string(aj) == string(bj)
}
