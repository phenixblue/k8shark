package server

import (
	"encoding/json"
	"sync"
)

// overlay is the in-memory write layer for --writable replay. Client writes
// (create/update/patch/delete) land here and are merged over the replayed
// capture state on read, so a controller can observe its own writes (closed-loop
// dev). It is nil for read-only replay.
//
// Conflict policy is "overlay wins (owned)": once an object identity is present
// in the overlay, replay events for it are suppressed (see owns) and reads use
// the overlay copy. Reset policy is "on loop wrap only" — syncEpoch clears the
// overlay when the replay clock's loop epoch advances; a manual reset is also
// exposed. The overlay persists across seeks.
type overlay struct {
	mu      sync.RWMutex
	items   map[string]*overlayEntry // key: overlayKey(g,v,resource,namespace,name)
	counter int64                    // monotonic RV source (never decreases)
	epoch   int                      // last-seen clock loop epoch (reset-on-loop)
}

type overlayEntry struct {
	group, version, resource string
	namespace, name          string
	obj                      json.RawMessage // last-known body (kept even when deleted, for the DELETED event)
	deleted                  bool            // tombstone
	rv                       int64
}

func newOverlay() *overlay {
	return &overlay{items: map[string]*overlayEntry{}}
}

// overlayKey identifies an object by GVR + namespace + name.
func overlayKey(group, version, resource, namespace, name string) string {
	return group + "/" + version + "/" + resource + "/" + namespace + "/" + name
}

// nsName is the namespace-qualified name used to match list items ("ns/name",
// or "/name" for cluster-scoped objects).
func nsName(namespace, name string) string { return namespace + "/" + name }

// itemNSName extracts the namespace-qualified name from a raw list item.
func itemNSName(raw json.RawMessage) string {
	var m struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return nsName(m.Metadata.Namespace, m.Metadata.Name)
}

// syncEpoch clears the overlay when the clock's loop epoch has advanced since the
// last call, implementing reset-on-loop. The RV counter stays monotonic so RVs
// are never reused. Safe to call on every read/write.
func (o *overlay) syncEpoch(clock *ReplayClock) {
	if o == nil || clock == nil {
		return
	}
	_, epoch, _ := clock.Sample()
	o.mu.Lock()
	if epoch != o.epoch {
		o.items = map[string]*overlayEntry{}
		o.epoch = epoch
	}
	o.mu.Unlock()
}

// reset clears all overlay entries (manual reset). The RV counter is preserved.
func (o *overlay) reset() {
	o.mu.Lock()
	o.items = map[string]*overlayEntry{}
	o.mu.Unlock()
}

// currentRV returns the highest RV the overlay has assigned so far.
func (o *overlay) currentRV() int64 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.counter
}

// bumpRV advances the monotonic counter to at least floorRV+1 and returns it.
// Callers hold o.mu.
func (o *overlay) bumpRVLocked(floorRV int64) int64 {
	rv := o.counter
	if floorRV > rv {
		rv = floorRV
	}
	rv++
	o.counter = rv
	return rv
}

// nextRV advances the monotonic counter above floorRV and returns the new RV,
// without storing anything — callers stamp the RV into the object, then store it.
func (o *overlay) nextRV(floorRV int64) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.bumpRVLocked(floorRV)
}

// store upserts a finalized object under a pre-assigned RV.
func (o *overlay) store(group, version, resource, namespace, name string, obj json.RawMessage, rv int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items[overlayKey(group, version, resource, namespace, name)] = &overlayEntry{
		group: group, version: version, resource: resource,
		namespace: namespace, name: name, obj: obj, rv: rv,
	}
}

// del marks an object deleted (tombstone) and returns its new RV. last is the
// object body to carry on the DELETED event (may be nil).
func (o *overlay) del(group, version, resource, namespace, name string, last json.RawMessage, floorRV int64) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	rv := o.bumpRVLocked(floorRV)
	o.items[overlayKey(group, version, resource, namespace, name)] = &overlayEntry{
		group: group, version: version, resource: resource,
		namespace: namespace, name: name, obj: last, deleted: true, rv: rv,
	}
	return rv
}

// get returns the overlay entry for an object identity, if present.
func (o *overlay) get(group, version, resource, namespace, name string) (*overlayEntry, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	e, ok := o.items[overlayKey(group, version, resource, namespace, name)]
	return e, ok
}

// applyToList merges the overlay over a base list's items for a given list scope
// (group/version/resource, and namespace — empty for a cluster-wide/-A list).
// Overlay entries replace matching base items (overlay wins), tombstones remove
// them, and overlay-created items not in the base are appended.
func (o *overlay) applyToList(group, version, resource, namespace string, base []json.RawMessage) []json.RawMessage {
	o.mu.RLock()
	defer o.mu.RUnlock()

	matched := map[string]*overlayEntry{}
	for _, e := range o.items {
		if e.group == group && e.version == version && e.resource == resource &&
			(namespace == "" || e.namespace == namespace) {
			matched[nsName(e.namespace, e.name)] = e
		}
	}
	if len(matched) == 0 {
		return base
	}

	out := make([]json.RawMessage, 0, len(base)+len(matched))
	seen := map[string]bool{}
	for _, item := range base {
		k := itemNSName(item)
		if e, ok := matched[k]; ok {
			seen[k] = true
			if e.deleted {
				continue // removed by overlay
			}
			out = append(out, e.obj) // overlay wins
		} else {
			out = append(out, item)
		}
	}
	for k, e := range matched {
		if seen[k] || e.deleted {
			continue
		}
		out = append(out, e.obj)
	}
	return out
}
