package a2a

// This file answers a control plane that did not answer (fw.ErrAuthzUnavailable).
// A2A has no error for a service that is down. The protocol code is
// therefore the server error, and the details and the metadata of the error
// info carry the reason and a retry hint, so a client can tell the outage
// from a refusal. The SDK sends every server error with HTTP 500 on the
// REST binding, so unavailableStatus turns this one error into a 503 with
// Retry-After, the same answer as the native API. A JSON-RPC error goes out
// with HTTP 200, as JSON-RPC defines, and its data carries the reason.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/errordetails"
)

const (
	// reasonUnavailable is the reason in the details and the value of
	// metaReason in the error info.
	reasonUnavailable = "AUTHZ_UNAVAILABLE"
	// metaReason is the key of the reason in the metadata of the error
	// info. The protocol reason of the error info stays SERVER_ERROR.
	metaReason = "fwReason"
	// retryAfterSeconds is the retry hint, as in the native API.
	retryAfterSeconds = 5
	// maxHeldBody bounds the bytes that unavailableStatus holds.
	maxHeldBody = 64 << 10
)

// unavailable builds the A2A error for a control plane that did not answer.
// It wraps both the server error and err, so errors.Is finds either one.
func unavailable(err error) error {
	msg := "The authorization service did not answer, so the request is refused. Try again in a few seconds. Cause: " + err.Error()
	return sdk.NewError(fmt.Errorf("a2a: %w: %w", sdk.ErrServerError, err), msg).
		WithDetails(map[string]any{"reason": reasonUnavailable, "retryAfterSeconds": retryAfterSeconds}).
		WithErrorInfoMeta(map[string]string{metaReason: reasonUnavailable, "retryAfter": strconv.Itoa(retryAfterSeconds)})
}

// unavailableStatus wraps the REST binding. It holds the body of a 500 and,
// when the body is the error of unavailable, sends it with a 503, the
// status UNAVAILABLE, and Retry-After. A stream starts with HTTP 200 before
// its first event, so the wrapper also holds the headers of a stream until
// the first event is complete. When that event is the error of
// unavailable, the answer is the same 503 as for a plain call. Every other
// response passes through unchanged.
func unavailableStatus(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hw := &holdWriter{ResponseWriter: w}
		next.ServeHTTP(hw, r)
		hw.finish()
	})
}

// Modes of a holdWriter.
const (
	passing   = iota // the response goes out as it is written
	holdError        // the status is 500, and the body waits in buf
	holdEvent        // a stream waits in buf for its first complete event
	dropping         // the wrapper answered, and the rest of the stream is dropped
)

// holdWriter holds the start of a response until it can tell whether the
// response is the error of unavailable.
type holdWriter struct {
	http.ResponseWriter
	wrote bool         // the handler set the status
	mode  int          // passing, holdError, holdEvent, or dropping
	buf   bytes.Buffer // the held bytes
}

// WriteHeader implements http.ResponseWriter.
func (w *holdWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	switch {
	case code == http.StatusInternalServerError:
		w.mode = holdError
	case code == http.StatusOK && strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream"):
		w.mode = holdEvent
	default:
		w.ResponseWriter.WriteHeader(code)
	}
}

// Write implements http.ResponseWriter. Held bytes that grow past
// maxHeldBody go out as they are.
func (w *holdWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	switch w.mode {
	case dropping:
		return len(b), nil
	case passing:
		return w.ResponseWriter.Write(b)
	}
	if w.buf.Len()+len(b) > maxHeldBody {
		w.release()
		return w.ResponseWriter.Write(b)
	}
	w.buf.Write(b)
	if w.mode == holdEvent {
		if end := bytes.Index(w.buf.Bytes(), []byte("\n\n")); end >= 0 {
			if out, ok := asUnavailable(eventData(w.buf.Bytes()[:end])); ok {
				w.answer(out)
				w.mode = dropping
			} else {
				w.release()
			}
		}
	}
	return len(b), nil
}

// Flush implements http.Flusher. Held bytes wait.
func (w *holdWriter) Flush() {
	if w.mode != passing {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *holdWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish sends what the writer still holds when the handler returns.
func (w *holdWriter) finish() {
	switch w.mode {
	case holdError:
		if out, ok := asUnavailable(w.buf.Bytes()); ok {
			w.answer(out)
			return
		}
		w.release()
	case holdEvent:
		w.release()
	}
}

// answer sends the 503 with the rewritten error document.
func (w *holdWriter) answer(doc []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	h.Del("Content-Length")
	w.ResponseWriter.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.ResponseWriter.Write(doc)
	w.buf.Reset()
}

// release sends the held status and bytes as the handler wrote them, and
// lets the rest of the response pass.
func (w *holdWriter) release() {
	code := http.StatusOK
	if w.mode == holdError {
		code = http.StatusInternalServerError
	}
	w.mode = passing
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(code)
	_, _ = w.ResponseWriter.Write(w.buf.Bytes())
	w.buf.Reset()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// eventData returns the data of one server-sent event: its data lines,
// joined with newlines.
func eventData(event []byte) []byte {
	var parts [][]byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			parts = append(parts, bytes.TrimPrefix(bytes.TrimSuffix(v, []byte("\r")), []byte(" ")))
		}
	}
	return bytes.Join(parts, []byte("\n"))
}

// asUnavailable returns the body with the code 503 and the status
// UNAVAILABLE when the error info of the body names reasonUnavailable.
func asUnavailable(body []byte) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil, false
	}
	var e map[string]json.RawMessage
	if json.Unmarshal(doc["error"], &e) != nil {
		return nil, false
	}
	var details []struct {
		Type     string            `json:"@type"`
		Metadata map[string]string `json:"metadata"`
	}
	if json.Unmarshal(e["details"], &details) != nil {
		return nil, false
	}
	found := false
	for _, d := range details {
		if d.Type == errordetails.ErrorInfoType && d.Metadata[metaReason] == reasonUnavailable {
			found = true
			break
		}
	}
	if !found {
		return nil, false
	}
	e["code"] = json.RawMessage(strconv.Itoa(http.StatusServiceUnavailable))
	e["status"] = json.RawMessage(`"UNAVAILABLE"`)
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, false
	}
	doc["error"] = raw
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return append(out, '\n'), true
}
