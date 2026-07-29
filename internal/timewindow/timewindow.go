// Package timewindow parses the CLI's time-point and time-window flags
// (--at, --from/--to) against a capture's [CapturedAt, CapturedUntil] bounds.
// It is the single implementation shared by cmd (diagnose, query), internal/diff,
// and internal/server (open/replay/ui) — previously each had its own copy, and
// only one of the three guarded a relative duration against an unknown capture
// end time (#221).
package timewindow

import (
	"fmt"
	"time"
)

// ParseAt parses raw as an RFC3339 timestamp or a duration relative to
// capturedUntil (e.g. "-5m"), validating the result falls within
// [capturedAt, capturedUntil]. Either bound may be the zero Time (unknown),
// in which case that side of the check is skipped. Empty raw returns the
// zero Time with no error, meaning "unspecified". flag is the flag name used
// in error messages, e.g. "--at" or "--from".
func ParseAt(raw string, capturedAt, capturedUntil time.Time, flag string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}

	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		d, derr := time.ParseDuration(raw)
		if derr != nil {
			return time.Time{}, fmt.Errorf("parsing %s %q: must be RFC3339 or a relative duration like -5m", flag, raw)
		}
		// A relative duration is anchored to the capture end; without a known
		// end we'd silently resolve against year 0001, so require an
		// absolute time instead.
		if capturedUntil.IsZero() {
			return time.Time{}, fmt.Errorf("parsing %s %q: capture end time is unknown; use an absolute RFC3339 time", flag, raw)
		}
		at = capturedUntil.Add(d)
	}

	// Reject times outside the capture window — otherwise reconstruction
	// returns 404s and callers misleadingly report empty results.
	if !capturedAt.IsZero() && at.Before(capturedAt) {
		return time.Time{}, fmt.Errorf("parsing %s %q: requested time %s is before capture start %s",
			flag, raw, at.Format(time.RFC3339), capturedAt.Format(time.RFC3339))
	}
	if !capturedUntil.IsZero() && at.After(capturedUntil) {
		return time.Time{}, fmt.Errorf("parsing %s %q: requested time %s is after capture end %s",
			flag, raw, at.Format(time.RFC3339), capturedUntil.Format(time.RFC3339))
	}
	return at, nil
}

// ParseWindow resolves the [--from, --to] window: an empty fromRaw/toRaw
// defaults to the capture's own bounds; a non-empty value is parsed via
// ParseAt. Returns an error if either bound is unknown and left unspecified,
// or if the resolved window is empty or inverted.
func ParseWindow(fromRaw, toRaw string, capturedAt, capturedUntil time.Time) (from, to time.Time, err error) {
	from = capturedAt
	if fromRaw != "" {
		from, err = ParseAt(fromRaw, capturedAt, capturedUntil, "--from")
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	to = capturedUntil
	if toRaw != "" {
		to, err = ParseAt(toRaw, capturedAt, capturedUntil, "--to")
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if from.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf("capture start time is unknown; specify an explicit --from")
	}
	if to.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf("capture end time is unknown; specify an explicit --to")
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("--to %s must be after --from %s", to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	return from, to, nil
}
