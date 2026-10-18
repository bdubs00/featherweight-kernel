package builtin

// This file covers the built-in ask_user tool across a restart of the
// process that held its run. The run died while the tool waited for the
// answer to its question. The ledger holds the question and its decided
// answer, and no channel survives the restart. On resume the kernel
// completes the call from the ledger, so the result that the model reads is
// the same as the result of the live path, and no new question goes out.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The question of the tests, as the recorded approval holds it.
const restartQuestion = "What is your name?"

// pauseSchema is the schema of a form question. The empty name of the const
// is "" here only to keep the shape explicit in the seeded events.
const pauseSchema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`

// testCtx returns a bounded context for one test.
func restartCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// restartScript returns canned responses in order and records the requests.
type restartScript struct {
	mu    sync.Mutex
	resps []*fw.Response
	reqs  []*fw.Request
}

func newRestartScript(resps ...*fw.Response) *restartScript {
	return &restartScript{resps: resps}
}

func (s *restartScript) Model() fw.Model {
	return fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reqs = append(s.reqs, req)
		if len(s.resps) == 0 {
			return nil, errors.New("restartscript: no response left")
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func (s *restartScript) requests() []*fw.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fw.Request(nil), s.reqs...)
}

// endTurnOf is the response that ends a turn, after the tool results.
func endTurnOf(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn}
}

// callAskUser is the response that starts the question.
func callAskUser(input string) *fw.Response {
	return &fw.Response{
		Content:    []fw.Block{toolUseOf("c1", "ask_user", input)},
		StopReason: fw.StopToolUse,
	}
}

func toolUseOf(id, name, input string) fw.Block {
	return fw.Block{Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}

// restartAgent builds an agent over a ledger that a restart left behind.
// Close runs with a bounded context, so a stuck wait cannot hold the test.
func restartAgent(t *testing.T, l fw.Ledger, model fw.Model, tools ...fw.Tool) *fw.Agent {
	t.Helper()
	a, err := fw.New(
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(model),
		fw.WithLedger(l),
		fw.WithBus(fw.NewMemBus()),
		fw.WithTools(tools...),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

// appendEvents writes events for one run.
func appendEvents(t *testing.T, l fw.Ledger, id string, evs ...fw.Event) {
	t.Helper()
	for _, e := range evs {
		e.Subject = id
		if _, err := l.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

// askUserBasis returns the events of a run whose model used ask_user with
// the input of a call, up to the recorded question a1 of call c1.
func askUserBasis(id, input string) []fw.Event {
	return []fw.Event{
		fw.MustEvent(id, fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "ask me", Origin: fw.OriginCLI}),
		fw.MustEvent(id, fw.EvRunStarted, fw.RunStartedData{}),
		fw.MustEvent(id, fw.EvTurnStarted, fw.TurnData{Turn: 1}),
		fw.MustEvent(id, fw.EvModelRequested, fw.ModelRequestedData{}),
		fw.MustEvent(id, fw.EvModelResponded, fw.ModelRespondedData{
			Content: []fw.Block{toolUseOf("c1", "ask_user", input)}, StopReason: fw.StopToolUse,
		}),
		fw.MustEvent(id, fw.EvToolRequested, fw.ToolRequestedData{Tool: "ask_user", CallID: "c1", Input: json.RawMessage(input)}),
		fw.MustEvent(id, fw.EvApprovalRequested, fw.ApprovalRequestedData{
			ApprovalID: "a1", Tool: "ask_user", CallID: "c1",
			Reason: restartQuestion, Schema: json.RawMessage(pauseSchema),
		}),
	}
}

func ledgerOf(t *testing.T, a *fw.Agent, id string) []fw.Event {
	t.Helper()
	var out []fw.Event
	for e, err := range a.Ledger().Read(context.Background(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func countOfType(evs []fw.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// resultText returns the text of the first tool_result block of the last
// message of a request, which is what the model read about the call.
func resultText(req *fw.Request) string {
	blocks := req.Messages[len(req.Messages)-1].Content
	for _, b := range blocks {
		if b.Type == fw.BlockToolResult {
			if len(b.Content) > 0 {
				return b.Content[0].Text
			}
			return b.Text
		}
	}
	return ""
}

func TestAskUserRestart(t *testing.T) {
	ctx := restartCtx(t)
	asker, err := fw.NewBuiltinTool("ask_user", fw.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("uses the recorded answer once", func(t *testing.T) {
		id := "aur1"
		l := fw.NewMemLedger()
		appendEvents(t, l, id, append(askUserBasis(id, `{"question":"`+restartQuestion+`"}`),
			fw.MustEvent(id, fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ada"}`)}))...)
		s := newRestartScript(endTurnOf("I know."))
		a := restartAgent(t, l, s.Model(), asker)
		r, err := a.Wait(ctx, id)
		if err != nil || r.State != fw.StateCompleted {
			t.Fatalf("run = %+v, err = %v", r, err)
		}
		reqs := s.requests()
		if len(reqs) != 1 {
			t.Fatalf("requests = %d", len(reqs))
		}
		if got := resultText(reqs[0]); got != `{"answer":"Ada"}` {
			t.Fatalf("tool result text = %q", got)
		}
		// The resume asked no fresh question.
		if n := countOfType(ledgerOf(t, a, id), fw.EvApprovalRequested); n != 1 {
			t.Fatal("the resume asked a fresh question")
		}
	})

	t.Run("a deny reaches the model as an error", func(t *testing.T) {
		id := "aur2"
		l := fw.NewMemLedger()
		appendEvents(t, l, id, append(askUserBasis(id, `{"question":"`+restartQuestion+`"}`),
			fw.MustEvent(id, fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "a1", Decision: "deny", Note: "no"}))...)
		s := newRestartScript(endTurnOf("understood"))
		a := restartAgent(t, l, s.Model(), asker)
		r, err := a.Wait(ctx, id)
		if err != nil || r.State != fw.StateCompleted {
			t.Fatalf("run = %+v, err = %v", r, err)
		}
		got := resultText(s.requests()[0])
		if !strings.Contains(got, "ask_user: no answer: fw: denied by") || !strings.Contains(got, "no") {
			t.Fatalf("tool result text = %q", got)
		}
		if n := countOfType(ledgerOf(t, a, id), fw.EvApprovalRequested); n != 1 {
			t.Fatal("the resume asked a fresh question")
		}
	})

	t.Run("a pending question stays idle until the answer", func(t *testing.T) {
		id := "aur3"
		l := fw.NewMemLedger()
		appendEvents(t, l, id, askUserBasis(id, `{"question":"`+restartQuestion+`"}`)...)
		s := newRestartScript(endTurnOf("I know."))
		a := restartAgent(t, l, s.Model(), asker)
		r, err := a.Get(ctx, id)
		if err != nil || r.State != fw.StateInputRequired {
			t.Fatalf("run = %+v, err = %v", r, err)
		}
		if r.Pending == nil || r.Pending.ID != "a1" || r.Pending.Reason != restartQuestion {
			t.Fatalf("pending = %+v", r.Pending)
		}
		if err := a.Decide(ctx, id, fw.ApprovalDecision{ApprovalID: "a1", Content: json.RawMessage(`{"answer":"Ada"}`)}); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, id)
		if err != nil || r.State != fw.StateCompleted {
			t.Fatalf("run = %+v, err = %v", r, err)
		}
		if got := resultText(s.requests()[0]); got != `{"answer":"Ada"}` {
			t.Fatalf("tool result text = %q", got)
		}
	})
}

// TestAskUserRestartMatchesLive runs every shape of answer through the live
// path and through the restored path, and makes the two feed the model the
// same result text.
func TestAskUserRestartMatchesLive(t *testing.T) {
	ctx := restartCtx(t)
	asker, err := fw.NewBuiltinTool("ask_user", fw.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	freeQuestion := `{"question":"` + restartQuestion + `"}`
	formQuestion := `{"question":"` + restartQuestion + `","schema":` + pauseSchema + `}`

	cases := []struct {
		name   string
		input  string              // the ask_user call input
		live   fw.ApprovalDecision // what the live decide records
		wanted string              // the result text of both paths
		misled bool                // the recorded question does not match the call
	}{
		{
			name:   "structured answer with an explicit schema",
			input:  formQuestion,
			live:   fw.ApprovalDecision{Decision: "allow", Content: json.RawMessage(`{"answer":"Ada"}`)},
			wanted: `{"answer":"Ada"}`,
		},
		{
			name:   "note only on a free-text question",
			input:  freeQuestion,
			live:   fw.ApprovalDecision{Decision: "allow", Note: "blue"},
			wanted: `{"answer":"blue"}`,
		},
		{
			name:   "a blank answer on a free-text question",
			input:  freeQuestion,
			live:   fw.ApprovalDecision{Decision: "allow"},
			wanted: `{"answer":""}`,
		},
		{
			name:   "a blank answer on a schema question",
			input:  formQuestion,
			live:   fw.ApprovalDecision{Decision: "allow"},
			wanted: "",
		},
		{
			name:   "a schema answer with trailing whitespace",
			input:  formQuestion,
			live:   fw.ApprovalDecision{Decision: "allow", Content: json.RawMessage(`{"answer":"  Ada  "}`)},
			wanted: `{"answer":"  Ada  "}`,
		},
		{
			name:   "a question of another call on the ledger",
			input:  freeQuestion,
			live:   fw.ApprovalDecision{Decision: "allow", Note: "blue"},
			wanted: "ask_user: the recorded question does not match this call, so the caller must answer it again.",
			misled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := func(ledger fw.Ledger, basis []fw.Event, decided fw.ApprovalDecidedData) (*restartScript, string) {
				t.Helper()
				appendEvents(t, ledger, tc.name+"-run", append(basis,
					fw.MustEvent(tc.name+"-run", fw.EvApprovalDecided, decided))...)
				rs := newRestartScript(endTurnOf("done"))
				b := restartAgent(t, ledger, rs.Model(), asker)
				id := tc.name + "-run"
				rr, err := b.Wait(ctx, id)
				if err != nil || rr.State != fw.StateCompleted {
					t.Fatalf("restored run = %+v, err = %v", rr, err)
				}
				if got := resultText(rs.requests()[0]); got != tc.wanted {
					t.Fatalf("restored result = %q, want %q", got, tc.wanted)
				}
				return rs, id
			}
			decided := fw.ApprovalDecidedData{
				ApprovalID: "a1", Decision: tc.live.Decision, Decider: "", Note: tc.live.Note, Content: tc.live.Content,
			}

			if tc.misled {
				// A question of another call sits under the call id on
				// the ledger, which is not a shape that the live path
				// can reach. The restored call refuses it instead of
				// turning the answer of one question into the answer of
				// another.
				basis := askUserBasis(tc.name+"-run", tc.input)
				basis[len(basis)-1] = fw.MustEvent(tc.name+"-run", fw.EvApprovalRequested, fw.ApprovalRequestedData{
					ApprovalID: "a1", Tool: "ask_user", CallID: "c1",
					Reason: "An unrelated question", Schema: json.RawMessage(pauseSchema),
				})
				restore(fw.NewMemLedger(), basis, decided)
				return
			}

			// The live path: a fresh run that asks, the caller decides,
			// and the run completes.
			live := fw.NewMemLedger()
			s := newRestartScript(callAskUser(tc.input), endTurnOf("done"))
			a := restartAgent(t, live, s.Model(), asker)
			r, err := a.Run(ctx, "ask me")
			if err != nil || r.State != fw.StateInputRequired {
				t.Fatalf("live run = %+v, err = %v", r, err)
			}
			if err := a.Decide(ctx, r.ID, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Decision: tc.live.Decision, Note: tc.live.Note, Content: tc.live.Content}); err != nil {
				t.Fatal(err)
			}
			r, err = a.Wait(ctx, r.ID)
			if err != nil || r.State != fw.StateCompleted {
				t.Fatalf("live run = %+v, err = %v", r, err)
			}
			if got := resultText(s.requests()[len(s.requests())-1]); got != tc.wanted {
				t.Fatalf("live result = %q, want %q", got, tc.wanted)
			}

			// The restored path: the same shape on the ledger, with the
			// decision recorded before the restart and no tool result.
			restore(fw.NewMemLedger(), askUserBasis(tc.name+"-run", tc.input), decided)
		})
	}
}
