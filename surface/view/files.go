package view

// This file serves the files that a run changed (docs/ARCHITECTURE.md
// section 7.9). The list comes from the ledger, and the content is the
// current file in the workspace, read through its rules.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// ErrNotInRun is the error for a path that no event of the run lists.
var ErrNotInRun = errors.New("view: the run did not change this file")

// Limits of a file view.
const (
	MaxFileText  = 256 << 10 // bytes of text in a view
	MaxFileImage = 4 << 20   // bytes of an image in a view
)

// FileView is a file that a run changed, as the page shows it. Size is the
// current size, or the recorded size of a file that is gone. Text holds a
// text file, and Data an image. A file that is neither, or that is too
// large, holds no content and sets Binary or Large.
type FileView struct {
	Path      string `json:"path"`
	Op        string `json:"op"`
	Tool      string `json:"tool,omitempty"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType,omitempty"`
	Text      string `json:"text,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Cut       bool   `json:"cut,omitempty"`     // Text holds the first MaxFileText bytes
	Binary    bool   `json:"binary,omitempty"`  // the file is not text or an image
	Large     bool   `json:"large,omitempty"`   // the file is larger than a view holds
	Changed   bool   `json:"changed,omitempty"` // the file changed after the last change of the run
	Gone      bool   `json:"gone,omitempty"`    // the file does not exist now
}

// RunFiles returns the files that a run changed. The read of the run
// authorizes the caller.
func RunFiles(ctx context.Context, a *fw.Agent, id string) ([]fw.RunFile, error) {
	if _, err := a.Get(ctx, id); err != nil {
		return nil, err
	}
	var events []fw.Event
	for e, err := range a.Ledger().Read(ctx, id, 1) {
		if err != nil {
			return nil, fmt.Errorf("view: read the ledger of run %s: %w", id, err)
		}
		if e.Type == fw.EvFileChanged {
			events = append(events, e)
		}
	}
	files := fw.RunFiles(events)
	if files == nil {
		files = []fw.RunFile{}
	}
	return files, nil
}

// ReadRunFile returns the last change of a run to a file and the current
// content of the file. A path that the run did not change is ErrNotInRun.
// A file that the run removed, or that is gone, is fs.ErrNotExist. The
// read holds the whole file, up to tools.files.maxFileBytes.
func ReadRunFile(ctx context.Context, a *fw.Agent, id, name string) (fw.RunFile, []byte, error) {
	f, ws, err := runFile(ctx, a, id, name)
	if err != nil {
		return f, nil, err
	}
	data, err := fs.ReadFile(ws, f.Path)
	return f, data, err
}

// BoundError is the error of ReadRunFileUpTo for a file larger than the
// bound. Size is the size that the workspace reports, or -1 when the file
// grew past the bound during the read.
type BoundError struct {
	Size  int64
	Bound int64
}

func (e *BoundError) Error() string {
	if e.Size < 0 {
		return fmt.Sprintf("view: the file is larger than the bound of %d bytes", e.Bound)
	}
	return fmt.Sprintf("view: the file has %d bytes, more than the bound of %d bytes", e.Size, e.Bound)
}

// ReadRunFileUpTo is ReadRunFile for at most bound bytes. The reader rules
// are those of ReadRunFile. A file larger than bound is a *BoundError: the
// size from Stat refuses it before the read, and the read takes at most
// bound+1 bytes, so a file that grows after Stat stays bounded too.
func ReadRunFileUpTo(ctx context.Context, a *fw.Agent, id, name string, bound int64) (fw.RunFile, []byte, error) {
	f, ws, err := runFile(ctx, a, id, name)
	if err != nil {
		return f, nil, err
	}
	info, err := ws.Stat(f.Path)
	if err != nil {
		return f, nil, err
	}
	if info.Size() > bound {
		return f, nil, &BoundError{Size: info.Size(), Bound: bound}
	}
	file, err := ws.Open(f.Path)
	if err != nil {
		return f, nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, bound+1))
	if err != nil {
		return f, nil, err
	}
	if int64(len(data)) > bound {
		return f, nil, &BoundError{Size: -1, Bound: bound}
	}
	return f, data, nil
}

// runFile returns the last change of a run to a file and the workspace
// that holds the file. The read of the run authorizes the caller. A path
// that the run did not change is ErrNotInRun. A file that the run removed,
// or a process without a workspace, is fs.ErrNotExist.
func runFile(ctx context.Context, a *fw.Agent, id, name string) (fw.RunFile, fw.Files, error) {
	files, err := RunFiles(ctx, a, id)
	if err != nil {
		return fw.RunFile{}, nil, err
	}
	name = strings.TrimPrefix(name, "/")
	for _, f := range files {
		if f.Path != name {
			continue
		}
		if f.Op == fw.FileRemoved {
			return f, nil, fs.ErrNotExist
		}
		ws := a.Files()
		if ws == nil {
			return f, nil, fs.ErrNotExist
		}
		return f, ws, nil
	}
	return fw.RunFile{}, nil, ErrNotInRun
}

// FileViewOf reads a file of a run for the page. A gone file and a file
// that is too large are views, not errors.
func FileViewOf(ctx context.Context, a *fw.Agent, id, name string) (FileView, error) {
	f, data, err := ReadRunFile(ctx, a, id, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return FileView{Path: f.Path, Op: f.Op, Tool: f.Tool, Size: f.Size, Gone: true}, nil
	case errors.Is(err, fw.ErrFileTooLarge):
		v := FileView{Path: f.Path, Op: f.Op, Tool: f.Tool, Size: f.Size, Large: true}
		if info, err := a.Files().Stat(f.Path); err == nil {
			v.Size = info.Size()
		}
		return v, nil
	case err != nil:
		return FileView{}, err
	}
	return viewOf(f, data), nil
}

// viewOf builds the view of a file from its content.
func viewOf(f fw.RunFile, data []byte) FileView {
	v := FileView{Path: f.Path, Op: f.Op, Tool: f.Tool, Size: int64(len(data)), MediaType: MediaType(data)}
	if f.SHA256 != "" {
		sum := sha256.Sum256(data)
		v.Changed = hex.EncodeToString(sum[:]) != f.SHA256
	}
	switch {
	case isText(data):
		v.Text, v.Cut = cutText(string(data), MaxFileText)
	case isImage(v.MediaType) && len(data) <= MaxFileImage:
		v.Data = data
	case isImage(v.MediaType):
		v.Large = true
	default:
		v.Binary = true
	}
	return v
}

// MediaType sniffs the type of content with the rules of the WHATWG.
func MediaType(data []byte) string {
	mt := http.DetectContentType(data)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return mt
}

// isText reports whether data is UTF-8 text without a NUL byte. It reads
// the first 8000 bytes, as git does.
func isText(data []byte) bool {
	head := data[:min(len(data), 8000)]
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	// The cut can split the last rune of the head.
	for i := 0; len(head) < len(data) && i < utf8.UTFMax-1 && !utf8.Valid(head); i++ {
		head = head[:len(head)-1]
	}
	return utf8.Valid(head)
}

// isImage reports whether the page shows a media type as an image. SVG is
// text, so the page shows its source and never runs it.
func isImage(mt string) bool {
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return false
}

// cutText shortens s to n bytes at a rune boundary.
func cutText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
