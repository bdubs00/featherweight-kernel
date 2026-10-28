# Routines: the muscle memory of an agent

This document is the contract for routines between the kernel, the
workbench, the page (bossmode), and the lab. It names one word for one
thing. It gives the data shape, the events, the routes, and the words on
the screen. Each part builds against it in its own worktree. The design
behind it is "memory as a build". The ledger is the only memory of record.
A routine is a compiled view of verified runs, and the kernel can run it
again without a model.

## 1. One word for one thing

| Thing | The word | Never |
|---|---|---|
| A vetted sequence of tool calls with typed holes, a decision schema, and a check afterward. The kernel runs it under the policy of the run. | routine | procedure, skill, playbook, macro, checkpoint |
| A hole that a decision model fills from the input | question, with its answers | slot, classifier label, feature |
| A hole that copies a value from the input or from an earlier result | a template (`{{ }}`) | data flow, binding |
| The model that answers typed questions with probabilities | decider | classifier, Clef, Jev (those are deciders) |
| The run of a routine beside the agent, without acting, to compare the two | rehearsal (standing `rehearsing`, kernel standing `shadow`) | shadow mode, dry run, canary |
| The sampled runs of a live routine that the agent still handles, for comparison | canary (a number, never a standing word on screen) | |
| The decision of a person that a routine can act on its own | sign off (verb), signed off | promote, approve, activate |
| What the world said afterward: a quality receipt of `rejected` | sent back | failure, negative receipt |
| A run that a routine handled with no model call | by a routine | rung 3, compiled |

Words that never appear on a screen or in a document for an operator:
procedure, rung, shadow, decider, calibration, receipt, template, hole,
memory, compile, anti-unification, Clef, Jev.

## 2. The routine

A routine is data: YAML in a file, JSON on the wire, and one struct in the
kernel (`fw.Routine`). A profile carries its routines in
`AgentProfile.Routines []Routine`. The resolved profile of every run
therefore pins the exact routines that the run saw. The kernel already
pins the profile in `fw.run.queued`.

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
    compare: [method, owner, repo]   # optional: the keys that a replay compares; default every key of the rendered input
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
  `text/template` texts. The data holds these fields:
  - `.input`, the input of the run as text
  - `.json`, the input parsed as JSON, or nil for an input that does not parse
  - `.meta`, the metadata of the run, a string map
  - one field per question, with the chosen option or `yes` or `no`, such as `.type` and `.beyond`
  - one field per step with `as`, with the text of its result
- The functions are `find text pattern`, `json value`, and `lower`.
  `find` gives the first capture group of the first match. `json` gives
  the value as a JSON string literal. The built-in functions of
  `text/template`, such as `printf`, `if`, and `eq`, stay. The kernel adds
  nothing else: a routine is a record of what an agent did, not a program.
- The output of an `input` template must parse as a JSON object.
  Otherwise the routine fails before any call, with the step and the parse
  error in the event.
- A rendered `find` that finds nothing fails the routine at that step,
  with the reason `hole <pattern> empty`. A routine therefore never
  calls a tool with a missing value. A missing field fails the step too.
- `compare` of a step names the keys of its input that a replay compares
  with the call of the agent (section 3.6). Without it, a replay compares
  every key of the rendered input. A comment in words is a key to leave
  out. The runner never reads `compare`.
- A `noul` takes no criteria, or criteria with the keys `true` and
  `false` only.
- The `steps` run in order. Each one is a normal tool call of the run.
  The policy applies (allow, ask, deny), the limits apply, and the budget
  counts the call. The ledger records it as `fw.tool.requested` and
  `fw.tool.responded`, with the field `routine: <name>` on the request.
  An error result, a denied call, or a failed check ends the routine
  (section 3.3).
- `after` runs after the steps. Each entry calls its tool. Then it checks
  the result text for every rendered `contains` and against every rendered
  `lacks`, as substrings. The check is honest, not clever.
- A step or a check fails on an error result and on a command that exits
  with a code other than 0, such as `test -f` on a missing file. `bash`
  reports such a command as a normal result for the model, so the runner
  reads the exit code itself.
- After a routine that completes, `answer` is the output of the run.

## 3. The kernel

The kernel gains one package, `routine`, and small hooks. Nothing in the
kernel depends on a GPU, a vendor, or a network at build time. The kernel
stays one static binary.

### 3.1 Configuration

```yaml
deciders:
  clef:
    driver: systemone                # optional, the registered client, systemone by default
    baseUrl: http://127.0.0.1:5100   # a System One server: POST {baseUrl}/v1/systemone
    apiKeyEnv: CLEF_API_KEY          # the secret that holds the bearer token, resolved as providers resolve theirs; optional
    model: clef                      # the model name sent; the response names the model served
    timeout: 30s
```

`Config.Deciders map[string]DeciderConfig` mirrors `Providers`. Each field
has a variable: `FW_DECIDERS_<NAME>_DRIVER`, `_BASE_URL`, `_API_KEY_ENV`,
`_MODEL`, and `_TIMEOUT`. A variable creates the entry when the file lacks
it. The configuration has no decider by default, and a decider has no
default model. A routine with questions and no decider is a configuration
error at load. So is a decider name that the configuration lacks. The
error names the problem: no silent default, and no silent skip. The token
never enters the ledger, a log line, or an event. The redactor already
strips the values of the environment. The client sends the token only as
the `Authorization` header.

### 3.2 The decider and the `decide` tool

```go
// Decide asks typed questions about a state and returns one answer per
// question, with probabilities. It is the only model-shaped thing a routine
// touches, and it never generates text.
type Decider interface {
    Decide(ctx context.Context, state string, questions map[string]Question) (DecideResult, error)
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
type DecideResult struct {
    Model       string            `json:"model"`      // the model the server reports
    Answers     map[string]Answer `json:"answers"`
    InputTokens int               `json:"inputTokens,omitempty"`
    LatencyMS   int64             `json:"latencyMs"`
}
```

The kernel names the result `fw.DecideResult`, not `fw.Decision`. The
policy of the kernel already has the name `fw.Decision`. The JSON shape
stays the same.

Package `fw` holds the interface, the configuration, the tool, and a
registry of the clients: a driver package calls
`fw.RegisterDecider(name, ctor)` in `init`, as a model provider does, and
`driver` names it. A `driver` that the binary does not link stops the
load. The one driver is the System One HTTP client, the package
`decider/systemone`. Every build of `fw` links it, the lean build of a
worker too, because it needs only `net/http`. So does `fw-workbench`. A
configuration with `deciders` therefore needs no rebuild. Its request is
`{model, state, questions}`, the shapes of the API of TypeSafe, which Clef
and Jev both speak. One call holds up to 64 questions. The runner splits
more questions of one decider into several calls. The client reads
both spellings of the answers. Clef and Workers AI send a `choice` with
`probabilities` and `confidence`, a `noul`, and a `score` with a `legend`
and `probabilities`. TypeSafe sends `answer` in place of `choice`, and
`probability` in place of `noul`. Any other server shape is an error that
names the field.

`decide` is a built-in tool. A run has it only under two conditions: the
configuration has at least one decider, and the profile allows the tool as
it allows any built-in tool. The input is this:

```json
{"decider": "<name, optional with one decider>", "state": "<optional, default the input of the run>", "questions": {}}
```

The output is the `fw.DecideResult` as JSON text. The ledger records it as
any tool call, so the probabilities behind every decision sit in
`fw.tool.responded`. The tool is part of the kernel (`fw.DecideTool`).
The routine runner calls it through the same path for its own decisions,
whatever `tools.builtin` holds and whatever the tool list of the profile
holds. A decision of a model and a decision of a routine therefore look
the same in the ledger.

### 3.3 The runner

The runner acts once per run, after `fw.run.started` and before the first
model turn. It acts on the routines of the resolved profile with the
standing `shadow` or `live`. It never acts on a follow-up turn or a
replay. It never acts twice: after a restart, it does not run a routine
again.

1. The runner asks the decider every question of every such routine in
   one call. The question ids are `<routine>.<question>`, and the state is
   the input of the run. Routines that share a decider share the call.
   Each other decider gets a call of its own. On a decider error, the
   runner records `fw.routine.decided` with `applies: false` and the
   reason for every routine of that decider. The run then goes on to the
   model, as a run without routines does.
2. For each routine, the runner decides `applies`: every question passes
   its `act` band and its `expect`. It records one `fw.routine.decided`
   per routine (section 3.4). A shadow routine has the `mode` `shadow`. A
   live routine has the mode `canary` on a run that the canary share
   picked, and `live` otherwise. The pick is a stable hash of the run id
   against `canary`, so the pick is reproducible.
3. The first routine in profile order with `mode: live` and `applies:
   true` runs. It runs its steps, then its `after` checks. A step that
   fails ends the routine: an error result, a denied call, a template
   error, or an empty hole. A check that fails ends it too. The runner
   records `fw.routine.applied` with `ok` and the reason.
4. A run with a definition of done (`ARCHITECTURE.md` section 24) checks
   the work of a routine that completed, as it checks an answer of the
   model. The kernel runs the checks before `fw.routine.applied` and
   records the pass as `fw.done.checked` with `answer` 0, because no model
   answered. The passes of the model leave that pass out, so the model
   keeps every attempt. When a check fails, the routine did not complete:
   `fw.routine.applied` has `ok: false`, `done: failed`, the failed step
   one place after its steps and its checks, and a reason that starts
   with `done:` and names the failed checks. After a routine that completed, the run
   finishes with the rendered `answer` as its output and no model call.
   With a definition of done, its end carries `done: checked` and the stop
   `checked`, so it counts as checked only as any run does. In every
   other case, the run goes to the model: the routine did not complete, or no live routine
   applies. For a live routine that did not apply or did not complete, the
   first user message gets one `<context source="routine">` block. The
   block holds the data of the decision and of what the routine did: the
   answers with their probabilities, the steps that ran with their results
   in brief, and the reason it stopped. The model therefore starts warm. A
   shadow or canary run gets no block. The model must not see the answers
   of the routine, or the comparison is worthless.
5. At the end of a run that the model completed, the runner compares the
   answers with what the agent did (section 3.5). It does so for each
   routine in the mode `shadow` or `canary`, and for each live routine
   that did not complete. It records one `fw.routine.compared` per
   question that has `observe`, before `fw.run.finished`. A finished run
   therefore stays finished, and `LeavesFinished` needs no change. A run
   that fails gets no comparison.
6. A routine in the mode `shadow` or `canary` that applies also gets one
   comparison of its calls: `fw.routine.compared` with `calls: true` and
   no question. The runner renders the steps and the checks on the run,
   with the answers of the routine and with the results of the calls of
   the agent, and compares them with those calls as Replay does (section
   3.6). It agrees when every step and every check with a call matched.
   So a routine without questions gains agreement too. A run on which a
   `find` finds no value is one that the routine hands to the agent, and
   it gets no comparison of the calls. The calls of the runner and of the
   checks of done are no calls of the agent.

The tool calls of the runner go through the same policy, limits, and
budget as those of a model. A policy `ask` parks the run in the same
approval flow, and a deny by a person ends the routine. A run without a
model still refuses (`ErrNoModel`). The fall-through needs a model, and
the lab profiles have one.

A restart can cut a live routine off. At the next start, the runner
records `fw.routine.applied` with `ok: false` and the reason `the process
stopped during the routine`. The model then starts with the block. For a
call of the routine that waited for an approval, the kernel first denies
the approval as `kernel`. A later answer of a person then gets the error
that no approval waits. A follow-up message that arrives during a routine
also goes to the model with the block, even after a routine that
completed.

A replay never runs a routine, also after a restart, because the mark
`replayOf` of its metadata says that it is one. A branch never compares
the decisions that it kept from its source. On a process whose runs a
hosted coding agent drives (ARCHITECTURE.md section 19), no turn loop
runs. The kernel therefore refuses a profile with routines there, at load
and on an inline profile.

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
    FailedStep int    `json:"failedStep,omitempty"`   // 1-based, among steps then after; one more for the checks of done
    Reason     string `json:"reason,omitempty"`
    Answer     string `json:"answer,omitempty"`       // the rendered answer of a routine that completed
    Done       string `json:"done,omitempty"`         // the checks of done on its work: checked or failed
}
type RoutineComparedData struct {
    Routine  string `json:"routine"`
    Version  int    `json:"version"`
    Mode     string `json:"mode"`                     // shadow | canary | live (a live routine that did not complete)
    Question string `json:"question"`                 // "" for the comparison of the calls
    Said     string `json:"said"`                     // the routine's chosen option, or yes/no; the input of the step that differs
    Observed string `json:"observed"`                 // what the agent did, "" when nothing matched; the input of its call
    Agree    bool   `json:"agree"`
    Calls    bool   `json:"calls,omitempty"`          // the comparison of the calls (section 3.3, item 6)
    Step     int    `json:"step,omitempty"`           // the first step that differs, 1-based, among steps then after
    Reason   string `json:"reason,omitempty"`         // how it differs: "step 2 files: the key file_text differs"
}
```

`ToolRequestedData` gains `Routine string json:"routine,omitempty"`. The
runner sets it on every call that it makes. A decide call that serves
several routines names them all, separated by commas. `RoutineAppliedData`
also carries `answer`. A restart between `fw.routine.applied` and
`fw.run.finished` can therefore still finish the run. Nothing else in the
existing events changes.

### 3.5 Observing what the agent did

`observe` reads the answer of the agent to a question out of the run that
it just made. It has two forms. `{answer: <regex>}` runs the regex over
the output text of the run. `{tool: <name>, match: <regex>}` runs it over
the JSON text of each `fw.tool.requested` input of that tool by the model,
and the first match wins. The first capture group is the observed value.
For a `noul` question, the observed value is `yes` for a regex that
matches, and `no` otherwise.

### 3.6 Replay and propose

Package `routine` offers two pure functions and the verb `fw routine`.

`Replay(ctx, r, runs, opts) ReplayResult` works as follows. For each
recorded run, it reads the answers of the agent with `observe`. Some runs
are ones that the routine leaves to the agent: an answer that a question
does not `expect`, or a `find` that finds no value. Replay counts such a
run as handed, apart from the comparison. For every other run, it renders
the steps with the answers and with the recorded results of the earlier
steps. A recorded result belongs to a step by tool name and by place among
the calls of the agent to that tool.

Replay then compares each step with the recorded call of the agent to the
same tool. It compares each key of `compare`, or else each key of the
rendered input, as canonical JSON. A key that only the call of the agent
holds is extra: Replay counts it and compares it not. A check `after`
matches the next call of its tool in the same way. Its recorded result
must hold every `contains` and no `lacks`. An agent that made no such call
leaves the check unchecked, and Replay counts it. For a match, every step
and every check with a call must match.

The result lists the matched runs, the diverged runs, the handed runs, and
the unreadable runs. A diverged run comes with the first diverging step,
the key that differs, the rendered input, and the recorded input, each
bounded to 200 bytes. With `--decide`, Replay also asks the configured
decider on the input of each run. It reports per question the agreement
with the observed answer and the Brier score. It also reports a
reliability table at the bands 0.5, 0.7, 0.85, and 0.95, in the text
output too.

```
fw routine replay --routine r.yaml [--ledger <dir>] [--agent name] [--decide] [--json]
```

Without `--ledger`, the verb reads the ledger of the configuration.

`Propose(runs [][]Event, opts) (Routine, Proposal)` is anti-unification of
the tool calls of verified runs. The calls of a run are the calls of the
model (`fw.ModelCalls`): the calls of the runner and of the checks of
done, whose call ids start with `routine_` and `done_`, are no steps. It
takes the sequence of tool names of each run. The most common sequence is the template. A list in an input
takes the length that most runs give it. The runs with another sequence or
another length are listed as divergent, with the reason. For each call
position and each leaf of the input JSON, across runs, Propose decides the
kind of the leaf:

- A value that is equal everywhere and that the agent read in an earlier
  result stays a value that the routine reads: a `find` over that result.
  This holds for a text with a space in it, such as a line of a file, and
  for a number after the word that labels it, such as `pens=` before
  `120`. So a routine that copies a line of a file writes the line that
  the file holds on its run, not the line of the runs that it came from.
- Any other value that is equal everywhere is a constant: a name, a path,
  a key, a number without a label, or a value that only the input holds.
- A key that some runs leave out and the others set to one value is left
  out, as an optional key. A key that some runs leave out and the others
  set to several values is a free hole of its own.
- A list goes element by element.
- A value that appears in the run input or in an earlier result text
  becomes a `find` template over that source. The context is the word
  that labels the value, such as `Number: (\d+)`. Only for a labeled
  context that is ambiguous, Propose takes the shortest unambiguous one.
  A text needs a mark after it: the next character, or the end of the
  text (`\z`), as for the last line of a file.
- A value from a small set becomes a question, with those values as
  options plus `other`. A small set holds at most 8 distinct values over
  at least 10 runs. The `observe` of the question comes from the tool and
  the context that revealed the value. The values of the answer become
  questions first.
- A value whose context in a result holds the value of a question becomes
  a `find` with `printf`, such as the label of a type.
- Anything else is a free hole, `{{/* free <place> */}}`, and the proposal
  names it. A free hole makes the routine incomplete. A person then
  templates it, or hands that step to the agent. A replay of a step with a
  free hole fails with `the routine cannot fill the value <place> yet`. Go
  does not parse the form `{{ /* free */ }}` with the spaces inside.

The proposal lists the runs used, the agreement of the sequence, and every
hole with its kind and its value counts. It also lists the optional keys
that it left out, and each problem that the kernel refuses in the routine.
A proposal with a problem or a free hole is not complete.

```
fw routine propose --agent name [--ledger <dir>] [--runs ids] [--out r.yaml] [--name n] [--decider d] [--json]
```

Without `--name`, the name is the agent name in lower case, with `-` for
every other character. `--decider` names the decider of the questions.
Without `--ledger`, the verb reads the ledger of the configuration.

A proposal reads many runs. That is the work of the workbench or of a
librarian over a ledger, not of a worker. A build with the tag `lean`,
such as the worker image, leaves out `Propose` and the verb, and `fw
routine propose` then says so. The tag `propose` brings them back. `fw`
without tags and `fw-workbench` carry them. Replay stays in every build,
because it needs the rendering of the kernel.

With little time, build Replay before Propose.

### 3.7 Bounds and what stays out

- The kernel does not store routines, compute evidence over days, judge
  readiness, or change a standing. The workbench does. The kernel runs
  what the profile says and records what happened.
- No new scheduler, no background work, and no second ledger. The runner
  is a function at the start of a run and a function at its end.
- At profile load, the kernel refuses more than 64 routines, and a
  routine with more than 16 steps, more than 16 checks, more than 16
  questions, or a template over 4 KiB, with a clear error.
- The tests cover the runner on a fake tool server and a fake decider, in
  these cases:
  - applies, below band, expect, and canary pick
  - step failure, check failure, and policy deny
  - the warm fall-through block, present or absent
  - compared events, and a finished run that stays finished
- The tests also cover the System One client against a recorded response,
  the registry of the deciders, the templates (find, json, missing hole),
  replay on a recorded ledger, and propose on a synthetic set. `go test
  ./...` and `go vet` pass, and `go test -tags lean ./cli/ ./routine/`
  covers the build without propose.

### 3.8 Turn routines on and off

Routines cost nothing until a configuration asks for them. Without
deciders and without routines, no run asks a decider, the tool `decide`
is off, and the runner returns at once. The front keeps an empty store,
which gives no profile a routine, so the hash of every pool stays and no
Task is replaced.

| Where | On | Off |
|---|---|---|
| The workers | `deciders.<name>` or `FW_DECIDERS_<NAME>_*`, and `decide` in `tools.builtin`, for a routine with questions | no `deciders` |
| A profile | `agents.<name>.routines`, in `fw.yaml` or a profile file, or the store of the front for a pool | no routine, or an inline profile with `routines: []` |
| The front | `workbench.routines.enabled` absent or true, on the bus `nats` or `mem` | `workbench.routines.enabled: false` or `FW_WORKBENCH_ROUTINES_ENABLED=off` |

The thresholds of the front are `workbench.routines.readyAfter` and
`readyAgreement`, or `FW_WORKBENCH_ROUTINES_READY_AFTER` and
`FW_WORKBENCH_ROUTINES_READY_AGREEMENT`.

One routine stops with a pause or a retirement in the store, or with its
removal from the profile. With the front off, the next pass of the pools
takes the routines of the store away from every profile, and the profiles
keep the routines of their own configuration. The page then says that
routines are off. Section 22.5 of `ARCHITECTURE.md` and section 21.7.1 of
the architecture of the workbench give the steps.

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
profile. The allowed moves are these:

- `proposed` to `rehearsing` or `retired`
- `rehearsing` to `live` (sign off), `paused`, or `retired`
- `live` to `paused` or `retired`
- `paused` to `rehearsing` or `live`
- `retired` to nothing

Every move records in `history` who made it and at what time. A move to
`live` also sets `signedOff`. The body of a routine changes only through
PUT, which bumps `version` and resets `ready`. A change of the body of a
live routine puts it back to `rehearsing`: a new version rehearses again.

### 4.2 Reaching the runs

The front merges the routines of an agent into the profile that its
workers run. The pool overlay gains `routines`, as it carries skills,
tools, and the policy. It sends `rehearsing` as standing `shadow` and
`live` as `live`, and the overlay pass ships them to every Task of the
pool. In some places the front starts a run itself with an inline profile:
the quick chat, a bridge seat, and `enqueue`. There the inline profile
carries the routines too. A run of an agent therefore always sees the
routines that the store holds at that moment, pinned by the kernel in
`fw.run.queued`.

### 4.3 Evidence

The front already consumes the egress stream of every run. A small
projection keeps these figures per routine, in the bucket under
`<agent>/<name>/evidence`:

- per day (UTC):
  - `decided`: shadow and canary runs
  - `acted`: the routine applies, so it is ready to act
  - `agreed`: every compared question agreed, and so did the calls
  - `disagreed`
  - `handed`: applies false
  - `applied`: live, ok
  - `failed`: live, not ok
- per question: a reliability table over the bands of `act`. For each
  band, the table counts the compared runs whose probability fell in it,
  and how many of them agreed.
- the last 50 disagreements: the run id, the first line of the input, what
  each question said and observed, and, for a run with a quality receipt,
  its verification.
- `sentBack`: the runs with a `rejected` receipt. They split into the runs
  that the routine applied to and the runs in which it agreed with the
  agent.
- `replay`: the result of the last `Replay` that the workbench ran
  (matched, total, at).

A `rehearsing` routine is `ready` under three conditions over its current
version:

- `acted >= readyAfter` (default 200)
- `agreed / acted >= readyAgreement` (default 0.95)
- `sentBack.agreed == 0`

The thresholds live in `workbench.routines` of the configuration of the
front, beside the switch `enabled` (section 3.8). The page shows `ready`
as `needs you`.

### 4.4 Routes and tools

All routes are under the front, with the permissions of the agent. To
read a routine, a person needs the right to view the agent. To change a
body or a standing, a person needs the right to edit the agent, the same
as `agent_put`.

| Route | What |
|---|---|
| `GET /v1/routines?agent=<name>` | `{"routines": [record...]}` without evidence, newest change first |
| `GET /v1/routines/{agent}/{name}` | the record with `evidence` |
| `PUT /v1/routines/{agent}/{name}` | create or replace the body. The body is `{"routine": {...}, "standing": "proposed", "source": {...}}`. A new routine starts as `proposed`. A caller with the right to edit the agent can say `rehearsing` instead. |
| `POST /v1/routines/{agent}/{name}/standing` | `{"standing": "...", "note": "..."}`. A move that the table forbids gets 409, with the allowed moves. |
| `GET /v1/routines/summary?agent=<name>&days=7` | `{"runs": n, "byRoutine": n, "byAgent": n, "withPerson": n, "costUSD": {"routine": x, "agent": y}, "sentBack": n}`. A run that finished with `fw.routine.applied ok` is by a routine. A run in which a person decided an approval is with a person. Every other run is by the agent. |

The bossmode stream carries `{"kind": "routine", "agent": "...", "name": "..."}`
on every change of a record or its evidence. The stream coalesces them, at
most one per routine per 2 s. The MCP tools for the manager are
`routines_list` and `routine_standing`, with the same permissions.

On a move to `live`, the front writes the runbook page
`agents/<agent>/routines/<name>.md` in the wiki. The page renders the
routine as prose: what it decides, the steps in words with the tool names
as meta, the check afterward, and where it comes from. It also names who
signed it off, with the time. The writer is `bossmode`, and the run id is
empty. The Runbook tab of the page shows that page. Confluence is a later
change.

### 4.5 Bounds

- At most 64 routines per agent. A PUT past that answers 409.
- The evidence projection is a consumer of the stream that the front
  already reads. It needs no second subscription, and no request scans
  every ledger.
- `go test ./...` and `go vet` pass. The tests cover the store moves, the
  overlay merge, the projection on a recorded sequence of events, the
  routes, and the readiness rule.

## 5. The page

The place `Routines` sits in the app bar, between Skills and Wiki, with
the three panes of the page. The design reference is the canvas of
2026-10-23, with three artboards. They are exported as `.dc.html` beside
this document in the scratchpad of the session:
`routines/project/Main.dc.html`, `Evidence.dc.html`, and
`Runbook.dc.html`. Follow `docs/DESIGN.md` and `docs/COPY.md` of the page.
Build from the primitives. Add no color, radius, shadow, or type size, and
keep the three motions.

The rules of the user for this screen:

- every element purposeful and not duplicative
- treat the person with the utmost respect, because their time counts
- the interface is immediately approachable
- focus on UX

In practice:

- The sign-off card is the first thing on a routine that needs you, with
  two keys.
- The evidence is one click away and says what happened, not why.
- A fact appears once on the screen. The details pane shows the week of
  the agent, never the figures of the routine itself. "Signed off by"
  lives on the routine, not in the details pane. "Made from" lives on the
  Evidence tab only.
- No explanation sentence in the chrome. A definition sits behind a
  question mark.
- Drop the word `matched`. The line of past tickets reads `212 of 212 past
  tickets, handled the same way`, with the key `Open the tickets`.
- Scrollbars are part of the instrument. They are thin, with a thumb of
  `--line` and the 4-pixel radius, which turns `--muted` under the
  pointer. They have no track, in both themes, on every scrolling pane.

| Pane | Content |
|---|---|
| List | `Routines` with its count chip and a `N live` chip. `Find a routine`. Groups in attention order: Needs you (ready), Live, Rehearsing, Proposed, Paused, then Retired behind a toggle. A row holds the name, the status word with its light, and one meta line. Examples of a meta line: `triage, rehearsed 1,480 tickets`, `identity desk, 96 this week`, `triage, 84% agreed over 120 tickets`, `triage, from 212 tickets`. |
| Main | header: the name as the title, the status word with its light, the tabs Steps, Evidence, Runbook, and More (Pause, Resume, Retire). Then the groove, then the body of the tab. |
| Steps | the card that needs you, then Steps, What it decides, and Afterward. The card says `Needs your sign-off`, with the three figures and the keys `Not yet` and `Sign off`. For a proposed routine, it says `Proposed from 212 tickets`, with the keys `Retire` and `Rehearse`. Steps are numbered, with the tool name as meta. What it decides shows the question, the answers as slots, and the band of its act. |
| Evidence | Rehearsal: per-day bars of agreed, disagreed, and handed to the agent, with the three totals. Past tickets. How sure it was: the reliability table in words. Where it disagreed: the last disagreements, with what happened afterward. Sent back. Made from. |
| Runbook | the wiki page of the routine. Before sign-off, the page renders the routine the same way, with the line `Not yet signed off`. |
| Details | `Triage` with the chip `agent` and the key `Profile`. The mark, the tagline. This week: the share bar by routines, by the agent, and with a person, and the three counts. Per run: the cost by a routine and by the agent. Sent back: the count of the week. |

Status words of a routine: `needs you` (amber, breathes), `live` (green),
`rehearsing`, `proposed`, `paused` (grey), `retired` (grey, struck). The
sign-off key says `Sign off`, the quiet card afterward says `Signed off`,
and the list row says `live`. One name runs through the flow.

The fake server gains `fake/routes-d.mjs` with the routes of section 4.4.
Its data tells the three stories of the canvas: one needs you, four live,
one rehearsing, one proposed, and two retired. Tests sit beside the files.
`npm run build` and `npm test` pass. `docs/DESIGN.md` gains the section
for the screen, and `docs/COPY.md` gains its rows. The screen gets
screenshots at 1440 in both themes, as the other screens have them.

## 6. The lab

- `e2e/fleet/tickets/routines/` holds the routine of triage (section 2,
  with the real labels and tools of the helpdesk). It also holds the
  deciders block for the configuration of the front: Clef on the
  workstation, the token from a Secret, and the egress host. Last, it
  holds `clef_eval.py`. That script asks the questions of the routine over
  every template of `corpus.py`, normal and adversarial, with a few fills
  each. It reports the accuracy per question against the expected labels
  and the recall of the escalation on the adversarial kinds. It also
  reports the Brier score and a reliability table. The summary is
  committed as `CLEF.md`, and the raw results go under `e2e/results`.
- A local stack serves the test of the night, so the shared cluster and
  the standing helpdesk stay untouched. It has Gitea and gitea-mcp in
  containers, seeded with the corpus. It runs the kernel of the worktree
  with a configuration that names Clef as its decider. The token comes
  from the environment, never from a file of the repository. The kernel
  reaches Claude Haiku through a port-forward to the proxy of the fleet,
  and the key stays in the cluster. The front of the worktree runs
  locally where it can. The page runs against the front, or against its
  fake server where the front does not run locally.
- Checks, each with a row in `RESULTS.md` under
  `e2e/results/routines-<ts>/`:
  - rehearsal records a decision and a comparison per ticket, and the
    model never sees the answers
  - the agreement over the corpus
  - sign-off through the page in a headless Chrome
  - a live routine labels and comments with no model call, and the ledger
    shows `applied ok`
  - the canary share goes to the agent
  - a step failure (gitea-mcp stopped) falls through warm
  - an adversarial ticket goes to the agent
  - the summary counts a run by a routine
  - screenshots of every screen that the walk saw

## 7. Working rules

- Worktrees live under the scratchpad of the session, one per repository,
  on the branches `routines/kernel`, `routines/workbench`,
  `routines/page`, and `routines/lab`. Commit there. Never push, and never
  touch the main checkouts or the worktrees of other sessions.
- No token, key, or secret enters a file of a repository, a ledger event,
  a log line, or a report. Reports say `[token]`.
- No paid default: a configuration names a model, a decider, or a price,
  and nothing assumes one.
- Documents and comments follow the plain style of the repositories:
  short sentences, one word for one thing, and no explanation in the
  chrome.
- Each part ends with a report: what landed (commits), what the tests
  prove, what was left out and why, and what the person must decide.
