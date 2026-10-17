package native

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// script returns canned responses in order and records every request.
type script struct {
	mu    sync.Mutex
	resps []*fw.Response
	reqs  []*fw.Request
}

func newScript(resps ...*fw.Response) *script { return &script{resps: resps} }

func (s *script) model() fw.Model {
	return fw.ModelFunc{GenerateFn: func(_ context.Context, req *fw.Request) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reqs = append(s.reqs, req)
		if len(s.resps) == 0 {
			return nil, errors.New("script: no response left")
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func (s *script) requests() []*fw.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fw.Request(nil), s.reqs...)
}

func endTurn(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

func callTool(id, name, input string) *fw.Response {
	return &fw.Response{
		Content:    []fw.Block{fw.Text("calling"), {Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		StopReason: fw.StopToolUse,
		Usage:      fw.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// gatedModel blocks every call until gate closes. It signals each call on
// started when started is not nil.
func gatedModel(gate <-chan struct{}, started chan<- struct{}) fw.Model {
	return fw.ModelFunc{GenerateFn: func(ctx context.Context, _ *fw.Request) (*fw.Response, error) {
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		select {
		case <-gate:
			return endTurn("released"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
}

func rmTool(calls *atomic.Int32) fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "rm", Description: "remove", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult("removed"), nil
	})
}

func newAgent(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(New()),
	}
	a, err := fw.New(append(append(base, fw.WithTitles(fw.TitlesInput)), opts...)...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func serve(t *testing.T, a *fw.Agent) *httptest.Server {
	t.Helper()
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// do sends a request and returns the response with its body read.
func do(t *testing.T, method, url, body string, header ...string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func decodeRun(t *testing.T, resp *http.Response, b []byte, status int) *fw.Run {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	var r fw.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("decode run: %v: %s", err, b)
	}
	return &r
}

func decodeProblem(t *testing.T, resp *http.Response, b []byte, status int) Problem {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type = %q: %s", ct, b)
	}
	var p Problem
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("decode problem: %v: %s", err, b)
	}
	if p.Type != "about:blank" || p.Status != status || p.Title != http.StatusText(status) || p.Detail == "" || p.Instance == "" {
		t.Fatalf("problem = %+v", p)
	}
	return p
}

// frame is one server-sent event, or one comment line.
type frame struct {
	id, event, data string
	comment         string
}

type sseReader struct{ sc *bufio.Scanner }

func newSSE(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &sseReader{sc: sc}
}

// next returns the next frame, or false at the end of the stream.
func (s *sseReader) next() (frame, bool) {
	var f frame
	seen := false
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			if seen {
				return f, true
			}
			continue
		}
		seen = true
		switch {
		case strings.HasPrefix(line, ":"):
			f.comment = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "id: "):
			f.id = line[len("id: "):]
		case strings.HasPrefix(line, "event: "):
			f.event = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			f.data = line[len("data: "):]
		}
	}
	return f, false
}

// events reads the remaining event frames until the stream ends.
func (s *sseReader) events(t *testing.T) []fw.Event {
	t.Helper()
	var out []fw.Event
	for {
		f, ok := s.next()
		if !ok {
			return out
		}
		if f.comment != "" {
			continue
		}
		out = append(out, checkFrame(t, f))
	}
}

func checkFrame(t *testing.T, f frame) fw.Event {
	t.Helper()
	var e fw.Event
	if err := json.Unmarshal([]byte(f.data), &e); err != nil {
		t.Fatalf("frame data: %v: %q", err, f.data)
	}
	if f.id != strconv.FormatUint(e.Seq, 10) || f.event != e.Type {
		t.Fatalf("frame id %q event %q, data seq %d type %s", f.id, f.event, e.Seq, e.Type)
	}
	return e
}

func openStream(t *testing.T, ctx context.Context, method, url, body string, header ...string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status = %d: %s", resp.StatusCode, b)
	}
	for k, v := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if got := resp.Header.Get(k); got != v {
			t.Fatalf("%s = %q, want %q", k, got, v)
		}
	}
	return resp
}

func checkContiguous(t *testing.T, evs []fw.Event, first uint64) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	for i, e := range evs {
		if e.Seq != first+uint64(i) {
			t.Fatalf("event %d has seq %d, want %d", i, e.Seq, first+uint64(i))
		}
	}
	if last := evs[len(evs)-1]; last.Type != fw.EvRunFinished {
		t.Fatalf("last event = %s", last.Type)
	}
}

func TestCreateWait(t *testing.T) {
	a := newAgent(t, newScript(endTurn("hello")).model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"say hi","wait":true,"metadata":{"ticket":"42"}}`)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateCompleted || r.Output != "hello" || r.Origin != fw.OriginHTTP || r.Principal != "anonymous" ||
		r.Agent != "default" || r.Metadata["ticket"] != "42" || r.Input != "say hi" {
		t.Fatalf("run = %+v", r)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r.ID, "")
	if got := decodeRun(t, resp, b, http.StatusOK); got.ID != r.ID || got.State != fw.StateCompleted {
		t.Fatalf("get = %+v", got)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs", "")
	var list struct{ Runs []fw.Run }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &list) != nil || len(list.Runs) != 1 || list.Runs[0].ID != r.ID {
		t.Fatalf("list = %d %s", resp.StatusCode, b)
	}
}

func TestCreateReturns201(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("hello")).model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"go","budget":{"maxTurns":5,"deadline":"90s"}}`)
	r := decodeRun(t, resp, b, http.StatusCreated)
	if resp.Header.Get("Location") != "/v1/runs/"+r.ID {
		t.Fatalf("location = %q", resp.Header.Get("Location"))
	}
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, a, r.ID)
	var q fw.RunQueuedData
	if err := evs[0].Decode(&q); err != nil || q.Budget == nil || q.Budget.MaxTurns != 5 || q.Budget.Deadline != 90*time.Second || q.Origin != fw.OriginHTTP {
		t.Fatalf("queued = %+v, %v", q, err)
	}
}

func collect(t *testing.T, a *fw.Agent, id string) []fw.Event {
	t.Helper()
	ch, err := a.Events(testCtx(t), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []fw.Event
	for e := range ch {
		out = append(out, e)
	}
	return out
}

func TestCreateStream(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("streamed")).model())
	srv := serve(t, a)
	resp := openStream(t, ctx, "POST", srv.URL+"/v1/runs", `{"input":"go"}`, "Accept", "application/json;q=0.5, text/event-stream")
	evs := newSSE(resp.Body).events(t)
	checkContiguous(t, evs, 1)
	if evs[0].Type != fw.EvRunQueued {
		t.Fatalf("first event = %s", evs[0].Type)
	}
	var d fw.RunFinishedData
	if err := evs[len(evs)-1].Decode(&d); err != nil || d.Output != "streamed" {
		t.Fatalf("finished = %+v, %v", d, err)
	}
	// The ledger delivers an event before the kernel applies it to the
	// status, so Wait, not Get, gives the final status.
	r, err := a.Wait(ctx, evs[0].Subject)
	if err != nil || r.State != fw.StateCompleted || r.Seq != uint64(len(evs)) {
		t.Fatalf("run = %+v, %v", r, err)
	}
}

func TestEventsResumeMidStream(t *testing.T) {
	ctx := testCtx(t)
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	a := newAgent(t, gatedModel(gate, started))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"go"}`)
	r := decodeRun(t, resp, b, http.StatusCreated)
	<-started // the run waits in its model call, after fw.model.requested

	// Read two events, then drop the connection.
	sctx, cancel := context.WithCancel(ctx)
	first := openStream(t, sctx, "GET", srv.URL+"/v1/runs/"+r.ID+"/events", "")
	rd := newSSE(first.Body)
	var got []fw.Event
	for len(got) < 2 {
		f, ok := rd.next()
		if !ok {
			t.Fatal("stream ended early")
		}
		got = append(got, checkFrame(t, f))
	}
	cancel()
	first.Body.Close()

	// Reconnect as an EventSource does. Last-Event-ID wins over from.
	second := openStream(t, ctx, "GET", srv.URL+"/v1/runs/"+r.ID+"/events?from=1", "", "Last-Event-ID", strconv.FormatUint(got[1].Seq, 10))
	rd = newSSE(second.Body)
	f, ok := rd.next()
	if !ok {
		t.Fatal("resumed stream is empty")
	}
	next := checkFrame(t, f)
	if next.Seq != got[1].Seq+1 {
		t.Fatalf("resumed at seq %d, want %d", next.Seq, got[1].Seq+1)
	}
	close(gate)
	rest := append([]fw.Event{next}, rd.events(t)...)
	checkContiguous(t, rest, got[1].Seq+1)

	// The query parameter from starts at that sequence number.
	third := openStream(t, ctx, "GET", srv.URL+"/v1/runs/"+r.ID+"/events?from=3", "")
	checkContiguous(t, newSSE(third.Body).events(t), 3)

	for _, bad := range [][]string{{"?from=x"}, {"", "Last-Event-ID", "nope"}} {
		resp, b := do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/events"+bad[0], "", bad[1:]...)
		decodeProblem(t, resp, b, http.StatusBadRequest)
	}
}

func TestEventsKeepAlive(t *testing.T) {
	ctx := testCtx(t)
	gate := make(chan struct{})
	defer close(gate)
	a := newAgent(t, gatedModel(gate, nil), fw.WithoutDefaults())
	h := http.NewServeMux()
	if err := newSurface(20*time.Millisecond, nil).Mount(h, a); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	r, err := a.Start(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp := openStream(t, sctx, "GET", srv.URL+"/v1/runs/"+r.ID+"/events", "")
	rd := newSSE(resp.Body)
	for {
		f, ok := rd.next()
		if !ok {
			t.Fatal("stream ended before a keep-alive")
		}
		if f.comment == "keep-alive" {
			return
		}
	}
}

func TestApprovalRoundTrip(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	a := newAgent(t, newScript(callTool("c1", "rm", `{"path":"x"}`), endTurn("done")).model(), fw.WithTools(rmTool(&calls)))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"clean up","wait":true}`)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.Tool != "rm" || calls.Load() != 0 {
		t.Fatalf("run = %+v", r)
	}
	base := srv.URL + "/v1/runs/" + r.ID + "/approvals/"

	resp, b = do(t, "POST", base+"wrong", `{"decision":"allow"}`)
	decodeProblem(t, resp, b, http.StatusConflict)
	resp, b = do(t, "POST", base+r.Pending.ID, `{"decision":"maybe"}`)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/nope/approvals/"+r.Pending.ID, `{"decision":"allow"}`)
	decodeProblem(t, resp, b, http.StatusNotFound)

	resp, b = do(t, "POST", base+r.Pending.ID, `{"decision":"Allow","note":"fine"}`)
	decodeRun(t, resp, b, http.StatusOK)
	r, err := a.Wait(ctx, r.ID)
	if err != nil || r.State != fw.StateCompleted || r.Output != "done" || calls.Load() != 1 {
		t.Fatalf("run = %+v, %v, calls %d", r, err, calls.Load())
	}
	var d fw.ApprovalDecidedData
	for _, e := range collect(t, a, r.ID) {
		if e.Type == fw.EvApprovalDecided {
			_ = e.Decode(&d)
		}
	}
	if d.Decision != "allow" || d.Decider != "anonymous" || d.Note != "fine" {
		t.Fatalf("decided = %+v", d)
	}
	// The approval is gone now.
	resp, b = do(t, "POST", base+d.ApprovalID, `{"decision":"deny"}`)
	decodeProblem(t, resp, b, http.StatusConflict)
}

func TestFollowUpMessage(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("first"), endTurn("second"), endTurn("third")).model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi","wait":true}`)
	r := decodeRun(t, resp, b, http.StatusOK)

	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"again"}`)
	decodeRun(t, resp, b, http.StatusAccepted)
	r, err := a.Wait(ctx, r.ID)
	if err != nil || r.State != fw.StateCompleted || r.Output != "second" || r.Usage.Turns != 2 {
		t.Fatalf("run = %+v, %v", r, err)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":[{"type":"text","text":"once more"}]}`)
	decodeRun(t, resp, b, http.StatusAccepted)
	if r, err = a.Wait(ctx, r.ID); err != nil || r.Output != "third" {
		t.Fatalf("run = %+v, %v", r, err)
	}
	var msgs []fw.MessageReceivedData
	for _, e := range collect(t, a, r.ID) {
		if e.Type == fw.EvMessageReceived {
			var d fw.MessageReceivedData
			_ = e.Decode(&d)
			msgs = append(msgs, d)
		}
	}
	if len(msgs) != 2 || msgs[0].Origin != fw.OriginHTTP || fw.TextOf(msgs[0].Content) != "again" || fw.TextOf(msgs[1].Content) != "once more" {
		t.Fatalf("messages = %+v", msgs)
	}
	for _, body := range []string{`{}`, `{"content":""}`, `{"content":[]}`, `{"content":[{"type":"tool_result"}]}`, `{"content":42}`} {
		resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", body)
		decodeProblem(t, resp, b, http.StatusBadRequest)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/nope/messages", `{"content":"x"}`)
	decodeProblem(t, resp, b, http.StatusNotFound)
}

func TestCancel(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	started := make(chan struct{}, 1)
	a := newAgent(t, gatedModel(gate, started))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"wait forever"}`)
	r := decodeRun(t, resp, b, http.StatusCreated)
	<-started
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/cancel", "")
	if got := decodeRun(t, resp, b, http.StatusOK); got.State != fw.StateCanceled {
		t.Fatalf("run = %+v", got)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/nope/cancel", "")
	decodeProblem(t, resp, b, http.StatusNotFound)
}

func TestTokenAuth(t *testing.T) {
	a := newAgent(t, newScript(endTurn("ok")).model(), fw.WithAuthn(fw.TokenAuthn{Token: "s3cret"}))
	srv := serve(t, a)
	for _, header := range [][]string{nil, {"Authorization", "Bearer wrong"}} {
		resp, b := do(t, "GET", srv.URL+"/v1/runs", "", header...)
		decodeProblem(t, resp, b, http.StatusUnauthorized)
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Fatal("no challenge header")
		}
	}
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"x"}`)
	decodeProblem(t, resp, b, http.StatusUnauthorized)

	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"x","wait":true}`, "Authorization", "Bearer s3cret")
	if r := decodeRun(t, resp, b, http.StatusOK); r.Principal != "token" || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v", r)
	}
}

func TestUnknownRunProblem(t *testing.T) {
	a := newAgent(t, newScript().model())
	srv := serve(t, a)
	resp, b := do(t, "GET", srv.URL+"/v1/runs/nope?secret=x", "")
	p := decodeProblem(t, resp, b, http.StatusNotFound)
	if p.Instance != "/v1/runs/nope" || !strings.Contains(p.Detail, "nope") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/nope/events", "")
	decodeProblem(t, resp, b, http.StatusNotFound)
}

func TestIdempotencyKey(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("once")).model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"job"}`, "Idempotency-Key", "k1")
	r1 := decodeRun(t, resp, b, http.StatusCreated)
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"job","wait":true}`, "Idempotency-Key", "k1")
	r2 := decodeRun(t, resp, b, http.StatusOK)
	if r2.ID != r1.ID || r2.State != fw.StateCompleted {
		t.Fatalf("second = %+v", r2)
	}
	if list, _ := a.List(ctx); len(list) != 1 {
		t.Fatalf("runs = %d", len(list))
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"job"}`, "Idempotency-Key", strings.Repeat("k", MaxIdempotencyKeyLen+1))
	decodeProblem(t, resp, b, http.StatusBadRequest)
}

func TestCreateValidation(t *testing.T) {
	a := newAgent(t, newScript().model())
	srv := serve(t, a)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{``, http.StatusBadRequest},
		{`{"input":""}`, http.StatusBadRequest},
		{`{"input":"  "}`, http.StatusBadRequest},
		{`{"input":"x"`, http.StatusBadRequest},
		{`{"input":"x","inputs":"y"}`, http.StatusBadRequest},
		{`{"input":"x"} {}`, http.StatusBadRequest},
		{`{"input":"x","budget":{"maxTurns":-1}}`, http.StatusBadRequest},
		{`{"input":"x","budget":{"deadline":"soon"}}`, http.StatusBadRequest},
		{`{"input":"x","metadata":{"idempotencyKey":"k"}}`, http.StatusBadRequest},
		{`{"input":"x","agent":"missing"}`, http.StatusNotFound},
		{`{"input":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		resp, b := do(t, "POST", srv.URL+"/v1/runs", tc.body)
		p := decodeProblem(t, resp, b, tc.status)
		if p.Instance != "/v1/runs" {
			t.Fatalf("instance = %q", p.Instance)
		}
	}
	if list, _ := a.List(context.Background()); len(list) != 0 {
		t.Fatalf("runs = %d", len(list))
	}
}

func TestClosedAgent(t *testing.T) {
	a := newAgent(t, newScript().model())
	srv := serve(t, a)
	a.Close(context.Background())
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"x"}`)
	decodeProblem(t, resp, b, http.StatusServiceUnavailable)
}

func TestAgentsAndTools(t *testing.T) {
	var calls atomic.Int32
	a := newAgent(t, newScript().model(), fw.WithTools(rmTool(&calls)), fw.WithProfile(fw.AgentProfile{Name: "reviewer", Model: "anthropic/x"}))
	srv := serve(t, a)
	resp, b := do(t, "GET", srv.URL+"/v1/agents", "")
	var agents struct{ Agents []fw.AgentProfile }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &agents) != nil || len(agents.Agents) != 1 || agents.Agents[0].Name != "reviewer" {
		t.Fatalf("agents = %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/tools", "")
	var tools struct{ Tools []fw.ToolSpec }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &tools) != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "rm" || !tools.Tools[0].Destructive {
		t.Fatalf("tools = %d %s", resp.StatusCode, b)
	}
}

func TestRegistered(t *testing.T) {
	s, err := fw.NewSurface(Name, fw.DefaultConfig())
	if err != nil || s == nil {
		t.Fatalf("NewSurface = %v, %v", s, err)
	}
}

func TestCreateWithInstructionsAndProfile(t *testing.T) {
	m := newScript(endTurn("shaped"))
	a := newAgent(t, m.model(), fw.WithProfile(fw.AgentProfile{Name: "writer", System: "You write.", Instructions: []string{"House rule."}}))
	srv := serve(t, a)
	body := `{"input":"go","agent":"writer","wait":true,"instructions":"Only this run.",
		"profile":{"system":"You edit.","instructions":["Be brief."],"tools":["files"],"skills":[],"effort":"LOW","timeZone":"America/Chicago",
		"policy":{"default":"ask","deny":["bash"]},"budget":{"maxTurns":3,"maxToolCalls":7,"maxCallsPerTurn":2,"maxRepeats":1,"deadline":"1m"}},
		"budget":{"maxTokens":500,"maxToolCalls":9}}`
	resp, b := do(t, "POST", srv.URL+"/v1/runs", body)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateCompleted || r.Output != "shaped" || r.Agent != "writer" {
		t.Fatalf("run = %+v", r)
	}
	reqs := m.requests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	sys := reqs[0].System
	for _, want := range []string{"You edit.", "House rule.", "Be brief.", "# Instructions for this run", "Only this run."} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "You write.") {
		t.Errorf("the inline system prompt did not replace the profile prompt:\n%s", sys)
	}
	if reqs[0].Effort != "low" {
		t.Errorf("effort = %q", reqs[0].Effort)
	}
	var q fw.RunQueuedData
	if err := collect(t, a, r.ID)[0].Decode(&q); err != nil {
		t.Fatal(err)
	}
	if q.Instructions != "Only this run." || q.Profile == nil || q.Profile.System != "You edit." || len(q.Profile.Tools) != 1 || q.Profile.TimeZone != "America/Chicago" ||
		len(q.Profile.Skills) != 0 || q.Profile.Policy == nil || q.Profile.Policy.Default != "ask" ||
		q.Profile.Budget == nil || q.Profile.Budget.MaxToolCalls != 7 || q.Profile.Budget.MaxCallsPerTurn != 2 ||
		q.Profile.Budget.MaxRepeats != 1 || q.Profile.Budget.Deadline != time.Minute ||
		q.Budget == nil || q.Budget.MaxTokens != 500 || q.Budget.MaxToolCalls != 9 {
		t.Fatalf("queued = %+v profile %+v budget %+v", q, q.Profile, q.Budget)
	}
	if strings.Contains(sys, "# Skills") {
		t.Errorf("an empty skills list still lists skills:\n%s", sys)
	}
}

func TestProfileValidation(t *testing.T) {
	a := newAgent(t, newScript().model())
	srv := serve(t, a)
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"input":"x","profile":{"effort":"extreme"}}`, "profile.effort"},
		{`{"input":"x","profile":{"timeZone":"Mars/Olympus"}}`, "profile.timeZone"},
		{`{"input":"x","profile":{"policy":{"default":"maybe"}}}`, "profile.policy.default"},
		{`{"input":"x","profile":{"policy":{"deny":[""]}}}`, "profile.policy.deny[0]"},
		{`{"input":"x","profile":{"budget":{"maxToolCalls":-1}}}`, "profile.budget"},
		{`{"input":"x","profile":{"tools":[" "]}}`, "profile.tools[0]"},
		{`{"input":"x","profile":{"skills":[` + strings.Repeat(`"a",`, maxGlobs) + `"b"]}}`, "profile.skills lists"},
		{`{"input":"x","profile":{"instructions":[""]}}`, "profile.instructions[0]"},
		{`{"input":"x","profile":{"model":"no such model"}}`, "profile.model"},
		{`{"input":"x","profile":{"model":"nowhere"}}`, "models"},
		{`{"input":"x","profile":{"budget":{"deadline":"later"}}}`, "deadline"},
		{`{"input":"x","budget":{"maxRepeats":-2}}`, "budget limits"},
		{`{"input":"x","profile":{"unknown":1}}`, "unknown"},
	} {
		resp, b := do(t, "POST", srv.URL+"/v1/runs", tc.body)
		p := decodeProblem(t, resp, b, http.StatusBadRequest)
		if !strings.Contains(p.Detail, tc.want) {
			t.Errorf("%s: detail %q lacks %q", tc.body, p.Detail, tc.want)
		}
	}
	if list, _ := a.List(context.Background()); len(list) != 0 {
		t.Fatalf("runs = %d", len(list))
	}
}

func TestChildren(t *testing.T) {
	ctx := testCtx(t)
	a := newAgent(t, newScript(endTurn("parent"), endTurn("child")).model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"plan","wait":true}`)
	parent := decodeRun(t, resp, b, http.StatusOK)
	child, err := a.Run(ctx, "part", fw.WithParent(parent.ID))
	if err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs?parent="+parent.ID, "")
	var list struct{ Runs []fw.Run }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &list) != nil {
		t.Fatalf("list = %d %s", resp.StatusCode, b)
	}
	if len(list.Runs) != 1 || list.Runs[0].ID != child.ID || list.Runs[0].Parent != parent.ID || list.Runs[0].Depth != 1 {
		t.Fatalf("children = %+v", list.Runs)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs?parent="+child.ID, "")
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &list) != nil || len(list.Runs) != 0 {
		t.Fatalf("children of a leaf = %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs?parent=nope", "")
	if p := decodeProblem(t, resp, b, http.StatusNotFound); !strings.Contains(p.Detail, "nope") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs", "")
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &list) != nil || len(list.Runs) != 2 {
		t.Fatalf("all runs = %d %s", resp.StatusCode, b)
	}
}

func TestSkills(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	dir := filepath.Join(cfg.Home, "skills", "review")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: review\ndescription: Review a pull request\n---\n# Review\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "lint.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg))
	srv := serve(t, a)

	resp, b := do(t, "GET", srv.URL+"/v1/skills", "")
	var list struct{ Skills []skillInfo }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &list) != nil || len(list.Skills) != 1 ||
		list.Skills[0].Name != "review" || list.Skills[0].Description != "Review a pull request" {
		t.Fatalf("skills = %d %s", resp.StatusCode, b)
	}
	if strings.Contains(string(b), cfg.Home) {
		t.Errorf("the list leaks the directory: %s", b)
	}

	resp, b = do(t, "GET", srv.URL+"/v1/skills/review", "")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") || string(b) != body {
		t.Fatalf("skill = %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}

	resp, b = do(t, "GET", srv.URL+"/v1/skills/review", "", "Accept", "application/json")
	var doc skillDoc
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" || json.Unmarshal(b, &doc) != nil {
		t.Fatalf("skill json = %d %s", resp.StatusCode, b)
	}
	if doc.Name != "review" || doc.Body != body || len(doc.Files) != 1 || doc.Files[0] != "scripts/lint.sh" {
		t.Fatalf("doc = %+v", doc)
	}

	for _, name := range []string{"nope", "..", "%2E%2E"} {
		resp, b = do(t, "GET", srv.URL+"/v1/skills/"+name, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d %s", name, resp.StatusCode, b)
		}
	}
}

func TestAgentsCarryInstructionsAndSkills(t *testing.T) {
	long := strings.Repeat("Be careful. ", 1000)
	a := newAgent(t, newScript().model(), fw.WithProfile(fw.AgentProfile{
		Name: "reviewer", Description: "Reviews code", System: long, Instructions: []string{"Rule one.", "Rule two."}, Skills: []string{"review-*"},
	}))
	srv := serve(t, a)
	resp, b := do(t, "GET", srv.URL+"/v1/agents", "")
	var agents struct{ Agents []map[string]any }
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &agents) != nil || len(agents.Agents) != 1 {
		t.Fatalf("agents = %d %s", resp.StatusCode, b)
	}
	got := agents.Agents[0]
	if got["description"] != "Reviews code" || got["system"] != long {
		t.Errorf("agent = %+v", got)
	}
	if ins, _ := got["instructions"].([]any); len(ins) != 2 || ins[1] != "Rule two." {
		t.Errorf("instructions = %v", got["instructions"])
	}
	if sk, _ := got["skills"].([]any); len(sk) != 1 || sk[0] != "review-*" {
		t.Errorf("skills = %v", got["skills"])
	}
}

func TestAttachments(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	a := newAgent(t, newScript(endTurn("first"), endTurn("second")).model(), fw.WithConfig(cfg))
	srv := serve(t, a)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	// A run starts with a file, and a message can hold files alone.
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"look","wait":true,"files":[{"name":"a.txt","data":"`+b64("alpha")+`"}]}`)
	r := decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"files":[{"name":"b.txt","data":"`+b64("beta")+`"}]}`)
	decodeRun(t, resp, b, http.StatusAccepted)
	if r, err := a.Wait(ctx, r.ID); err != nil || r.Output != "second" {
		t.Fatalf("run = %+v, %v", r, err)
	}
	for name, want := range map[string]string{"a.txt": "alpha", "b.txt": "beta"} {
		if got, err := os.ReadFile(filepath.Join(root, "attachments", name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
	var refs []fw.FileRef
	for _, e := range collect(t, a, r.ID) {
		var q fw.RunQueuedData
		var m fw.MessageReceivedData
		switch {
		case e.Type == fw.EvRunQueued && e.Decode(&q) == nil:
			for _, f := range q.Files {
				refs = append(refs, *f.File)
			}
		case e.Type == fw.EvMessageReceived && e.Decode(&m) == nil:
			for _, f := range m.Content {
				refs = append(refs, *f.File)
			}
		}
	}
	if len(refs) != 2 || refs[0].Path != "attachments/a.txt" || refs[1].Path != "attachments/b.txt" || refs[1].Size != 4 {
		t.Fatalf("references = %+v", refs)
	}

	// The workspace refuses a secret, and a message without files keeps
	// the limit of 1 MB.
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"files":[{"name":".env","data":"`+b64("KEY=1")+`"}]}`)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, `".env"`) {
		t.Errorf("detail = %q", p.Detail)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"`+strings.Repeat("x", MaxBodyBytes)+`"}`)
	decodeProblem(t, resp, b, http.StatusRequestEntityTooLarge)
}

// GET /v1/runs?by= keeps a part of the runs, and the total and the glance
// count that part.
func TestListRunsByTeam(t *testing.T) {
	team := tokenMap{
		"alice-token": {Subject: "alice", Kind: fw.KindUser, Groups: []string{"pay"}},
		"bob-token":   {Subject: "bob", Kind: fw.KindUser, Groups: []string{"pay"}},
		"carol-token": {Subject: "carol", Kind: fw.KindUser, Groups: []string{"ops"}},
	}
	a := newAgent(t, newScript(endTurn("a"), endTurn("b"), endTurn("c")).model(), fw.WithAuthn(team))
	srv := serve(t, a)
	for _, tok := range []string{"alice-token", "bob-token", "carol-token"} {
		resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi","wait":true}`, "Authorization", "Bearer "+tok)
		decodeRun(t, resp, b, http.StatusOK)
	}
	for by, want := range map[string]int{"me": 1, "team": 2, "others": 1, "": 3} {
		resp, b := do(t, "GET", srv.URL+"/v1/runs?by="+by, "", "Authorization", "Bearer alice-token")
		var body struct {
			Runs   []fw.Run `json:"runs"`
			Total  int      `json:"total"`
			Glance struct {
				Finished int `json:"finished"`
			} `json:"glance"`
		}
		if err := json.Unmarshal(b, &body); err != nil || resp.StatusCode != 200 || body.Total != want || len(body.Runs) != want || body.Glance.Finished != want {
			t.Errorf("by=%q: %d %s", by, resp.StatusCode, b)
		}
	}
	resp, b := do(t, "GET", srv.URL+"/v1/runs?by=everyone", "", "Authorization", "Bearer alice-token")
	decodeProblem(t, resp, b, http.StatusBadRequest)
}
