package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

type streamTransport func(*http.Request) (*http.Response, error)

func (f streamTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type streamReadError struct{ err error }

func (r streamReadError) Read([]byte) (int, error) { return 0, r.err }

func TestStreamInterruptionClassification(t *testing.T) {
	partial := "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"
	cases := []struct {
		name        string
		body        string
		err         error
		interrupted bool
	}{
		{"EOF without finish", partial, io.EOF, true},
		{"EOF in unframed JSON", partial + `data: {"choices":[{"delta":{"content":"cut`, io.EOF, true},
		{"EOF in unframed JSON with newline", partial + "data: {\"choices\":[\n", io.EOF, true},
		{"framed truncated JSON", "data: {\"choices\":[\n\n", io.EOF, false},
		{"unframed invalid JSON", "data: {nope", io.EOF, false},
		{"unexpected EOF", partial, io.ErrUnexpectedEOF, true},
		{"reset", partial, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"malformed chunk", "data: {nope\n\n", io.EOF, false},
		{"server error", "data: {\"error\":{\"message\":\"overloaded\"}}\n\n", io.EOF, false},
		{"stall", partial, errStalled, false},
		{"size cap", partial, errTooLarge, false},
		{"cancel", partial, context.Canceled, false},
		{"deadline", partial, context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(t, "http://model.invalid", "", "local", false)
			var calls, texts, blocks int
			m.client = &http.Client{Transport: streamTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(io.MultiReader(strings.NewReader(tc.body), streamReadError{tc.err}))}, nil
			})}
			resp, err := m.Stream(context.Background(), hello, func(d fw.Delta) {
				if d.Type == "text" {
					texts++
				}
				if d.Type == "block" {
					blocks++
				}
			})
			var interrupted *fw.StreamInterruptedError
			if err == nil || resp != nil || errors.As(err, &interrupted) != tc.interrupted {
				t.Fatalf("response=%+v error=%v interrupted=%v, want %v", resp, err, interrupted, tc.interrupted)
			}
			if calls != 1 || blocks != 0 {
				t.Fatalf("calls=%d blocks=%d, want one call and no partial blocks", calls, blocks)
			}
			underlying := tc.err
			if underlying == io.EOF {
				underlying = io.ErrUnexpectedEOF
			}
			if tc.interrupted && (texts != 1 || !errors.Is(err, underlying)) {
				t.Fatalf("lost incremental text or underlying error: texts=%d error=%v", texts, err)
			}
		})
	}
}

func TestStreamRemainsIncremental(t *testing.T) {
	delivered := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-delivered:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, sse(`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	m := testModel(t, srv.URL, "", "local", false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var texts []string
	var blocks int
	resp, err := m.Stream(ctx, hello, func(d fw.Delta) {
		if d.Type == "text" {
			texts = append(texts, d.Text)
			if len(texts) == 1 {
				close(delivered)
			}
		}
		if d.Type == "block" {
			blocks++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !reflect.DeepEqual(texts, []string{"Hel", "lo"}) || blocks != 1 || fw.TextOf(resp.Content) != "Hello" {
		t.Fatalf("calls=%d texts=%v blocks=%d response=%+v", calls.Load(), texts, blocks, resp)
	}
}

func TestStreamFinishWithoutDoneIsComplete(t *testing.T) {
	for _, ending := range []string{"", "\n", "\n\n"} {
		t.Run(fmt.Sprintf("ending=%q", ending), func(t *testing.T) {
			_, srv := newFake(t, reply{body: `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}` + ending})
			resp, err := testModel(t, srv.URL, "", "local", false).Generate(context.Background(), hello)
			if err != nil || fw.TextOf(resp.Content) != "done" {
				t.Fatalf("response=%+v error=%v", resp, err)
			}
		})
	}
}

func TestKernelRetriesCutOpenAIStreamInSameRun(t *testing.T) {
	// Even a syntactically complete tool call is not executable until the
	// response finishes. Its failed attempt also must not enter history.
	partial := "data: {\"choices\":[{\"delta\":{\"content\":\"discard\",\"tool_calls\":[{\"index\":0,\"id\":\"partial\",\"function\":{\"name\":\"echo\",\"arguments\":\"{}\"}}]}}]}\n\n"
	f, srv := newFake(t, reply{body: partial}, reply{body: sse(`{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)})
	m := testModelWith(t, settings{base: srv.URL, model: "local", cacheKey: "on"})
	var tools atomic.Int32
	tool := fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		tools.Add(1)
		return fw.TextResult("never"), nil
	})
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModelName("openai/local"), fw.WithModel(m), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(m.log), fw.WithTools(tool))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer a.Close(ctx)
	r, err := a.Run(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != fw.StateCompleted || r.Output != "done" || r.Usage.Turns != 1 || tools.Load() != 0 {
		t.Fatalf("tools=%d run=%+v", tools.Load(), r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 2 || !reflect.DeepEqual(f.bodies[0], f.bodies[1]) || f.bodies[0]["prompt_cache_key"] != r.ID {
		t.Fatalf("retry did not preserve request and run ID: %v", f.bodies)
	}
}
