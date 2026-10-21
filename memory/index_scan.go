//go:build !memory_sqlite

package memory

// IndexDriver names the index of the notes that this build carries.
const IndexDriver = "scan"

// newNoteIndex returns no index. This build scans the notes for a recall.
func newNoteIndex(string) noteIndex { return nil }
