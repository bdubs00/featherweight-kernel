package fw

// This file is the tool limits middleware (docs/ARCHITECTURE.md section
// 7.7). It bounds the input, the output, and the time of every tool call, so
// a large data source cannot flood the context window and a stuck tool
// cannot hold a run.

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// cutNote is the text that marks a cut in a tool result.
const cutNote = "\n\n[... %d bytes cut here; the result is limited to %d bytes. Ask for a smaller part, for example with a line range, a narrower command, or a filter ...]\n\n"

// keptNote marks a cut in a tool result whose full text the kernel kept in
// a file of the workspace of the run.
const keptNote = "\n\n[... %d bytes cut here; the result is limited to %d bytes. The full result of %d bytes is in %s in the directory of this run. Read the rest with the files tool and a view_range ...]\n\n"

// KeptResultsDir is the directory, under the directory of a run, where the
// output limit keeps the full text of each result that it cuts, in a file
// named after the call: <call id>.txt.
const KeptResultsDir = ".fw-tool"

// LimitTools returns middleware that applies limits to every call of the
// wrapped tool. The limit of a tool comes from l.For with the tool name.
//
// An input larger than the input limit is refused with an error result that
// the model reads. A text result larger than the output limit keeps its
// head and its tail, with a note at the cut that says how many bytes were
// cut. In a run, the full text goes to KeptResultsDir/<call id>.txt in the
// workspace of the run, with every secret replaced as in the ledger, and
// the note names the file, so the model reads the rest with a tool. An image or a document in a result counts with its size and is
// dropped with a note when it does not fit. A call that outlives the
// timeout gets an error result, and the tool sees its context end. The
// output limit does not cut the result of a step of a check run, which
// bounds the result itself (check.go).
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
	if n := t.limit.MaxOutputBytes; n > 0 && !wholeOutput(ctx) {
		res = boundResult(res, n, keepFull(ctx))
	}
	return res, nil
}

var errToolTimeout = fmt.Errorf("fw: tool call timed out")

// wholeOutputKey marks the call of a caller that bounds the result itself,
// such as a step of a check run (check.go), which no model reads: the
// output limit does not cut the result of such a call.
type wholeOutputKey struct{}

// withWholeOutput returns ctx with the mark of wholeOutputKey.
func withWholeOutput(ctx context.Context) context.Context {
	return context.WithValue(ctx, wholeOutputKey{}, true)
}

// wholeOutput reports whether ctx holds the mark of wholeOutputKey.
func wholeOutput(ctx context.Context) bool {
	v, _ := ctx.Value(wholeOutputKey{}).(bool)
	return v
}

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
		res = boundResult(res, n, keepFull(ctx))
	}
	return res, nil
}

// keepFull returns the function that keeps the full text of a cut result
// in a file of the workspace of the call and returns its name, or "" when
// it kept nothing. It is nil outside a run and for a call without a
// workspace of its own, such as the continuation of a resumed call.
func keepFull(ctx context.Context) func(text string) string {
	info := RunInfoFrom(ctx)
	id := fileSafe(info.CallID)
	f, _ := ctx.Value(filesKey{}).(Files)
	if info.RunID == "" || id == "" || f == nil {
		return nil
	}
	return func(text string) string {
		a := AgentFrom(ctx)
		if a != nil {
			text = a.Redact(text)
		}
		name := path.Join(KeptResultsDir, fileSafe(info.RunID)+"-"+id+".txt")
		if err := f.WriteFile(name, []byte(text)); err != nil {
			if a != nil {
				a.Logger().Warn("fw: the full result of a cut tool call is not kept", "run", info.RunID, "call", info.CallID, "error", err)
			}
			return ""
		}
		return name
	}
}

// fileSafe returns s with every character that is not a letter, a digit,
// a dash, or an underscore replaced by an underscore, or "" for an empty
// or a long s.
func fileSafe(s string) string {
	if s == "" || len(s) > 128 {
		return ""
	}
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			b[i] = '_'
		}
	}
	return string(b)
}

// truncateResult bounds a result to n bytes of content. Text blocks are cut
// as one text, so the head and the tail of the whole result stay. Other
// blocks count with their size and are dropped when they do not fit.
func truncateResult(res ToolResult, n int) ToolResult { return boundResult(res, n, nil) }

// boundResult is truncateResult with keep, which keeps the full text of a
// cut result and names the place for the note at the cut. A nil keep, or
// one that returns "", gives the note of truncateResult.
func boundResult(res ToolResult, n int, keep func(text string) string) ToolResult {
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
		name := ""
		if keep != nil {
			name = keep(text)
		}
		if name == "" {
			text = CutMiddle(text, rest, n)
		} else {
			size := len(text)
			text = cutWith(text, rest, func(cut int) string { return fmt.Sprintf(keptNote, cut, n, size, name) })
		}
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
	return cutWith(s, n, func(cut int) string { return fmt.Sprintf(cutNote, cut, limit) })
}

// cutWith is CutMiddle with the note that note returns for the number of
// bytes cut.
func cutWith(s string, n int, note func(cut int) string) string {
	if len(s) <= n {
		return s
	}
	room := n - len(note(0)) - 8 // digits of the byte count
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
	return s[:head] + note(cut) + s[start:]
}

// WithToolTimeout returns a context that ends after d, and a cancel
// function. It exists for tools that run a step of their own with a limit.
func WithToolTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
