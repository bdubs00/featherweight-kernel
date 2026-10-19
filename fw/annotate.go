package fw

import (
	"context"
	"fmt"
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

// EvRunAnnotated is the event of Annotate.
const EvRunAnnotated = "fw.run.annotated"

// RunAnnotatedData is the payload of fw.run.annotated: metadata that a
// surface or a linked package adds to a run after the kernel queued it, such
// as a title that a model wrote. Metadata merges into the metadata of the
// run, and a later entry replaces an earlier one. Usage, Provider, and Model
// name the model call that produced the metadata, when a model wrote it.
// The usage and the cost of the run include that usage, and the metrics
// count it. The event changes the state of a run in no way, so one that
// arrives after fw.run.finished opens nothing again.
type RunAnnotatedData struct {
	Metadata map[string]string `json:"metadata,omitempty"`
	Usage    *Usage            `json:"usage,omitempty"`
	Provider string            `json:"provider,omitempty"`
	Model    string            `json:"model,omitempty"`
}

// metaIdempotencyKey is the metadata entry that the kernel keeps for the
// idempotency key of a request. Annotate refuses to change it.
const metaIdempotencyKey = "idempotencyKey"

// Annotate adds metadata to a run after the kernel queued it, as
// fw.run.annotated. With a principal in ctx, the caller needs the operator
// role on the run. The data must hold metadata or usage. The metadata of
// the run after the merge must fit the bounds of CheckMetadata, and the
// entry idempotencyKey cannot change. A finished run takes an annotation
// and stays finished.
func (a *Agent) Annotate(ctx context.Context, id string, d RunAnnotatedData) error {
	rs, err := a.k.lookup(id)
	if err != nil {
		return err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
		return err
	}
	if len(d.Metadata) == 0 && d.Usage == nil {
		return fmt.Errorf("%w: an annotation holds metadata or usage", ErrInvalidRequest)
	}
	for k := range d.Metadata {
		if kernelMeta(k) {
			return fmt.Errorf("%w: the metadata entry %s is the kernel's and cannot change", ErrInvalidRequest, k)
		}
	}
	merged := maps.Clone(rs.snapshot().Metadata)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, d.Metadata)
	if err := checkMetadataBounds(merged); err != nil {
		return err
	}
	_, err = a.emit(nil, rs, EvRunAnnotated, d)
	return err
}
