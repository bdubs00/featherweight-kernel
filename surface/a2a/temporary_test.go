package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// waitModel blocks every call until its context ends, so the runs of a
// test hold their places.
var waitModel = fw.ModelFunc{GenerateFn: func(ctx context.Context, _ *fw.Request) (*fw.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}}

// fullEnv is an environment whose backlog of runs is full.
func fullEnv(t *testing.T) *env {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.MaxRuns, cfg.MaxQueuedRuns = 1, 1
	e := newEnv(t, waitModel, fw.WithConfig(cfg))
	for range 2 {
		if _, err := e.a.Start(testCtx(t), "hold a place"); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// restError is the error document of the REST binding.
type restError struct {
	Error struct {
		Code    int              `json:"code"`
		Status  string           `json:"status"`
		Message string           `json:"message"`
		Details []map[string]any `json:"details"`
	} `json:"error"`
}

// reasonOf returns the fw reason and the retry hint of the error info.
func (d restError) reasonOf() (string, string) {
	for _, det := range d.Error.Details {
		if det["reason"] == "SERVER_ERROR" {
			m, _ := det["metadata"].(map[string]any)
			r, _ := m[metaReason].(string)
			after, _ := m["retryAfter"].(string)
			return r, after
		}
	}
	return "", ""
}

// A full backlog reaches an A2A client as a condition to retry: the server
// error with the reason SATURATED and a retry hint, and on the REST binding
// 429 with Retry-After, as the native API answers.
func TestSaturatedIsRetryable(t *testing.T) {
	if err := mapErr(&fw.SaturationError{MaxRuns: 1, MaxQueuedRuns: 1}); !errors.Is(err, sdk.ErrServerError) || !errors.Is(err, fw.ErrSaturated) || errors.Is(err, sdk.ErrInternalError) {
		t.Fatalf("mapErr = %v", err)
	}
	e := fullEnv(t)
	resp, body := postRaw(t, e, "/a2a/message:send", `{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"review"}]}}`)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("status %s, Retry-After %q, body %s", resp.Status, resp.Header.Get("Retry-After"), body)
	}
	var doc restError
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if reason, after := doc.reasonOf(); doc.Error.Code != 429 || doc.Error.Status != "RESOURCE_EXHAUSTED" || reason != reasonSaturated || after != "5" {
		t.Fatalf("error = %+v", doc.Error)
	}

	resp, body = postRaw(t, e, PathJSONRPC, `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"role":"ROLE_USER","messageId":"m2","parts":[{"text":"review"}]}}}`)
	var rpc struct {
		Error struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatalf("status %s, body %s: %v", resp.Status, body, err)
	}
	if rpc.Error.Code != -32000 || !strings.Contains(string(rpc.Error.Data), reasonSaturated) || !strings.Contains(string(rpc.Error.Data), `"retryAfterSeconds":5`) {
		t.Fatalf("JSON-RPC error = %d %s", rpc.Error.Code, rpc.Error.Data)
	}
}

// failLedger fails every append after the cut.
type failLedger struct {
	fw.Ledger
	cut atomic.Bool
}

func (l *failLedger) Append(ctx context.Context, ev fw.Event) (uint64, error) {
	if l.cut.Load() {
		return 0, errors.New("write /srv/private/ledger.jsonl: no space left on device")
	}
	return l.Ledger.Append(ctx, ev)
}

// A process that a storage failure blocked answers as unavailable with a
// retry hint, not as an internal error, and the answer holds no cause.
func TestStorageUnavailableIsRetryable(t *testing.T) {
	if err := mapErr(errors.Join(fw.ErrStorageUnavailable, errors.New("detail"))); !errors.Is(err, sdk.ErrServerError) || !errors.Is(err, fw.ErrStorageUnavailable) {
		t.Fatalf("mapErr = %v", err)
	}
	l := &failLedger{Ledger: fw.NewMemLedger()}
	e := newEnv(t, waitModel, fw.WithLedger(l))
	l.cut.Store(true)
	resp, body := postRaw(t, e, "/a2a/message:send", `{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"review"}]}}`)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "30" || strings.Contains(string(body), "/srv/private") {
		t.Fatalf("status %s, Retry-After %q, body %s", resp.Status, resp.Header.Get("Retry-After"), body)
	}
	var doc restError
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if reason, after := doc.reasonOf(); doc.Error.Status != "UNAVAILABLE" || reason != reasonStorage || after != "30" {
		t.Fatalf("error = %+v", doc.Error)
	}
}
