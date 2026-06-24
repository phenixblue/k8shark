package capture

import "testing"

func TestCheckFormatVersion(t *testing.T) {
	t.Run("zero is legacy-compatible", func(t *testing.T) {
		if err := CheckFormatVersion(CaptureMetadata{FormatVersion: 0}); err != nil {
			t.Errorf("pre-versioning archive rejected: %v", err)
		}
	})
	t.Run("current is accepted", func(t *testing.T) {
		if err := CheckFormatVersion(CaptureMetadata{FormatVersion: CurrentFormatVersion}); err != nil {
			t.Errorf("current version rejected: %v", err)
		}
	})
	t.Run("newer is rejected", func(t *testing.T) {
		if err := CheckFormatVersion(CaptureMetadata{FormatVersion: CurrentFormatVersion + 1}); err == nil {
			t.Error("expected error for a newer-than-supported format version")
		}
	})
	t.Run("negative is rejected", func(t *testing.T) {
		if err := CheckFormatVersion(CaptureMetadata{FormatVersion: -1}); err == nil {
			t.Error("expected error for a negative (corrupt) format version")
		}
	})
}
