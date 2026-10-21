package fw

import (
	"context"
	"fmt"
)

// QualityObservation is a trusted application's observation, not a model's
// claim. ID is a stable, opaque receipt id for retry deduplication within this
// run. Do not put secrets in it. DuplicateEffects counts confirmed additional
// external effects, not tool intents, retries, or prevented duplicates.
//
// Completion names an own fw.run.finished sequence with outcome completed.
// Verification is accepted (a person accepted the result), satisfied (an
// external verifier checked it), rejected, or unknown. Empty reports effects
// only and requires Completion zero. There is one final verification per
// completion; unknown is final too. Report after all cost annotations for that
// completion. A later follow-up is a new completion, not a revision of a receipt.
type QualityObservation struct {
	ID               string `json:"id"`
	Completion       uint64 `json:"completion,omitempty"`
	Verification     string `json:"verification,omitempty"`
	DuplicateEffects uint64 `json:"duplicateEffects,omitempty"`
}

// QualityAnnotatedData extends fw.run.annotated without changing run state or
// metadata. Existing readers ignore Quality. CostUSD and UnpricedTokens are
// computed by ObserveQuality from own ledger usage, never supplied by callers.
// Cost is per completion episode: after the preceding own finished event,
// through this finish and subsequent annotations before the next work event,
// up to this receipt. Child usage and copied branch history are excluded.
// Future annotations cannot revise an exported histogram.
type QualityAnnotatedData struct {
	Quality        *QualityObservation `json:"quality,omitempty"`
	CostUSD        float64             `json:"qualityCostUSD"`
	UnpricedTokens int                 `json:"qualityUnpricedTokens"`
	CostComplete   bool                `json:"qualityCostComplete"`
}

// ObserveQuality records a trusted application's receipt as fw.run.annotated.
// It needs operator access with a principal, and is not exposed as a model
// tool. The application must verify external facts before calling it. A retry
// with the same ID and identical contents writes nothing, also after restart.
// A conflicting ID or second final verification returns ErrInvalidRequest.
// An effects-only receipt can arrive at any time and never reopens a run.
func (a *Agent) ObserveQuality(ctx context.Context, id string, d QualityObservation) error {
	rs, err := a.k.lookup(id)
	if err != nil {
		return err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
		return err
	}
	if d.ID == "" || len(d.ID) > 128 {
		return fmt.Errorf("%w: quality receipt id must hold 1 to 128 bytes", ErrInvalidRequest)
	}
	switch d.Verification {
	case "":
		if d.Completion != 0 || d.DuplicateEffects == 0 {
			return fmt.Errorf("%w: an effects receipt needs confirmed effects and no completion", ErrInvalidRequest)
		}
	case "accepted", "satisfied", "rejected", "unknown":
		if d.Completion == 0 {
			return fmt.Errorf("%w: verification needs a completion sequence", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unknown quality verification", ErrInvalidRequest)
	}
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	var events []Event
	var kept uint64
	for e, err := range a.ledger.Read(ctx, id, 0) {
		if err != nil {
			return fmt.Errorf("fw: read quality history: %w", err)
		}
		events = append(events, e)
		if e.Type == EvRunQueued {
			var q RunQueuedData
			_ = e.Decode(&q)
			kept = q.Kept
		}
	}
	found := d.Completion == 0
	start, stop := uint64(0), uint64(0)
	for _, e := range events {
		if e.Seq <= kept {
			continue
		}
		if e.Type == EvRunAnnotated {
			var old QualityAnnotatedData
			if e.Decode(&old) == nil && old.Quality != nil {
				if old.Quality.ID == d.ID {
					if *old.Quality == d {
						return nil
					}
					return fmt.Errorf("%w: conflicting quality receipt", ErrInvalidRequest)
				}
				if d.Completion != 0 && old.Quality.Completion == d.Completion {
					return fmt.Errorf("%w: completion already verified", ErrInvalidRequest)
				}
			}
		}
		if e.Type == EvRunFinished {
			if e.Seq < d.Completion {
				start = e.Seq
			}
			if e.Seq == d.Completion {
				var f RunFinishedData
				_ = e.Decode(&f)
				found = f.Outcome == OutcomeCompleted
			}
		}
		// Notes after a finish belong to that episode until work resumes.
		if d.Completion != 0 && e.Seq > d.Completion && !LeavesFinished(e.Type) && stop == 0 {
			stop = e.Seq
		}
	}
	if !found {
		return fmt.Errorf("%w: quality completion is not an own completed finish", ErrInvalidRequest)
	}
	out := QualityAnnotatedData{Quality: &d, CostComplete: true}
	if d.Completion != 0 {
		begun := start == 0
		for _, e := range events {
			if e.Seq <= kept || e.Seq <= start || (stop != 0 && e.Seq >= stop) {
				continue
			}
			// Post-finish annotations before work reopens belong to the
			// preceding episode, not both that episode and this one.
			if !begun {
				if LeavesFinished(e.Type) {
					continue
				}
				begun = true
			}
			var u struct {
				Usage    *Usage `json:"usage"`
				Provider string `json:"provider"`
				Model    string `json:"model"`
			}
			switch e.Type {
			case EvModelResponded, EvRunAnnotated, EvContextCompacted:
			default:
				continue
			}
			if e.Decode(&u) != nil || u.Usage == nil {
				if e.Type == EvModelResponded {
					out.CostComplete = false
				}
				continue
			}
			p, ok := Price{}, false
			if a.prices != nil {
				p, ok = a.prices.Price(u.Provider + "/" + u.Model)
				if !ok {
					p, ok = a.prices.Price(u.Model)
				}
			}
			if ok {
				out.CostUSD += Cost(*u.Usage, p)
			} else {
				out.UnpricedTokens += u.Usage.InputTokens + u.Usage.OutputTokens + u.Usage.CacheReadTokens + u.Usage.CacheWriteTokens
			}
		}
	}
	if out.UnpricedTokens != 0 {
		out.CostComplete = false
	}
	_, err = a.emitLocked(ctx, rs, EvRunAnnotated, out)
	return err
}
