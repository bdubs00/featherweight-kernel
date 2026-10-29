package memory

// This file holds what a memory tool tells the model about its changes
// beyond the result of the editor (docs/KNOWHOW.md section 4.2): that a
// change waits for a person, and that a page that a note names is not a
// page of the front.

import (
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// WaitingText is the answer of a memory tool to a change that the front
// holds for a person, after the path of the note.
const WaitingText = "waits for a person. The owner of this agent reads each change of a note before the agent keeps it, " +
	"so later runs do not see this change until a person keeps it. " +
	"Tell the user that the note waits for them to keep it. Do not say that it is saved."

// Answer returns what a memory tool tells the model about the changes
// that one call made, beside the result of the call. When a change waits
// for a person, replace is true and text replaces the result, because the
// result says that the note was saved. Otherwise text, when it is not
// empty, follows the result: a page source that got no version from the
// front, because the wiki has no such page or the front did not answer. front says whether a front keeps the
// notes of the scope of the call.
func Answer(changes []fw.MemoryChangedData, front bool) (text string, replace bool) {
	var waiting, missing []string
	created := false
	for _, d := range changes {
		shown := shownOf(d)
		if d.Waiting {
			waiting = append(waiting, shown)
			created = d.Op == fw.FileCreated
			continue
		}
		if !front {
			continue
		}
		for _, l := range d.Sources {
			if p, ok := strings.CutPrefix(l.Ref, SourcePage); ok && l.Version == "" {
				missing = append(missing, p)
			}
		}
	}
	if len(waiting) > 0 {
		what := "The change of " + strings.Join(waiting, " and ")
		if len(waiting) == 1 && created {
			what = "The new note " + waiting[0]
		}
		return "Not kept yet. " + what + " " + WaitingText, true
	}
	if len(missing) > 0 {
		return fmt.Sprintf("The front gave no version of the page %s: the wiki has no such page, or the front did not answer. "+
			"So the note names the page without a version, and it does not go out of date when the page changes. "+
			"Check the path in the line Source.", strings.Join(missing, " and ")), false
	}
	return "", false
}

// shownOf returns the path of the note of a change as the model sees it,
// /memories/<name>, or the ref when it names no note of its scope.
func shownOf(d fw.MemoryChangedData) string {
	name, err := noteName(fw.MemoryScope{Kind: d.Scope, Agent: d.Agent}, d.Ref)
	if err != nil {
		return d.Ref
	}
	return Prefix + "/" + name
}
