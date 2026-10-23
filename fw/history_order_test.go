package fw

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// roleTexts names each message of a history as role:text, so a test can
// compare the order in one line.
func roleTexts(msgs []Message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		text := TextOf(m.Content)
		if text == "" && len(m.Content) > 0 {
			text = string(m.Content[0].Type)
		}
		parts[i] = string(m.Role) + ":" + text
	}
	return strings.Join(parts, " | ")
}

// A message that arrives while the model thinks follows the answer of that
// call, so the next request ends with it. The model of this test refuses a
// request that ends with an answer, as Claude 5.5 models refuse a prefill.
// The run of 2026-10-23 (01M5KPW7C3Y57WGMQF0H5JQA2K) failed so.
func TestFollowUpDuringAModelCallFollowsTheAnswer(t *testing.T) {
	ctx := testCtx(t)
	entered, release := make(chan struct{}), make(chan struct{})
	answers := []*Response{endTurn("first answer"), endTurn("second answer")}
	var mu sync.Mutex
	var reqs [][]Message
	m := ModelFunc{StreamFn: func(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, append([]Message(nil), req.Messages...))
		mu.Unlock()
		if n == 0 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if last := req.Messages[len(req.Messages)-1]; last.Role == RoleAssistant {
			return nil, errors.New("400: This model does not support assistant message prefill. The conversation must end with a user message.")
		}
		if n >= len(answers) {
			return nil, errors.New("no answer left")
		}
		return answers[n], nil
	}}
	a := newTestAgent(t, m)
	r, err := a.Start(ctx, "check the cluster")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := a.Send(ctx, r.ID, UserText("@k8s-operator from reviewer: also check the ingress"), OriginHTTP); err != nil {
		t.Fatal(err)
	}
	close(release)
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)

	// The ledger holds the order of the incident: the request, the
	// message, then the answer.
	types := eventTypes(ledgerEvents(t, a.Ledger(), r.ID))
	got := strings.Join(types, " ")
	if !strings.Contains(got, EvModelRequested+" "+EvMessageReceived+" "+EvModelResponded) {
		t.Fatalf("events = %v", types)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "user:check the cluster | assistant:first answer | user:@k8s-operator from reviewer: also check the ingress"
	if len(reqs) != 2 || roleTexts(reqs[1]) != want {
		t.Fatalf("second request = %s", roleTexts(reqs[len(reqs)-1]))
	}
}

// A kernel that restarts rebuilds the history from the ledger in the same
// order: the message that arrived during a call follows its answer.
func TestResumedHistoryKeepsTheFollowUpAfterTheAnswer(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "run-order"
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "check the cluster", Origin: OriginHTTP}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{Provider: "anthropic", Model: "claude-opus-5-5"}),
		MustEvent(id, EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text("also check the ingress")}, Origin: OriginHTTP}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{Text("first answer")}, StopReason: StopEndTurn}),
		MustEvent(id, EvTurnFinished, TurnData{Turn: 1}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 2}),
		MustEvent(id, EvModelRequested, ModelRequestedData{Provider: "anthropic", Model: "claude-opus-5-5"}),
	)
	s := newScript(endTurn("second answer"))
	a := newTestAgent(t, s.Model(), WithLedger(l))
	r, err := a.Wait(ctx, id)
	mustState(t, r, err, StateCompleted)
	want := "user:check the cluster | assistant:first answer | user:also check the ingress"
	if reqs := s.requests(); len(reqs) != 1 || roleTexts(reqs[0].Messages) != want {
		t.Fatalf("requests = %d, resumed request = %s", len(reqs), roleTexts(reqs[len(reqs)-1].Messages))
	}
}

// The other orders in which a message can meet a model call.
func TestProjectHistoryOrdersMessagesOfAModelCall(t *testing.T) {
	q := func() Event { return MustEvent("R", EvRunQueued, RunQueuedData{Input: "go"}) }
	req := func() Event { return MustEvent("R", EvModelRequested, ModelRequestedData{}) }
	msg := func(text string) Event {
		return MustEvent("R", EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text(text)}})
	}
	answer := func() Event {
		return MustEvent("R", EvModelResponded, ModelRespondedData{Content: []Block{Text("answer")}, StopReason: StopEndTurn})
	}
	cases := []struct {
		name string
		log  []Event
		want string
	}{
		{"two messages during a call", []Event{q(), req(), msg("m1"), msg("m2"), answer()},
			"user:go | assistant:answer | user:m1 | user:m2"},
		{"a message during a call that calls a tool waits for the result", []Event{q(), req(), msg("m"),
			MustEvent("R", EvModelResponded, ModelRespondedData{Content: []Block{use("c1", "t", `{}`)}, StopReason: StopToolUse}),
			MustEvent("R", EvToolResponded, ToolRespondedData{CallID: "c1", Content: []Block{Text("done")}})},
			"user:go | assistant:tool_use | user:tool_result | user:m"},
		{"a call without an answer sends the message with the next request", []Event{q(), req(), msg("m")},
			"user:go | user:m"},
		{"the next request answers after the message", []Event{q(), req(), msg("m"), req(), answer()},
			"user:go | user:m | assistant:answer"},
		{"a retried response keeps the message after it", []Event{q(), req(), msg("m"),
			MustEvent("R", EvError, ErrorData{Message: "model: cut (retrying the interrupted response)", Retryable: true}), answer()},
			"user:go | assistant:answer | user:m"},
		{"a run that ends during a call keeps the message", []Event{q(), req(), msg("m"),
			MustEvent("R", EvRunFinished, RunFinishedData{Outcome: OutcomeFailed}), msg("m2"), req(), answer()},
			"user:go | user:m | user:m2 | assistant:answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleTexts(ProjectHistory(evs("R", tc.log...)).Messages); got != tc.want {
				t.Fatalf("history = %s", got)
			}
		})
	}
}

// The kernel never sends a request that ends with an answer of the model.
// A ledger whose message the history drops leaves the run with nothing to
// say after the answer, and the run fails with the reason before any call.
func TestRequestThatEndsWithAnAnswerFailsBeforeTheCall(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "run-prefill"
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi"}),
		MustEvent(id, EvRunStarted, RunStartedData{}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{Text("answer")}, StopReason: StopEndTurn}),
		MustEvent(id, EvTurnFinished, TurnData{Turn: 1}),
		MustEvent(id, EvMessageReceived, MessageReceivedData{Role: RoleUser}),
	)
	s := newScript(endTurn("never"))
	a := newTestAgent(t, s.Model(), WithLedger(l))
	r, err := a.Wait(ctx, id)
	mustState(t, r, err, StateFailed)
	if len(s.requests()) != 0 || !strings.Contains(r.Error, "ends with an answer of the model") {
		t.Fatalf("model calls = %d, error = %q", len(s.requests()), r.Error)
	}
}
