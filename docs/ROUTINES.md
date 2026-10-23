# Routines: the muscle memory of an agent

This document is the contract between the kernel, the workbench, the page
(bossmode), and the lab for routines. It names one word for one thing, the
data shape, the events, the routes, and the words on the screen. Each part
builds against it in its own worktree. The design behind it is "memory as a
build": the ledger is the only memory of record, and a routine is a compiled
view of verified runs that the kernel can run again without a model.

## 1. One word for one thing

| Thing | The word | Never |
|---|---|---|
| A vetted sequence of tool calls with typed holes, a decision schema, and a check afterward, that the kernel runs under the policy of the run | routine | procedure, skill, playbook, macro, checkpoint |
| A hole that a decision model fills from the input | question, with its answers | slot, classifier label, feature |
| A hole that copies a value from the input or from an earlier result | a template (`{{ }}`) | data flow, binding |
| The model that answers typed questions with probabilities | decider | classifier, Clef, Jev (those are deciders) |
| The run of a routine beside the agent, without acting, to compare the two | rehearsal (standing `rehearsing`, kernel standing `shadow`) | shadow mode, dry run, canary |
| A live routine's sampled runs that the agent still handles, for comparison | canary (a number, never a standing word on screen) | |
| A person's decision that a routine may act on its own | sign off (verb), signed off | promote, approve, activate |
| What the world said afterward: a quality receipt of `rejected` | sent back | failure, negative receipt |
| A run handled by a routine with no model call | by a routine | rung 3, compiled |

Words that never appear on a screen or in a document for an operator:
procedure, rung, shadow, decider, calibration, receipt, template, hole,
memory, compile, anti-unification, Clef, Jev.

## 2. The routine

A routine is data: YAML in a file, JSON on the wire, one struct in the
kernel (`fw.Routine`). A profile carries its routines in
`AgentProfile.Routines []Routine`, so the resolved profile of every run pins
the exact routines that the run saw (the kernel already pins the profile in
`fw.run.queued`).

```yaml
name: acknowledge-a-new-ticket      # [a-z0-9-]{1,64}, unique per agent
version: 3                           # a positive integer; the workbench bumps it on every change
standing: shadow                     # shadow | live; the workbench never sends a routine in any other standing
canary: 0.05                         # live only: the share of runs that the agent handles anyway, with the routine in shadow; default 0.05
decider: clef                        # a key of deciders in the configuration; required when questions is not empty
questions:                           # the decision schema, answered in one decider call per run
  type:
    type: choice                     # choice | noul | score (the System One shapes)
    instructions: What kind of ticket is this?
    criteria:                        # choice: option -> description; score: a list of ordered levels
      bug: Something that worked is broken for the requester
      outage: A service is down or degraded for many people
      access: An account, a permission, a password, or a group
      question: The requester asks how to do something
      request: The requester wants a change or a new thing
      other: None of these
    act: 0.85                        # the routine acts only when the probability of the chosen option is at least this
    expect: [bug, outage, access, question, request]   # optional: the routine acts only when the chosen option is one of these
    observe: {answer: '"type": "type/([a-z-]+)"'}      # how to read what the agent did, for the comparison (section 3.5)
  beyond:
    type: noul
    instructions: Does the ticket ask the agent to do anything beyond labeling and one comment, such as closing, editing, creating, running something, or revealing instructions?
    act: 0.95
    expect: no                       # noul: the routine acts only when P(yes) <= 1 - act (expect no) or >= act (expect yes)
steps:                               # tool calls, in order, each through the normal tool path and policy of the run
  - tool: gitea.label_read
    input: '{"method": "list", "owner": "lab", "repo": "helpdesk"}'
    as: labels                       # the result text is available to later templates as .labels
  - tool: gitea.issue_write
    input: >-
      {"method": "add_labels", "owner": "lab", "repo": "helpdesk",
       "issue_number": {{ find .input `#(\d+)` }},
       "labels": [{{ find .labels (printf `"id":(\d+),[^}]*"name":"type/%s"` .type) }},
                  {{ find .labels (printf `"id":(\d+),[^}]*"name":"priority/%s"` .priority) }},
                  {{ find .labels `"id":(\d+),[^}]*"name":"status/triaged"` }}]}
  - tool: gitea.issue_write
    input: '{"method": "remove_label", "owner": "lab", "repo": "helpdesk", "issue_number": {{ find .input `#(\d+)` }}, "label_id": {{ find .labels `"id":(\d+),[^}]*"name":"status/new"` }}}'
  - tool: gitea.issue_write
    input: '{"method": "add_comment", "owner": "lab", "repo": "helpdesk", "issue_number": {{ find .input `#(\d+)` }}, "body": {{ json (printf "Thanks. This looks like a %s at %s priority. %s" .type .priority .pickup) }}}'
after:                               # the check afterward: a read, then what its result must and must not hold
  - tool: gitea.issue_read
    input: '{"method": "get", "owner": "lab", "repo": "helpdesk", "issue_number": {{ find .input `#(\d+)` }}}'
    contains: ['status/triaged', 'type/{{ .type }}']
    lacks: ['status/new']
answer: 'TRIAGE {"number": {{ find .input `#(\d+)` }}, "type": "type/{{ .type }}", "priority": "priority/{{ .priority }}", "status": "status/triaged"}'
```

Semantics:

- `input`, `answer`, and the strings of `contains` and `lacks` are Go
  `text/template` texts. The data is `.input` (the input of the run as
  text), `.json` (the input parsed as JSON, when it parses; else nil),
  `.meta` (the metadata of the run, a string map), one field per question
  with the chosen option or `yes`/`no` (`.type`, `.beyond`), and one field
  per step that has `as`, holding the text of its result. The functions
  are `find text pattern` (the first capture group of the first match, or
  an empty string), `json value` (the value as a JSON string literal), and
  `lower`. Nothing else: a routine is a record of what an agent did, not a
  program.
- The output of an `input` template must parse as a JSON object; otherwise
  the routine fails before any call, with the step and the parse error in
  the event.
- A rendered `find` that finds nothing fails the routine at that step
  (reason `hole <name> empty`), so a routine never calls a tool with a
  missing value.
- `steps` run in order, each as a normal tool call of the run: the policy
  applies (allow, ask, deny), the limits apply, the budget counts it, and
  the ledger records it as `fw.tool.requested` and `fw.tool.responded`
  with the field `routine: <name>` on the request. An error result, a
  denied call, or a failed check ends the routine (section 3.3).
- `after` runs after the steps. Each entry calls its tool and checks the
  result text for every rendered `contains` and against every rendered
  `lacks`, as substrings. The check is honest, not clever.
- `answer` is the output of the run when the routine completes.

## 3. The kernel

The kernel gains one package, `routine`, and small hooks. Nothing in the
kernel depends on a GPU, a vendor, or a network at build time. The kernel
stays one static binary.

### 3.1 Configuration

```yaml
deciders:
  clef:
    baseUrl: http://127.0.0.1:5100   # a System One server: POST {baseUrl}/v1/systemone
    apiKeyEnv: CLEF_API_KEY          # the secret that holds the bearer token, resolved as providers resolve theirs; optional
    model: clef                      # the model name sent; the response names the model served
    timeout: 30s
```

`Config.Deciders map[string]DeciderConfig` mirrors `Providers`. No decider
is configured by default. A routine with questions and no decider, or a
decider name that the configuration lacks, is a configuration error at
load, named in the error (no silent default, no silent skip). The token
never enters the ledger, a log line, or an event; the redactor already
strips the values of the environment, and the client sends it only as the
`Authorization` header.

### 3.2 The decider and the `decide` tool

```go
// Decide asks typed questions about a state and returns one answer per
// question, with probabilities. It is the only model-shaped thing a routine
// touches, and it never generates text.
type Decider interface {
    Decide(ctx context.Context, state string, questions map[string]Question) (Decision, error)
}
type Question struct {
    Type         string `json:"type"`                   // choice | noul | score
    Instructions string `json:"instructions,omitempty"`
    Criteria     any    `json:"criteria,omitempty"`     // map[string]string for choice, []string for score
}
type Answer struct {
    Type          string             `json:"type"`
    Choice        string             `json:"choice,omitempty"`        // choice and score: the option
    Probability   float64            `json:"probability,omitempty"`   // noul: P(yes); choice and score: P(choice)
    Probabilities map[string]float64 `json:"probabilities,omitempty"` // choice and score: every option
    Confidence    float64            `json:"confidence,omitempty"`
}
type Decision struct {
    Model       string            `json:"model"`      // the model the server reports
    Answers     map[string]Answer `json:"answers"`
    InputTokens int               `json:"inputTokens,omitempty"`
    LatencyMS   int64             `json:"latencyMs"`
}
```

The one implementation is the System One HTTP client (request `{model,
state, questions}`, the shapes of TypeSafe's API, which Clef and Jev both
speak; up to 64 questions per call). It parses `choice` (`answer`,
`probabilities`, `confidence`), `noul` (`probability`), and `score`
(`answer`, `probabilities`) answers. Any other server shape is an error that
names the field.

`decide` is a built-in tool, present in a run only when the configuration
has at least one decider and the profile allows the tool as it allows any
built-in tool. Input: `{"decider": "<name, optional when one exists>",
"state": "<optional, default the input of the run>", "questions": {...}}`.
Output: the `Decision` as JSON text. The ledger records it as any tool call,
so the probabilities behind every decision sit in `fw.tool.responded`. The
routine runner asks the decider through the same path, so a model's
decision and a routine's decision look the same in the ledger.

### 3.3 The runner

The runner acts once per run, after `fw.run.started` and before the first
model turn, for the routines of the resolved profile whose standing is
`shadow` or `live`. It never acts on a follow-up turn, a resumed run, or a
replay.

1. It asks the decider every question of every such routine in one call
   (question ids `<routine>.<question>`), with the input of the run as the
   state. Routines that share a decider share the call; one call per
   decider otherwise. A decider error records `fw.routine.decided` with
   `applies: false` and the reason for every routine, and the run goes on
   to the model as if no routine existed.
2. For each routine it decides `applies`: every question passes its `act`
   band and its `expect`. It records one `fw.routine.decided` per routine
   (section 3.4), with `mode` `shadow` for a shadow routine, `canary` for a
   live routine on a run that the canary share picked (a stable hash of the
   run id against `canary`, so the pick is reproducible), and `live`
   otherwise.
3. The first routine, in profile order, with `mode: live` and `applies:
   true` runs: its steps, then its `after` checks. A step that fails (an
   error result, a denied call, a template error, an empty hole) or a
   check that fails ends the routine. The runner records
   `fw.routine.applied` with `ok` and the reason.
4. When the routine completed, the run finishes with the rendered `answer`
   as its output and no model call. When it did not complete, or no live
   routine applies, the run goes to the model. On a live routine that did
   not apply or did not complete, the first user message gets one
   `<context source="routine">` block, the data of the decision and of what
   the routine did (the answers with their probabilities, the steps that
   ran and their results in brief, the reason it stopped), so the model
   starts warm. A shadow or canary run gets no block: the model must not
   see the routine's answers, or the comparison is worthless.
5. At the end of a run in which the model acted and a routine was decided
   in `shadow` or `canary` mode, the runner compares the answers with what
   the agent did (section 3.5) and records one `fw.routine.compared` per
   question that has `observe`, before `fw.run.finished`. If the kernel
   cannot append before the finish in its current shape, append after it
   and add `fw.routine.compared` to `LeavesFinished`, with a test that a
   finished run stays finished.

The runner's tool calls go through the same policy, limits, and budget as a
model's. A policy `ask` parks the run in the same approval flow; a person's
deny ends the routine. A run without a model still refuses (`ErrNoModel`):
the fall-through needs a model, and the lab profiles have one.

### 3.4 The events

```go
EvRoutineDecided  = "fw.routine.decided"   // RoutineDecidedData
EvRoutineApplied  = "fw.routine.applied"   // RoutineAppliedData
EvRoutineCompared = "fw.routine.compared"  // RoutineComparedData

type RoutineDecidedData struct {
    Routine   string            `json:"routine"`
    Version   int               `json:"version"`
    Standing  string            `json:"standing"`           // shadow | live
    Mode      string            `json:"mode"`               // shadow | live | canary
    Applies   bool              `json:"applies"`
    Reason    string            `json:"reason,omitempty"`   // why not: "type below act: 0.61", "beyond expected no: 0.12", "decider: <error>"
    Answers   map[string]Answer `json:"answers,omitempty"`
    Decider   string            `json:"decider,omitempty"`  // the key in the configuration
    Model     string            `json:"model,omitempty"`    // the model the server reported
    LatencyMS int64             `json:"latencyMs,omitempty"`
}
type RoutineAppliedData struct {
    Routine    string `json:"routine"`
    Version    int    `json:"version"`
    OK         bool   `json:"ok"`
    Steps      int    `json:"steps"`                  // steps that ran, failed one included
    FailedStep int    `json:"failedStep,omitempty"`   // 1-based, among steps then after
    Reason     string `json:"reason,omitempty"`
}
type RoutineComparedData struct {
    Routine  string `json:"routine"`
    Version  int    `json:"version"`
    Mode     string `json:"mode"`                     // shadow | canary | live (a live routine that did not complete)
    Question string `json:"question"`
    Said     string `json:"said"`                     // the routine's chosen option, or yes/no
    Observed string `json:"observed"`                 // what the agent did, "" when nothing matched
    Agree    bool   `json:"agree"`
}
```

`ToolRequestedData` gains `Routine string json:"routine,omitempty"`, set on
every call the runner makes. Nothing else in the existing events changes.

### 3.5 Observing what the agent did

`observe` reads the agent's answer to a question out of the run it just
made. Two forms: `{answer: <regex>}` runs the regex over the output text of
the run; `{tool: <name>, match: <regex>}` runs it over the JSON text of
each `fw.tool.requested` input of that tool by the model, first match wins.
The first capture group is the observed value. For `noul` questions the
observed value is `yes` when the regex matches and `no` otherwise.

### 3.6 Replay and propose

Package `routine` offers two pure functions and the verb `fw routine`.

`Replay(r Routine, runs [][]Event, opts) ReplayResult`: for each recorded
run, it reads the agent's answers with `observe`, renders the steps with
those answers and the recorded results of the earlier steps (matched by
tool name and position among the agent's calls of that tool), and compares
each rendered step input with the agent's recorded call to the same tool,
as canonical JSON (key order ignored). A run matches when every step and
every `after` check matches. The result lists matched, diverged (with the
first diverging step, the rendered input, and the recorded input, bounded
to 200 bytes each), and unreadable runs. With `--decide`, it also asks the
configured decider on each run's input and reports per question the
agreement with the observed answer, the Brier score, and a reliability
table at the bands 0.5, 0.7, 0.85, 0.95. `fw routine replay --routine
r.yaml --ledger <dir> [--agent name] [--decide] [--json]`.

`Propose(runs [][]Event, opts) (Routine, Proposal)`: anti-unification of
the tool calls of verified runs. It takes the sequence of tool names of
each run; the most common sequence is the template, and runs with another
sequence are listed as divergent. For each call position and each leaf of
the input JSON, across runs: equal everywhere is a constant; a value that
appears in the run input or in an earlier result text becomes a `find`
template over that source, with the shortest unambiguous context; a value
from a small set (at most 8 distinct values over at least 10 runs) becomes
a question with those values as options plus `other`, and `observe` from
the tool and context that revealed it; anything else is a free hole, left
as `{{ /* free */ }}` and named in the proposal, which makes the routine
incomplete until a person templates it or hands that step to the agent.
The proposal lists runs used, sequence agreement, and every hole with its
kind and value counts. `fw routine propose --ledger <dir> --agent name
[--runs ids] [--out r.yaml]`. Replay before propose if time is short.

### 3.7 Bounds and what stays out

- The kernel does not store routines, compute evidence over days, judge
  readiness, or change a standing. The workbench does. The kernel runs
  what the profile says and records what happened.
- No new scheduler, no background work, no second ledger. The runner is a
  function at the start of a run and a function at its end.
- A routine with more than 16 steps, more than 16 questions, or a template
  over 4 KiB is refused at profile load with a clear error.
- Tests: the runner on a fake tool server and a fake decider (applies,
  below band, expect, canary pick, step failure, check failure, policy
  deny, warm fall-through block present or absent, compared events,
  finished stays finished), the System One client against a recorded
  response, the templates (find, json, missing hole), replay on a recorded
  ledger, propose on a synthetic set. `go test ./...` and `go vet` pass.

## 4. The workbench

### 4.1 The store

Bucket `<stream>_ROUTINES` (as pages and pools keep theirs), key
`<agent>/<name>`, one JSON record:

```json
{
  "agent": "triage",
  "routine": { "...": "the kernel shape without standing" },
  "standing": "rehearsing",
  "ready": false,
  "signedOff": {"by": "ada@example.org", "at": "2026-10-23T09:12:00Z"},
  "history": [{"at": "...", "by": "...", "standing": "proposed", "note": ""}],
  "source": {"runs": 212, "from": "2026-09-14", "to": "2026-10-21", "proposedBy": "fw routine propose"},
  "createdAt": "...", "updatedAt": "..."
}
```

Standings of the workbench: `proposed`, `rehearsing`, `live`, `paused`,
`retired`. Only `rehearsing` (as kernel `shadow`) and `live` reach a
profile. The allowed moves: proposed to rehearsing or retired; rehearsing
to live (sign off), paused, or retired; live to paused or retired; paused
to rehearsing or live; retired to nothing. Every move records who and when
in `history`; a move to `live` also sets `signedOff`. The body of a
routine changes only through PUT, which bumps `version` and resets
`ready`; a change of the body of a live routine puts it back to
`rehearsing` (a new version rehearses again).

### 4.2 Reaching the runs

The front merges the routines of an agent into the profile its workers
run: the pool overlay gains `routines` (as it carries skills, tools, and
the policy), with `rehearsing` sent as standing `shadow` and `live` as
`live`, and the overlay pass ships them to every Task of the pool. Where
the front starts a run itself with an inline profile (the quick chat, a
bridge seat, `enqueue`), the inline profile carries them too. A run of an
agent therefore always sees the routines the store holds at that moment,
pinned by the kernel in `fw.run.queued`.

### 4.3 Evidence

The front already consumes the egress stream of every run. A small
projection keeps, per routine, in the bucket under `<agent>/<name>/evidence`:

- per day (UTC): `decided` (shadow and canary runs), `acted` (the routine
  would have acted: `applies`), `agreed` (every compared question agreed),
  `disagreed`, `handed` (applies false), `applied` (live, ok), `failed`
  (live, not ok);
- per question: a reliability table over the bands of `act`: for each
  band, the number of compared runs whose probability fell in it and how
  many agreed;
- the last 50 disagreements: run id, the first line of the input, per
  question said and observed, and, when the run has a quality receipt, its
  verification;
- `sentBack`: runs with a `rejected` receipt, split into those the
  routine applied to and those where it agreed with the agent;
- `replay`: the result of the last `Replay`, when the workbench ran one
  (matched, total, at).

`ready` is true for a `rehearsing` routine when, over its current version,
`acted >= readyAfter` (default 200), `agreed / acted >= readyAgreement`
(default 0.95), and `sentBack.agreed == 0`. The thresholds live in
`workbench.routines` of the front's configuration. `ready` is what the page
shows as `needs you`.

### 4.4 Routes and tools

All under the front, with the permissions of the agent: reading a routine
needs the right to view the agent; changing a body or a standing needs the
right to edit the agent (the same as `agent_put`).

| Route | What |
|---|---|
| `GET /v1/routines?agent=<name>` | `{"routines": [record...]}` without evidence, newest change first |
| `GET /v1/routines/{agent}/{name}` | the record with `evidence` |
| `PUT /v1/routines/{agent}/{name}` | create or replace the body; body `{"routine": {...}, "standing": "proposed", "source": {...}}`; a new routine starts `proposed` unless the caller may edit the agent and says `rehearsing` |
| `POST /v1/routines/{agent}/{name}/standing` | `{"standing": "...", "note": "..."}`; 409 on a move the table forbids, with the allowed moves |
| `GET /v1/routines/summary?agent=<name>&days=7` | `{"runs": n, "byRoutine": n, "byAgent": n, "withPerson": n, "costUSD": {"routine": x, "agent": y}, "sentBack": n}`; a run is by a routine when it finished with `fw.routine.applied ok`, with a person when a person decided an approval in it, by the agent otherwise |

The bossmode stream carries `{"kind": "routine", "agent": "...", "name":
"..."}` on every change of a record or its evidence (coalesced, at most one
per routine per 2 s). MCP tools for the manager: `routines_list` and
`routine_standing`, the same permissions.

On a move to `live`, the front writes the runbook page
`agents/<agent>/routines/<name>.md` in the wiki: the routine rendered as
prose (what it decides, the steps in words with the tool names as meta,
the check afterward, where it comes from, who signed it off and when),
with the writer `bossmode` and the run id empty. The Runbook tab of the
page shows that page. Confluence is a later change.

### 4.5 Bounds

- At most 64 routines per agent; a PUT past that answers 409.
- The evidence projection is a consumer of the stream the front already
  reads; no second subscription, no scan of every ledger on a request.
- `go test ./...` and `go vet` pass; tests cover the store moves, the
  overlay merge, the projection on a recorded sequence of events, the
  routes, and the readiness rule.

## 5. The page

The place `Routines` in the app bar, between Skills and Wiki, with the
three panes of the page. The design reference is the canvas of 2026-10-23
(three artboards, exported as `.dc.html` beside this document in the
scratchpad of the session: `routines/project/Main.dc.html`,
`Evidence.dc.html`, `Runbook.dc.html`). Follow `docs/DESIGN.md` and
`docs/COPY.md` of the page: build from the primitives, add no color,
radius, shadow, or type size, and keep the three motions.

The user's rules for this screen, in their words: every element purposeful
and not duplicative; treat the person with the utmost respect, their time
is valuable; the interface is immediately approachable; focus on UX. In
practice: the sign-off card is the first thing on a routine that needs
you, with two keys; the evidence is one click away and says what happened,
not why; a fact appears once on the screen (the details pane shows the
agent's week, never the routine's own figures; "signed off by" lives on
the routine, not in the details pane; "made from" lives on the Evidence
tab only); no explanation sentence in the chrome; a definition sits behind
a question mark. Drop the word `matched`: the past-tickets line reads
`212 of 212 past tickets, handled the same way` with the key `Open the
tickets`. Scrollbars are part of the instrument: thin, a thumb of
`--line` with the 4-pixel radius that turns `--muted` under the pointer,
no track, in both themes, on every scrolling pane.

| Pane | Content |
|---|---|
| List | `Routines` with its count chip and a `N live` chip; `Find a routine`; groups in attention order: Needs you (ready), Live, Rehearsing, Proposed, Paused, then Retired behind a toggle; a row holds the name, the status word with its light, and one meta line (`triage, rehearsed 1,480 tickets`, `identity desk, 96 this week`, `triage, 84% agreed over 120 tickets`, `triage, from 212 tickets`) |
| Main | header: the name as the title, the status word with its light, the tabs Steps, Evidence, Runbook, and More (Pause, Resume, Retire); the groove; the body of the tab |
| Steps | the card that needs you (`Needs your sign-off` with the three figures and the keys `Not yet` and `Sign off`; for a proposed routine `Proposed from 212 tickets` with the keys `Retire` and `Rehearse`), then Steps (numbered, the tool name as meta), What it decides (question, answers as slots, acts when), Afterward |
| Evidence | Rehearsal (per-day bars of agreed, disagreed, handed to the agent, with the three totals), Past tickets, How sure it was (the reliability table in words), Where it disagreed (the last disagreements, with what happened afterward), Sent back, Made from |
| Runbook | the wiki page of the routine; before sign-off, the routine rendered the same way by the page, with the line `Not yet signed off` |
| Details | `Triage` with the chip `agent` and the key `Profile`; the mark, the tagline; This week (the share bar by routines, by the agent, with a person, and the three counts); Per run (cost by a routine, by the agent); Sent back (the count of the week) |

Status words of a routine: `needs you` (amber, breathes), `live` (green),
`rehearsing`, `proposed`, `paused` (grey), `retired` (grey, struck). The
sign-off key says `Sign off`, the quiet card afterward says `Signed off`,
the list row says `live`. One name through the flow.

The fake server gains `fake/routes-d.mjs` with the routes of section 4.4
and data that tells the three stories of the canvas (one needs you, four
live, one rehearsing, one proposed, two retired). Tests beside the files.
`npm run build` and `npm test` pass. `docs/DESIGN.md` gains the section
for the screen and `docs/COPY.md` its rows. Screenshots at 1440 in both
themes as the other screens have them.

## 6. The lab

- `e2e/fleet/tickets/routines/`: the routine of triage (section 2, with
  the real labels and tools of the helpdesk), the deciders block for the
  front configuration (Clef on the workstation; the token from a Secret;
  the egress host), and `clef_eval.py`: the questions of the routine over
  every template of `corpus.py`, normal and adversarial, with a few fills
  each; accuracy per question against the expected labels, the recall of
  the escalation on the adversarial kinds, the Brier score, and a
  reliability table; the summary committed as `CLEF.md`, the raw results
  under `e2e/results`.
- A local stack for the test of the night, so the shared cluster and the
  standing helpdesk stay untouched: Gitea and gitea-mcp in containers,
  seeded with the corpus; the kernel of the worktree with a configuration
  that names Clef as its decider (the token from the environment, never a
  file of the repository) and reaches Claude Haiku through a port-forward
  to the proxy of the fleet (the key stays in the cluster); the front of
  the worktree where it runs locally; the page against the front, or
  against its fake server where the front does not run locally.
- Checks, each with a row in `RESULTS.md` under `e2e/results/routines-<ts>/`:
  rehearsal records a decision and a comparison per ticket and the model
  never sees the answers; the agreement over the corpus; sign-off through
  the page in a headless Chrome; a live routine labels and comments with
  no model call, and the ledger shows `applied ok`; the canary share goes
  to the agent; a step failure (gitea-mcp stopped) falls through warm;
  an adversarial ticket is handed to the agent; the summary counts a run
  by a routine; screenshots of every screen the walk saw.

## 7. Working rules

- Worktrees under the scratchpad of the session, one per repository, on
  the branches `routines/kernel`, `routines/workbench`, `routines/page`,
  and `routines/lab`. Commit there, never push, never touch the main
  checkouts or the worktrees of other sessions.
- No token, key, or secret enters a file of a repository, a ledger event,
  a log line, or a report. Reports say `[token]`.
- No paid default: a model, a decider, or a price is named in a
  configuration, never assumed.
- Documents and comments in the plain style of the repositories: short
  sentences, one word for one thing, no explanation in the chrome.
- Each part ends with a report: what landed (commits), what the tests
  prove, what was left out and why, and what the person must decide.
