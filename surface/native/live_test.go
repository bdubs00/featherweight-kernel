package native

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestLiveRoute checks GET /v1/runs/{id}/live: a viewer of the run gets the
// text of the answer as the model yields it, then done and idle, and the
// stream ends with the run. A person who cannot view the run gets a 403,
// and an unknown run a 404.
func TestLiveRoute(t *testing.T) {
	ctx := testCtx(t)
	ready := make(chan struct{})
	model := fw.ModelFunc{StreamFn: func(ctx context.Context, _ *fw.Request, on func(fw.Delta)) (*fw.Response, error) {
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		on(fw.Delta{Type: "text", Text: "Hi there"})
		return endTurn("Hi there"), nil
	}}
	callers := tokenMap{
		"alice-token":   {Subject: "alice", Kind: fw.KindUser},
		"mallory-token": {Subject: "mallory", Kind: fw.KindUser},
	}
	cfg := fw.DefaultConfig()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"alice"}
	a := newAgent(t, model, fw.WithAuthn(callers), fw.WithConfig(cfg))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi"}`, "Authorization", "Bearer alice-token")
	r := decodeRun(t, resp, b, http.StatusCreated)

	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r.ID+"/live", "", "Authorization", "Bearer mallory-token")
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a stranger opened the live stream: %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/nope/live", "", "Authorization", "Bearer alice-token")
	decodeProblem(t, resp, b, http.StatusNotFound)

	stream := openStream(t, ctx, "GET", srv.URL+"/v1/runs/"+r.ID+"/live", "", "Authorization", "Bearer alice-token")
	close(ready)
	sse := newSSE(stream.Body)
	var got []string
	var text string
	for {
		f, ok := sse.next()
		if !ok {
			break
		}
		if f.comment != "" {
			continue
		}
		if f.id != "" {
			t.Fatalf("a live event has an id: %+v", f)
		}
		got = append(got, f.event)
		var d struct {
			Turn       int    `json:"turn"`
			Text       string `json:"text"`
			StopReason string `json:"stopReason"`
			State      string `json:"state"`
		}
		if err := json.Unmarshal([]byte(f.data), &d); err != nil {
			t.Fatalf("data %q: %v", f.data, err)
		}
		switch f.event {
		case fw.LiveDelta:
			text += d.Text
			if d.Turn != 1 {
				t.Fatalf("delta = %+v", d)
			}
		case fw.LiveDone:
			if d.StopReason != string(fw.StopEndTurn) {
				t.Fatalf("done = %+v", d)
			}
		case fw.LiveIdle:
			if d.State != string(fw.StateCompleted) {
				t.Fatalf("idle = %+v", d)
			}
		}
	}
	if len(got) != 3 || got[0] != "delta" || got[1] != "done" || got[2] != "idle" || text != "Hi there" {
		t.Fatalf("events = %v, text %q", got, text)
	}
}
