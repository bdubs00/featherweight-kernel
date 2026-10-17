package native

// This file forwards requests in a fleet, when a program links a fleet
// driver such as the one of the workbench. A request about a run that
// another member owns goes to that member with the credential of the
// caller, and the answer comes back as the member sent it. POST
// /v1/runs?member=<name> starts a run on another member. Without a fleet,
// nothing here changes a route.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// maxRelayBytes bounds a JSON answer of a member that the relay rewrites.
const maxRelayBytes = 64 << 20

// forwardHeaders are the request headers that go to the member.
var forwardHeaders = []string{"Accept", "Content-Type", HeaderIdempotencyKey, "Last-Event-ID"}

// relayHeaders are the response headers that come back from the member.
var relayHeaders = []string{"Content-Type", "Cache-Control", "Retry-After", "Location", "X-Accel-Buffering",
	"Content-Disposition", "Content-Security-Policy", "X-Content-Type-Options"}

// remote wraps the handler of a run route. When this process does not
// have the run and the fleet knows it, the request goes to the member that
// owns the run. A request that a member forwarded is never forwarded again.
func (s *server) remote(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := s.ownerOf(w, r, r.PathValue("id"))
		switch {
		case !ok:
		case member != "":
			s.forward(w, r, member)
		default:
			next(w, r)
		}
	}
}

// ownerOf names the fleet member that owns a run when another member owns
// it, and "" when the run is local, unknown, or the fleet is off. A request
// that a member forwarded never goes out again. On failure it writes the
// problem and returns false.
func (s *server) ownerOf(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	f := s.a.Fleet()
	if f == nil || r.Header.Get(fw.HeaderFleetVia) != "" {
		return "", true
	}
	// A context without a principal asks the kernel whether the run exists
	// at all, without an authorization check. The handler authorizes a
	// local run as before.
	if _, err := s.a.Get(context.Background(), id); !errors.Is(err, fw.ErrRunNotFound) {
		return "", true
	}
	run, err := f.Run(r.Context(), id)
	switch {
	case err == nil:
		return run.Member, true
	case errors.Is(err, fw.ErrRunNotFound):
		return "", true
	default:
		s.fail(w, r, err)
		return "", false
	}
}

// startOn handles POST /v1/runs?member=<name>. It reports true when it
// answered the request, and false when the run starts here, because the
// member is this process.
func (s *server) startOn(w http.ResponseWriter, r *http.Request, member string) bool {
	f := s.a.Fleet()
	switch {
	case f == nil:
		problem(w, r, http.StatusBadRequest, "the query parameter member needs the fleet, and the fleet is off; set fleet.subject (FW_FLEET_SUBJECT) to turn it on")
		return true
	case member == f.Name():
		return false
	case r.Header.Get(fw.HeaderFleetVia) != "":
		problem(w, r, http.StatusBadGateway, fmt.Sprintf("the member %s forwarded this request, and a member never forwards a request again", r.Header.Get(fw.HeaderFleetVia)))
		return true
	}
	s.forward(w, r, member)
	return true
}

// forward sends the request to a member and relays the answer. The query
// parameter member stays here.
func (s *server) forward(w http.ResponseWriter, r *http.Request, member string) {
	q := r.URL.Query()
	q.Del("member")
	target := r.URL.EscapedPath()
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	var body io.Reader = http.NoBody
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Body != nil {
		// The member applies its own limit. A run or a message with files
		// is the largest body that a route takes.
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxFilesBodyBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				problem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", MaxFilesBodyBytes))
				return
			}
			problem(w, r, http.StatusBadRequest, "the body is not readable")
			return
		}
		body = bytes.NewReader(b)
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		s.fail(w, r, fmt.Errorf("native: build the request for the member %s: %w", member, err))
		return
	}
	for _, k := range forwardHeaders {
		if v := r.Header.Get(k); v != "" {
			out.Header.Set(k, v)
		}
	}
	resp, err := s.a.Fleet().Forward(r.Context(), member, out)
	if err != nil {
		switch {
		case errors.Is(err, fw.ErrFleetNoRoute), errors.Is(err, fw.ErrFleetNoCredential):
			s.fail(w, r, err)
		case r.Context().Err() != nil:
			// The client is gone.
		default:
			s.a.Logger().Warn("native: the fleet member did not answer", "member", member, "method", r.Method, "path", r.URL.Path, "error", err)
			problem(w, r, http.StatusBadGateway, fmt.Sprintf("the fleet member %s did not answer; try again later", member))
		}
		return
	}
	defer resp.Body.Close()
	s.relay(w, r, member, resp)
}

// relay copies the answer of a member. An event stream flows through with
// a flush after each read. A JSON run object, and the run of a page of
// events, gets the field member. A 401 of the member becomes a 403, because
// the caller is signed in here and only the member refused the credential.
func (s *server) relay(w http.ResponseWriter, r *http.Request, member string, resp *http.Response) {
	if resp.StatusCode == http.StatusUnauthorized {
		problem(w, r, http.StatusForbidden, fmt.Sprintf("the fleet member %s did not accept your credential; both members need the same issuer and audience, or the same token", member))
		return
	}
	h := w.Header()
	for _, k := range relayHeaders {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case mt == "text/event-stream":
		rc := http.NewResponseController(w)
		// A write timeout of the server cuts a long stream, so it goes.
		_ = rc.SetWriteDeadline(time.Time{})
		w.WriteHeader(resp.StatusCode)
		_ = rc.Flush()
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				_ = rc.Flush()
			}
			if err != nil {
				return
			}
		}
	case mt == "application/json" && resp.StatusCode < http.StatusMultipleChoices:
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes+1))
		if err != nil || len(b) > maxRelayBytes {
			h.Del("Content-Type")
			problem(w, r, http.StatusBadGateway, fmt.Sprintf("this process cannot read the answer of the fleet member %s", member))
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(withMember(b, member))
	default:
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// withMember sets the field member on a run object, or on the run of a
// page of events. Any other JSON stays as it is.
func withMember(b []byte, member string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		return b
	}
	name, _ := json.Marshal(member)
	switch {
	case m["id"] != nil && m["state"] != nil:
		m["member"] = name
	case m["run"] != nil:
		var run map[string]json.RawMessage
		if json.Unmarshal(m["run"], &run) != nil || run == nil {
			return b
		}
		run["member"] = name
		raw, err := json.Marshal(run)
		if err != nil {
			return b
		}
		m["run"] = raw
	default:
		return b
	}
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return append(out, '\n')
}

// fleetDetail is the detail of a problem for a fleet error, without the
// package prefix.
func fleetDetail(err error) string {
	return strings.TrimPrefix(err.Error(), "fw: ")
}
