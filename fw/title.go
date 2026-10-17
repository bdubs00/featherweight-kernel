package fw

// This file is the title of a run. A run gets a title from its input at
// once (TitleFrom). With titles: model, the default, the kernel also asks a
// model for a title of a few words when a top-level run starts,
// beside the first turn, and records the answer as fw.run.titled. A title
// call that fails changes nothing: the run keeps the title of its input.
// The title of a short run can arrive after fw.run.finished. It changes
// only the title and the usage, so an observer never reads it as a run
// that opened again.

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
)

// Values of Config.Titles.
const (
	TitlesInput = "input" // the first sentence of the input, with no extra model call (the default)
	TitlesModel = "model" // a title that the summary model writes
)

// titleSystem is the system prompt of a title call.
const titleSystem = "Write a title for the task below in at most six words, in the language of the task. " +
	"Name the work, not the worker. Use sentence case, as in \"Fix checkout retries\". Answer with the title only: no quotes, no final period, no Markdown."

// Bounds of a title call.
const (
	titleTimeout   = 60 * time.Second
	titleMaxTokens = 512 // room for a model that reasons before it answers
	titleMaxInput  = 2000
)

// maybeTitle starts the title call of a run that just started, when the
// configuration asks for model titles. A child run, a run whose request
// set a title, a replay, and a run without text in its input keep the
// title of their input. The call goes to compaction.model when it is set,
// else to the model of the run.
func (a *Agent) maybeTitle(rs *runState, q RunQueuedData, model string) {
	if !strings.EqualFold(strings.TrimSpace(a.cfg.Titles), TitlesModel) || rs.depth > 0 ||
		strings.TrimSpace(q.Metadata[MetaTitle]) != "" || q.Metadata["replayOf"] != "" || strings.TrimSpace(q.Input) == "" {
		return
	}
	k := a.k
	k.lifeMu.Lock()
	if k.draining {
		k.lifeMu.Unlock()
		return
	}
	k.wg.Add(1)
	k.lifeMu.Unlock()
	go func() {
		defer k.wg.Done()
		ctx, cancel := context.WithTimeout(k.ctx, titleTimeout)
		defer cancel()
		// A shutdown never waits for a title.
		go func() {
			select {
			case <-k.drainCh:
				cancel()
			case <-ctx.Done():
			}
		}()
		if name := a.cfg.Compaction.Model; name != "" {
			model = name
		}
		d, err := a.writeTitle(ctx, model, q.Input)
		if err != nil {
			a.log.Debug("run title: the model wrote no title", "run", rs.id, "error", err)
			return
		}
		if _, err := a.emit(ctx, rs, EvRunTitled, d); err != nil {
			a.log.Debug("run title: append failed", "run", rs.id, "error", err)
		}
	}()
}

// writeTitle asks a model, by name or selector, for a title of input.
func (a *Agent) writeTitle(ctx context.Context, model, input string) (RunTitledData, error) {
	m, err := a.ModelFor(ctx, model)
	if err != nil {
		return RunTitledData{}, err
	}
	if r := []rune(input); len(r) > titleMaxInput {
		input = string(r[:titleMaxInput])
	}
	resp, err := m.Generate(ctx, &Request{
		System:    titleSystem,
		Messages:  []Message{{Role: RoleUser, Content: []Block{Text(input)}}},
		Effort:    "low",
		MaxTokens: titleMaxTokens,
	})
	if err != nil {
		return RunTitledData{}, err
	}
	d := RunTitledData{Title: CleanTitle(TextOf(resp.Content)), Provider: resp.Provider, Model: resp.Model}
	if resp.Usage != (Usage{}) {
		u := resp.Usage
		d.Usage = &u
	}
	if d.Title == "" {
		return RunTitledData{}, errNoTitle
	}
	return d, nil
}

// errNoTitle marks a title call whose answer holds no title.
var errNoTitle = errors.New("fw: the answer holds no title")

// CleanTitle turns the answer of a title call into a title: its last line
// with text, without Markdown marks, quotes, a "Title:" label, or a final
// period, folded to one line and cut to MaxTitleRunes. It returns "" when
// nothing is left.
func CleanTitle(s string) string {
	var line string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
		}
	}
	line = strings.TrimLeft(line, "#>*-+ \t")
	if i := strings.IndexByte(line, ':'); i >= 0 && strings.EqualFold(strings.TrimSpace(line[:i]), "title") {
		line = line[i+1:]
	}
	line = strings.Trim(strings.TrimSpace(line), "\"'`*_“”‘’")
	line = strings.TrimRight(line, ".。")
	line = strings.Join(strings.FieldsFunc(line, unicode.IsSpace), " ")
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, line)
	if r := []rune(line); len(r) > MaxTitleRunes {
		line = strings.TrimSpace(string(r[:MaxTitleRunes])) + "…"
	}
	return line
}
