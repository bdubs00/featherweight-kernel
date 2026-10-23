package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// POST /v1/runs takes check: a check run with an empty input that calls
// the tool with no model and answers with the results.
func TestCreateCheckRun(t *testing.T) {
	s := newScript() // the model must not be called
	queue := fw.NewTool(fw.ToolSpec{Name: "queue.read", ReadOnly: true}, func(_ context.Context, in json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult(`{"records": [{"number": "INC0012"}], "query": ` + string(in) + `}`), nil
	})
	a := newAgent(t, s.model(), fw.WithTools(queue))
	srv := serve(t, a)

	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"agent": "default", "input": "", "wait": true, "title": "Look at the queue",
		"check": {"steps": [{"tool": "queue.read", "input": {"q": "open"}}], "timeout": "10s", "maxBytes": 4096}}`)
	r := decodeRun(t, resp, b, 200)
	if r.State != fw.StateCompleted || r.Kind() != fw.RunKindCheck || r.Title != "Look at the queue" {
		t.Fatalf("run = %+v", r)
	}
	var out fw.CheckOutput
	if err := json.Unmarshal([]byte(r.Output), &out); err != nil || len(out.Results) != 1 {
		t.Fatalf("output = %s (%v)", r.Output, err)
	}
	if res := out.Results[0]; res.Tool != "queue.read" || res.IsError || res.Cut || string(res.Output) != `{"records":[{"number":"INC0012"}],"query":{"q":"open"}}` {
		t.Fatalf("result = %+v (output %s)", res, res.Output)
	}
	if r.Usage.InputTokens != 0 || r.Usage.OutputTokens != 0 || r.Usage.CostUSD != 0 || len(s.requests()) != 0 {
		t.Fatalf("usage %+v, model calls %d", r.Usage, len(s.requests()))
	}
}

// A check that does not fit, and a request with the kind of the kernel,
// get a 400 that names the problem. An empty input without a check
// still gets a 400.
func TestCreateCheckRunRefusals(t *testing.T) {
	a := newAgent(t, newScript().model())
	srv := serve(t, a)
	for _, tc := range []struct{ body, want string }{
		{`{"input": "", "check": {"steps": []}}`, "check.steps has 0 steps; a check has 1 to 5"},
		{`{"input": "", "check": {"steps": [{"tool": "a", "input": [1]}]}}`, "check.steps[0].input is not a JSON object"},
		{`{"input": "", "check": {"steps": [{"tool": "a"}], "timeout": "2m"}}`, "check.timeout 2m0s is longer than the limit of 1m0s"},
		{`{"input": "", "check": {"steps": [{"tool": "a"}], "timeout": "soon"}}`, `check.timeout "soon" is not a duration`},
		{`{"input": "", "check": {"steps": [{"tool": "a"}], "maxBytes": 5000000}}`, "check.maxBytes 5000000 is above the limit"},
		{`{"input": "", "check": {"steps": [{"tool": "a", "extra": 1}]}}`, "unknown field"},
		{`{"input": "look", "metadata": {"fw.kind": "check"}}`, "fw.kind is reserved"},
		{`{"input": " "}`, "the field input is required"},
	} {
		resp, b := do(t, "POST", srv.URL+"/v1/runs", tc.body)
		p := decodeProblem(t, resp, b, 400)
		if !strings.Contains(p.Detail, tc.want) {
			t.Errorf("%s: detail %q lacks %q", tc.body, p.Detail, tc.want)
		}
	}
}

// GET /v1/runs leaves out the check runs, also from the total and the
// glance, unless the query has kind=check, which lists them alone.
func TestListRunsLeavesOutCheckRuns(t *testing.T) {
	ctx := testCtx(t)
	queue := fw.NewTool(fw.ToolSpec{Name: "queue.read", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("{}"), nil
	})
	a := newAgent(t, newScript(endTurn("done")).model(), fw.WithTools(queue))
	srv := serve(t, a)
	plain, err := a.Run(ctx, "an ordinary run")
	if err != nil {
		t.Fatal(err)
	}
	check, err := a.Run(ctx, "", fw.WithCheck(fw.Check{Steps: []fw.CheckStep{{Tool: "queue.read"}}}))
	if err != nil || check.State != fw.StateCompleted {
		t.Fatalf("check run = %+v, %v", check, err)
	}
	list := func(query string) ([]string, int) {
		t.Helper()
		resp, b := do(t, "GET", srv.URL+"/v1/runs"+query, "")
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d: %s", query, resp.StatusCode, b)
		}
		var v struct {
			Runs  []fw.Run `json:"runs"`
			Total int      `json:"total"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range v.Runs {
			ids = append(ids, r.ID)
		}
		return ids, v.Total
	}
	if ids, total := list(""); len(ids) != 1 || ids[0] != plain.ID || total != 1 {
		t.Fatalf("list = %v, total %d", ids, total)
	}
	if ids, total := list("?kind=check"); len(ids) != 1 || ids[0] != check.ID || total != 1 {
		t.Fatalf("kind=check = %v, total %d", ids, total)
	}
	if ids, _ := list("?kind=check&state=completed&agent=default"); len(ids) != 1 || ids[0] != check.ID {
		t.Fatalf("kind=check with filters = %v", ids)
	}
	// The run itself stays readable by its id.
	resp, b := do(t, "GET", srv.URL+"/v1/runs/"+check.ID, "")
	if r := decodeRun(t, resp, b, 200); r.Kind() != fw.RunKindCheck {
		t.Fatalf("run = %+v", r)
	}
}
