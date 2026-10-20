package fw

import "testing"

// A hold keeps the process busy until its release, and a second release
// changes nothing.
func TestHold(t *testing.T) {
	a := newTestAgent(t, nil)
	if a.Holds() != 0 {
		t.Fatalf("holds at start = %d", a.Holds())
	}
	release := a.Hold()
	other := a.Hold()
	if a.Holds() != 2 {
		t.Fatalf("holds = %d, want 2", a.Holds())
	}
	release()
	release()
	if a.Holds() != 1 {
		t.Fatalf("holds after a double release = %d, want 1", a.Holds())
	}
	other()
	if a.Holds() != 0 {
		t.Fatalf("holds at the end = %d", a.Holds())
	}
}
