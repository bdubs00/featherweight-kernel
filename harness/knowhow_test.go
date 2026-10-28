package harness

import (
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
)

// TestOpencodeRunContext checks that a hosted run without memory tools
// gets the data blocks of its caller at the start of its first prompt, and
// the sentence that says that such a block holds data in its added system
// text.
func TestOpencodeRunContext(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, answerOnly)
	cfg := testConfig(t, d, ocBlock(d, f))
	a := newAgent(t, cfg, fw.NewMemLedger())
	r, err := a.Start(testCtx(t), "hello", fw.WithRunContext("found", "ISSUE 42"))
	if err != nil {
		t.Fatal(err)
	}
	if r = waitState(t, a, r.ID, terminal); r.State != fw.StateCompleted {
		t.Fatalf("run = %+v", r)
	}
	f.mu.Lock()
	first := f.prompts[0]
	f.mu.Unlock()
	want := fw.ContextBlock("found", "ISSUE 42") + "\n\nhello"
	if first.Text != want || !strings.Contains(first.System, fw.ContextNote) {
		t.Fatalf("first prompt = %q, system = %q", first.Text, first.System)
	}
}

// TestOpencodeHeldBack checks that a hosted run with fw.knowhow off gets
// neither the index of the notes nor the memory section, although the
// configuration has the memory tools and notes exist.
func TestOpencodeHeldBack(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, answerOnly)
	cfg := testConfig(t, d, ocBlock(d, f))
	cfg.Tools.Builtin = []string{"memory", "recall"}
	manyNotes(t, cfg)
	a := newAgent(t, cfg, fw.NewMemLedger())
	r, err := a.Start(testCtx(t), "hello", fw.WithMetadata(map[string]string{fw.MetaKnowhow: fw.KnowhowOff}))
	if err != nil {
		t.Fatal(err)
	}
	if r = waitState(t, a, r.ID, terminal); r.State != fw.StateCompleted {
		t.Fatalf("run = %+v", r)
	}
	f.mu.Lock()
	first := f.prompts[0]
	f.mu.Unlock()
	if first.Text != "hello" || strings.Contains(first.System, memory.SystemSection) || strings.Contains(first.System, fw.ContextNote) {
		t.Fatalf("first prompt = %q, system = %q", first.Text, first.System)
	}
}
