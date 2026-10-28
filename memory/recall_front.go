package memory

// This file is the recall of runs across Tasks (docs/KNOWHOW.md section
// 4.7): with a front, the search of runs also asks the archive of the
// front for the runs of the same agent and the same principal.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// maxCachedRuns bounds the runs of the front that a Store remembers for a
// ref.
const maxCachedRuns = 256

// FrontRunsNote is the note of a recall whose search of the archive of the
// front got no answer.
const FrontRunsNote = "The archive of the front did not answer, so the search found only the runs of this Task."

// runCache holds the runs of the archive of the front that a search found,
// by scope and id, so a ref of such a run reads it. It forgets the oldest
// run past maxCachedRuns.
type runCache struct {
	mu    sync.Mutex
	runs  map[string]FrontRun
	order []string
}

// runKey names a run of the front in the cache: the agent, the principal,
// and the id, so a scope never reads a run that another scope found.
func runKey(scope fw.MemoryScope, id string) string {
	return scope.Agent + "\x00" + scope.Principal + "\x00" + id
}

func (c *runCache) put(scope fw.MemoryScope, r FrontRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runs == nil {
		c.runs = map[string]FrontRun{}
	}
	k := runKey(scope, r.ID)
	if _, ok := c.runs[k]; !ok {
		c.order = append(c.order, k)
	}
	c.runs[k] = r
	for len(c.order) > maxCachedRuns {
		delete(c.runs, c.order[0])
		c.order = c.order[1:]
	}
}

func (c *runCache) get(scope fw.MemoryScope, id string) (FrontRun, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runKey(scope, id)]
	return r, ok
}

// frontRuns searches the archive of the front for the runs of the agent
// and the principal of the scope that match the words, with the weights
// of searchRuns, and leaves out the run of the scope. It returns a note
// for the reader when the front did not answer. Without a front it finds
// nothing.
func (s *Store) frontRuns(ctx context.Context, scope fw.MemoryScope, text string, terms []string, limit int) ([]fw.MemoryHit, string) {
	if s.front == nil {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(ctx, PullWait)
	defer cancel()
	runs, err := s.front.runs(ctx, scope, text, limit)
	if err != nil {
		return nil, FrontRunsNote
	}
	var hits []fw.MemoryHit
	for _, r := range runs {
		if r.ID == "" || r.ID == scope.Task {
			continue
		}
		score, snippet := fieldScore(terms,
			weightedField{text: r.Title, weight: 6},
			weightedField{text: r.Input, weight: 2},
			weightedField{text: r.Output, weight: 2})
		if score == 0 {
			// The front matched the run by its own rules; it still counts,
			// below every run that matches here.
			score = 0.5
		}
		s.runs.put(scope, r)
		hits = append(hits, fw.MemoryHit{
			Record: fw.MemoryRecord{
				Ref:         "run://" + r.ID,
				Kind:        fw.MemoryKindRun,
				Scope:       scope,
				Title:       r.Title,
				Description: r.Outcome,
				Source:      "archive",
				Trust:       "untrusted",
				RunID:       r.ID,
				Updated:     r.At,
			},
			Score:   score,
			Snippet: snippet,
			Why:     "archive",
		})
	}
	return hits, ""
}

// mergeRuns adds the runs of the front to the runs of this Task, one hit
// per run id: a run that both hold keeps the hit of this Task.
func mergeRuns(local, front []fw.MemoryHit) []fw.MemoryHit {
	seen := map[string]bool{}
	for _, h := range local {
		seen[h.Record.RunID] = true
	}
	out := local
	for _, h := range front {
		if !seen[h.Record.RunID] {
			seen[h.Record.RunID] = true
			out = append(out, h)
		}
	}
	return out
}

// frontRunRecord returns a run of the front that a search of the scope
// found, as Get returns a run.
func (s *Store) frontRunRecord(scope fw.MemoryScope, id string) (fw.MemoryRecord, bool) {
	r, ok := s.runs.get(scope, id)
	if !ok {
		return fw.MemoryRecord{}, false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s\nAgent: %s\n", r.ID, scope.Agent)
	if r.Outcome != "" {
		fmt.Fprintf(&b, "Outcome: %s\n", r.Outcome)
	}
	if r.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", r.Title)
	}
	if r.Input != "" {
		fmt.Fprintf(&b, "\nInput:\n%s\n", r.Input)
	}
	if r.Output != "" {
		fmt.Fprintf(&b, "\nOutput:\n%s\n", r.Output)
	}
	return fw.MemoryRecord{
		Ref:     "run://" + r.ID,
		Kind:    fw.MemoryKindRun,
		Scope:   scope,
		Title:   r.Title,
		Body:    b.String(),
		Source:  "archive",
		Trust:   "untrusted",
		RunID:   r.ID,
		Updated: r.At,
	}, true
}
