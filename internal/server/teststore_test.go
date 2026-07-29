package server

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"

	kstore "github.com/phenixblue/k8shark/internal/store"
)

// buildTestStore creates a kstore.CaptureStore with the given per-path
// response bodies. A near-duplicate of internal/store's own buildTestStore:
// that one can't be reused here directly since unexported test helpers aren't
// visible across package boundaries, and this package's HTTP-layer tests
// (handler_test.go, writes_test.go, ...) need a real *kstore.CaptureStore to
// drive the mock apiserver against.
func buildTestStore(t *testing.T, records map[string][]byte) *kstore.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.kshrk")

	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}

	index := make(capture.Index)
	for apiPath, body := range records {
		now := time.Now().UTC()
		rec := capture.Record{
			ID:           "rec-" + apiPath,
			CapturedAt:   now,
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(body),
		}
		if _, err := sw.WriteRecord(&rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		index[apiPath] = &capture.IndexEntry{
			APIPath: apiPath,
			Seqs:    []int{0},
			Times:   []time.Time{now},
		}
	}

	meta := capture.CaptureMetadata{
		CaptureID:         "test-capture-id",
		KubernetesVersion: "v1.29.0",
		CapturedAt:        time.Now().UTC().Add(-time.Minute),
		CapturedUntil:     time.Now().UTC(),
		RecordCount:       len(records),
	}
	if err := sw.Finish(&meta, index, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ar, err := archive.Open(outPath)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })

	store, err := kstore.LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

// podSpec describes one Pod for listWithPods.
type podSpec struct {
	name      string
	namespace string
	labels    map[string]string
}

// listWithPods builds a PodList body from pods, defaulting an empty namespace
// to "default". A near-duplicate of internal/store's own listWithPods — see
// buildTestStore's doc comment for why.
func listWithPods(pods []podSpec) []byte {
	items := make([]map[string]any, 0, len(pods))
	for _, p := range pods {
		ns := p.namespace
		if ns == "" {
			ns = "default"
		}
		items = append(items, map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      p.name,
				"namespace": ns,
				"labels":    p.labels,
			},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "PodList",
		"metadata":   map[string]any{},
		"items":      items,
	})
	return body
}

// itemNames returns the metadata.name of every item in a list body.
func itemNames(t *testing.T, body []byte) []string {
	t.Helper()
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("itemNames unmarshal: %v\nbody: %s", err, body)
	}
	names := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		names = append(names, it.Metadata.Name)
	}
	return names
}

// watchTestRecord describes a single snapshot record for buildTestStoreWithWatch.
type watchTestRecord struct {
	id   string
	at   time.Time
	body string
}

// watchTestEvent describes a single watch event record for buildTestStoreWithWatch.
type watchTestEvent struct {
	id         string
	apiPath    string
	at         time.Time
	eventType  string
	objectBody string
}

// buildTestStoreWithWatch creates a kstore.CaptureStore with snapshot records
// in index.json and watch event records in watch-index.json.
func buildTestStoreWithWatch(t *testing.T, snapshots map[string]watchTestRecord, events []watchTestEvent) *kstore.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.kshrk")

	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}

	index := make(capture.Index)
	watchIndex := make(capture.WatchIndex)

	for apiPath, s := range snapshots {
		rec := capture.Record{
			ID:           s.id,
			CapturedAt:   s.at,
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(s.body),
		}
		if _, err := sw.WriteRecord(&rec); err != nil {
			t.Fatalf("WriteRecord(snap): %v", err)
		}
		index[apiPath] = &capture.IndexEntry{
			APIPath: apiPath,
			Seqs:    []int{0},
			Times:   []time.Time{s.at},
		}
	}

	// Track per-path seq counter for ALL records (snap + watch share path namespace).
	allSeq := map[string]int{}
	for apiPath := range snapshots {
		allSeq[apiPath] = 1 // snapshot already wrote seq=0
	}
	for _, ev := range events {
		rec := capture.Record{
			ID:           ev.id,
			CapturedAt:   ev.at,
			APIPath:      ev.apiPath,
			EventType:    ev.eventType,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(ev.objectBody),
		}
		if _, err := sw.WriteRecord(&rec); err != nil {
			t.Fatalf("WriteRecord(watch %s): %v", ev.id, err)
		}
		wi := watchIndex[ev.apiPath]
		if wi == nil {
			wi = &capture.WatchIndexEntry{APIPath: ev.apiPath}
			watchIndex[ev.apiPath] = wi
		}
		seq := allSeq[ev.apiPath]
		allSeq[ev.apiPath] = seq + 1
		wi.Seqs = append(wi.Seqs, seq)
		wi.Times = append(wi.Times, ev.at)
		wi.EventTypes = append(wi.EventTypes, ev.eventType)
	}

	meta := capture.CaptureMetadata{
		CaptureID: "test-watch-id", KubernetesVersion: "v1.29.0",
		CapturedAt: time.Now().UTC().Add(-time.Minute), CapturedUntil: time.Now().UTC(),
	}
	var wi any
	if len(watchIndex) > 0 {
		wi = watchIndex
	}
	if err := sw.Finish(&meta, index, wi); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ar, err := archive.Open(outPath)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })

	store, err := kstore.LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}
