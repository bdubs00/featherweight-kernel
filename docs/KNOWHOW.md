# Know-how: what an agent learns on the job

This document is the contract for Know-how between the kernel, the
workbench, the page (bossmode), and the lab. It names one word for one
thing, and it gives the data shapes, the events, the routes, and the words
on the screen. Each part builds against it in its own worktree.
`docs/ROUTINES.md` stays the contract for routines. This document extends
it and does not repeat it.

The research behind it is the report "What makes agent memory good"
(27 October 2026). Its finding: retrieval is not what makes memory good.
What makes memory good is this: when the agent acts on a remembered
item, the item is correct. Such an item comes from verified results, it matches its
source now, it is small, and it is cheap to undo.

## 1. The story

Executives read this paragraph. The page and the docs say nothing that
contradicts it.

> Your agents learn on the job, the way a good new hire does. Work that
> they repeat becomes a routine, which they rehearse beside the real work
> before they use it. When something unusual comes up, they hand the work
> back to the agent, which thinks it through. Their notes say where each
> fact came from, and a note whose source changed is marked out of date.
> You can read what each agent learned, undo any of it, and switch any
> agent to ask you first.

Four numbers back the story. The page shows them for each agent.

| Number | Meaning | Source |
|---|---|---|
| Cost per job | The mean cost of the runs of the agent, and its trend | `fw.run.finished` |
| Done by routines | The share of runs that a routine handled with no model call | `fw.routine.applied` with `ok` |
| Notes corrected | The notes that went out of date and were replaced or removed | the journal |
| With and without what it learned | Success and cost of normal runs beside the held-back runs | section 8.3 |

Know-how promises three things. The design must keep each one.

1. Work that repeats gets cheaper and more consistent.
2. The agent never trusts a note over the system that the note came from.
3. Everything that the agent learns is visible and can be undone.

Know-how does not learn from the opinion of a model about its own work. It
learns only from results that a system records. These are a receipt of
`accepted` or `satisfied`, an outcome grader that passed, and a person's
thumbs up. A definition of done that passed counts too: `done: checked`
on the newest `fw.run.finished` (docs/ARCHITECTURE.md section 24).

## 2. One word for one thing

| Thing | The word | Never |
|---|---|---|
| Everything in this document, as a feature name | Know-how | memory, learning, experience, brain |
| A short text that an agent keeps between runs | note | memory, fact, entry |
| A note whose sources match the systems now | checked | verified, fresh, valid |
| A note whose source changed after the note was written | out of date | stale, invalid, dirty |
| A note that no run used for a long time, left out of the agent's start | resting | stale, cold, archived |
| A note that was set aside, which an undo can bring back | put away | deleted, archived, purged |
| The place where a note came from: a page, a run, an item of a source | source | pointer, citation, provenance, link |
| The list of what an agent learned, changed, and put away | journal ("What it learned") | changelog, log, diary, audit trail |
| Taking back one entry of the journal | undo | revert, rollback |
| The switch that holds every change for a person | Ask me before it keeps what it learns (`askFirst`) | approval mode, review mode, human in the loop |
| A change that waits for a person under that switch | waiting | pending, staged, queued |
| The scheduled run that reads finished work and writes notes | after-work review | librarian, consolidation, reflection, dreaming |
| A run that gets none of what the agent learned, for comparison | held back | holdout, control group, ablation |
| A live routine that went back to rehearsal by itself | stepped back | demoted, rolled back |

These words never appear on a screen or in a document for an operator:
memory, ledger, cache, compile, guard, provenance, pointer, taint,
librarian, holdout, ablation, manifest, CAS, hash. The words of
`docs/ROUTINES.md` section 1 still hold.

## 3. Where Know-how lives

The ledger is the only record. Every other part of Know-how is a view of
the ledger or of a source, and the system can check, rebuild, or undo it.

| Layer | What it holds | Who writes | Where |
|---|---|---|---|
| The record | Every event of every run | the kernel | the ledger of the Task, copied to the archive of the front |
| Notes | Short texts with their sources | the agent, a person, the after-work review | the front, with a copy in the Task |
| Routines | Work compiled from repeated runs | the front, from the archive | the front (`docs/ROUTINES.md`) |
| Pages | Shared knowledge of the fleet | people, and agents in their own folder | the front |
| The journal | Every change to notes and routines | the front | the front |

The kernel holds what one run needs: the notes of its scope, the block at
the start of the run, the events, and the routine runner. The workbench
holds what outlives a Task: the store of notes, the journal, the switch,
the after-work review, the proposals of routines, and the numbers.

## 4. The kernel

### 4.1 Data, not instructions: `RunRequest.Context`

A caller can now pass data blocks with a run.

```go
// RunContext is one block of data that the caller of a run passes. The
// kernel puts it at the start of the first user message, as a <context>
// block, beside the blocks of the tools. It is never an instruction.
type RunContext struct {
	Source string `json:"source"` // [a-z][a-z0-9-]{0,31}
	Text   string `json:"text"`
}

// in RunRequest:
Context []RunContext `json:"context,omitempty"`
```

- Bounds: at most 8 blocks, at most 16 KiB of text each. A request past a
  bound fails with a 400 that names the bound.
- The kernel wraps each block with `ContextBlock(source, text)` and puts
  it after the blocks of the tools. If any block exists, the kernel adds
  `ContextNote` to the system prompt.
- `fw.run.started` records the blocks in its field `context`, as it records
  the blocks of the tools. A resume, a replay, and a fork keep them.
- The native API (`POST /v1/runs`), the bus job, and `send` with a new run
  accept the field. `WithRunContext(source, text)` is the Go option. The
  kernel has no `send` of its own that starts a run. A `send` of the
  workbench starts a run through the native API or the bus, so it takes
  the same field.
- A check run calls no model, so a request with a check and blocks fails
  with a 400.
- A hosted run gets the blocks after the index of the notes, at the start
  of its first prompt.
- `ContextNote` now says that tools and the caller of the run add the
  blocks.

### 4.2 Notes kept at the front

The configuration gains the front of the fleet:

```yaml
tools:
  memory:
    scope: principal
    front:
      url: http://fw-front:8080      # the front of the fleet
      tokenEnv: FW_API_TOKEN         # the name of the variable that holds the static token
```

With `front` set, the front holds the notes, and the files in
`$FW_HOME/memory` are a copy. Without it, nothing changes. The front keeps
the scopes `agent` and `principal`. The scope `shared` of a process stays
in the files of the process, because the front has no such scope.

- Pull: when a run starts, before the tools brief, the store asks the front
  for the list of the notes of the scope of the run (section 5.3). It
  fetches each note whose hash differs from the copy. It removes each copy
  that the front lists as put away or waiting. It also removes each copy
  that the front listed before and no longer lists. Some copies were never
  on the list, such as notes from before the front was set. Each of them
  goes to the front as a new note with `by: agent`. So the first pull of a
  Task never removes notes that the front never had. If the front does not answer
  within 5 s, the run uses the copy, and the block says so in one line.
- Push: every change through `memory.Store` sends the new text to the
  front. This includes the memory tool, the native API, and the hosted
  `fw-memory` server. The push carries the hash that the copy had as
  `ifHash`, or `absent` for a new note. The push happens inside the lock
  of the scope, after the local write.
- Conflict: if the front answers 412, the store puts back the text of the
  front in the copy and returns an error to the caller: "This note changed
  in another run. View it again before you change it."
- No answer: if the front does not answer within 10 s, or refuses the
  change with another status, the store undoes the local write. It then
  returns an error that says the note was not kept. The native API answers
  503.
- What the kernel sends with a push: the text, `by`, the run id, and the
  sources. `by` is `agent`, or `person:<subject>` for the native API. The
  sources come from `MemoryRecord.Links`. A model cannot set `by` or the
  run. A write without links sends `"sources": []`. The front must not
  keep the sources of an older text for the new one, because the agent did
  not check them.
- A removal sends `DELETE` with the hash of the copy. A rename sends the
  note under its new name with `absent`, and then removes the old name.
  With a front, the memory tool deletes and renames one note at a time. It
  refuses a directory, because each note needs a change of its own.
- `Remember` keeps `Links` (as sources) and `Metadata` beside the text, in
  a hidden file of the scope. `Get` returns them while the text stays the
  same. A new text drops them.
- The token never enters a log line or an event. Each holder of a store
  gives it to the redactor of the agent, whatever the name of its
  variable.

### 4.3 Safe to repeat

`insert` and `str_replace` become safe to repeat. Before the write, the
tool records the call id, the hash before, and the hash after, in the
scope. A repeated call whose note already has the hash after returns the
first result. A repeated call whose note has neither hash fails, because
another write came between. The whole memory tool is then
`IdempotentCall` true, and an unattended run never waits for a person on a
note. `ToolSpec.Idempotent` of the tool stays false, so a policy rule
`idempotent` does not match a tool that writes notes.

### 4.4 The block at the start of a run

The index of the memory tool keeps its place, a `<context source="memory">`
block in the first user message, and its bound of about 4 KB. If the front
is set, its order and its labels change:

1. Notes that a person wrote, labeled `(from a person)`.
2. Checked notes, labeled `(checked <date>)`.
3. Notes without sources.

Inside each group, the notes that runs used most come first, then the
newest. Notes that are out of date, resting, put away, or waiting stay out
of the block. One line counts them: "3 notes are out of date. View
/memories to read them." The first line shows how full the block is:
"Notes: 23 kept, 3.1 of 4 KB shown." Hermes Agent shows the same, and an
agent that sees the limit keeps its notes short.

What the kernel reads for this block:

- The group and the label come from the list of the front: `by`, `state`,
  `uses`, and `at`. The date of `(checked <date>)` is `at`.
- Group 3 holds every other note that the copy keeps, such as an
  `unchecked` note with a source of a run. A note whose copy has another
  hash than the list gets no label.
- The line that counts the notes left out counts the out-of-date and the
  resting notes, because only those stay in `/memories`. The pull keeps no
  copy of a note that is put away or waiting.
- When the front does not answer, the second line says so: "The front of
  the fleet did not answer. This list comes from the copy in this Task and
  can miss recent changes." The labels then come from the last answer.
- The memory section of the system prompt names this order in place of
  "newest first".

### 4.5 The event `fw.memory.used`

```json
{"how": "brief", "refs": [{"ref": "mem://principal/triage/x/gotchas.md", "hash": "…"}]}
```

- `how` is `brief` (the block of the start), `view` (the memory tool), or
  `recall` (a hit of recall that the run read with `Get`).
- The event never holds text or the principal. The kernel records one event
  per call of a tool and one at the start.
- The workbench adds `fw.memory.used` to `fleet.EgressTypes`. The front
  counts the uses of each note from it.
- A view of a directory reads no note and records nothing. A search of
  recall records nothing either: only a read with a ref counts.
- Without a front, `brief` names its notes without a hash.
- A hosted run records `brief` for its first prompt and `recall` for a
  read of `fw-memory` with a ref.

### 4.6 Held back

The metadata key `fw.knowhow` with the value `off` makes a run held back.
The kernel then does three things:

- It adds no memory block.
- It takes the tools `memory` and `recall` out of the run.
- It skips the routine runner.

`fw.run.started` records `knowhow: "off"`. Every other run is a normal run.
A held-back run of Claude Code or opencode gets no memory tools either.

### 4.7 Recall across Tasks

With `front` set, the run search of `recall` also asks the front
(`GET /v1/knowhow/runs`, section 5.3). The front searches the archive for
the runs of the same agent and the same principal. The kernel merges the
hits with the local runs, keeps one hit per run id, and keeps the bound of
20 hits. Every hit stays `untrusted`. A run that both hold keeps the local
hit. A ref of a run that only the front holds reads the run that the last
search of the same scope found. If the front does not answer within 5 s,
the result says so in a note.

### 4.8 Routines bound to a Duty, and their risk

Two small changes to `fw.Routine` (`docs/ROUTINES.md` section 2):

- `duty: <name>`: when set, the runner considers the routine only for a run
  whose metadata names that Duty (the metadata key that the duties engine
  sets). A routine bound to a Duty needs no choice among routines.
- The runner adds `risk` to `fw.routine.decided`. The risk is `read` if
  the annotations of every tool of the steps say read-only. Otherwise it
  is `write`. The tools of the checks (`after`) count too, and a tool that
  the run lacks counts as a write.
- The metadata key is `workbench.duty` (`fw.MetaDuty`).

`routine.LeaveOneOut(runs, opts)` proposes a routine from all runs but one
and replays it on the one that it left out, for each run. It returns the
count of matched, handed, and diverged runs. It is a pure function of the
events, like `Propose`, with the same build tags.

## 5. The workbench: the store of Know-how

A new package `knowhow` in the workbench.

### 5.1 The store

Bucket `<stream>_KNOWHOW` with a history of 16 values per key. A note is
one record:

```json
{
  "agent": "triage",
  "scope": {"kind": "principal", "principal": "duty:nightly-sweep"},
  "name": "gotchas/gitea-labels.md",
  "text": "Gitea refuses a label name with a space.",
  "hash": "<sha256 of text>",
  "description": "Gitea refuses a label name with a space.",
  "by": "review",
  "run": "01K…",
  "at": "2026-10-27T21:04:00Z",
  "sources": [
    {"ref": "run:01K…"},
    {"ref": "page:agents/triage/labels.md", "version": "<sha256>"},
    {"ref": "item:gitea/issue/42", "version": "2026-10-26T10:00:00Z"}
  ],
  "state": "checked",
  "uses": 7,
  "lastUsed": "2026-10-27T20:00:00Z",
  "outOfDate": {"since": "…", "source": "page:agents/triage/labels.md"}
}
```

- `state` is one of `checked`, `unchecked`, `out-of-date`, `resting`,
  `put-away`, and `waiting`. If a note has a source that can go out of
  date, and no source went out of date, its state is `checked`. A note
  without such a source is `unchecked`.
- A note that a person wrote never changes by automation, and never rests.
- A put-away note keeps its record and its history, so undo can bring it
  back. The kernel's `delete` puts a note away.
- A scope at the front is the agent with `agent` or `principal` and the
  subject of the principal. The key holds a hash of the subject, as the
  kernel's directory does.

### 5.2 Sources and out of date

- `run:<id>` never goes out of date.
- `page:<path>` with `version`: when the page changes, every note that
  names the page with an older version goes out of date. The pages store
  calls a hook on every write (section 6.4).
- `item:<source>/<key>` with `version`: when a source or a Duty sees an
  event for the item with another version, the note goes out of date. The
  sources and duties packages call a hook (section 6.4).

If a note goes out of date, the journal names the source that changed.
The next after-work review then sees the note as a task (section 7).

### 5.3 Routes

All routes are under the front. A worker reaches them with the static
token. The kernel's run metadata (`mcp.CallRun`, and the headers that the
kernel sends) names the agent and the run, never the model. A person needs
the right to view the agent to read, and the right to edit it to change.
A person reads the notes of their own principal and the agent notes. An
operator of the agent also reads the notes of the agent's workload
principals, such as `duty:<name>`.

| Route | What |
|---|---|
| `GET /v1/knowhow/notes?agent=&scope=&principal=` | the list: name, hash, state, description, by, at, uses, sources count |
| `GET /v1/knowhow/notes/{agent}/{scope}/{name…}?principal=` | one record with text |
| `PUT /v1/knowhow/notes/{agent}/{scope}/{name…}?principal=` | `{"text", "ifHash", "sources", "by", "run"}`. 412 when `ifHash` does not match. |
| `DELETE /v1/knowhow/notes/{agent}/{scope}/{name…}?principal=&ifHash=` | put the note away |
| `GET /v1/knowhow/runs?agent=&principal=&q=&limit=` | runs of the archive of that agent and that principal: id, title, input, output, at, outcome, each cut to 2 KB |
| `GET /v1/knowhow/journal?agent=&limit=&before=` | entries, newest first |
| `POST /v1/knowhow/journal/{id}/undo` | take back one entry. 409 when a later change touched the same note or routine. |
| `GET` and `PUT /v1/knowhow/settings/{agent}` | `{"askFirst": false}` |
| `GET /v1/knowhow/waiting?agent=` | the changes that wait for a person |
| `POST /v1/knowhow/waiting/{id}` | `{"keep": true}` or `{"keep": false}`. Each change pins the hash of its target. A target that changed since gets 409. |
| `GET /v1/knowhow/summary?agent=&days=7` | the four numbers of section 1, split into normal and held-back runs |

The kernel reads and sends these shapes on the routes of the notes and the
runs (`memory/front.go`, and the fake front `memory/fronttest`):

- The list answers `{"notes": [...]}`. Each item holds `name`, `hash`,
  `state`, `description`, `by`, `at` (RFC 3339), `uses`, and `sources`.
  The kernel takes `sources` as a count or as a list.
- One note answers `{"name", "text", "hash", "state", "sources": [{"ref",
  "version"}]}`. A note that is put away answers 404, or its record with
  `state: put-away`. The kernel treats both as no note.
- A `PUT` with `ifHash: absent` for a note that the front keeps answers
  412. A note that is put away counts as absent: the new text takes its
  place, and the journal keeps the old one for undo. Otherwise an agent
  can never write a note again under a name that it deleted. A `PUT`
  answers any 2xx on success.
- A `DELETE` answers any 2xx on success, 404 for a note that the front does
  not have, which the kernel treats as done, and 412 for another hash.
- The runs answer `{"runs": [{"id", "title", "input", "output", "at",
  "outcome"}]}`. An empty `principal` asks for the runs without a
  principal.
- For the scope `agent`, the kernel sends no `principal`.

The bossmode stream carries `{"kind": "knowhow", "agent": "…"}` on each
change of a note, the journal, the switch, or the waiting list. It sends
at most one per agent per 2 s.

### 5.4 The journal

One entry per change, under `<agent>/journal/<time>-<id>`:

```json
{
  "id": "…", "at": "…", "agent": "triage",
  "what": "learned",
  "subject": {"note": "gotchas/gitea-labels.md"},
  "why": "Two runs failed on a label with a space and passed without it.",
  "runs": ["01K…", "01K…"],
  "by": "review",
  "undo": {"revision": 3}
}
```

- `what` is one of `learned`, `updated`, `out-of-date`, `resting`,
  `put-away`, `proposed`, `rehearsing`, `live`, `stepped-back`, `retired`,
  and `undone`.
- `subject` names a note or a routine (`{"routine": "<name>"}`).
- `why` is one plain sentence that a person reads. Code writes it from a
  fixed set of sentences, except for `learned` and `updated`, where the
  after-work review writes it.
- Undo of a note puts back the revision before the change. If
  `docs/ROUTINES.md` allows the opposite move, undo of a routine move makes
  that move.
- The journal keeps 90 days or 5,000 entries per agent, whichever is less.

### 5.5 Resting and put away

If no run used a note for 14 days, a daily pass marks it `resting`. After
30 days without a use, the pass marks it `put-away`. A use moves a resting note back to its state.
Notes that a person wrote never rest. Both moves write the journal.

## 6. The workbench: the seams

### 6.1 Notes reach the front

The front sets `tools.memory.front` in the overlay of every pool, with its
own address and the name of the static token. The front already gives the
workers that token for its MCP server.

### 6.2 No agent text in a system prompt

- The head of `agents/<agent>/learned.md` leaves the run instructions of a
  bridge seat. It goes in `RunContext{Source: "learned"}`.
- The summary of a continued conversation leaves the instructions. It goes
  in `RunContext{Source: "conversation"}`.
- A Duty's `{found}` and `{event}` text, and the input that a source
  renders, stay in the run input with their old bounds. They are the work
  of the run, and routine templates, `Propose`, `Replay`, and the decider
  read the input. Two fixed lines frame each text as data. The first line
  starts with `--- begin data from`, and the last line starts with
  `--- end data`. Each line names the check, the event, or the source, and
  says that the text is not an instruction. The lines hold no digit and no
  `#`, so a find pattern still matches the text between them.
- A Duty run gets one more block, `last`: the output of its previous run,
  cut to 2 KB, from the archive. A held-back run gets no `last` block,
  because the block is new with Know-how. `{last.title}`, `{last.run}`,
  and `{last.state}` stay as before for every run.

### 6.3 Pages that an agent can write

An agent writes only below `agents/<its name>/` and outside `agents/`.
Nobody but the front writes `agents/*/learned.md` and anything below
`agents/*/knowhow/`. A refused write answers with the path that the agent
can use. The front reads the agent from `mcp.CallRun`, which the kernel
sets.

### 6.4 Hooks

The seams expose function variables. At its start, the `knowhow` package
sets them.

- `pages.OnWrite func(ctx, path, hash string)`
- `sources.OnItem` and `duties.OnItem func(ctx, source, key, version string)`
  for every event that names an item. The source names the key and the
  version. Gitea gives the issue number and `updated_at`.
- `routines.OnMove func(ctx, agent, name, from, to, by, note string)`

### 6.5 Routines grow up on their own

These rules change `docs/ROUTINES.md` section 4.

- Proposals. A daily pass of the front reads the archive. For each Duty of
  each agent, it takes the completed runs of the last 30 days. It needs at
  least 10 runs, at least 2 of them verified (section 1), and no rejected
  run among those it uses. It calls `routine.Propose` and then
  `routine.LeaveOneOut`. A proposal with a free hole, or with a diverged
  run, stays out, and the journal says why. A good proposal is stored
  bound to its Duty (`duty`), as `proposed`, and moves to `rehearsing` at
  once, unless `askFirst` is on.
- Agreement. A rehearsal compares what the routine would do with what the
  agent did on the same run: its questions, and its calls with their
  results (`docs/ROUTINES.md` section 3.3, item 6). So a routine without
  questions, the only kind that Know-how proposes without a decider,
  gains agreement and can become ready.
- Step back. One disagreement in a canary run, or a run that the routine
  applied and that came back with a `rejected` receipt, moves a live
  routine to `rehearsing`. The journal says `stepped-back` with the run.
- Ready by risk. A rehearsing routine is ready under three conditions:
  - `acted` reaches the bound of its risk: `read` 20 and `write` 50, the
    defaults of `workbench.routines.readyAfter`.
  - Agreement is at least 0.95.
  - `sentBack.agreed` is 0.

  A ready `read` routine goes live by itself, unless `askFirst` is on. A
  ready `write` routine waits for one sign-off of a person (`needs you`).
  The sign-off holds for that version. If a signed-off version stepped
  back and is ready again, it goes live again by itself.
- Every move writes the journal through `routines.OnMove`.

### 6.6 Held back

The duties engine marks a share of the firings of each Duty as held back,
with `fw.knowhow: off`. The share is `workbench.knowhow.heldBack`, default
0.05. The choice is a stable hash of the run id. The value 0 turns it off.
The summary compares held-back runs with normal runs of the same Duties.

## 7. The after-work review

The review is a run of a profile that the operator names. It runs on a
schedule and reads finished work.

```yaml
workbench:
  knowhow:
    review:
      agent: review        # the profile; it names its own model and budget
      every: 24h
```

Without `review.agent`, no review runs, and the page says "The after-work
review is off: no agent is set." Nothing picks a model or a price by
default. The kernel refuses a run with no model, as it does now.

### 7.1 Candidates

A pass of the front reads the archive behind a durable cursor and finds
candidates by code, without a model:

| Kind | What the code finds |
|---|---|
| `gotcha` | A tool call that failed, and a later call of the same tool in the same run that passed with a different input |
| `place` | The same read call (tool and input) in two or more runs of an agent, with the same result |
| `outcome` | A run with a receipt, an outcome grader verdict, a definition of done that passed, or a thumbs up or down |
| `recheck` | A note that went out of date |

Each candidate holds its runs, its sources, and its origin. If its text
came from any of these, its origin is `untrusted`:

- a `found`, `event`, or `source` block
- a tool that can read the open world (its annotation)
- an MCP tool of another service
- a message of another principal

Candidates wait 14 days, then drop.

### 7.2 Tools of the review

The front's MCP server offers two tools to the review profile only:

- `knowhow_candidates {agent, limit}` lists open candidates with their
  evidence.
- `knowhow_keep {candidate, op, name, text, why}` decides one candidate.
  `op` is `add`, `supersede`, or `skip`. The note goes to the scope of the
  runs of the candidate. Notes of one principal never move to another
  scope.

The front checks every `knowhow_keep`:

- The text is at most 600 characters and states a fact. The front refuses
  a line that starts with one of these words, and it gives the reason:
  `always`, `never`, `do not`, `don't`, `you must`, `make sure`.
- The note gets the sources of the candidate. The model cannot add others.
- An `untrusted` candidate needs two runs with different triggers, or it
  waits for a second one.
- A `supersede` keeps the old revision for undo.
- With `askFirst` on, the change waits for a person.

### 7.3 The review's own instructions

The profile of the example configuration carries the list of what never
becomes a note:

- a failure that came from the environment and not from the task
- a claim that a tool does not work
- a passing error
- a one-off story
- a failure without its fix
- anything that a source already says

The list comes from the Hermes Agent issue history in the report.

## 8. The page

### 8.1 The place

The place `Know-how` replaces `Routines` in the app bar, at the same
position. It has three tabs, with the agent picker of the place:

- What it learned (the default): the journal as a list in plain words.
  Each entry has its time, its sentence, links to its runs, and `Undo`.
- Notes: the notes of the agent and its Duties, with `checked`,
  `out of date`, and `resting` as small labels, and the text on open.
  People can edit and put away their own notes.
- Routines: the place as it is today, moved inside.

### 8.2 The header

The header of the place shows the four numbers of section 1 for the last
7 days. It also holds the switch "Ask me before it keeps what it learns".
With the switch on, waiting changes show as decision cards: "Keep this?"
with `Keep` and `Not now`, as the sign-off card does.

### 8.3 With and without what it learned

One line under the numbers compares the two kinds of runs. If fewer than
20 runs were held back, the line says that there are not enough yet. The
page never shows a percentage from fewer runs. Two examples:

> Held back 1 in 20 runs. Done well: 82% without what it learned, 91% with it.

> Not enough held-back runs yet.

## 9. The lab

- `TestKnowhow` drives a fleet: a note survives the replacement of a Task,
  a context block arrives as data, a held-back run has no memory tools, and
  the review refuses to start without an agent.
- A walk on Haiku with a ticket Duty. It shows a proposal, a rehearsal, a
  routine that steps back, a note that goes out of date after a change to
  its issue, and an undo. The person runs the walk.

## 10. Bounds and what stays out

- No embeddings. Search stays lexical.
- No erasure from the ledger or the bus. Put away is not erasure, and the
  page never calls it deletion.
- No note moves from one person to another. Agent notes come only from a
  person with the right to edit the agent.
- No model fills a free hole of a routine.
- Context editing inside a run is a separate change.
- Hosted runs (Claude Code and opencode) keep notes at the front through
  the store. They still refuse routines.

## 11. Working rules

- Worktrees live under the scratchpad of the session, on the branches
  `knowhow/kernel`, `knowhow/workbench`, `knowhow/store`, `knowhow/page`,
  and `knowhow/lab`. Commit there. Never push, never deploy, and never
  touch the main checkouts or the worktrees of other sessions.
- Another session works on `flow/*` branches: definitions of done,
  receipts of a system of record, and the archive backfill. Do not change
  the files that those branches change unless this contract needs it, and
  then keep the change small.
- No token, key, or secret enters a file, a ledger event, a log line, or a
  report. Reports say `[token]`.
- No paid default.
- Documents and comments follow the plain style of the repositories.
- Each part ends with a report: what landed, what the tests prove, what was
  left out and why, and what the person must decide.
