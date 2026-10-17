package fw

// This file holds attachments: files that a person sends with a message
// (docs/ARCHITECTURE.md section 7.10). The kernel keeps the bytes in the
// workspace under attachments/, and the ledger holds a FileRef. When the
// kernel builds a model request, it reads each file again. The model gets
// the bytes when it reads the media type, the text of a small text file,
// or a note that names the path.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AttachmentsDir is the directory of the workspace that holds attachments.
const AttachmentsDir = "attachments"

// Limits of attachments. The rules of tools.files also apply to each file,
// maxFileBytes included.
const (
	MaxAttachments     = 20       // files in one message
	MaxAttachmentBytes = 32 << 20 // bytes of the files of one message
	maxInlineText      = 256 << 10
	maxAttachmentName  = 255
)

// FileRef names a file that a person attached. Path is the real name in
// the workspace. MediaType is sniffed from the bytes, never taken from the
// caller.
type FileRef struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// Attachment returns a file block that carries the bytes of a new file.
// Start and Send keep the bytes in the workspace and replace the block with
// a reference, so the ledger never holds the bytes.
func Attachment(name string, data []byte) Block {
	return Block{Type: BlockFile, File: &FileRef{Name: name}, Data: data}
}

// MediaTyper is a Model that names the media types that it reads in a
// request, such as image/png or application/pdf. A model middleware passes
// the question on when it has the method Unwrap() Model.
type MediaTyper interface {
	MediaTypes() []string
}

// MediaTypesOf returns the media types that a model reads, or nil.
func MediaTypesOf(m Model) []string {
	for m != nil {
		if t, ok := m.(MediaTyper); ok {
			return t.MediaTypes()
		}
		u, ok := m.(interface{ Unwrap() Model })
		if !ok {
			return nil
		}
		m = u.Unwrap()
	}
	return nil
}

// SniffMediaType returns the media type of data by the rules of the WHATWG,
// without parameters.
func SniffMediaType(data []byte) string {
	mt := http.DetectContentType(data)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return mt
}

// keepFiles writes the bytes of each file block into the workspace. It
// returns the blocks with references in place of the bytes, and the
// changes to record. Only the kernel makes a reference, so a file block
// without bytes is an error.
func (a *Agent) keepFiles(blocks []Block) ([]Block, []FileChangedData, error) {
	n, total := 0, 0
	for _, b := range blocks {
		if b.Type == BlockFile {
			n++
			total += len(b.Data)
		}
	}
	switch {
	case n == 0:
		return blocks, nil, nil
	case n > MaxAttachments:
		return nil, nil, fmt.Errorf("%w: a message holds %d files, and the limit is %d", ErrInvalidRequest, n, MaxAttachments)
	case total > MaxAttachmentBytes:
		return nil, nil, fmt.Errorf("%w: the files of a message hold %d bytes, and the limit is %d", ErrInvalidRequest, total, MaxAttachmentBytes)
	case a.files == nil:
		return nil, nil, fmt.Errorf("%w: this agent has no workspace for files", ErrInvalidRequest)
	}
	var changes []FileChangedData
	ws := a.files.bind(func(d FileChangedData) { changes = append(changes, d) })
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != BlockFile {
			out = append(out, b)
			continue
		}
		name := ""
		if b.File != nil {
			name = b.File.Name
		}
		name = cleanFileName(name)
		if len(b.Data) == 0 {
			return nil, nil, fmt.Errorf("%w: the file %q has no bytes", ErrInvalidRequest, name)
		}
		ref, kept, err := attach(ws, name, b.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: the file %q is not accepted: %w", ErrInvalidRequest, name, err)
		}
		if kept {
			// The same file is there already, so the workspace recorded no
			// write. The run still lists it.
			changes = append(changes, FileChangedData{Op: FileCreated, Path: ref.Path, Size: ref.Size, SHA256: ref.SHA256})
		}
		out = append(out, Block{Type: BlockFile, File: &ref})
	}
	return out, changes, nil
}

// attach writes data to attachments/<name>, or to a numbered name when a
// different file has that name. It reports whether the same file was there
// already.
func attach(ws *workspace, name string, data []byte) (FileRef, bool, error) {
	sum := sha256.Sum256(data)
	ref := FileRef{Name: name, MediaType: SniffMediaType(data), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	for i := 1; i <= 100; i++ {
		p := path.Join(AttachmentsDir, numberedName(name, i))
		info, err := ws.Stat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := ws.WriteFile(p, data); err != nil {
				return FileRef{}, false, err
			}
		case err != nil:
			return FileRef{}, false, err
		case info.IsDir() || info.Size() != ref.Size:
			continue
		default:
			old, err := fs.ReadFile(ws, p)
			if err != nil {
				return FileRef{}, false, err
			}
			if !bytes.Equal(old, data) {
				continue
			}
		}
		real, err := RealName(ws, p)
		if err != nil {
			return FileRef{}, false, err
		}
		ref.Path = real
		return ref, info != nil, nil
	}
	return FileRef{}, false, fmt.Errorf("%w: 100 other files have this name", fs.ErrExist)
}

// numberedName returns name for 1, and "<stem>-<i><ext>" after that.
func numberedName(name string, i int) string {
	if i == 1 {
		return name
	}
	ext := path.Ext(name)
	if ext == name {
		ext = ""
	}
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext)
}

// cleanFileName keeps the last part of a name that a caller sent, without
// control characters, and at most 255 bytes long.
func cleanFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		name = "file"
	}
	if len(name) > maxAttachmentName {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		stem := name[:maxAttachmentName-len(ext)]
		for len(stem) > 0 && !utf8.RuneStart(name[len(stem)]) {
			stem = stem[:len(stem)-1]
		}
		name = stem + ext
	}
	return name
}

// readFiles returns msgs with each file block replaced by what the model
// reads: the bytes for a media type in types, the text of a small text
// file, or a note. A label first names the file and its path, so the
// agent can open it with a tool. It copies only the messages that change.
func (a *Agent) readFiles(msgs []Message, types []string) []Message {
	var out []Message
	for i, m := range msgs {
		if !slices.ContainsFunc(m.Content, func(b Block) bool { return b.Type == BlockFile }) {
			continue
		}
		if out == nil {
			out = slices.Clone(msgs)
		}
		content := make([]Block, 0, len(m.Content)+1)
		for _, b := range m.Content {
			if b.Type != BlockFile || b.File == nil {
				content = append(content, b)
				continue
			}
			content = append(content, a.fileBlocks(*b.File, types)...)
		}
		out[i] = Message{Role: m.Role, Content: content}
	}
	if out == nil {
		return msgs
	}
	return out
}

// fileBlocks reads one attachment for a model request. A file that is gone
// or that cannot be read becomes a note, and never fails the run.
func (a *Agent) fileBlocks(f FileRef, types []string) []Block {
	label := fmt.Sprintf("The person attached %q (%s, %s). It is in the workspace at %s.", f.Name, f.MediaType, sizeWords(f.Size), f.Path)
	if a.files == nil {
		return []Block{Text(label + " The workspace is not available.")}
	}
	data, err := fs.ReadFile(a.files, f.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return []Block{Text(label + " The file is gone.")}
	case err != nil:
		return []Block{Text(label + " It cannot be read: " + err.Error() + ".")}
	}
	mt := f.MediaType
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != f.SHA256 {
		label += " It changed after the person attached it."
		mt = SniffMediaType(data)
	}
	switch {
	case slices.Contains(types, mt) && strings.HasPrefix(mt, "image/"):
		return []Block{Text(label), {Type: BlockImage, MediaType: mt, Data: data}}
	case slices.Contains(types, mt):
		return []Block{Text(label), {Type: BlockDocument, MediaType: mt, Data: data}}
	case strings.HasPrefix(mt, "text/") && len(data) <= maxInlineText && utf8.Valid(data):
		return []Block{Text(label + " Its content follows.\n\n" + string(data))}
	}
	return []Block{Text(label + " Read it with a tool.")}
}

// sizeWords writes a size for a person: 812 bytes, 14 KB, or 2.1 MB.
func sizeWords(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d bytes", n)
	case n < 1000*1000:
		return fmt.Sprintf("%d KB", (n+500)/1000)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}
