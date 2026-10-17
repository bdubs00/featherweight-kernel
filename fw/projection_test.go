package fw

import (
	"encoding/json"
	"testing"
	"time"
)

func evs(id string, list ...Event) []Event {
	for i := range list {
		list[i].Subject = id
		list[i].Seq = uint64(i + 1)
	}
	return list
}

func TestProjectHistoryOrdersFollowUpsAfterToolResults(t *testing.T) {
	opaque := Block{Type: BlockOpaque, Provider: "anthropic", Raw: json.RawMessage(`{"type":"thinking"}`)}
	log := evs("R",
		MustEvent("R", EvRunQueued, RunQueuedData{Input: "start"}),
		MustEvent("R", EvModelResponded, ModelRespondedData{Content: []Block{opaque, use("a", "t", `{}`), use("b", "t", `{}`)}, StopReason: StopToolUse}),
		MustEvent("R", EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text("mid-turn")}}),
		MustEvent("R", EvToolResponded, ToolRespondedData{CallID: "b", Content: []Block{Text("B")}}),
		MustEvent("R", EvToolResponded, ToolRespondedData{CallID: "a", Content: []Block{Text("A")}}),
		MustEvent("R", EvModelResponded, ModelRespondedData{Content: []Block{use("c", "t", `{}`)}, StopReason: StopToolUse}),
		MustEvent("R", EvRunFinished, RunFinishedData{Outcome: OutcomeCanceled}),
		MustEvent("R", EvMessageReceived, MessageReceivedData{Role: RoleAssistant, Content: []Block{Text("ignored")}}),
	)
	h := ProjectHistory(log)
	if len(h.Messages) != 6 {
		t.Fatalf("messages = %+v", h.Messages)
	}
	if h.Messages[1].Content[0].Type != BlockOpaque {
		t.Fatal("the opaque block is not kept first")
	}
	res := h.Messages[2].Content
	if res[0].ToolUseID != "a" || TextOf(res[0].Content) != "A" || res[1].ToolUseID != "b" {
		t.Fatalf("results = %+v", res)
	}
	if TextOf(h.Messages[3].Content) != "mid-turn" {
		t.Fatalf("follow-up = %+v", h.Messages[3])
	}
	if last := h.Messages[5].Content[0]; last.ToolUseID != "c" || !last.IsError {
		t.Fatalf("canceled call = %+v", last)
	}
}

func TestProjectHistoryEpoch(t *testing.T) {
	kept := []Message{{Role: RoleAssistant, Content: []Block{Text("kept")}}}
	log := evs("R",
		MustEvent("R", EvRunQueued, RunQueuedData{Input: "old"}),
		MustEvent("R", EvContextCompacted, ContextCompactedData{Epoch: 2, Summary: Text("sum"), Kept: kept}),
		MustEvent("R", EvMessageReceived, MessageReceivedData{Content: []Block{Text("new")}}),
	)
	h := ProjectHistory(log)
	if h.Epoch != 2 || len(h.Messages) != 3 || TextOf(h.Messages[0].Content) != "sum" || TextOf(h.Messages[2].Content) != "new" {
		t.Fatalf("history = %+v", h)
	}
	var plain ContextCompactedData
	if err := log[1].Decode(&plain); err != nil || plain.Epoch != 2 {
		t.Fatalf("plain decode = %+v, %v", plain, err)
	}
}

func TestProjectRun(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	log := evs("R",
		MustEvent("R", EvRunQueued, RunQueuedData{Agent: "a", Input: "in", Origin: OriginHTTP, Principal: "p", Parent: "P", Metadata: map[string]string{"k": "v"}}),
		MustEvent("R", EvRunStarted, nil),
		MustEvent("R", EvTurnStarted, TurnData{Turn: 1}),
		MustEvent("R", EvModelResponded, ModelRespondedData{Provider: "fake", Model: "m", Usage: Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}}),
		MustEvent("R", EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap", Tool: "rm"}),
	)
	log[0].Time = t0
	r := ProjectRun(log, PriceTable{"fake/m": {Input: 1, Output: 2}})
	if r.State != StateInputRequired || r.Pending == nil || r.Pending.ID != "ap" || r.Usage.CostUSD != 3 || r.Usage.Turns != 1 ||
		r.Seq != 5 || !r.CreatedAt.Equal(t0) || r.Origin != OriginHTTP || r.Principal != "p" || r.Parent != "P" || r.Metadata["k"] != "v" {
		t.Fatalf("run = %+v", r)
	}
	decided := MustEvent("R", EvApprovalDecided, ApprovalDecidedData{ApprovalID: "ap", Decision: "allow"})
	decided.Seq = 6
	done := MustEvent("R", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "out"})
	done.Seq = 7
	more := append(log, decided, done)
	r = ProjectRun(more)
	if r.State != StateCompleted || r.Pending != nil || r.Output != "out" || r.Usage.CostUSD != 0 {
		t.Fatalf("finished run = %+v", r)
	}
}

func TestProjectHistoryClosesToolUseOfOtherStops(t *testing.T) {
	log := evs("R",
		MustEvent("R", EvRunQueued, RunQueuedData{Input: "go"}),
		MustEvent("R", EvModelResponded, ModelRespondedData{Content: []Block{Text("part"), use("t1", "x", `{}`)}, StopReason: StopMaxTokens}),
		MustEvent("R", EvMessageReceived, MessageReceivedData{Content: []Block{Text("Continue.")}, Origin: OriginKernel}),
	)
	h := ProjectHistory(log)
	if len(h.Messages) != 4 {
		t.Fatalf("messages = %+v", h.Messages)
	}
	if r := h.Messages[2].Content[0]; r.Type != BlockToolResult || r.ToolUseID != "t1" || !r.IsError {
		t.Fatalf("result = %+v", r)
	}
	if TextOf(h.Messages[3].Content) != "Continue." {
		t.Fatalf("last = %+v", h.Messages[3])
	}
}
