package builtin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// memoryResultCutLedger loses the first tool result and every later write,
// as a process that stops after the memory file changed but before the result
// reached its ledger. The underlying file ledger keeps the real request.
type memoryResultCutLedger struct {
	fw.Ledger
	cut atomic.Bool
	hit chan struct{}
}

func (l *memoryResultCutLedger) Append(ctx context.Context, e fw.Event) (uint64, error) {
	if e.Type == fw.EvToolResponded && l.cut.CompareAndSwap(false, true) {
		close(l.hit)
	}
	if l.cut.Load() {
		return 0, errors.New("memory recovery test: process stopped before recording result")
	}
	return l.Ledger.Append(ctx, e)
}

func memoryRecoveryAgent(t *testing.T, cfg *fw.Config, ledger fw.Ledger, model fw.Model) *fw.Agent {
	t.Helper()
	memory, err := fw.NewBuiltinTool("memory", cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := fw.New(
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithLedger(ledger),
		fw.WithBus(fw.NewMemBus()),
		fw.WithModel(model),
		fw.WithTools(memory),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeMemoryRecoveryAgent(a) })
	return a
}

func closeMemoryRecoveryAgent(a *fw.Agent) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a.Close(ctx)
}

// TestMemoryFileLedgerRecovery checks that an insert or a str_replace that
// a restart cut after its write and before its result runs again at once,
// without an approval, and returns the result of the first run without a
// second edit: the tool recorded the call before it wrote
// (docs/KNOWHOW.md section 4.3).
func TestMemoryFileLedgerRecovery(t *testing.T) {
	for _, command := range []struct {
		name, input, once string
	}{
		{
			name:  "insert",
			input: `{"command":"insert","path":"/memories/note.txt","insert_line":0,"insert_text":"inserted"}`,
			once:  "inserted\noriginal\n",
		},
		{
			// The old text still appears exactly once after the first edit,
			// so a second replacement would change the file again.
			name:  "str_replace retaining original",
			input: `{"command":"str_replace","path":"/memories/note.txt","old_str":"original","new_str":"original expanded"}`,
			once:  "original expanded\n",
		},
	} {
		t.Run(command.name, func(t *testing.T) {
			ctx := restartCtx(t)
			root := t.TempDir()
			cfg := fw.DefaultConfig()
			cfg.Home = filepath.Join(root, "home")
			cfg.Tools.Files.Root = root
			cfg.Model = "fake/test"
			dir := filepath.Join(cfg.Home, "memory", "agents", "default")
			note := filepath.Join(dir, "note.txt")
			writeNote(t, dir, "note.txt", "original\n", 0)
			ledgerRoot := filepath.Join(root, "runs")
			original := openRunsAt(t, ledgerRoot)
			cut := &memoryResultCutLedger{Ledger: original, hit: make(chan struct{})}
			first := newRestartScript(&fw.Response{
				StopReason: fw.StopToolUse,
				Content:    []fw.Block{toolUseOf("memory-1", "memory", command.input)},
			})
			a := memoryRecoveryAgent(t, cfg, cut, first.Model())
			r, err := a.Start(ctx, "edit my note")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-cut.hit:
			case <-ctx.Done():
				t.Fatal("memory call never reached its unrecorded result")
			}
			closeMemoryRecoveryAgent(a)
			if got := readFile(t, note); got != command.once {
				t.Fatalf("file after interrupted call = %q, want %q", got, command.once)
			}
			events := ledgerOf(t, a, r.ID)
			if countOfType(events, fw.EvToolRequested) != 1 || countOfType(events, fw.EvToolResponded) != 0 {
				t.Fatalf("interrupted call did not leave one request without a result: %+v", events)
			}

			// Reopen the file ledger with a fresh agent and a fresh memory tool.
			// The wrapper exposes only Ledger, so close its file owner here.
			if err := original.Close(); err != nil {
				t.Fatal(err)
			}
			second := newRestartScript(endTurnOf("done"))
			b := memoryRecoveryAgent(t, cfg, openRunsAt(t, ledgerRoot), second.Model())
			r, err = b.WaitFor(ctx, r.ID, func(r *fw.Run) bool { return r.State.Terminal() })
			if err != nil || r.State != fw.StateCompleted {
				t.Fatalf("recovery = %+v, err = %v; want a run that goes on by itself", r, err)
			}
			if got := readFile(t, note); got != command.once {
				t.Fatalf("the repeat edited the note again: %q", got)
			}
			events = ledgerOf(t, b, r.ID)
			if n := countOfType(events, fw.EvApprovalRequested); n != 0 {
				t.Fatalf("the repeat asked for %d approvals", n)
			}
			var res fw.ToolRespondedData
			for _, e := range events {
				if e.Type == fw.EvToolResponded {
					if err := e.Decode(&res); err != nil {
						t.Fatal(err)
					}
				}
			}
			if res.IsError || !strings.Contains(fw.TextOf(res.Content), "has been edited") {
				t.Fatalf("the result of the repeat = %+v", res)
			}
		})
	}
}

// TestMemoryFileLedgerRecoverySafeCommands checks that a view or a create
// that a restart cut before its result runs again at once, without an
// approval: the memory tool marks these calls safe to repeat
// (fw.CallIdempotency), so an unattended run goes on by itself.
func TestMemoryFileLedgerRecoverySafeCommands(t *testing.T) {
	for _, command := range []struct {
		name, input, want string
	}{
		{name: "view", input: `{"command":"view","path":"/memories/note.txt"}`, want: "original\n"},
		{name: "create", input: `{"command":"create","path":"/memories/note.txt","file_text":"created\n"}`, want: "created\n"},
	} {
		t.Run(command.name, func(t *testing.T) {
			ctx := restartCtx(t)
			root := t.TempDir()
			cfg := fw.DefaultConfig()
			cfg.Home = filepath.Join(root, "home")
			cfg.Tools.Files.Root = root
			cfg.Model = "fake/test"
			dir := filepath.Join(cfg.Home, "memory", "agents", "default")
			note := filepath.Join(dir, "note.txt")
			writeNote(t, dir, "note.txt", "original\n", 0)
			ledgerRoot := filepath.Join(root, "runs")
			original := openRunsAt(t, ledgerRoot)
			cut := &memoryResultCutLedger{Ledger: original, hit: make(chan struct{})}
			first := newRestartScript(&fw.Response{
				StopReason: fw.StopToolUse,
				Content:    []fw.Block{toolUseOf("memory-1", "memory", command.input)},
			})
			a := memoryRecoveryAgent(t, cfg, cut, first.Model())
			r, err := a.Start(ctx, "read my note")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-cut.hit:
			case <-ctx.Done():
				t.Fatal("memory call never reached its unrecorded result")
			}
			closeMemoryRecoveryAgent(a)
			if err := original.Close(); err != nil {
				t.Fatal(err)
			}

			second := newRestartScript(endTurnOf("done"))
			b := memoryRecoveryAgent(t, cfg, openRunsAt(t, ledgerRoot), second.Model())
			r, err = b.WaitFor(ctx, r.ID, func(r *fw.Run) bool { return r.State.Terminal() || r.State == fw.StateInputRequired })
			if err != nil || r.State != fw.StateCompleted {
				t.Fatalf("recovery = %+v, err = %v; want a completed run without an approval", r, err)
			}
			if got := readFile(t, note); got != command.want {
				t.Fatalf("file after the rerun = %q, want %q", got, command.want)
			}
			events := ledgerOf(t, b, r.ID)
			if n := countOfType(events, fw.EvApprovalRequested); n != 0 {
				t.Fatalf("%d approvals for a call that is safe to repeat", n)
			}
			if n := countOfType(events, fw.EvToolResponded); n != 1 {
				t.Fatalf("%d tool results, want 1", n)
			}
			for _, e := range events {
				if e.Type == fw.EvToolResponded {
					var data fw.ToolRespondedData
					if err := e.Decode(&data); err != nil {
						t.Fatal(err)
					}
					if data.IsError {
						t.Fatalf("the rerun failed: %+v", data)
					}
				}
			}
		})
	}
}
