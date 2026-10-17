package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestWireNameFor(t *testing.T) {
	taken := map[string]string{}
	cases := map[string]string{
		"bash":       "bash",
		"git.commit": "git__commit",
		"a b/c":      "a_b_c",
		"":           "tool",
	}
	for in, want := range cases {
		if got := wireNameFor(in, taken); got != want {
			t.Errorf("wireNameFor(%q) = %q, want %q", in, got, want)
		}
	}
	taken["git__commit"] = "git.commit"
	if got := wireNameFor("git_commit", taken); got != "git_commit" {
		t.Errorf("distinct name should be kept, got %q", got)
	}
	taken["git__commit"] = "other"
	if got := wireNameFor("git.commit", taken); got != "git__commit_2" {
		t.Errorf("sanitized collision should get a suffix, got %q", got)
	}
	taken["x"] = "other"
	if got := wireNameFor("x", taken); got != "x" {
		t.Errorf("a clean name is returned as it is, got %q", got)
	}
}

func TestDottedToolNamesRoundTrip(t *testing.T) {
	req := &fw.Request{
		Messages: []fw.Message{fw.UserText("hi")},
		Tools: []fw.ToolSpec{{
			Name: "git.commit", Description: "commit",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	p, names, err := BuildParams(req, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if names["git__commit"] != "git.commit" {
		t.Fatalf("names map = %v", names)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "git__commit" {
		t.Fatalf("wire tools = %+v", body.Tools)
	}
}
