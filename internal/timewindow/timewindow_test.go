package timewindow

import (
	"strings"
	"testing"
	"time"
)

var (
	capturedAt    = time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	capturedUntil = time.Date(2026, 4, 10, 10, 10, 0, 0, time.UTC)
)

func TestParseAt_Empty(t *testing.T) {
	at, err := ParseAt("", capturedAt, capturedUntil, "--at")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !at.IsZero() {
		t.Errorf("at = %v, want zero", at)
	}
}

func TestParseAt_RFC3339(t *testing.T) {
	want := capturedAt.Add(5 * time.Minute)
	at, err := ParseAt(want.Format(time.RFC3339), capturedAt, capturedUntil, "--at")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !at.Equal(want) {
		t.Errorf("at = %v, want %v", at, want)
	}
}

func TestParseAt_RelativeDuration(t *testing.T) {
	at, err := ParseAt("-2m", capturedAt, capturedUntil, "--at")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := capturedUntil.Add(-2 * time.Minute)
	if !at.Equal(want) {
		t.Errorf("at = %v, want %v (anchored to capturedUntil)", at, want)
	}
}

func TestParseAt_RelativeDuration_UnknownEnd_Errors(t *testing.T) {
	_, err := ParseAt("-5m", capturedAt, time.Time{}, "--at")
	if err == nil {
		t.Fatal("expected an error when capturedUntil is zero")
	}
	if !strings.Contains(err.Error(), "capture end time is unknown") {
		t.Errorf("error = %v, want it to mention the unknown capture end time", err)
	}
}

func TestParseAt_BeforeCaptureStart_Errors(t *testing.T) {
	_, err := ParseAt(capturedAt.Add(-time.Minute).Format(time.RFC3339), capturedAt, capturedUntil, "--at")
	if err == nil {
		t.Fatal("expected an error for a time before the capture window")
	}
	if !strings.Contains(err.Error(), "before capture start") {
		t.Errorf("error = %v, want it to mention 'before capture start'", err)
	}
}

func TestParseAt_AfterCaptureEnd_Errors(t *testing.T) {
	_, err := ParseAt(capturedUntil.Add(time.Minute).Format(time.RFC3339), capturedAt, capturedUntil, "--at")
	if err == nil {
		t.Fatal("expected an error for a time after the capture window")
	}
	if !strings.Contains(err.Error(), "after capture end") {
		t.Errorf("error = %v, want it to mention 'after capture end'", err)
	}
}

func TestParseAt_MalformedValue_Errors(t *testing.T) {
	_, err := ParseAt("not-a-time", capturedAt, capturedUntil, "--at")
	if err == nil {
		t.Fatal("expected an error for an unparseable value")
	}
	if !strings.Contains(err.Error(), "--at") {
		t.Errorf("error = %v, want it to mention the flag name", err)
	}
}

func TestParseAt_UnknownBounds_SkipsThatSideOfTheCheck(t *testing.T) {
	// A far-future absolute time is fine when both bounds are unknown — no
	// window to be outside of.
	at, err := ParseAt("2099-01-01T00:00:00Z", time.Time{}, time.Time{}, "--at")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if at.Year() != 2099 {
		t.Errorf("at = %v, want year 2099", at)
	}
}

func TestParseWindow_DefaultsToCaptureBounds(t *testing.T) {
	from, to, err := ParseWindow("", "", capturedAt, capturedUntil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !from.Equal(capturedAt) || !to.Equal(capturedUntil) {
		t.Errorf("from,to = %v,%v want %v,%v", from, to, capturedAt, capturedUntil)
	}
}

func TestParseWindow_ExplicitRelativeBounds(t *testing.T) {
	from, to, err := ParseWindow("-10m", "-1m", capturedAt.Add(-time.Hour), capturedUntil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := capturedUntil.Add(-10 * time.Minute); !from.Equal(want) {
		t.Errorf("from = %v, want %v", from, want)
	}
	if want := capturedUntil.Add(-1 * time.Minute); !to.Equal(want) {
		t.Errorf("to = %v, want %v", to, want)
	}
}

func TestParseWindow_ToBeforeFrom_Errors(t *testing.T) {
	_, _, err := ParseWindow("-1m", "-10m", capturedAt, capturedUntil)
	if err == nil {
		t.Fatal("expected an error when --to resolves before --from")
	}
	if !strings.Contains(err.Error(), "must be after") {
		t.Errorf("error = %v, want it to mention the ordering requirement", err)
	}
}

func TestParseWindow_UnknownStart_RequiresExplicitFrom(t *testing.T) {
	_, _, err := ParseWindow("", "-1m", time.Time{}, capturedUntil)
	if err == nil {
		t.Fatal("expected an error when the capture start is unknown and --from is unset")
	}
	if !strings.Contains(err.Error(), "--from") {
		t.Errorf("error = %v, want it to mention --from", err)
	}
}

func TestParseWindow_UnknownEnd_RequiresExplicitTo(t *testing.T) {
	_, _, err := ParseWindow("2026-04-10T10:00:00Z", "", capturedAt, time.Time{})
	if err == nil {
		t.Fatal("expected an error when the capture end is unknown and --to is unset")
	}
	if !strings.Contains(err.Error(), "--to") {
		t.Errorf("error = %v, want it to mention --to", err)
	}
}
