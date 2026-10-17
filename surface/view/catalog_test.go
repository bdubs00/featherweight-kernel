package view

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"runtime"
	"strings"
	"sync"
	"testing"
	"weak"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// scripted is a fake model that answers with the responses in order and
// then ends every turn with "ok".
func scripted(responses ...*fw.Response) fw.Model {
	var mu sync.Mutex
	next := 0
	return fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if next < len(responses) {
			r := responses[next]
			next++
			return r, nil
		}
		return answer("ok"), nil
	}}
}

// calls is a model response with an optional text and tool calls.
func calls(text string, uses ...fw.Block) *fw.Response {
	var content []fw.Block
	if text != "" {
		content = append(content, fw.Text(text))
	}
	content = append(content, uses...)
	return &fw.Response{Content: content, StopReason: fw.StopToolUse, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

// answer is a model response that ends the turn with text.
func answer(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

// use is a tool call block.
func use(id, name, input string) fw.Block {
	return fw.Block{Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}

// okTool is a tool that answers "done".
func okTool(spec fw.ToolSpec) fw.Tool {
	return fw.NewTool(spec, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("done"), nil
	})
}

func TestCatalog(t *testing.T) {
	a := newAgent(t, fw.WithTools(
		okTool(fw.ToolSpec{Name: "zeta", Source: "builtin", Title: "Do zeta"}),
		okTool(fw.ToolSpec{Name: "remote", Source: "github", Description: "Opens pull requests. It needs a token."}),
		okTool(fw.ToolSpec{Name: "alpha", Description: "Alpha does things.\nMore."}),
		okTool(fw.ToolSpec{Name: "a2", Source: "atlassian"}),
		okTool(fw.ToolSpec{Name: "beta", Source: "builtin", Action: "Run beta"}),
	))
	got, err := Catalog(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, c := range got {
		rows = append(rows, c.Name+"="+c.Text)
	}
	want := "alpha=Alpha does things|beta=beta|zeta=Do zeta|a2=a2|remote=Opens pull requests"
	if s := strings.Join(rows, "|"); s != want {
		t.Errorf("catalog = %s\nwant      %s", s, want)
	}
	b, _ := json.Marshal(got[1])
	for _, want := range []string{`"name":"beta"`, `"action":"Run beta"`, `"source":"builtin"`, `"text":"beta"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("capability JSON lacks %s: %s", want, b)
		}
	}
	empty, err := Catalog(context.Background(), newAgent(t))
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("empty catalog = %#v, %v", empty, err)
	}
}

// paddedLedger appends filler events to a read of one run, after the
// events of the run. Each filler has a payload of its own. Before the last
// filler, it counts how many earlier payloads are still in memory. A reader
// that holds the whole ledger keeps them all.
type paddedLedger struct {
	fw.Ledger
	mu    sync.Mutex
	run   string
	n     int
	alive int // the payloads in memory before the last filler, -1 before a padded read
}

// pad pads the next reads of run with n fillers.
func (l *paddedLedger) pad(run string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.run, l.n, l.alive = run, n, -1
}

func (l *paddedLedger) kept() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alive
}

func (l *paddedLedger) Read(ctx context.Context, runID string, from uint64) iter.Seq2[fw.Event, error] {
	l.mu.Lock()
	n := l.n
	if runID != l.run {
		n = 0
	}
	l.mu.Unlock()
	inner := l.Ledger.Read(ctx, runID, from)
	if n == 0 {
		return inner
	}
	return func(yield func(fw.Event, error) bool) {
		var seq uint64
		for e, err := range inner {
			if !yield(e, err) || err != nil {
				return
			}
			seq = e.Seq
		}
		payloads := make([]weak.Pointer[byte], 0, n)
		for i := range n {
			if i == n-1 {
				runtime.GC()
				alive := 0
				for _, p := range payloads {
					if p.Value() != nil {
						alive++
					}
				}
				l.mu.Lock()
				l.alive = alive
				l.mu.Unlock()
			}
			data := []byte(`"` + strings.Repeat("x", 4<<10) + `"`)
			payloads = append(payloads, weak.Make(&data[0]))
			seq++
			if !yield(fw.Event{SpecVersion: "1.0", ID: fmt.Sprint("filler-", seq), Type: "test.filler", Subject: runID, Seq: seq, Data: data}, nil) {
				return
			}
		}
	}
}
