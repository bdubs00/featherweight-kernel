package routine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// labels is the label list of the fake helpdesk.
const labels = `[{"id":11,"name":"type/bug"},{"id":12,"name":"type/outage"},{"id":13,"name":"type/access"},{"id":21,"name":"status/new"},{"id":22,"name":"status/triaged"}]`

var labelIDs = map[string]int{"bug": 11, "outage": 12, "access": 13}

// ticket is one run of the triage agent: the agent labels the ticket with
// its kind and status/triaged, comments, and answers TRIAGE.
type ticket struct {
	number  int
	kind    string
	comment string // replaces the comment of the agent when set
	noReply bool   // the agent answers without the TRIAGE line
	extra   bool   // the agent also reads the issue at the end
}

func (k ticket) input() string {
	return fmt.Sprintf("Ticket #%d: something about %s", k.number, k.kind)
}

// script returns the responses of the model for the ticket.
func (k ticket) script() []*fw.Response {
	use := func(id, tool string, input any) fw.Block {
		b, _ := json.Marshal(input)
		return fw.Block{Type: fw.BlockToolUse, ID: id, Name: tool, Input: b}
	}
	calls := func(b ...fw.Block) *fw.Response {
		return &fw.Response{Content: b, StopReason: fw.StopToolUse}
	}
	comment := k.comment
	if comment == "" {
		comment = "Thanks. This looks like a " + k.kind + "."
	}
	out := []*fw.Response{
		calls(use("c1", "gitea.label_read", map[string]any{"method": "list", "owner": "lab", "repo": "helpdesk"})),
		calls(use("c2", "gitea.issue_write", map[string]any{"repo": "helpdesk", "owner": "lab", "method": "add_labels", "issue_number": k.number, "labels": []int{labelIDs[k.kind], 22}})),
		calls(use("c3", "gitea.issue_write", map[string]any{"method": "add_comment", "owner": "lab", "repo": "helpdesk", "issue_number": k.number, "body": comment})),
	}
	if k.extra {
		out = append(out, calls(use("c4", "gitea.issue_read", map[string]any{"method": "get", "owner": "lab", "repo": "helpdesk", "issue_number": k.number})))
	}
	reply := fmt.Sprintf(`TRIAGE {"number": %d, "type": "type/%s", "status": "status/triaged"}`, k.number, k.kind)
	if k.noReply {
		reply = "Done."
	}
	return append(out, &fw.Response{Content: []fw.Block{fw.Text(reply)}, StopReason: fw.StopEndTurn})
}

// recordRuns runs the triage agent over the tickets with a file ledger in
// dir, and returns the events of each run in order.
func recordRuns(t *testing.T, dir string, tickets []ticket) [][]fw.Event {
	t.Helper()
	scripts := map[string][]*fw.Response{}
	for _, k := range tickets {
		scripts[k.input()] = k.script()
	}
	model := fw.ModelFunc{GenerateFn: func(_ context.Context, req *fw.Request) (*fw.Response, error) {
		steps := 0
		for _, m := range req.Messages {
			if m.Role == fw.RoleAssistant {
				steps++
			}
		}
		s := scripts[fw.TextOf(req.Messages[0].Content)]
		if steps >= len(s) {
			return nil, fmt.Errorf("no response left")
		}
		return s[steps], nil
	}}
	tool := func(name string, result func(json.RawMessage) string) fw.Tool {
		return fw.NewTool(fw.ToolSpec{Name: name}, func(_ context.Context, in json.RawMessage) (fw.ToolResult, error) {
			return fw.TextResult(result(in)), nil
		})
	}
	tools := []fw.Tool{
		tool("gitea.label_read", func(json.RawMessage) string { return labels }),
		tool("gitea.issue_write", func(json.RawMessage) string { return `{"ok": true}` }),
		tool("gitea.issue_read", func(in json.RawMessage) string { return `{"labels": ["status/triaged"]}` }),
	}
	l, err := fw.OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModelName("anthropic/claude-opus-5-5"), fw.WithModel(model),
		fw.WithLedger(l), fw.WithBus(fw.NewMemBus()), fw.WithTools(tools...), fw.WithProfile(fw.AgentProfile{Name: "triage"}),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var ids []string
	for _, k := range tickets {
		r, err := a.Run(ctx, k.input(), fw.AsAgent("triage"))
		if err != nil || r.State != fw.StateCompleted {
			t.Fatalf("run %d: %+v, %v", k.number, r, err)
		}
		ids = append(ids, r.ID)
	}
	a.Close(ctx)
	_ = l.Close()
	ro, err := fw.OpenFileLedgerReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var out [][]fw.Event
	for _, id := range ids {
		var evs []fw.Event
		for e, err := range ro.Read(ctx, id, 0) {
			if err != nil {
				t.Fatal(err)
			}
			evs = append(evs, e)
		}
		out = append(out, evs)
	}
	return out
}

// triage is the routine of the triage agent, as a person would write it.
func triage() fw.Routine {
	return fw.Routine{
		Name: "triage", Version: 1, Standing: fw.RoutineShadow, Decider: "clef",
		Questions: map[string]fw.RoutineQuestion{
			"type": {
				Question: fw.Question{Type: fw.QuestionChoice, Instructions: "What kind of ticket is this?", Criteria: map[string]any{"bug": "Broken", "outage": "Down", "access": "Access", "other": "Else"}},
				Act:      0.85, Expect: fw.Expect{"bug", "outage", "access"},
				Observe: &fw.Observe{Answer: `"type": "type/([a-z-]+)"`},
			},
		},
		Steps: []fw.RoutineStep{
			{Tool: "gitea.label_read", Input: `{"method": "list", "owner": "lab", "repo": "helpdesk"}`, As: "labels"},
			{Tool: "gitea.issue_write", Input: `{"method": "add_labels", "owner": "lab", "repo": "helpdesk", "issue_number": {{ find .input "#(\\d+)" }}, "labels": [{{ find .labels (printf "\"id\":(\\d+),\"name\":\"type/%s\"" .type) }}, {{ find .labels "\"id\":(\\d+),\"name\":\"status/triaged\"" }}]}`},
			{Tool: "gitea.issue_write", Input: `{"method": "add_comment", "owner": "lab", "repo": "helpdesk", "issue_number": {{ find .input "#(\\d+)" }}, "body": {{ json (printf "Thanks. This looks like a %s." .type) }}}`},
		},
		Answer: `TRIAGE {"number": {{ find .input "#(\\d+)" }}, "type": "type/{{ .type }}", "status": "status/triaged"}`,
	}
}
