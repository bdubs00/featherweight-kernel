package a2a

// This file answers the conditions that end by themselves: a control plane
// that did not answer (fw.ErrAuthzUnavailable), a full backlog of runs
// (fw.ErrSaturated), and a process that a storage failure blocked until
// its restart (fw.ErrStorageUnavailable). A2A and its SDK have no error for
// a service that is busy or down. The protocol code is therefore the
// server error, the JSON-RPC code -32000 that JSON-RPC reserves for the
// server, and the details and the metadata of the error info carry the
// reason and a retry hint, so a client can tell the condition from a
// refusal and try again. The SDK sends every server error with HTTP 500
// on the REST binding, so unavailableStatus turns these errors into the
// answers of the native API: 429 with Retry-After for a full backlog, and
// 503 with Retry-After for the other two. A JSON-RPC error goes out with
// HTTP 200, as JSON-RPC defines, and its data carries the reason.

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
	// reasonUnavailable, reasonSaturated, and reasonStorage are the
	// reasons in the details and the values of metaReason in the error
	// info.
	reasonUnavailable = "AUTHZ_UNAVAILABLE"
	reasonSaturated   = "SATURATED"
	reasonStorage     = "STORAGE_UNAVAILABLE"
	// metaReason is the key of the reason in the metadata of the error
	// info. The protocol reason of the error info stays SERVER_ERROR.
	metaReason = "fwReason"
	// retryAfterSeconds is the retry hint, as in the native API.
	retryAfterSeconds = 5
	// storageRetryAfterSeconds is the retry hint of a blocked process: it
	// does no work until a controller or an operator restarts it.
	storageRetryAfterSeconds = 30
	// maxHeldBody bounds the bytes that unavailableStatus holds.
	maxHeldBody = 64 << 10
)

// temporary builds the A2A server error of a condition that ends by
// itself, with the reason and the retry hint in the details and in the
// metadata of the error info. It wraps both the server error and err, so
// errors.Is finds either one.
func temporary(err error, reason, msg string, retryAfter int) error {
	return sdk.NewError(fmt.Errorf("a2a: %w: %w", sdk.ErrServerError, err), msg).
		WithDetails(map[string]any{"reason": reason, "retryAfterSeconds": retryAfter}).
		WithErrorInfoMeta(map[string]string{metaReason: reason, "retryAfter": strconv.Itoa(retryAfter)})
}

// unavailable builds the A2A error for a control plane that did not answer.
func unavailable(err error) error {
	msg := "The authorization service did not answer, so the request is refused. Try again in a few seconds. Cause: " + err.Error()
	return temporary(err, reasonUnavailable, msg, retryAfterSeconds)
}

// saturated builds the A2A error for a full backlog of runs. A retry with
// the same message id or idempotency key gives one run.
func saturated(err error) error {
	msg := "The run admission of the agent is full, so the request is refused. Try again after the retry hint with the same message id."
	return temporary(err, reasonSaturated, msg, retryAfterSeconds)
}

// storageUnavailable builds the A2A error for a process that a storage
// failure blocked. The message does not hold the cause, because the cause
// can name a path of the server.
func storageUnavailable(err error) error {
	msg := "The storage of the agent failed, so the agent does no work until it restarts. Try again later with the same message id."
	return temporary(err, reasonStorage, msg, storageRetryAfterSeconds)
}

// temporaryStatus returns the HTTP status and the status word of the REST
// binding for the reason of a temporary error, and false for any other
// reason.
func temporaryStatus(reason string) (int, string, bool) {
	switch reason {
	case reasonUnavailable, reasonStorage:
		return http.StatusServiceUnavailable, "UNAVAILABLE", true
	case reasonSaturated:
		return http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", true
	}
	return 0, "", false
}

// unavailableStatus wraps the REST binding. It holds the body of a 500
// and, when the body is the error of temporary, sends it with the status
// of its reason (temporaryStatus) and Retry-After. A stream starts with
// HTTP 200 before its first event, so the wrapper also holds the headers
// of a stream until the first event is complete. When that event is the
// error of temporary, the answer is the same as for a plain call. Every
// other response passes through unchanged.
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

// answer sends the rewritten error document with its status and its retry
// hint.
func (w *holdWriter) answer(out temporaryAnswer) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", out.retryAfter)
	h.Del("Content-Length")
	w.ResponseWriter.WriteHeader(out.code)
	_, _ = w.ResponseWriter.Write(out.doc)
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

// temporaryAnswer is the REST answer of an error of temporary: the
// rewritten document, its HTTP status, and the value of Retry-After.
type temporaryAnswer struct {
	doc        []byte
	code       int
	retryAfter string
}

// asUnavailable returns the body with the code and the status of its
// reason (temporaryStatus) when the error info of the body names the
// reason of an error of temporary.
func asUnavailable(body []byte) (temporaryAnswer, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return temporaryAnswer{}, false
	}
	var e map[string]json.RawMessage
	if json.Unmarshal(doc["error"], &e) != nil {
		return temporaryAnswer{}, false
	}
	var details []struct {
		Type     string            `json:"@type"`
		Metadata map[string]string `json:"metadata"`
	}
	if json.Unmarshal(e["details"], &details) != nil {
		return temporaryAnswer{}, false
	}
	code, status, retryAfter := 0, "", ""
	for _, d := range details {
		if d.Type != errordetails.ErrorInfoType {
			continue
		}
		if c, s, ok := temporaryStatus(d.Metadata[metaReason]); ok {
			code, status, retryAfter = c, s, d.Metadata["retryAfter"]
			break
		}
	}
	if code == 0 {
		return temporaryAnswer{}, false
	}
	if _, err := strconv.Atoi(retryAfter); err != nil {
		retryAfter = strconv.Itoa(retryAfterSeconds)
	}
	e["code"] = json.RawMessage(strconv.Itoa(code))
	e["status"] = json.RawMessage(strconv.Quote(status))
	raw, err := json.Marshal(e)
	if err != nil {
		return temporaryAnswer{}, false
	}
	doc["error"] = raw
	out, err := json.Marshal(doc)
	if err != nil {
		return temporaryAnswer{}, false
	}
	return temporaryAnswer{doc: append(out, '\n'), code: code, retryAfter: retryAfter}, true
}
