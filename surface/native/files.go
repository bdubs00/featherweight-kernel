package native

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// runFiles answers GET /v1/runs/{id}/files with the files that the run
// changed.
func (s *server) runFiles(w http.ResponseWriter, r *http.Request) {
	files, err := view.RunFiles(r.Context(), s.a, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// runFile answers GET /v1/runs/{id}/files/{path}: the content of a file
// that the run changed, or its view for the dashboard under
// Accept: application/json. The content never runs on this origin: it is
// an attachment in a sandbox, and the browser does not sniff its type.
//
// The query parameter download=1 asks for the raw content as a download,
// also from a client that accepts JSON. The raw answer always names the
// file in Content-Disposition, as RFC 6266 says: an ASCII filename, and
// filename* with the exact name when the name needs more than that.
func (s *server) runFile(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("path")
	var download bool
	switch v := r.URL.Query().Get("download"); v {
	case "", "0", "false":
	case "1", "true":
		download = true
	default:
		problem(w, r, http.StatusBadRequest, "the query parameter download must be 1 or 0")
		return
	}
	if accepts(r, "application/json") && !download {
		v, err := view.FileViewOf(r.Context(), s.a, id, name)
		if err != nil {
			s.failFile(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
		return
	}
	f, data, err := view.ReadRunFile(r.Context(), s.a, id, name)
	if err != nil {
		s.failFile(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", http.DetectContentType(data))
	h.Set("Content-Disposition", contentDisposition(path.Base(f.Path)))
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// contentDisposition is the attachment header of a file name (RFC 6266).
// Control characters and format characters (Unicode category Cf, such as
// the right-to-left override U+202E, which can show "exe.txt" for
// "txt.exe") leave the name, and a path separator becomes "_". The
// parameter filename holds an ASCII form of the name, with "_" for each
// character that is not printable ASCII, a quote, or a percent sign. When
// that form differs from the name, filename* holds the name in UTF-8 (RFC
// 8187). A name without a usable character becomes "file".
func contentDisposition(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		case r == '/' || r == '\\' || r == utf8.RuneError:
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	var ascii strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '%' {
			ascii.WriteByte('_')
			continue
		}
		ascii.WriteRune(r)
	}
	fallback := ascii.String()
	out := mime.FormatMediaType("attachment", map[string]string{"filename": fallback})
	if fallback == name {
		return out
	}
	return out + "; filename*=UTF-8''" + encodeExtValue(name)
}

// encodeExtValue percent-encodes a value for an extended parameter. Only
// the attr-char of RFC 8187 stays as it is.
func encodeExtValue(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			strings.IndexByte("!#$&+-.^_`|~", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}

// failFile writes the problem of a file route.
func (s *server) failFile(w http.ResponseWriter, r *http.Request, err error) {
	name := strconv.Quote(r.PathValue("path"))
	switch {
	case errors.Is(err, view.ErrNotInRun):
		problem(w, r, http.StatusNotFound, "the run did not change the file "+name)
	case errors.Is(err, fs.ErrNotExist):
		problem(w, r, http.StatusNotFound, "the file "+name+" does not exist now")
	case errors.Is(err, fw.ErrFileTooLarge):
		problem(w, r, http.StatusRequestEntityTooLarge, "the file "+name+" is larger than tools.files.maxFileBytes")
	case errors.Is(err, fs.ErrPermission):
		problem(w, r, http.StatusForbidden, "the workspace does not open the file "+name+": "+cause(err))
	default:
		s.fail(w, r, err)
	}
}

// cause returns the reason inside a path error, without the path.
func cause(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
