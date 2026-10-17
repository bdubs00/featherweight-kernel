package fw

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngBytes start like a PNG file, so the sniffer names the type.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

// typedModel is a model that reads the media types in types.
type typedModel struct {
	Model
	types []string
}

func (m typedModel) MediaTypes() []string { return m.types }

// attachAgent returns an agent with a workspace at the returned root and a
// model that reads PNG images.
func attachAgent(t *testing.T, s *scriptModel) (*Agent, string) {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	return newTestAgent(t, typedModel{Model: s.Model(), types: []string{"image/png"}}, WithConfig(cfg)), root
}

func TestAttachmentsReachTheModel(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("seen"))
	a, root := attachAgent(t, s)
	r, err := a.Run(ctx, "look at these", WithAttachments(
		Attachment("shot.png", pngBytes),
		Attachment("../../etc/rows.csv", []byte("a,b\n1,2\n")),
		Attachment("blob.bin", []byte{0, 1, 2, 3}),
	))
	mustState(t, r, err, StateCompleted)

	// The model reads the image, the text of the CSV, and a note for the
	// rest. A label names the path of each file.
	msg := s.requests()[0].Messages[0]
	if len(msg.Content) != 5 || msg.Content[0].Text != "look at these" {
		t.Fatalf("first message = %+v", msg.Content)
	}
	if img := msg.Content[2]; img.Type != BlockImage || img.MediaType != "image/png" || !bytes.Equal(img.Data, pngBytes) {
		t.Fatalf("image block = %+v", img)
	}
	if l := msg.Content[1].Text; !strings.Contains(l, `"shot.png"`) || !strings.Contains(l, "attachments/shot.png") {
		t.Fatalf("image label = %q", l)
	}
	if csv := msg.Content[3].Text; !strings.Contains(csv, "attachments/rows.csv") || !strings.HasSuffix(csv, "a,b\n1,2\n") {
		t.Fatalf("text file = %q", csv)
	}
	if note := msg.Content[4].Text; !strings.Contains(note, "attachments/blob.bin") || !strings.HasSuffix(note, "Read it with a tool.") {
		t.Fatalf("note = %q", note)
	}

	// The ledger holds references and records each file, never the bytes.
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var q RunQueuedData
	raw := firstOf(t, evs, EvRunQueued, &q).Data
	if len(q.Files) != 3 || q.Files[0].File.Path != "attachments/shot.png" || q.Files[0].File.MediaType != "image/png" || len(q.Files[0].Data) != 0 {
		t.Fatalf("queued files = %+v", q.Files)
	}
	if bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(pngBytes))) {
		t.Fatal("the ledger holds the bytes of the image")
	}
	if files := RunFiles(evs); len(files) != 3 || files[1].Path != "attachments/rows.csv" || files[1].Tool != "" {
		t.Fatalf("RunFiles = %+v", files)
	}
	if b, err := os.ReadFile(filepath.Join(root, "attachments", "shot.png")); err != nil || !bytes.Equal(b, pngBytes) {
		t.Fatalf("workspace file = %v", err)
	}
}

func TestAttachmentsOnAFollowUp(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("one"), endTurn("two"))
	a, root := attachAgent(t, s)
	r, err := a.Run(ctx, "start", WithAttachments(Attachment("a.txt", []byte("first"))))
	mustState(t, r, err, StateCompleted)

	// The same name with other bytes gets a number. The same bytes keep
	// the path that they have.
	content := []Block{Text("more"), Attachment("a.txt", []byte("second")), Attachment("a.txt", []byte("first"))}
	if err := a.Send(ctx, r.ID, Message{Content: content}, "test"); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	var got []string
	for _, b := range s.requests()[1].Messages[2].Content {
		if b.Type == BlockText {
			got = append(got, b.Text)
		}
	}
	if len(got) != 3 || !strings.Contains(got[1], "attachments/a-2.txt") || !strings.HasSuffix(got[1], "second") || !strings.Contains(got[2], "at attachments/a.txt.") {
		t.Fatalf("follow-up = %q", got)
	}

	// A file that changed or went away says so on the next turn.
	if err := os.WriteFile(filepath.Join(root, "attachments", "a.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "attachments", "a-2.txt")); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("first"))
	ref := func(p string) []Message {
		f := &FileRef{Path: p, Name: "a.txt", MediaType: "text/plain", Size: 5, SHA256: hex.EncodeToString(sum[:])}
		return []Message{{Role: RoleUser, Content: []Block{{Type: BlockFile, File: f}}}}
	}
	if l := a.readFiles(ref("attachments/a.txt"), nil)[0].Content[0].Text; !strings.Contains(l, "It changed after the person attached it.") || !strings.HasSuffix(l, "edited") {
		t.Fatalf("changed file = %q", l)
	}
	if l := a.readFiles(ref("attachments/a-2.txt"), nil)[0].Content[0].Text; !strings.HasSuffix(l, "The file is gone.") {
		t.Fatalf("gone file = %q", l)
	}
}

func TestAttachmentsRefused(t *testing.T) {
	ctx := testCtx(t)
	a, _ := attachAgent(t, newScript())
	many := make([]Block, MaxAttachments+1)
	for i := range many {
		many[i] = Attachment("f.txt", []byte("x"))
	}
	forged := Block{Type: BlockFile, File: &FileRef{Path: ".fw/agents/x.yaml", Name: "x.yaml"}}
	for _, tc := range []struct {
		name  string
		files []Block
		want  error
	}{
		{"denied name", []Block{Attachment(".env", []byte("KEY=1"))}, ErrFileDenied},
		{"no bytes", []Block{Attachment("empty.txt", nil)}, ErrInvalidRequest},
		{"a reference from the caller", []Block{forged}, ErrInvalidRequest},
		{"too many files", many, ErrInvalidRequest},
	} {
		if _, err := a.Start(ctx, "go", WithAttachments(tc.files...)); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestCleanFileName(t *testing.T) {
	long := strings.Repeat("é", 200) + ".png"
	for in, want := range map[string]string{
		"report.pdf":         "report.pdf",
		"../../etc/passwd":   "passwd",
		`C:\Users\me\a.txt`:  "a.txt",
		"  spaced name.md  ": "spaced name.md",
		"bad\x00\nname.txt":  "badname.txt",
		"":                   "file",
		"..":                 "file",
	} {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cleanFileName(long); len(got) > maxAttachmentName || !strings.HasSuffix(got, "é.png") {
		t.Errorf("long name = %q (%d bytes)", got, len(got))
	}
	if got := numberedName("a.tar.gz", 3); got != "a.tar-3.gz" {
		t.Errorf("numberedName = %q", got)
	}
	if got := numberedName(".profile", 2); got != ".profile-2" {
		t.Errorf("numberedName(.profile) = %q", got)
	}
}
