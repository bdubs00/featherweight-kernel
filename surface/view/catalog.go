package view

// This file is the catalog of tools: every tool of the runtime with its
// plain phrase, as GET /v1/tools returns it.

import (
	"context"
	"fmt"
	"sort"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Capability is one tool of the runtime in the catalog: its spec and, in
// plain words, what it does.
type Capability struct {
	fw.ToolSpec
	Text string `json:"text"` // the plain phrase, as Ability.Text
}

// Catalog lists the tools of the runtime that the caller can learn about,
// as GET /v1/tools does, each with its plain phrase. What one agent can
// do is the Abilities of its view (see Agent).
func Catalog(ctx context.Context, a *fw.Agent) ([]Capability, error) {
	specs, err := a.ToolSpecs(ctx)
	if err != nil {
		return nil, fmt.Errorf("view: list the tools: %w", err)
	}
	out := make([]Capability, 0, len(specs))
	for _, s := range specs {
		out = append(out, Capability{ToolSpec: s, Text: toolPhrase(s)})
	}
	// The built-in tools come first, then the tools of each source by the
	// name of the source, each group by tool name.
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := sourceKey(out[i].Source), sourceKey(out[j].Source)
		if si != sj {
			return si < sj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// sourceKey orders the sources of tools: the built-in tools first, then
// the other sources by name.
func sourceKey(source string) string {
	if source == "" || source == "builtin" {
		return ""
	}
	return "\x01" + source
}
