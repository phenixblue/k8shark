package server

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ReplayClock maps wall-clock time onto capture time at a configurable speed,
// advancing a position within a [from, to] window. It is the heart of replay
// mode: LIST/GET reconstruct state as-of the clock's current position, and the
// watch stream emits captured events as the clock crosses their timestamps.
//
// A single global clock backs a replay server (phase 1). It supports pause,
// resume, seek, and speed changes so later phases can expose transport
// controls; the streaming path polls the clock and adapts automatically.
type ReplayClock struct {
	mu sync.Mutex

	from  time.Time
	to    time.Time
	speed float64
	loop  bool

	paused        bool
	wallAnchor    time.Time // wall-clock instant the current segment started
	captureAnchor time.Time // capture-time position at wallAnchor (within [from,to])
	baseEpoch     int       // loop wraps accumulated before the current segment
	seekGen       int       // increments on each Seek, so watchers can restart

	// parkedAtEnd is set by ParkAtWindowEnd: the clock is paused and displaying
	// the window end for preview purposes only, not because a replay actually
	// played through to completion. It suppresses the "ended" report (nothing
	// has played yet) and makes the next Resume rewind to from first — without
	// it, resuming a clock anchored at to with looping disabled would
	// immediately re-clamp to to and appear to end on the spot. Seek or a
	// second Resume clears it, since the user has now taken an explicit action.
	parkedAtEnd bool

	events atomic.Int64

	now func() time.Time // injectable wall clock (tests)
}

// NewReplayClock creates a clock over [from, to] advancing at speed (a factor:
// 1 = real time, 2 = twice as fast, 0.5 = half). When loop is true the position
// wraps back to from on reaching to; otherwise it stops at to. When paused the
// clock does not advance until Resume is called. The initial position is
// always from — callers that want a paused clock parked elsewhere (e.g. the
// Web UI defaulting to the window end) should Seek() right after construction.
func NewReplayClock(from, to time.Time, speed float64, loop, paused bool) *ReplayClock {
	if !to.After(from) {
		to = from // degenerate window; Sample reports ended immediately
	}
	if speed <= 0 {
		speed = 1
	}
	c := &ReplayClock{
		from:          from,
		to:            to,
		speed:         speed,
		loop:          loop,
		paused:        paused,
		captureAnchor: from,
		now:           time.Now,
	}
	c.wallAnchor = c.now()
	return c
}

// sample computes the current position. Callers must hold c.mu.
func (c *ReplayClock) sample() (pos time.Time, epoch int, ended bool) {
	if c.paused {
		ended := !c.parkedAtEnd && !c.loop && !c.captureAnchor.Before(c.to)
		return c.captureAnchor, c.baseEpoch, ended
	}
	span := c.to.Sub(c.from)
	if span <= 0 {
		return c.to, c.baseEpoch, !c.loop
	}
	// Clamp the scaled elapsed time to the representable Duration range before
	// converting: wallDelta × speed can exceed int64 nanoseconds over a long run
	// at a high speed, and an overflowing conversion would produce a negative
	// elapsed (making the clock jump backward or spin).
	scaled := float64(c.now().Sub(c.wallAnchor)) * c.speed
	var elapsed time.Duration
	switch {
	case scaled <= 0:
		elapsed = 0
	case scaled >= float64(math.MaxInt64):
		elapsed = time.Duration(math.MaxInt64)
	default:
		elapsed = time.Duration(scaled)
	}
	raw := c.captureAnchor.Add(elapsed)
	if raw.Before(c.to) {
		return raw, c.baseEpoch, false
	}
	if !c.loop {
		return c.to, c.baseEpoch, true
	}
	// Loop: position `to` is the end of the current epoch, not the start of the
	// next — wrap only once raw is strictly after `to`, so events timestamped
	// exactly at the window end aren't skipped by an early epoch change.
	if !raw.After(c.to) {
		return c.to, c.baseEpoch, false
	}
	over := raw.Sub(c.from)
	wraps := int(over / span)
	rem := over % span
	return c.from.Add(rem), c.baseEpoch + wraps, false
}

// reanchor freezes the current position into the anchors so a subsequent state
// change (pause, speed) does not jump the clock. Callers must hold c.mu.
func (c *ReplayClock) reanchor() {
	p, e, _ := c.sample()
	c.captureAnchor = p
	c.baseEpoch = e
	c.wallAnchor = c.now()
}

// Now returns the clock's current capture-time position.
func (c *ReplayClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, _, _ := c.sample()
	return p
}

// Sample returns the current position, the loop epoch (which increments each
// time the window wraps under loop), and whether the clock has reached the end
// (only possible when loop is disabled).
func (c *ReplayClock) Sample() (pos time.Time, epoch int, ended bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sample()
}

// Pause stops the clock at its current position.
func (c *ReplayClock) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		return
	}
	c.reanchor()
	c.paused = true
}

// Resume restarts a paused clock from where it stopped — unless it was merely
// parked at the window end for preview (ParkAtWindowEnd), in which case it
// rewinds to from first, so Play actually plays the window rather than
// re-reporting the end it was already sitting at.
func (c *ReplayClock) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused {
		return
	}
	if c.parkedAtEnd {
		c.captureAnchor = c.from
		c.parkedAtEnd = false
	}
	c.paused = false
	c.wallAnchor = c.now()
}

// Paused reports whether the clock is currently paused.
func (c *ReplayClock) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// Seek jumps the position to t, clamped to [from, to]. The loop epoch is
// unchanged. Clears any pending ParkAtWindowEnd preview state — an explicit
// seek is the user choosing a position, not a request to preview the end.
func (c *ReplayClock) Seek(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.parkedAtEnd = false
	if t.Before(c.from) {
		t = c.from
	}
	if t.After(c.to) {
		t = c.to
	}
	c.captureAnchor = t
	c.wallAnchor = c.now()
	c.seekGen++
}

// SeekGen returns a counter that increments on every Seek. A watch stream polls
// it to detect a seek (which, unlike a loop wrap, doesn't change the epoch) and
// restart from the new position.
func (c *ReplayClock) SeekGen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seekGen
}

// ParkAtWindowEnd positions a paused clock at the window end for preview —
// the Web UI's default, so a client reading through the clock sees the most
// complete captured state rather than a capture's typically-sparse opening
// moments. It does not count as the replay having ended: Sample reports
// ended=false, and the next Resume rewinds to the window start and plays
// forward normally, rather than immediately re-reporting the end it was
// parked at. A no-op if the clock isn't paused.
func (c *ReplayClock) ParkAtWindowEnd() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused {
		return
	}
	c.captureAnchor = c.to
	c.parkedAtEnd = true
}

// SetSpeed changes the speed factor, preserving the current position.
func (c *ReplayClock) SetSpeed(s float64) {
	if s <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reanchor()
	c.speed = s
}

// Speed returns the current speed factor.
func (c *ReplayClock) Speed() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.speed
}

// Window returns the immutable [from, to] replay window.
func (c *ReplayClock) Window() (time.Time, time.Time) { return c.from, c.to }

// Loop reports whether the clock wraps at the end of the window.
func (c *ReplayClock) Loop() bool { return c.loop }

// AddEvents increments the emitted-event counter (surfaced on the status line).
func (c *ReplayClock) AddEvents(n int64) { c.events.Add(n) }

// EventsEmitted returns how many watch events have been streamed so far.
func (c *ReplayClock) EventsEmitted() int64 { return c.events.Load() }

// ParseSpeed parses a speed factor such as "2x", "0.5x", "3", or "1.5x" for
// callers outside this package (e.g. the UI transport control endpoint). An
// empty string means real time (1x).
func ParseSpeed(raw string) (float64, error) { return parseSpeed(raw) }

// parseSpeed parses a speed factor such as "2x", "0.5x", "3", or "1.5x".
// An empty string means real time (1x).
func parseSpeed(raw string) (float64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 1, nil
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "x"), "X")
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --speed %q: use forms like 2x, 0.5x, 3", raw)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid --speed %q: must be a finite number", raw)
	}
	if f <= 0 {
		return 0, fmt.Errorf("invalid --speed %q: must be greater than 0", raw)
	}
	// Cap absurd values so speed × elapsed can't overflow time.Duration later.
	const maxSpeed = 1e6
	if f > maxSpeed {
		return 0, fmt.Errorf("invalid --speed %q: must be at most %g", raw, maxSpeed)
	}
	return f, nil
}
