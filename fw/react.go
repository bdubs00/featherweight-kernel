package fw

// This file holds the reaction of a person on an event of a run
// (docs/ARCHITECTURE.md sections 4.2 and 8.2): a thumb up, a thumb down,
// or a heart. A thumb on the end of a completed run is also a quality
// receipt (section 10.1).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EvRunReacted is the event of React.
const EvRunReacted = "fw.run.reacted"

// Reactions of a person (ReactionData.Reaction).
const (
	ReactionUp    = "up"
	ReactionDown  = "down"
	ReactionHeart = "heart"
)

// MaxReactionNoteBytes bounds ReactionData.Note.
const MaxReactionNoteBytes = 1024

// maxReactionByBytes bounds ReactionData.By.
const maxReactionByBytes = 256

// ReactionData is the payload of fw.run.reacted: a person reacted to an
// event of a run, or to the run as a whole. Seq is the sequence number of
// the event, and 0 means the run as a whole. Reaction is ReactionUp,
// ReactionDown, or ReactionHeart. By is the subject of the person. Note
// is free text of at most MaxReactionNoteBytes bytes. Receipt is true when
// the reaction also recorded a quality receipt (see React); a caller
// cannot set it. Removed is true when the person took the reaction back:
// the event then takes one from the count of the reaction, and it keeps
// no note. The event changes the state of a run in no way, so one that
// arrives after fw.run.finished leaves the run finished, as an
// annotation does (LeavesFinished).
type ReactionData struct {
	Seq      uint64 `json:"seq"`
	Reaction string `json:"reaction"`
	By       string `json:"by,omitempty"`
	Note     string `json:"note,omitempty"`
	Receipt  bool   `json:"receipt"`
	Removed  bool   `json:"removed,omitempty"`
}

// React records the reaction of a person on an event of a run, or on the
// run as a whole, as fw.run.reacted. With a principal in ctx, the caller
// needs the operator role on the run, as for Annotate. By is the person in
// ctx when d leaves it empty: the person for whom the caller acts
// (WithActingFor), or the subject of the principal. Only the process
// itself, the static token, or a workload names another person in By, so
// the front of a fleet records the reaction of a person on a run of a Task;
// any other principal gets ErrForbidden. A Seq past the latest event of
// the run, an unknown reaction, and a note longer than
// MaxReactionNoteBytes are ErrInvalidRequest.
//
// A reaction that the same person already put on the same event appends
// nothing, also with another note. With d.Removed, React takes the
// reaction of the person back: it appends the event with Removed when the
// person has the reaction on the event, and nothing otherwise. A quality
// receipt that the reaction recorded stays, because a receipt is final.
// A thumb, ReactionUp or ReactionDown, on the fw.run.finished of
// a completed episode of the run also records a quality receipt with
// ObserveQuality: the verification accepted or rejected of that
// completion, under the receipt id "reaction:<run>:<seq>:<by>", so a
// second thumb of the same person on the same result records nothing
// twice. When ObserveQuality refuses the receipt, because another
// receipt or a thumb of another person already verified the completion,
// the reaction still records, with Receipt false, and the log says why.
// A finished run takes a reaction and stays finished.
func (a *Agent) React(ctx context.Context, id string, d ReactionData) error {
	if err := a.k.writeError(); err != nil {
		return err
	}
	rs, err := a.k.lookup(id)
	if err != nil {
		return err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
		return err
	}
	switch d.Reaction {
	case ReactionUp, ReactionDown, ReactionHeart:
	default:
		return fmt.Errorf("%w: a reaction is %q, %q, or %q, not %q", ErrInvalidRequest, ReactionUp, ReactionDown, ReactionHeart, d.Reaction)
	}
	if len(d.Note) > MaxReactionNoteBytes {
		return fmt.Errorf("%w: the note of a reaction is longer than %d bytes", ErrInvalidRequest, MaxReactionNoteBytes)
	}
	if !utf8.ValidString(d.Note) {
		return fmt.Errorf("%w: the note of a reaction is not UTF-8", ErrInvalidRequest)
	}
	switch subject := personOf(ctx); {
	case d.By == "":
		d.By = subject
	case d.By != subject && !speaksForOthers(ctx):
		return fmt.Errorf("%w: only the static token or a workload can react for another person", ErrForbidden)
	}
	if len(d.By) > maxReactionByBytes || strings.IndexFunc(d.By, unicode.IsControl) >= 0 || !utf8.ValidString(d.By) {
		return fmt.Errorf("%w: the person of a reaction holds more than %d bytes or a control character", ErrInvalidRequest, maxReactionByBytes)
	}
	d.Receipt = false
	if d.Removed {
		d.Note = ""
	}

	a.k.reactMu.Lock()
	defer a.k.reactMu.Unlock()
	last := rs.snapshot().Seq
	if d.Seq > last {
		return fmt.Errorf("%w: run %s has no event %d; its latest event is %d", ErrInvalidRequest, id, d.Seq, last)
	}
	completion, has := false, false
	for e, err := range a.ledger.Read(ctx, id, 0) {
		if err != nil {
			return fmt.Errorf("fw: read the reactions of run %s: %w", id, err)
		}
		switch {
		case e.Type == EvRunReacted:
			var old ReactionData
			if e.Decode(&old) == nil && old.Seq == d.Seq && old.Reaction == d.Reaction && old.By == d.By {
				// The last event of the person on the event decides.
				has = !old.Removed
			}
		case d.Seq != 0 && e.Seq == d.Seq && e.Type == EvRunFinished:
			var f RunFinishedData
			completion = e.Decode(&f) == nil && f.Outcome == OutcomeCompleted
		}
	}
	if has != d.Removed {
		// The person has the reaction already, or has none to take back.
		return nil
	}
	if completion && !d.Removed && (d.Reaction == ReactionUp || d.Reaction == ReactionDown) {
		verification := "accepted"
		if d.Reaction == ReactionDown {
			verification = "rejected"
		}
		receipt := QualityObservation{ID: reactionReceipt(id, d.Seq, d.By), Completion: d.Seq, Verification: verification}
		switch err := a.ObserveQuality(ctx, id, receipt); {
		case err == nil:
			d.Receipt = true
		case errors.Is(err, ErrInvalidRequest):
			a.log.Info("fw: the reaction records no quality receipt", "run", id, "seq", d.Seq, "by", d.By, "reaction", d.Reaction, "reason", err)
		default:
			return err
		}
	}
	_, err = a.emit(nil, rs, EvRunReacted, d)
	return err
}

// reactionReceipt is the id of the quality receipt of a thumb:
// "reaction:<run>:<seq>:<by>". A subject that makes the id longer than a
// receipt id can be gives its place to the first 16 bytes of its SHA-256.
func reactionReceipt(run string, seq uint64, by string) string {
	id := fmt.Sprintf("reaction:%s:%d:%s", run, seq, by)
	if len(id) <= maxReceiptIDBytes {
		return id
	}
	sum := sha256.Sum256([]byte(by))
	return fmt.Sprintf("reaction:%s:%d:sha256:%s", run, seq, hex.EncodeToString(sum[:16]))
}
