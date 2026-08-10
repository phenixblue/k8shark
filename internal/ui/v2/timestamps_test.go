package v2

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestServeTimestamps(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	t1 := now.Add(30 * time.Second)
	pods := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"web","namespace":"default"}}]}`
	path := "/api/v1/namespaces/default/pods"
	recs := []*capture.Record{
		{ID: "p0", CapturedAt: now, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(pods)},
		{ID: "p1", CapturedAt: t1, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(pods)},
	}
	idx := capture.Index{path: {APIPath: path, Seqs: []int{0, 1}, Times: []time.Time{now, t1}, Counts: []int{1, 1}}}
	meta := &capture.CaptureMetadata{CaptureID: "ts-test", CapturedAt: now.Add(-time.Minute), CapturedUntil: t1, RecordCount: len(recs)}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: t1}

	var resp TimestampsResponse
	if code := getJSONInto(t, h, h.serveTimestamps, "/v2/api/timestamps", "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want 2", resp.TotalCount)
	}
	if len(resp.Timestamps) != 2 {
		t.Errorf("len(Timestamps) = %d, want 2", len(resp.Timestamps))
	}
	if resp.Sampled {
		t.Errorf("Sampled = true, want false for only 2 stops")
	}
	if !resp.CapturedUntil.Equal(t1) {
		t.Errorf("CapturedUntil = %v, want %v", resp.CapturedUntil, t1)
	}
}

// The scrubber indexes stops by position in Timestamps, so a repeated string
// value is a dead zone the step buttons cannot cross (#257). The bug was a
// dedupe at nanosecond precision feeding a second-precision Format, so the
// records here deliberately share a wall-clock second while differing in their
// sub-second parts — the exact shape that produced 180 stops with 4 distinct
// values on a real capture.
func TestServeTimestamps_NoDuplicateStops(t *testing.T) {
	base := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	// Three records inside second :00 and two inside :01 → 2 distinct stops.
	times := []time.Time{
		base,
		base.Add(10 * time.Millisecond),
		base.Add(750 * time.Microsecond),
		base.Add(time.Second),
		base.Add(time.Second + 300*time.Millisecond),
	}
	pods := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"web","namespace":"default"}}]}`
	path := "/api/v1/namespaces/default/pods"
	recs := make([]*capture.Record, 0, len(times))
	seqs := make([]int, 0, len(times))
	counts := make([]int, 0, len(times))
	for i, ts := range times {
		recs = append(recs, &capture.Record{
			ID: "p" + strconv.Itoa(i), CapturedAt: ts, APIPath: path,
			HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(pods),
		})
		seqs = append(seqs, i)
		counts = append(counts, 1)
	}
	idx := capture.Index{path: {APIPath: path, Seqs: seqs, Times: times, Counts: counts}}
	meta := &capture.CaptureMetadata{
		CaptureID: "ts-dupes", CapturedAt: base.Add(-time.Minute),
		CapturedUntil: times[len(times)-1], RecordCount: len(recs),
	}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta)}

	var resp TimestampsResponse
	if code := getJSONInto(t, h, h.serveTimestamps, "/v2/api/timestamps", "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	seen := make(map[string]int, len(resp.Timestamps))
	for _, ts := range resp.Timestamps {
		seen[ts]++
	}
	for ts, n := range seen {
		if n > 1 {
			t.Errorf("timestamp %q emitted %d times; the scrubber cannot step across a repeated stop", ts, n)
		}
	}
	if len(seen) != len(resp.Timestamps) {
		t.Errorf("len(Timestamps) = %d but only %d are distinct", len(resp.Timestamps), len(seen))
	}
	// 5 records collapsing to the 2 wall-clock seconds they fall in.
	if len(resp.Timestamps) != 2 {
		t.Errorf("len(Timestamps) = %d, want 2 (one stop per distinct second), got %v", len(resp.Timestamps), resp.Timestamps)
	}
	// TotalCount is distinct stops, so it must agree with the emitted list
	// whenever nothing was sampled away.
	if resp.Sampled {
		t.Fatalf("Sampled = true, want false for 2 stops")
	}
	if resp.TotalCount != len(resp.Timestamps) {
		t.Errorf("TotalCount = %d, want %d (unsampled, so it must equal the stop count)", resp.TotalCount, len(resp.Timestamps))
	}
	// Ordering must survive the dedupe.
	for i := 1; i < len(resp.Timestamps); i++ {
		if resp.Timestamps[i-1] >= resp.Timestamps[i] {
			t.Errorf("Timestamps not strictly increasing at %d: %v", i, resp.Timestamps)
		}
	}
}

// sampleTimes runs after the dedupe, so a capture with more distinct stops than
// the cap must still come back duplicate-free — sampling an already-unique list
// cannot reintroduce repeats, and this pins that down.
func TestServeTimestamps_SampledStopsStayDistinct(t *testing.T) {
	base := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	n := scrubberMaxStops * 2
	times := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		times = append(times, base.Add(time.Duration(i)*time.Second))
	}
	pods := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"web","namespace":"default"}}]}`
	path := "/api/v1/namespaces/default/pods"
	recs := make([]*capture.Record, 0, n)
	seqs := make([]int, 0, n)
	counts := make([]int, 0, n)
	for i, ts := range times {
		recs = append(recs, &capture.Record{
			ID: "p" + strconv.Itoa(i), CapturedAt: ts, APIPath: path,
			HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(pods),
		})
		seqs = append(seqs, i)
		counts = append(counts, 1)
	}
	idx := capture.Index{path: {APIPath: path, Seqs: seqs, Times: times, Counts: counts}}
	meta := &capture.CaptureMetadata{
		CaptureID: "ts-sampled", CapturedAt: base, CapturedUntil: times[n-1], RecordCount: n,
	}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta)}

	var resp TimestampsResponse
	if code := getJSONInto(t, h, h.serveTimestamps, "/v2/api/timestamps", "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !resp.Sampled {
		t.Errorf("Sampled = false, want true for %d stops", n)
	}
	if resp.TotalCount != n {
		t.Errorf("TotalCount = %d, want %d", resp.TotalCount, n)
	}
	if len(resp.Timestamps) > scrubberMaxStops {
		t.Errorf("len(Timestamps) = %d, want <= %d", len(resp.Timestamps), scrubberMaxStops)
	}
	seen := make(map[string]struct{}, len(resp.Timestamps))
	for _, ts := range resp.Timestamps {
		if _, dup := seen[ts]; dup {
			t.Errorf("sampled list repeats %q", ts)
		}
		seen[ts] = struct{}{}
	}
}

func TestServeTimestamps_NilStore(t *testing.T) {
	h := &Handler{}
	if code := getJSONInto(t, h, h.serveTimestamps, "/v2/api/timestamps", "", nil); code != http.StatusInternalServerError {
		t.Errorf("nil store: status = %d, want 500", code)
	}
}
