package capture

import "github.com/phenixblue/k8shark/internal/archive/format"

// CurrentFormatVersion and the schema types below are defined in
// internal/archive/format (a stdlib-only leaf package) so internal/archive
// can reference them directly without an import cycle — internal/capture
// imports internal/archive, not the other way around. These aliases keep
// every existing capture.X call site working unchanged.
const CurrentFormatVersion = format.CurrentFormatVersion

// CheckFormatVersion reports whether an archive can be read by this build.
// See format.CheckFormatVersion for the full doc.
func CheckFormatVersion(m CaptureMetadata) error {
	return format.CheckFormatVersion(m)
}

type (
	Record          = format.Record
	CaptureMetadata = format.CaptureMetadata
	IndexEntry      = format.IndexEntry
	Index           = format.Index
	WatchIndexEntry = format.WatchIndexEntry
	WatchIndex      = format.WatchIndex
)
