// Package axmeta serves the ax metadata endpoints and the fw status for the
// ax control plane on the shared listener. See docs/ARCHITECTURE.md sections
// 11.1 and 11.8.
//
// The endpoints carry no authentication, as the ax runner contract expects.
// They serve the manifests that ax.Stash recorded, with the values of secret
// spec.env entries replaced by "***", and counts from the ledger.
package axmeta

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// Paths of the endpoints.
const (
	PathTask       = "/metadata/v1alpha1/ax/task"
	PathWorkspaces = "/metadata/v1alpha1/ax/workspaces"
	PathStatus     = "/metadata/v1alpha1/fw/status"
)

func init() {
	fw.RegisterSurface(ax.SurfaceName, func(*fw.Config) (fw.Surface, error) { return New(), nil })
}

// New returns the axmeta surface.
func New() fw.Surface { return fw.SurfaceFunc(mount) }

func mount(mux *http.ServeMux, a *fw.Agent) error {
	mux.HandleFunc("GET "+PathTask, serveTask)
	mux.HandleFunc("GET "+PathWorkspaces, serveWorkspaces)
	mux.HandleFunc("GET "+PathStatus, func(w http.ResponseWriter, r *http.Request) { serveStatus(w, r, a) })
	return nil
}

func serveTask(w http.ResponseWriter, _ *http.Request) {
	task, _ := ax.Stashed()
	if task == nil {
		http.Error(w, "task metadata not available", http.StatusNotFound)
		return
	}
	writeYAML(w, ax.Redacted(task))
}

func serveWorkspaces(w http.ResponseWriter, _ *http.Request) {
	task, workspaces := ax.Stashed()
	var docs []any
	for _, b := range ax.Bindings(task, workspaces) {
		if b.Workspace != nil {
			docs = append(docs, b.Workspace)
		}
	}
	if len(docs) == 0 {
		http.Error(w, "workspace metadata not available", http.StatusNotFound)
		return
	}
	writeYAML(w, docs...)
}

// writeYAML writes the documents as one YAML stream with "---" between them.
func writeYAML(w http.ResponseWriter, docs ...any) {
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			http.Error(w, "encode yaml", http.StatusInternalServerError)
			return
		}
	}
	if err := enc.Close(); err != nil {
		http.Error(w, "encode yaml", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(sb.String()))
}

// Status is the body of GET /metadata/v1alpha1/fw/status. Its usage and
// pendingApproval fields have the shape of the ax TaskStatus.
type Status struct {
	Usage           Usage            `json:"usage"`
	PendingApproval *PendingApproval `json:"pendingApproval"`
	Runs            RunCounts        `json:"runs"`
}

// Usage is the token usage over every run.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
}

// PendingApproval is the oldest approval that waits for a decision.
type PendingApproval struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	RequestedAt time.Time `json:"requestedAt"`
}

// RunCounts counts the runs by state.
type RunCounts struct {
	// Active counts the runs that are not in a terminal state.
	Active        int `json:"active"`
	InputRequired int `json:"inputRequired"`
	Total         int `json:"total"`
}

// ComputeStatus builds the status from a list of runs.
func ComputeStatus(runs []*fw.Run) Status {
	var s Status
	for _, r := range runs {
		if r == nil {
			continue
		}
		s.Runs.Total++
		s.Usage.PromptTokens += r.Usage.InputTokens
		s.Usage.CompletionTokens += r.Usage.OutputTokens
		if !r.State.Terminal() {
			s.Runs.Active++
		}
		if r.State == fw.StateInputRequired {
			s.Runs.InputRequired++
		}
		if p := r.Pending; p != nil {
			if s.PendingApproval == nil || p.RequestedAt.Before(s.PendingApproval.RequestedAt) {
				s.PendingApproval = &PendingApproval{ID: p.ID, Action: Action(p), RequestedAt: p.RequestedAt.UTC()}
			}
		}
	}
	return s
}

const maxSummary = 120

// Action describes an approval as "<tool>: <command or input summary>". The
// summary is the "command" field of the input when there is one, else the
// input as compact JSON, cut to 120 characters.
func Action(p *fw.Approval) string {
	tool := p.Tool
	if tool == "" {
		tool = "input"
	}
	summary := inputSummary(p.Input)
	if summary == "" {
		summary = p.Reason
	}
	summary = truncate(strings.Join(strings.Fields(summary), " "), maxSummary)
	if summary == "" {
		return tool
	}
	return tool + ": " + summary
}

func inputSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &in); err == nil && in.Command != "" {
		return in.Command
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func serveStatus(w http.ResponseWriter, r *http.Request, a *fw.Agent) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	runs, err := a.List(ctx)
	if err != nil && !errors.Is(err, fw.ErrNotImplemented) {
		a.Logger().Warn("axmeta: list runs", "error", err)
		http.Error(w, "status not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(ComputeStatus(runs))
}
