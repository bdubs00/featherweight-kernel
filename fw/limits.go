package fw

// This file is the tool limits middleware (docs/ARCHITECTURE.md section
// 7.7). It bounds the input, the output, and the time of every tool call, so
// a large data source cannot flood the context window and a stuck tool
// cannot hold a run.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// cutNote is the text that marks a cut in a tool result.
const cutNote = "\n\n[... %d bytes cut here; the result is limited to %d bytes. Ask for a smaller part, for example with a line range, a narrower command, or a filter ...]\n\n"

// LimitTools returns middleware that applies limits to every call of the
// wrapped tool. The limit of a tool comes from l.For with the tool name.
//
// An input larger than the input limit is refused with an error result that
// the model reads. A text result larger than the output limit keeps its
// head and its tail, with a note at the cut that says how many bytes were
// cut. An image or a document in a result counts with its size and is
// dropped with a note when it does not fit. A call that outlives the
// timeout gets an error result, and the tool sees its context end.
//
// A tool that can continue a resumed call (see Continuer) keeps the same
// limits in its continuation. The middleware forwards the Continuer of the
// tool behind it only when that tool is one, so an ordinary limited tool
// never looks like a Continuer and never bypasses the rerun gate.
func LimitTools(l ToolLimits) ToolMiddleware {
	return func(t Tool) Tool {
		spec := t.Spec()
		lim := l.For(spec.Name)
		if lim == (Limit{}) {
			return t
		}
		lt := &limitedTool{Tool: t, limit: lim}
		if _, ok := continuerOf(t); ok {
			return &limitedContinuer{limitedTool: lt}
		}
		return lt
	}
}

// limitedTool is one tool behind LimitTools.
type limitedTool struct {
	Tool
	limit Limit
}

// Spec implements Tool.
func (t *limitedTool) Spec() ToolSpec { return t.Tool.Spec() }

// Call implements Tool.
func (t *limitedTool) Call(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	name := t.Tool.Spec().Name
	if n := t.limit.MaxInputBytes; n > 0 && len(input) > n {
		return ErrorResult(fmt.Sprintf("%s: the input is %d bytes, and the limit is %d bytes. Send less data per call, for example by writing a file in parts.", name, len(input), n)), nil
	}
	if d := t.limit.Timeout; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, d, errToolTimeout)
		defer cancel()
	}
	res, err := t.Tool.Call(ctx, input)
	if err != nil && context.Cause(ctx) == errToolTimeout {
		return ErrorResult(fmt.Sprintf("%s: the call did not finish within %s and was stopped. Try a smaller step.", name, t.limit.Timeout)), nil
	}
	if err != nil {
		return res, err
	}
	if n := t.limit.MaxOutputBytes; n > 0 {
		res = truncateResult(res, n)
	}
	return res, nil
}

var errToolTimeout = fmt.Errorf("fw: tool call timed out")

// Unwrap returns the wrapped tool, so a caller can find the tool behind the
// limits with a type assertion on the result.
func (t *limitedTool) Unwrap() Tool { return t.Tool }

// Close closes the wrapped tool when it can be closed.
func (t *limitedTool) Close() error {
	if c, ok := t.Tool.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// limitedContinuer is a limited tool whose tool can continue a resumed
// call. Its Continue holds the same bounds as its Call, so a continuation
// cannot flood the context window or outlive the timeout. LimitTools
// builds it only for a tool that is a Continuer behind the middleware, so
// an ordinary limited tool keeps the rerun-after-resume gate.
//
// Any middleware that hides a Continuer and has an Unwrap method needs the
// same forwarding if the limits of a continuation are to hold behind it:
// find the Continuer with continuerOf at wrap time and wrap only then.
type limitedContinuer struct {
	*limitedTool
}

// Continue implements Continuer.
func (t *limitedContinuer) Continue(ctx context.Context, input json.RawMessage, answered []AnsweredQuestion) (ToolResult, error) {
	name := t.Tool.Spec().Name
	c, ok := continuerOf(t.Tool)
	if !ok {
		return ToolResult{}, fmt.Errorf("fw: tool %s cannot continue from the recorded answers", name)
	}
	if n := t.limit.MaxInputBytes; n > 0 && len(input) > n {
		return ErrorResult(fmt.Sprintf("%s: the input is %d bytes, and the limit is %d bytes. Send less data per call, for example by writing a file in parts.", name, len(input), n)), nil
	}
	if d := t.limit.Timeout; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, d, errToolTimeout)
		defer cancel()
	}
	res, err := c.Continue(ctx, input, answered)
	if err != nil && context.Cause(ctx) == errToolTimeout {
		return ErrorResult(fmt.Sprintf("%s: the call did not finish within %s and was stopped. Try a smaller step.", name, t.limit.Timeout)), nil
	}
	if err != nil {
		return res, err
	}
	if n := t.limit.MaxOutputBytes; n > 0 {
		res = truncateResult(res, n)
	}
	return res, nil
}

// truncateResult bounds a result to n bytes of content. Text blocks are cut
// as one text, so the head and the tail of the whole result stay. Other
// blocks count with their size and are dropped when they do not fit.
func truncateResult(res ToolResult, n int) ToolResult {
	total := 0
	for _, b := range res.Content {
		total += blockSize(b)
	}
	if len(res.Structured) > n {
		res.Structured = nil
	}
	if total <= n {
		return res
	}
	var (
		out   []Block
		texts strings.Builder
		rest  = n
		notes []string
	)
	for _, b := range res.Content {
		if b.Type == BlockText {
			if texts.Len() > 0 {
				texts.WriteByte('\n')
			}
			texts.WriteString(b.Text)
			continue
		}
		size := blockSize(b)
		if size <= rest {
			out = append(out, b)
			rest -= size
			continue
		}
		notes = append(notes, fmt.Sprintf("[a %s of %d bytes was dropped, because the result is limited to %d bytes]", b.Type, size, n))
	}
	text := texts.String()
	if len(notes) > 0 {
		if text != "" {
			text += "\n"
		}
		text += strings.Join(notes, "\n")
	}
	if len(text) > rest {
		text = CutMiddle(text, rest, n)
	}
	if text != "" {
		out = append(out, Text(text))
	}
	res.Content = out
	return res
}

// blockSize is the size of a block in bytes, as it reaches the model.
func blockSize(b Block) int {
	switch b.Type {
	case BlockText:
		return len(b.Text)
	case BlockImage, BlockDocument:
		return len(b.Data) + len(b.URL)
	}
	return jsonSize(b)
}

// CutMiddle shortens s to about n bytes by removing the middle. The head
// keeps two thirds of the room and the tail keeps one third, both on rune
// boundaries, with a note between them that names the cut and the limit.
func CutMiddle(s string, n, limit int) string {
	if len(s) <= n {
		return s
	}
	note := fmt.Sprintf(cutNote, 0, limit)
	room := n - len(note) - 8 // digits of the byte count
	if room < 64 {
		room = min(64, len(s))
	}
	head := room * 2 / 3
	tail := room - head
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	cut := start - head
	if cut <= 0 {
		return s
	}
	return s[:head] + fmt.Sprintf(cutNote, cut, limit) + s[start:]
}

// WithToolTimeout returns a context that ends after d, and a cancel
// function. It exists for tools that run a step of their own with a limit.
func WithToolTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
