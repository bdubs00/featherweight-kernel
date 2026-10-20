//go:build !linux

package fw

// MissingToolCaps returns nil on this platform, where root has every
// right that tools.user needs.
func MissingToolCaps() []string { return nil }
