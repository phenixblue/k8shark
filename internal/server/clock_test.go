package server

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

// newTestClock builds a clock whose wall-clock source is driven manually via the
// returned setter, so tests can advance time deterministically.
func newTestClock(t *testing.T, from, to time.Time, speed float64, loop, paused bool) (*ReplayClock, func(d time.Duration)) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0).UTC()
	var wall atomic.Int64
	wall.Store(base.UnixNano())
	c := NewReplayClock(from, to, speed, loop, paused)
	c.now = func() time.Time { return time.Unix(0, wall.Load()).UTC() }
	c.wallAnchor = c.now()
	advance := func(d time.Duration) { wall.Add(int64(d)) }
	return c, advance
}

func TestReplayClock_AdvancesAtSpeed(t *testing.T) {
	from := time.Unix(2_000_000, 0).UTC()
	to := from.Add(10 * time.Second)
	c, advance := newTestClock(t, from, to, 2, false, false)

	if got := c.Now(); !got.Equal(from) {
		t.Fatalf("start position = %s, want %s", got, from)
	}
	advance(1 * time.Second) // 1s wall × 2 = 2s capture
	if got, want := c.Now(), from.Add(2*time.Second); !got.Equal(want) {
		t.Errorf("after 1s at 2x: pos = %s, want %s", got, want)
	}
}

func TestReplayClock_StopsAtEndWithoutLoop(t *testing.T) {
	from := time.Unix(2_000_000, 0).UTC()
	to := from.Add(10 * time.Second)
	c, advance := newTestClock(t, from, to, 1, false, false)

	advance(25 * time.Second)
	pos, epoch, ended := c.Sample()
	if !pos.Equal(to) {
		t.Errorf("pos = %s, want end %s", pos, to)
	}
	if !ended {
		t.Error("expected ended=true past the window")
	}
	if epoch != 0 {
		t.Errorf("epoch = %d, want 0 (no loop)", epoch)
	}
}

func TestReplayClock_LoopWraps(t *testing.T) {
	from := time.Unix(2_000_000, 0).UTC()
	to := from.Add(10 * time.Second)
	c, advance := newTestClock(t, from, to, 1, true, false)

	advance(25 * time.Second) // 2 full wraps + 5s
	pos, epoch, ended := c.Sample()
	if want := from.Add(5 * time.Second); !pos.Equal(want) {
		t.Errorf("looped pos = %s, want %s", pos, want)
	}
	if epoch != 2 {
		t.Errorf("epoch = %d, want 2", epoch)
	}
	if ended {
		t.Error("looping clock should never report ended")
	}
}

func TestReplayClock_PauseResume(t *testing.T) {
	from := time.Unix(2_000_000, 0).UTC()
	to := from.Add(60 * time.Second)
	c, advance := newTestClock(t, from, to, 1, false, false)

	advance(3 * time.Second)
	c.Pause()
	if !c.Paused() {
		t.Fatal("expected paused")
	}
	advance(5 * time.Second) // ignored while paused
	if got, want := c.Now(), from.Add(3*time.Second); !got.Equal(want) {
		t.Errorf("paused pos = %s, want %s", got, want)
	}
	c.Resume()
	advance(2 * time.Second)
	if got, want := c.Now(), from.Add(5*time.Second); !got.Equal(want) {
		t.Errorf("resumed pos = %s, want %s", got, want)
	}
}

func TestReplayClock_Seek(t *testing.T) {
	from := time.Unix(2_000_000, 0).UTC()
	to := from.Add(60 * time.Second)
	c, advance := newTestClock(t, from, to, 1, false, false)

	c.Seek(from.Add(30 * time.Second))
	advance(1 * time.Second)
	if got, want := c.Now(), from.Add(31*time.Second); !got.Equal(want) {
		t.Errorf("after seek: pos = %s, want %s", got, want)
	}
	// Clamp beyond the window.
	c.Seek(to.Add(time.Hour))
	if got := c.Now(); !got.Equal(to) {
		t.Errorf("seek past end: pos = %s, want %s", got, to)
	}
}

func TestReplayClock_SetSpeedPreservesPosition(t *testing.T) {
	from := time.Unix(2_000_000, 0).UTC()
	to := from.Add(60 * time.Second)
	c, advance := newTestClock(t, from, to, 1, false, false)

	advance(3 * time.Second)
	c.SetSpeed(4)
	if got, want := c.Now(), from.Add(3*time.Second); !got.Equal(want) {
		t.Errorf("position jumped on speed change: %s, want %s", got, want)
	}
	advance(1 * time.Second) // now 4x
	if got, want := c.Now(), from.Add(7*time.Second); !got.Equal(want) {
		t.Errorf("after speed change: pos = %s, want %s", got, want)
	}
}

func TestParseSpeed(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"", 1, false},
		{"2x", 2, false},
		{"0.5x", 0.5, false},
		{"3", 3, false},
		{"1.5X", 1.5, false},
		{"0x", 0, true},
		{"-2x", 0, true},
		{"fast", 0, true},
		{"inf", 0, true},
		{"Inf", 0, true},
		{"NaN", 0, true},
		{"1e9", 0, true}, // above the sanity cap
	}
	for _, tc := range cases {
		got, err := parseSpeed(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSpeed(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSpeed(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSpeed(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseReplayWindow(t *testing.T) {
	start := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	meta := capture.CaptureMetadata{CapturedAt: start, CapturedUntil: end}

	// Defaults to the capture bounds.
	from, to, err := parseReplayWindow(meta, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !from.Equal(start) || !to.Equal(end) {
		t.Errorf("defaults = [%s, %s], want [%s, %s]", from, to, start, end)
	}

	// Relative durations resolve against the capture end.
	from, to, err = parseReplayWindow(meta, "-10m", "-1m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !from.Equal(start) || !to.Equal(end.Add(-time.Minute)) {
		t.Errorf("relative window = [%s, %s]", from, to)
	}

	// to must be after from.
	if _, _, err := parseReplayWindow(meta, "-1m", "-5m"); err == nil {
		t.Error("expected error when --to precedes --from")
	}

	// Errors name the offending flag, not "--at".
	_, _, err = parseReplayWindow(meta, "garbage", "")
	if err == nil || !strings.Contains(err.Error(), "--from") {
		t.Errorf("invalid --from error = %v, want it to mention --from", err)
	}

	// Missing capture bounds give an actionable error, not a zero-time compare.
	_, _, err = parseReplayWindow(capture.CaptureMetadata{}, "", "")
	if err == nil || !strings.Contains(err.Error(), "--from") {
		t.Errorf("zero-bounds error = %v, want it to ask for --from", err)
	}
	_, _, err = parseReplayWindow(capture.CaptureMetadata{}, "2026-04-09T10:00:00Z", "")
	if err == nil || !strings.Contains(err.Error(), "--to") {
		t.Errorf("zero end-bound error = %v, want it to ask for --to", err)
	}

	// A relative duration with no capture end bound is an actionable error, not a
	// silent resolve against year 0001.
	_, _, err = parseReplayWindow(capture.CaptureMetadata{}, "-5m", "")
	if err == nil || !strings.Contains(err.Error(), "capture end time is unknown") {
		t.Errorf("relative --from with no end bound: err = %v, want 'capture end time is unknown'", err)
	}
}
