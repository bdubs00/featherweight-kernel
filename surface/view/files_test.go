package view

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// pngBytes start like a PNG file, so the sniffer names the type.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

// writeRun runs an agent whose one tool call writes files through the
// workspace, and returns the agent, the run id, and the workspace root.
func writeRun(t *testing.T, files map[string][]byte) (*fw.Agent, string, string) {
	t.Helper()
	root := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	writer := fw.NewTool(fw.ToolSpec{Name: "writer"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		for name, data := range files {
			if err := fw.FilesFrom(ctx).WriteFile(name, data); err != nil {
				return fw.ErrorResult(err.Error()), nil
			}
		}
		return fw.TextResult("written"), nil
	})
	var turn atomic.Int32
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		if turn.Add(1) == 1 {
			return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: "writer", Input: json.RawMessage(`{}`)}}, StopReason: fw.StopToolUse}, nil
		}
		return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
	}}
	a := newAgent(t, fw.WithConfig(cfg), fw.WithModel(model), fw.WithTools(writer))
	r, err := a.Run(context.Background(), "write")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	return a, r.ID, root
}

func TestFileViews(t *testing.T) {
	ctx := context.Background()
	a, id, root := writeRun(t, map[string][]byte{
		"notes/plan.md": []byte("# Plan\n"),
		"img/dot.png":   pngBytes,
		"bin/blob":      {0, 1, 2, 3},
		"gone.txt":      []byte("bye"),
	})
	files, err := RunFiles(ctx, a, id)
	if err != nil || len(files) != 4 {
		t.Fatalf("RunFiles = %+v, %v", files, err)
	}
	v, err := FileViewOf(ctx, a, id, "notes/plan.md")
	if err != nil || v.Text != "# Plan\n" || v.Op != fw.FileCreated || v.Tool != "writer" || v.Changed || v.MediaType != "text/plain" {
		t.Fatalf("text view = %+v, %v", v, err)
	}
	if v, err := FileViewOf(ctx, a, id, "img/dot.png"); err != nil || v.MediaType != "image/png" || string(v.Data) != string(pngBytes) || v.Text != "" {
		t.Fatalf("image view = %+v, %v", v, err)
	}
	if v, err := FileViewOf(ctx, a, id, "bin/blob"); err != nil || !v.Binary || len(v.Data) != 0 {
		t.Fatalf("binary view = %+v, %v", v, err)
	}
	// A change after the run shows, and the view holds the current content.
	if err := os.WriteFile(filepath.Join(root, "notes", "plan.md"), []byte("# Plan v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := FileViewOf(ctx, a, id, "notes/plan.md"); err != nil || !v.Changed || v.Text != "# Plan v2\n" {
		t.Fatalf("changed view = %+v, %v", v, err)
	}
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if v, err := FileViewOf(ctx, a, id, "gone.txt"); err != nil || !v.Gone || v.Size != 3 {
		t.Fatalf("gone view = %+v, %v", v, err)
	}
	// A path that the run did not change stays closed, with or without dots.
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"other.txt", "notes/../other.txt", "/other.txt"} {
		if _, err := FileViewOf(ctx, a, id, p); !errors.Is(err, ErrNotInRun) {
			t.Errorf("FileViewOf(%q) = %v, want ErrNotInRun", p, err)
		}
	}
	if _, err := FileViewOf(ctx, a, "01NOPE", "notes/plan.md"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Errorf("unknown run = %v", err)
	}
}
