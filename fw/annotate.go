package fw

import (
	"context"
	"maps"
)

// A RunAnnotator adds metadata to a run when the kernel queues it. The
// kernel calls each annotator after the authorization of the request, with
// the context of the caller, the request, and the parent run when the
// request starts a child. The annotator returns the entries to add. An
// entry that the request already holds stays as it is. The result must
// fit the bounds of CheckMetadata, or the kernel logs a warning and adds
// nothing from that annotator.
//
// The kernel knows nothing about the entries. A package that a program
// links in gives them a meaning. The workbench records the teams of the
// person who starts a run this way.
type RunAnnotator func(ctx context.Context, cfg *Config, req *RunRequest, parent *Run) map[string]string

// annotate adds the entries of the annotators to meta, which it returns.
// It never changes the metadata of the request.
func (a *Agent) annotate(ctx context.Context, req *RunRequest, parent *Run, meta map[string]string) map[string]string {
	if len(a.annotators) == 0 {
		return meta
	}
	out := maps.Clone(meta)
	for _, fn := range a.annotators {
		add := fn(ctx, a.cfg, req, parent)
		if len(add) == 0 {
			continue
		}
		next := maps.Clone(out)
		if next == nil {
			next = map[string]string{}
		}
		for k, v := range add {
			if _, ok := next[k]; !ok {
				next[k] = v
			}
		}
		if err := CheckMetadata(next); err != nil {
			a.log.Warn("fw: a run annotator gave metadata out of bounds; skipping it", "run", req.ID, "error", err)
			continue
		}
		out = next
	}
	return out
}
