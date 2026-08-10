package v2

import (
	"net/http"
	"sort"
	"time"
)

// TimestampsResponse is what /v2/api/timestamps returns. Sampled when the
// capture has more than ~180 distinct event timestamps so the scrubber stays
// usable on long captures.
//
// TotalCount counts distinct *scrubber stops* — event timestamps at the second
// precision Timestamps is emitted in — not records. Timestamps is guaranteed
// free of duplicates (#257); the client indexes the scrubber by position in it.
type TimestampsResponse struct {
	CapturedAt    time.Time `json:"captured_at"`
	CapturedUntil time.Time `json:"captured_until"`
	DefaultAt     string    `json:"default_at,omitempty"`
	TotalCount    int       `json:"total_count"`
	Sampled       bool      `json:"sampled"`
	Timestamps    []string  `json:"timestamps"`
}

const scrubberMaxStops = 180

func (h *Handler) timestampsHandler(w http.ResponseWriter, _ *http.Request) {
	resp := TimestampsResponse{
		CapturedAt:    h.Store.Metadata.CapturedAt,
		CapturedUntil: h.Store.Metadata.CapturedUntil,
	}
	if !h.At.IsZero() {
		resp.DefaultAt = h.At.UTC().Format(time.RFC3339)
	}

	// Dedupe at the precision we actually emit (see the RFC3339 Format below),
	// NOT at the nanosecond precision the index carries. Keying this map on the
	// full-precision time produced a list whose *string* values repeated — on
	// examples/auto-discovery/capture.kshrk, 180 stops with 4 distinct values,
	// one of them 96 times. The client keys the scrubber off those strings, so
	// every duplicate run was a dead zone: stepping forward landed on an
	// identical value and snapped back, making the button a permanent no-op
	// (#257).
	//
	// Second precision is deliberate rather than a concession. Every `at` in
	// this API is RFC3339 seconds — parsed that way, echoed back that way by
	// each handler, rendered by formatTS and by the diff pickers, and accepted
	// from `--at`. Emitting RFC3339Nano would keep more stops but would put
	// nine decimal places into the scrubber label and the diff dropdowns, and
	// would make the stops disagree with the `at` every handler echoes. A stop
	// the rest of the contract cannot address is not a usable stop.
	uniq := make(map[time.Time]struct{})
	add := func(t time.Time) {
		if t.IsZero() {
			return
		}
		uniq[t.UTC().Truncate(time.Second)] = struct{}{}
	}
	for _, entry := range h.Store.Index {
		if entry == nil {
			continue
		}
		for _, t := range entry.Times {
			add(t)
		}
	}
	for _, wi := range h.Store.WatchIndex {
		if wi == nil {
			continue
		}
		for _, t := range wi.Times {
			add(t)
		}
	}

	all := make([]time.Time, 0, len(uniq))
	for t := range uniq {
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
	resp.TotalCount = len(all)

	out := all
	if len(out) > scrubberMaxStops {
		resp.Sampled = true
		out = sampleTimes(out, scrubberMaxStops)
	}
	resp.Timestamps = make([]string, 0, len(out))
	for _, t := range out {
		resp.Timestamps = append(resp.Timestamps, t.Format(time.RFC3339))
	}
	writeJSON(w, http.StatusOK, resp)
}

// sampleTimes picks roughly-evenly-spaced timestamps so the scrubber doesn't
// render a million ticks on long captures.
func sampleTimes(all []time.Time, n int) []time.Time {
	if len(all) <= n || n <= 1 {
		return all
	}
	out := make([]time.Time, 0, n)
	step := float64(len(all)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		idx := int(float64(i) * step)
		if idx >= len(all) {
			idx = len(all) - 1
		}
		out = append(out, all[idx])
	}
	return out
}
