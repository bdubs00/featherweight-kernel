package fw

// This file holds the parts of Know-how that the kernel owns
// (docs/KNOWHOW.md section 4): the data blocks of the caller of a run, the
// runs that are held back from what the agent learned, and the record of
// the notes that a run read.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// runContextSource is the pattern of RunContext.Source.
var runContextSource = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// CheckRunContext checks the data blocks of a request: at most
// MaxRunContexts blocks, each with a source that matches
// [a-z][a-z0-9-]{0,31} and at most MaxRunContextBytes of text. The error
// wraps ErrInvalidRequest and names the bound, so the native API answers
// 400 with it.
func CheckRunContext(blocks []RunContext) error {
	if len(blocks) > MaxRunContexts {
		return fmt.Errorf("%w: the field context holds %d blocks; a run takes at most %d", ErrInvalidRequest, len(blocks), MaxRunContexts)
	}
	for i, b := range blocks {
		if !runContextSource.MatchString(b.Source) {
			return fmt.Errorf("%w: context[%d].source %q must match [a-z][a-z0-9-]{0,31}", ErrInvalidRequest, i, b.Source)
		}
		if len(b.Text) > MaxRunContextBytes {
			return fmt.Errorf("%w: context[%d].text holds %d bytes; a block holds at most %d bytes (16 KiB)", ErrInvalidRequest, i, len(b.Text), MaxRunContextBytes)
		}
	}
	return nil
}

// RunContextText wraps each data block of the caller with ContextBlock, in
// the order of the request. A block without text adds nothing. The kernel
// puts the result after the blocks of the tools, and a host of a harness
// puts it at the start of the first prompt of a session.
func RunContextText(blocks []RunContext) string {
	var parts []string
	for _, b := range blocks {
		if text := strings.TrimSpace(b.Text); text != "" {
			parts = append(parts, ContextBlock(b.Source, text))
		}
	}
	return strings.Join(parts, "\n\n")
}

// HeldBack reports whether the metadata of a run holds the run back from
// Know-how: MetaKnowhow is KnowhowOff. Such a run gets no memory block, no
// memory or recall tool, and no routine (docs/KNOWHOW.md section 4.6).
func HeldBack(meta map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(meta[MetaKnowhow]), KnowhowOff)
}

// KnowhowTool reports whether a tool is one that a held-back run does not
// get: the memory tool and the recall tool.
func KnowhowTool(name string) bool { return name == "memory" || name == "recall" }

// knowhowOf returns the value of RunStartedData.Knowhow for a run:
// KnowhowOff for a held-back run, and "" for every other run.
func knowhowOf(q RunQueuedData) string {
	if HeldBack(q.Metadata) {
		return KnowhowOff
	}
	return ""
}

type memoryUseKey struct{}

// WithMemoryUseRecorder stores the recorder of the notes that a run reads
// in ctx. The kernel sets one for each tool call, which appends
// fw.memory.used to the ledger of the run, and one while a tool writes its
// data for the start of the run. A host of a harness sets one too.
func WithMemoryUseRecorder(ctx context.Context, fn func(MemoryUsedData)) context.Context {
	return context.WithValue(ctx, memoryUseKey{}, fn)
}

// RecordMemoryUse records the notes that a run read, as fw.memory.used.
// Outside a tool call and outside the start of a run it does nothing. Data
// without refs records nothing.
func RecordMemoryUse(ctx context.Context, d MemoryUsedData) {
	if len(d.Refs) == 0 {
		return
	}
	if fn, ok := ctx.Value(memoryUseKey{}).(func(MemoryUsedData)); ok && fn != nil {
		fn(d)
	}
}

// recordBriefUse records fw.memory.used for each list of notes that a tool
// put in the data of the start of the run (briefContext), after
// fw.run.started. A failed append leaves the run as it is: the event
// counts uses, and the run does not depend on it.
func (a *Agent) recordBriefUse(ctx context.Context, rs *runState) {
	for _, d := range rs.briefUse {
		if _, err := a.emit(ctx, rs, EvMemoryUsed, d); err != nil {
			a.log.Warn("record the notes of the start of the run", "run", rs.id, "error", err)
			break
		}
	}
	rs.briefUse = nil
}
