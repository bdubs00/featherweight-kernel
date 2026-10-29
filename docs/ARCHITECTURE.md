# Featherweight Kernel architecture

For a source-backed visual tour of the current implementation, see the [kernel architecture map](KERNEL_MAP.md): components, run execution, and ax deployment.

## 1. Summary

Featherweight Kernel is one static Go binary, named `fw`, that runs an agent loop in production. It is the agent runtime for one google/ax Task. It runs as the main process inside a Task, as a plain container, or as a library inside your own Go program. It exposes one agent through three surfaces on one port: a native HTTP API, an A2A server, and a Prometheus endpoint. It accepts work from HTTP, from A2A, from a message queue, or from the command line. Every component is a Go interface with one default implementation, so you replace a component without a fork.

The workbench for people is a separate repository, featherweight-workbench. Its binary, `fw-workbench`, is the kernel plus the page at `/ui`, the browser sign-in, the MCP server, the fleet, and the agent builder. Its document `docs/ARCHITECTURE.md` describes these parts. This document calls it the workbench architecture.

This document describes the design of the kernel. Section 16 gives the delivery order.

## 2. Design rules

Eight rules shape every decision below. Each rule removes a class of features.

1. One binary. `fw` is a static binary with no sidecar and no interpreter.
2. One port. Every surface mounts on one HTTP listener. Under ax, that port is 80.
3. One source of truth. The ledger is the only state. The ledger is the append-only log of events for a run. Every other state is a projection, and a projection can be rebuilt from the ledger.
4. One default per interface. Every component is a Go interface with exactly one default implementation. The kernel imports no driver.
5. Append only. The kernel never edits history. It appends events and messages.
6. Zero configuration works. The command `fw run "goal"` with one API key in the environment is a complete setup.
7. A feature is a projection or a plug. The kernel gains no feature branches.
8. The kernel depends on the Go standard library only. Each driver brings its own dependencies in its own package.

## 3. The shape

```
   HTTP (/v1)   A2A (/a2a)   Bus intake   CLI
        \           |            |         /
         \          |            |        /
          +--------------------------------------------+
          |                  Mailbox                    |  one inbox per run, one writer
          +--------------------------------------------+
                               |
          +--------------------------------------------+
          |                  Kernel                     |  the turn loop
          |     Model  <->  turn  <->  Tools, Policy    |
          +--------------------------------------------+
                               |  append
          +--------------------------------------------+
          |                  Ledger                     |  append-only CloudEvents
          +--------------------------------------------+
             |         |          |         |        |
         messages   status    metrics     bus     traces      projections
```

Work enters through a surface or the bus. The kernel serializes appends for each run and gives each driven run one goroutine. That goroutine calls the model and runs tools. Projections observe the recorded events and never write to the ledger. Surfaces read projections and call the kernel. Each process owns its ledger; replicas do not share a file ledger. A fleet distributes work across processes with separate ledgers through the bus and the workbench. The workbench adds the MCP server as one more surface (the workbench architecture, section 3).

## 4. The ledger

### 4.1 Envelope

Every event is a CloudEvents 1.0 JSON object. CloudEvents is a CNCF standard envelope for events. The envelope has these fields:

| Field | Value |
|---|---|
| `specversion` | `1.0` |
| `id` | A ULID. A ULID is a unique id that sorts by time. |
| `source` | `fw://<atespace>/<agent>/<run>` |
| `type` | One of the types in section 4.2, for example `fw.model.responded` |
| `time` | An RFC 3339 timestamp from the clock of the agent (section 5.8) |
| `subject` | The run id |
| `seq` | The position of the event in its run, from 1 |
| `traceparent` | The W3C trace context of the run |
| `data` | The payload for the type |

### 4.2 Event types

| Type | Data |
|---|---|
| `fw.run.queued` | agent, input, instructions, inline profile, budget, metadata, origin, principal and its kind, parent, depth, file references (section 7.10), extra tools (section 12.5), resolved profile (section 12.5), kept events of a branch (section 4.7), the check of a check run (section 23), the data blocks of the caller (section 7.8.4) |
| `fw.run.started` | model (none for a check run, section 23), tool names, brief (the sections that tools add to the system prompt), context (the data that tools and the caller add to the first user message, sections 7.8.2 and 7.8.4), time zone, `knowhow: "off"` for a comparison run (section 7.8.4), and `routines`, the routines that the front gave the run at its start with their version, or why it gave none (section 22.1) |
| `fw.memory.changed` | one change of a memory note by a tool call: call id, tool, operation, ref, old ref, scope, agent, size, SHA-256, the sources of a write, and `waiting` when the front holds the change for a person; never the text (section 7.8.1) |
| `fw.memory.used` | the notes that a run read: `how` (`brief`, `view`, or `recall`) and `refs`, each a ref and the hash of the text that the run read. It never holds the text or the principal (section 7.8.4) |
| `fw.run.annotated` | metadata, usage, provider, model (section 8.2), or a trusted quality receipt with its own cost snapshot (section 10.1) |
| `fw.run.reacted` | the reaction of a person on an event of the run: `seq`, the event, `0` for the run as a whole; `reaction`, `up`, `down`, or `heart`; `by`, the subject of the person; `note`, at most 1 KB; `receipt`, true when a thumb also recorded a quality receipt (sections 8.2 and 10.1); and `removed`, true when the person took the reaction back |
| `fw.message.received` | role, content, origin (`http`, `a2a`, `mcp`, `bus`, `cli`, `kernel`), note kind, and for a budget note what it is about: the limit, the threshold, the use, the limit value, and the run that holds the limit, or `changed` (section 5.2); `by`, the subject of the principal that sent the message, and `from`, the party that a trusted caller relays the message for: kind (`person` or `agent`), name, run, and member (section 5.5); `pinned` for a message that every compaction keeps (section 5.4) |
| `fw.turn.started` | turn number |
| `fw.model.requested` | provider, model, effort (`default` when the provider chooses it, section 6.1), estimated input tokens, and `report` for the final report call at a limit (section 5.2). `systemNotes` names the sentences of the kernel at the end of the system prompt, in their order. `context` stands for `fw.ContextNote` (section 7.8.2), and `message` for `fw.MessageNote` (section 5.5). It holds the names only, never the text of the prompt. A hosted harness records no `systemNotes`. |
| `fw.model.responded` | content blocks, raw provider blocks, stop reason, usage, latency |
| `fw.tool.requested` | tool, call id, input, idempotent classification at invocation, and `routine` for a call of the routine runner (section 22.3) |
| `fw.tool.responded` | call id, output, `is_error`, latency |
| `fw.tool.progress` | call id, progress, total, message |
| `fw.file.changed` | call id, tool, operation, path, old path, size, SHA-256 (section 7.9) |
| `fw.run.child` | child, member, `started`, and for a child whose start is open its kind (`spawn` or `branch`) and its facts: person, metadata, profile name, input, idempotency key, and the runs of the tree that it reserved; for a start that ended without a child for its caller, the outcome and the reason; and why a look kept a child that can still run (sections 4.7 and 11.9) |
| `fw.child.reported` | child, member, state, the usage of the tree of the child, its open runs and its working runs, and `lost` when the parent no longer reaches it: what a child in another process last reported (section 11.9) |
| `fw.approval.requested` | approval id, tool, call id, input, reason, form schema of a question, connect request (section 7.11), wait (`approval`, `question`, `connect`, `rerun`, section 25) |
| `fw.approval.decided` | approval id, decision, decider (the subject of the caller, or the person that a trusted relay names, section 8.2), `deciderKind`, the kind of the decider (section 10.2), note |
| `fw.context.compacted` | epoch, summary block, kept messages, usage of the summary calls |
| `fw.budget.exceeded` | limit name, value, the limit, for a limit of a tree the scope `tree` and the run whose budget holds it, and `report` when the final report call follows (section 5.2) |
| `fw.budget.changed` | the fields of the budget that the change sets, the subject of the person who changed it, `byKind`, the kind of that person (section 10.2), origin (section 5.2) |
| `fw.loop.detected` | tool, count, period, action (`warned`, `failed`) |
| `fw.error` | message, retryable |
| `fw.turn.finished` | turn number |
| `fw.run.suspended` | reason, stop (`interrupted`, section 25), totals (the usage of the run so far, section 8.2) |
| `fw.run.resumed` | from seq |
| `fw.run.finished` | outcome (`completed`, `failed`, `canceled`), output, error, stop (why the run stopped, section 25), done (section 24), totals (the usage of the run so far, section 8.2). A cancel adds `by`, who canceled the run (section 8.2). |
| `fw.routine.decided` | the decision on one routine at the start of a run: routine, version, standing, and mode (`shadow`, `live`, or `canary`). Then `applies`, the reason, the answers of the decider, the decider, the model, the latency, and `risk`, `read` or `write` (sections 22.3 and 7.8.4) |
| `fw.routine.applied` | a live routine that ran: routine, version, `ok`, the steps that ran, the failed step, the reason, the answer of a routine that completed, and `done`, the checks of done on its work (section 22.3) |
| `fw.routine.compared` | the end of a run that the model completed: routine, version, mode, question, what the routine said, what the agent did, and `agree`. The comparison of the calls has `calls: true` and no question, and for a step that differs, `step` and `reason` (section 22.3) |
| `fw.outcome.evaluated` | verdict (`satisfied`, `needs_revision`, `max_iterations`, `unparsed`, `skipped`), notes, iteration, iterations, the grader run, whether the loop continues, reason (section 20) |
| `fw.harness.event` | kind and a compact payload of one event of a hosted harness, with the markers of its session (section 19) |
| `fw.env.ready`, `fw.env.failed`, `fw.env.changed` | the state of the environment of the durable volume: the digest, the versions, the time, or the failed step and its output. These belong to the agent, not to a run, so they go to the bus subject `fw.environment` and to the log, not to a ledger (section 18) |

The raw provider blocks in `fw.model.responded` are stored unchanged. They include thinking blocks. The kernel sends them back to the same provider on the next turn, because the provider requires the blocks it produced.

Before the kernel appends an event, it replaces each secret in the data with `[redacted]`. A secret is one of these:

- a value that `fw` resolved through `Secrets`, such as a model API key or `FW_API_TOKEN`
- the value of a variable with a secret name, such as `OPENAI_API_KEY`, `GITHUB_TOKEN`, or `DB_PASSWORD`
- text with the shape of a secret: a PEM private key, a provider or GitHub token, an AWS access key, a JWT, a bearer token, or the user information of a URL. A bearer token follows the word `Bearer` on the same line, after spaces or tabs, and has a digit or capital and small letters, unless `Authorization:` comes before `Bearer`. A key or a word after `Bearer`, such as `credentials_file` on the next line of a Prometheus block, stays.

The model builds its history from the stored events, so the model does not see the secret either. A JSON escape in a string, such as a tab between `Bearer` and a token, does not hide a secret: the kernel reads each string as it decodes. An event in which no string holds a secret keeps its bytes, so the input of a tool call and its result reach the tool and the model as they were. The raw provider blocks stay unchanged, because the provider signs them. The bytes of an image or a document also stay unchanged. A value of fewer than eight characters is not replaced. `a.Redact` applies the same rules to the text of a log line.

### 4.3 Storage

The default ledger driver writes one file per run at `$FW_HOME/runs/<run>/ledger.jsonl`. JSONL is one JSON object per line. It calls `fsync` before acknowledging each of these events:

- `fw.run.queued`, for every origin, including HTTP and the command line
- external `fw.message.received`, `fw.budget.changed`, `fw.run.annotated`, and `fw.run.reacted`
- every approval event, every terminal event, and every `fw.run.child`
- every `fw.tool.requested` that does not explicitly record `idempotent: true`

Known internal kernel notes, model events, and other batched events become durable at the next sync boundary or `Flush`. The tool intent append must succeed before the tool body runs, and the record of a child before its spawner creates a process (section 11.9). An index file at `$FW_HOME/runs/index.jsonl` lists each run id once; the run state comes from its ledger. An append error can leave a complete event on disk, so failure does not prove that the event is absent.

Durability includes directory entries: opening the ledger syncs its directory chain, and a run's first durable event syncs the ledger file's entry and the run directory's entry. The index is also synced before its run can execute. A failed sync returns an error and retains outstanding entry-sync work for a retry; an existing path alone does not prove durability. Incomplete trailing records are removed before appending to a run file or the index. Completed records are never rewritten. These guarantees rely on filesystem `fsync`; they do not make external effects exactly-once or preserve writes excluded from a Substrate snapshot.

When `/workspace` exists, `$FW_HOME` defaults to `/workspace/.fw`. Otherwise it defaults to `./.fw`. Under ax, `/workspace` is the durable volume that survives suspend and resume. The implemented ledger drivers are `file` and `mem`. NATS and Redis are bus drivers, not ledger drivers. A custom ledger implements `fw.Ledger`; a distributed ledger also needs an ownership and fencing protocol before several processes can drive its runs.

`OpenFileLedger` takes exclusive writer ownership of the root directory until `Close`. A second writer, in this process or another process, gets `ErrLedgerLocked`; symlink aliases contend on the same directory inode. The OS releases the lock if the owner exits. Linux, macOS, and the supported BSD platforms use `flock`; other platforms refuse writable opens. This contract requires a filesystem that honors the lock and `fsync` and a stable root directory. It is not a distributed lease or fencing service.

`Flush` syncs outstanding writes and keeps ownership. `Close` flushes, ends watches, permanently closes the handle, and releases ownership even when the flush fails. Callers that reused a handle after `Close` must use `Flush` instead. `OpenFileLedgerReadOnly` opens an existing root without a writer lock and never repairs or creates files. It reads complete lines, and polling `Read` sees later appends; it provides no cross-run snapshot or cross-process watch. `fw events` and an explicit replay source use this mode. A replay still needs exclusive ownership of its destination ledger, so replay a live server into a different home.

### 4.4 Projections

A projection is an `Observer`. An observer receives each event once, in order, after the ledger stored it. The kernel ships these observers:

- `messages` builds the message history that the next model call sends.
- `status` builds the run object that every surface returns.
- `metrics` updates Prometheus counters and histograms.
- `bus` publishes selected events to a queue subject.
- `a2a` translates events into A2A task updates.
- `trace` opens and closes OpenTelemetry spans.

A projection holds no state that the ledger does not hold. When the process restarts, each projection rebuilds from the ledger.

### 4.5 Resume

On start, `fw` reads the index and finds every run whose state is not terminal. The state, and not the last event, decides, so a finished run with a late annotation stays finished (section 8.2). For each such run, it rebuilds the message history and appends `fw.run.resumed`. Then it continues from the last complete point:

1. If the last event is `fw.model.requested`, send the request again. This holds for the final report call at a limit too (section 5.2).
2. If the last event is `fw.tool.requested` and the call is safe to run again, run the tool again. A call is safe when the tool is idempotent, or when the tool implements `fw.CallIdempotency` and its `IdempotentCall` says so for that input.
3. If the last event is `fw.tool.requested` and the call is not safe to run again, append `fw.approval.requested` and wait.
4. If the last event is `fw.approval.requested`, wait for the decision.

A call that waited for a connection is an exception to step 3. The service never saw such a call, so the call runs again (section 7.11).

A tool that implements `fw.Continuer` is another exception to step 3 when every recorded question of the interrupted call has an answer. Its `Continue` method constructs the result from those questions and decisions, without rerunning the call or asking again. The built-in `ask_user` uses this contract, including when the answer arrived before the process stopped. An unanswered question stays idle. Other non-idempotent tools still need approval to rerun; a read-only flag or a recorded question alone does not make a tool safe to continue. This contract does not restore MCP tasks or child-run waits.

The built-in `memory` tool decides per command through `fw.CallIdempotency`. `view`, `create`, `delete`, and `rename` are safe to run again, so an interrupted call of one of them runs again without an approval. `insert` and `str_replace` change the file again when repeated, so an interrupted call of one of them needs a recovery approval. Denial leaves the existing edit alone. Approval authorizes a real rerun and can repeat an edit that already took effect.

The approved rerun of a `spawn_task` call takes the child of the first attempt instead of starting a second one, and a child in another process whose start was open is looked for at its member (section 11.9).

### 4.6 Replay

The command `fw replay <ledger> --model <selector>` runs a recorded run again against a different model. It answers every tool call from the recorded `fw.tool.responded` events instead of running the tool. It uses the brief and the context of the recorded run (section 7.8), so the replay sees the memory index that the recorded run saw. The replay is a new run, so the kernel resolves its profile again (section 12.5): the named profile of the replaying process with the inline profile of the recorded run on top. A change of the prompt in the configuration therefore shows in the replay. The result is a second ledger. A diff of the two ledgers is a regression test for a model change or a prompt change. This covers the common evaluation case with no extra framework.

A recorded result matches the tool name and its JSON input. Object key order and whitespace do not matter; number literals keep their precision and spelling, so `1` and `1.0` are distinct. Repeated identical calls consume recorded results in ledger order. Replay serves calls serially in model-response order, including tools that are normally parallel-safe. A different input, an exhausted result queue, or a result without a recorded input produces an explicit error result; it never falls back to the live tool. This is argument-aware fixture replay, not a guarantee of identical model output or a pinned execution environment. The replay overrides are process-local and do not survive a restart.

### 4.7 Branches

A run is defined by its ledger and its workspace, so a run can branch. `Agent.Fork` and `POST /v1/runs/{id}/fork` make a new run whose ledger begins with a copy of the events of the source up to one sequence number, `seq`; without one, every event. The copied events get the id of the branch, new event ids, and the time of the copy; the `fw.run.queued` event of the branch names the person who branched as its principal and carries the metadata `fw.forkOf`, the id of the source, and `fw.forkSeq`, the sequence number kept. A trusted relay that branches for a person names that person (section 8.2). The branch then belongs to that person, with the kind `user`. The metadata of the source, with the annotations that the branch keeps, and the metadata of the request must fit the bounds of `fw.CheckMetadata` together; otherwise the kernel refuses the branch, here and in a process that takes it through `POST /v1/runs/fork`. The branch starts from `fw.run.queued` as the ledger of the source stores it, with every secret replaced, so its model reads what the model of the source read, and a branch in another process holds no secret that only the source knows. The branch has no parent, and the children of the source stay with the source. The source does not change.

A branch counts its own work only. Its `fw.run.queued` holds `kept`, the number of events at the start of its ledger that are copies, which leaves out the `fw.run.child` events of the source. The copies add nothing to the usage of the branch, and their totals set nothing (section 8.2), because the source holds that usage. A branch that its copies end, such as a branch of a finished run without a message, did no work: a copy after which the branch is finished, such as the copied `fw.run.finished`, gives it a known usage of zero, so a consumer of the lifecycle events reads it as final (`usageSeq`), even when the last copy is an event that the stream does not carry. The budget of the branch counts its own turns, tool calls, tokens, and cost in the same way, so a branch of a run near a limit gets the whole budget for its own work. A turn that is open at the end of the copies, as in a branch at a `fw.model.requested`, is a turn of the branch, because the branch makes its model call. The turn numbers in the ledger of the branch go on from the source. Only a fork sets `kept`: the process that writes the branch sets it to the number of the copies, also for a branch that `POST /v1/runs/fork` brings from another process, so a `kept` in that body counts for nothing, and the metadata `fw.forkOf` and `fw.forkSeq`, which a request can set and a replay copies, change no count. A branch that an older kernel made has no `kept` and counts the copies as its own.

The copy of the kept events is the acceptance of a branch in this process: a caller that receives the branch holds one whose events are in the ledger, and a retry of `POST /v1/runs/fork` under the same id waits for the first copy and returns its branch or its failure. A branch is new work, so it takes its place in the backlog of section 5.6 before its first append and holds that place through the copy. A full backlog therefore appends nothing of the branch, and a copied `fw.run.finished` does not give the place to another start in the middle of the copy, so the ledger never holds a part of a branch because of a full backlog. A branch in another process checks the drain and the storage of its source before `Place`: during a shutdown, `Fork` returns `fw.ErrClosed`, and after a storage failure `fw.ErrStorageUnavailable`, and no process exists for the branch. A branch in another process starts only after the source recorded it: if that record fails, the source returns the error, sends nothing, and abandons the process that it placed before `Fork` returns, for at most 30 seconds, so a process that exits after `Fork` leaves no empty child Task without a record.

A branch goes on as a resumed run does (section 4.5). A model request without an answer is sent again, a tool call without a result follows the resume rules, and a pending approval waits for its decision, under the approval id that the branch copied. A message in the request, `input`, follows the kept events as a follow-up from the person, so a branch of a finished run goes on in another direction; without a message, a branch that keeps the end of a finished run stays finished. The branch keeps the resolved profile of its source (section 12.5), because it copies the `fw.run.queued` event of the source, so a change of the profile after the start of the source does not reach the branch. A branch in this process also keeps the text of the profile of its source; a branch in another process takes the text of the profile there and records a change of it as a restart does (section 12.5). A `model` in the request changes the model of the branch: it replaces the model in the resolved profile and in the inline profile. Because the system prompt, the history, and the tool results of the kept events are the same bytes, the branch keeps the prompt cache prefix of the provider up to the point of the branch.

A branch in this process shares the workspace of its process, and with `tools.files.perRun` it works in the directory of its source (section 7.9). The source and its branches in the same process therefore see each other's changes. With `spawn` in the request, the branch runs in another process: the `Spawner` of the agent must be a `Forker`. A `Forker` works in two steps, because the other process takes a while to run and the caller does not wait for it. `Place` creates the process, a child Task with the `ax` driver, and names it at once. The kernel then gives the branch its id, records it in the source as a child in `fw.run.child` with that member, and answers the caller with the branch as queued, with `Member` set; the route answers 202. `Branch` runs in the background: once the process runs, it sends the kept events through `POST /v1/runs/fork`, a route for the static token or a workload alone, because the events name the person of the branch, and the process takes the branch under the id that the body names. Until then the source answers `Get` for the branch, `WaitFor` waits for the start, `Decide` has nothing to decide, and `Cancel` ends the start. The status of the source is not idle while a start is open (`Agent.Starting`), so the suspend controller of the workbench does not put it to sleep before its child has the branch. The source records the branch twice in `fw.run.child`: when it places the branch, with the person and the metadata of the branch, and again once the process took it, with `started`. If the source process goes away during the start, nothing sends the branch, so at its next start the source records a placed branch without a start as a failed run with the reason, from the facts in the event, and the `Forker` abandons the process it placed: the `ax` driver deletes the empty child Task. The start of the kernel does both before it goes on, the abandon first, so a restart between the two abandons again. A failed abandon is tried again, five times in all, with a wait that starts at one second and doubles, for at most 30 seconds, because under ax a Task boots before its egress policy takes effect. A process that the kernel still cannot remove is named in the reason of the record. A shutdown of the source ends every open start the same way, as canceled. A branch is therefore never lost without a trace, and nothing of it stays behind in the fleet. A branch that its process does not take, because the Task never runs or refuses the events, and a branch whose start was canceled are recorded in the source process under the id of the branch, as a failed run with the reason or as a canceled run, so nothing is lost without a trace; after a restart that record wins over the `fw.run.child` event. The `ax` driver deletes a child Task that did not take its branch. The branch has the workspace of its own Task: a fresh copy of the Workspace of the parent Task, not the files that the parent changed. Several branches of one run in several Tasks therefore try different prompts or models from the same base, and their `fw.file.changed` events and their outputs compare.

## 5. The kernel

### 5.1 Run and turn

A run is one conversation with one agent profile, one budget, and one ledger. The kernel resolves the profile and the budget once, when it queues the run, and the run keeps them for its life, as section 12.5 describes. A turn is one model call and the tool calls that it requests. The loop for a run is:

1. Build the request from the profile system prompt, the tool specifications, and the message history.
2. Call the model with streaming. Append `fw.model.requested` and `fw.model.responded`.
3. If the stop reason is `end_turn`, append `fw.run.finished` with the final text and stop.
4. If the stop reason is `tool_use`, ask the policy about each call (section 7.5).
5. Run the allowed calls. Run parallel-safe calls at the same time. Append one `fw.tool.responded` per call.
6. Append all tool results in one user message. Go to step 1.

Other stop reasons end the turn as follows. `max_tokens` appends `fw.error` and retries once with a continuation message. `refusal` appends `fw.run.finished` with outcome `failed` and the refusal category. `pause_turn` goes to step 1 at once.

A stream that the network cuts in step 2 has no answer. An adapter marks such a cut with `fw.StreamInterruptedError`: the body ended before a finish reason, in the middle of an event, or with a reset. The kernel then appends `fw.error` with `retryable` set, waits half a second, and calls the model once more with the same request, in the same turn and the same run. It records nothing of the cut response and runs none of its tool calls, even a tool call that arrived whole. A second cut fails the run. The deadline of the budget and a cancel bound both calls and the wait. A suspend of an ax Task cuts the network of the process before it freezes it, so the call meets the cut first and the retry carries the run over the suspend. Any other model error fails the run as before.

The text of an answer also goes out live while the model produces it. The adapters stream from the provider in step 2, and the turn loop hands each text piece of the answer and each tool call to a small fan-out of the run, which `GET /v1/runs/{id}/live` serves (section 8.2). The thinking of a provider never goes out, only the text of the answer. A run takes at most 16 live clients. The fan-out joins the pieces to at most one event per 40 ms, and it drops a client that falls 64 events behind. Nothing of it enters the ledger, which records the answer whole in `fw.model.responded`, so a client that connects in the middle of a call gets the pieces from then on and no replay. A run that nobody watches pays one atomic load per piece.

The execution cursor consumes new ledger events incrementally. A cold load reconstructs it once; later steps decode only events after its last boundary. Driver snapshots detach mutable call and report records, so a concurrent budget check can advance the cursor safely. The repeat guard keeps exact counts for periods of one to three calls in constant space. The ledger remains authoritative and retains all events. Context compaction bounds the model history, not the ledger; startup still reads historical runs, and an open run still holds its event history.

### 5.2 Budget

A budget has seven limits. Four bound the size of a run, and three bound its shape.

| Limit | Default | Holds for | What it stops |
|---|---|---|---|
| `maxTurns` | 200 | the run | A run that calls the model again and again |
| `maxTokens` | 2,000,000 | the tree | A run, with its child runs, that reads more than the context can hold |
| `maxCostUSD` | 0 (off) | the tree | Spend above a price. It needs a price table. |
| `deadline` | 2h | the lineage | A run that works too long. It counts working time, not the time that the run waits (section 5.8). |
| `maxToolCalls` | 1000 | the tree | A run, with its child runs, that calls tools without end |
| `maxCallsPerTurn` | 32 | the run | One response that fans out into too many calls |
| `maxRepeats` | 5 | the run | The same call, or the same cycle of calls, again and again (section 5.7) |

The kernel compares usage with the budget before every model call. When a limit is reached, it appends `fw.budget.exceeded`. At a limit of turns, tool calls, or working time, the run then makes its final report (below) and finishes with outcome `failed`. At every other limit, it finishes with outcome `failed` at once. A call past `maxCallsPerTurn` or `maxToolCalls` does not run. The model reads an error result that names the limit, and the run ends at the next model call. Cost needs a price table. The price table is a plug with an empty default, so `maxCostUSD` is off until you supply prices.

Three more fields of a budget shape the final report. They are not limits.

| Field | Default | What it does |
|---|---|---|
| `finalReport` | `true` | At a limit of turns, tool calls, or working time, one more model call writes the report that becomes the output of the run. `false` stops the run at once. |
| `reportMaxTokens` | 1,024 | The output cap of the report call. Zero takes the default. |
| `reportTimeout` | 2m | The time limit of the report call. Zero takes the default. |

A run takes its budget from the request, then from the profile, then from `budget:` in `fw.yaml`. The kernel takes the budget of the profile and of `fw.yaml` when it queues the run and records them in the resolved profile (section 12.5), so a later change of either reaches new runs only. A change of the budget of the run comes on top (below). A zero field means no limit for that field, and the kernel adds no default for a field that is zero. The fields of the final report are the exception: zero takes their bounded default, and a missing `finalReport` means off, as in a ledger of an older kernel. A branch (section 4.7) is charged for its own work only, not for the events that it kept from its source.

#### Budget notes

A model does not see its budget. Before a model call, the kernel looks at how much of each limit the run used: the turn of the call against `maxTurns`, the tool calls, the tokens, and the cost of the tightest tree of the lineage against `maxToolCalls`, `maxTokens`, and `maxCostUSD`, and the working time against the nearest deadline of the lineage (section 5.8). When the use of a limit passes 75%, the kernel adds a budget note. The note names the limit, the use, and what is left, and asks the model to plan the rest of the work to fit. When the use passes 90%, a second note asks the model to wrap up: finish the step that it is on, and end its turn with a report of what it did, what is left, and where its work is. At 90%, a note about a limit of tokens or cost says that the run ends at once at the limit; a note about another limit says that the kernel asks for the report in one last call. A note looks like this:

```
Budget note: this is model turn 180 of the 200 that the budget of this run allows (budget.maxTurns, 90% used). 20 turns are left after this one. Wrap up now: finish the step that you are on, then end your turn with a report of what you did, what is left, and where your work is (files, branches). If the run reaches the limit first, the kernel asks you for that report in one last call without tools.
```

A note is a `fw.message.received` event with the role `system`, the origin `kernel`, the note kind `budget`, and the field `budget`: the limit, the threshold, the use, the limit value, and the run that holds the limit when that is not this run. The model reads it at its next call, as it reads the loop note (section 5.7). Each note comes once for each limit, limit value, and threshold. A use that jumps past 90% gets the 90% note alone. A resume and a branch read the notes from the ledger and give none twice. A larger limit is a new limit value, so it gets notes of its own. The first model call of a run gets no note, as for the time notes (section 5.8).

#### The final report

A run that stops at a limit without a word leaves the person with an empty output. So at a limit of turns (`maxTurns`), of tool calls (`maxToolCalls`), or of working time (`deadline`), the kernel makes one more model call before the run ends. The call has the prefix of a turn: the system prompt, the tools, and the effort, so it reads the prompt cache of the run. The history ends with an instruction from the kernel, a system message with the note kind `report`: report what the run did, what is left, and where the work is (files, branches), with text only. The kernel runs no tool call of the answer. The text of the answer becomes the output of the run, which still finishes as `failed` with the budget error, such as `budget exceeded: deadline`. The call is not a turn: its `fw.model.requested` carries `report`, and the count of turns does not grow. Its tokens and its cost count in the usage of the run.

The report call is one bounded call past a limit of turns, tool calls, or working time, and never past a limit of tokens or cost. `budget.reportMaxTokens` caps its output, and `budget.reportTimeout` bounds its time. A call that fails or takes too long appends `fw.error`, and the run finishes with no output. A limit of tokens or cost (`maxTokens`, `maxCostUSD`) gets no report call, because that call spends past a limit of money: the 90% note asks the model to finish instead, and the run stops at once. The kernel also makes the report call only when its most, the whole context as input and the output cap, fits what the run and the trees of its lineage have left of `maxTokens` and `maxCostUSD`; otherwise the run stops at once and the log says why. `budget.finalReport: false` turns the call off. A run that stops at a limit that an ancestor holds for its tree stops at once, and the ancestor reports for the tree at its own next step. A run makes one report for each budget: a follow-up without a change of the budget fails at once again, with the earlier report as its output. A child makes no report where its parent makes none: the budget of the parent caps `finalReport`, `reportMaxTokens`, and `reportTimeout` as it caps a limit, and a child in another process gets `finalReport: false` when its parent turns the report off.

A follow-up or a change of the budget that arrives while the report call is in flight keeps the run open after the answer, as at the end of a turn (section 5.5). The next step checks the budget again: a larger budget lets the run go on with the report in its history, and a run that stays over its limit stops with the report as its output.

A run with a definition of done (section 24) runs its checks before it finishes at a limit of turns, tool calls, tokens, or cost: after the final report when it makes one, else at once. The checks call no model, so they cost no tokens, and they have a tool budget of their own. A run that stopped at a limit may still have done its work, such as a run whose last tool call did it. The pass, `fw.done.checked` with `limit` set and `answer` 0, gives the run its state of done: `fw.run.finished` carries `done` and `doneWeak` as at the end of a turn. The run still finishes as `failed` with the budget error and the stop of the limit, such as `budget:maxToolCalls`. A pass that failed gives the model no other turn. At the deadline no check runs, because the deadline is the limit of a person's time. A run that stops at a limit that an ancestor holds for its tree runs none either, because the tree must stop now.

#### The deadline during a model call

The kernel checks the budget before a model call, so a slow call can run long past the deadline. A model call therefore carries a timer for the working time that is left before the nearest deadline of the lineage. When the timer ends the call, the call has no answer, so the kernel appends `fw.error` and stops the run at its deadline, with the final report, which has its own time limit. An answer that arrives before the timer is recorded as any other, so the kernel records an answered call once and drops none. The tool calls of a response that arrives past the deadline do not run: each gets an error result that names the deadline, and the run stops at its next step. The timer runs on the clock of the process, and the deadline counts working time from the ledger (section 5.8). A tool call that runs past the deadline still ends only at its own limit (section 7.7).

#### A change of the budget

A run keeps the budget that it was queued with until a person changes it. `Agent.ChangeBudget` and `PATCH /v1/runs/{id}/budget` change the budget of a run. `Agent.Send` with `fw.WithSendBudget`, or the field `budget` of `POST /v1/runs/{id}/messages`, changes it together with a follow-up, before the message, so the turn that the message starts has the new limits. Each field that the change sets replaces the same field of the budget of the run for the work that follows; a zero field keeps the field as it was. The kernel records each change as `fw.budget.changed` with the fields, the subject of the person, and the origin. A trusted relay names that person (section 8.2). The budget of the run is the budget that it was queued with, with every change on top, in order, after a restart and in a branch too. A change needs the operator role on the run, as a decision does (section 13.2). A change of a finished run leaves it finished, and a follow-up starts it with the new budget.

Every limit counts the whole run. The turns, the tool calls, the tokens, the cost, and the working time before the change count against the new limits. A run that failed after 45 minutes of work at a deadline of 45 minutes therefore gets 30 more minutes from `deadline: 75m`, not 75. A running run reads the new budget at its next check. Its next model call carries a budget note that lists the limits now and the use of each, so a model that a 90% note told to wrap up learns that it can go on. The budget of the parent of a child still caps the child. The parent of a child in another process refuses a change of the child with a 400 that names the process; change the budget in that process.

#### The tree of a run

A run can start child runs (section 11.9), and those can start more. A run and every run under it are the tree of the run. The limits hold in three ways:

- `maxTokens`, `maxCostUSD`, and `maxToolCalls` hold for the tree. The tree of a run is its own usage and the usage of every run under it, at every depth, in this process and in other processes. Each run bounds its own tree with its own budget, so the budget of the root bounds the whole tree, and the budget of each run in it bounds its part. The tokens are the input and output tokens, as for a single run.
- `maxTurns`, `maxCallsPerTurn`, and `maxRepeats` hold for each run alone. They describe the shape of one conversation, not its spend.
- The `deadline` counts the working time of the run and of each ancestor (section 5.8). A child stops when it or any ancestor uses up its working time.

The children of a run share what it has left. No child gets a grant up front, so children that work at the same time all draw on one pool, and the first one does not starve the others. Before each model call and before the tool calls of a response, the kernel adds up the tree of the run and the tree of each ancestor, and compares each with the budget of that run, as it walks the ancestors for the deadline. A child in this process counts as it is now. A child in another process counts as it last reported its own tree (section 11.9). A child also gets the budget of its parent as a cap on its own limits, so a child alone never outgrows its parent. A response whose run, or whose tree, used up its tokens or its cost runs none of its tool calls: each call gets an error result that names the limit, and the run stops at its next step. The tool calls of a tree count the same way: a call past the room of the tightest tree does not run, and the model reads which run holds the limit.

When the tree of a run reaches a limit, every run in it stops. The run that sees it first, the run itself or a run under it, appends `fw.budget.exceeded` with the limit, the usage, the limit value, the scope `tree`, and the run whose budget holds the limit, and finishes as `failed` with an error such as `budget exceeded: maxTokens of the run tree of run 01J… (it and the runs under it used 1012 tokens of 1000 tokens)`. The kernel then cancels every open run under that run, at any depth and also below a run that finished, in this process and in other processes, with the reason, so a child that waits in a long tool call or for a person stops too. A branch is a run of its own and stays (section 4.7). The runs of this process stop at once. The children in other processes are canceled in the background, so neither the driver of a run nor a caller of `Cancel` waits for another process, and a shutdown ends the cancels in flight. After each such cancel, the kernel reads the child once, so the parent records that the child ended. A cancel that the shutdown cut, or that a crash or a kill lost, is sent again when the process starts: the cancel of a run is in its ledger already, so the start sends the cancels again to the children in other processes under each canceled run whose latest record shows them open, and records each end. A cancel of a run that had finished appends nothing to it, so a cut cancel of such a run reaches no child after a restart; its children stay within their caps, and a stop of a spent tree reaches them when a later check finds it spent. The run itself stops at its next step, when a tool call that waits for a child returns. The stop lasts for the life of the process: each later check or read that finds the tree spent cancels again what is still open, such as a child whose start ended after the stop, a child that a look found after a restart (section 11.9), or a child that a person started again. A cancel of a child in another process that fails is tried again, first after a tenth of a second and then with a wait that doubles up to a minute, until it reaches the child or the child counts as lost (section 11.9). A child that refuses its cancels but answers its reads stays reachable: the kernel goes on reading it, counting its spend, and trying its cancel. A child that would start in a tree that has nothing left of a limit is refused (`fw.ErrTreeBudget`), and `spawn_task` tells the model which limit ran out. A run that reached a limit of its own alone gets the message of a single run, `budget exceeded: maxTokens`, as before.

The check is live, not a reservation, so a tree can pass a limit by a bounded amount. Every run that works at the same time can pass the check and then make one more model call, and a response can hold up to `maxCallsPerTurn` tool calls. The final report adds no tool call and passes no limit of tokens or cost, because it runs only when its most fits what is left. A child in another process counts only from its latest read, so the spend of such a child between two reads is not seen at once. A compaction (section 5.4) is a model call of its own, so the kernel checks the budget again after a compaction and before the call of the turn. The worst case past a limit is therefore one model call, or one summary call of a compaction, and the tool calls of one response, of each run of the tree that works at the same time, plus what the children in other processes spent since the latest read of each. In this process, `spawn.maxRunning` (default 8) bounds the child runs that work at the same time. A child in another process never spends more than the tree had left when it started, because that is its cap (section 11.9).

### 5.3 Approvals

A tool call that the policy marks `Ask` moves the run to the state `input-required`. The kernel appends `fw.approval.requested`. Every surface shows the pending approval, and every surface accepts a decision. A decision appends `fw.approval.decided`, and the run continues. The event names the decider: the subject of the caller, or the person that a trusted relay names (section 8.2). Approvals survive a restart and a suspend because they are ledger events.

### 5.4 Context management

Long runs outgrow the context window of the model. The `Compactor` interface handles this, and one path serves every provider: the default compactor asks the model of the run for a summary. No adapter uses the compaction of a provider, such as the server-side compaction of Anthropic. Such a compaction fits behind the same interface as a plug of the future.

Before each model call, the kernel estimates the size of the context. It starts from the usage that the provider reported for the latest response of the epoch: input, cache read, cache write, and output tokens. It adds an estimate for the messages after that response, about one token per four bytes of text and 1,600 tokens per image. Without a reported usage, the estimate covers the system prompt, the tool specifications, and the whole history.

When the estimate reaches `compaction.at`, the kernel compacts before the call. The default is 262,144 tokens.

A profile can set its own block `compaction` with three fields, so one agent with a small model compacts early and another one never compacts:

| Field | Meaning |
|---|---|
| `at` | The size in estimated tokens that starts a compaction for the runs of the profile. 0 takes `compaction.at` of the configuration. A negative value is invalid. |
| `model` | A selector or a name from `models` for the summary call, resolved as the grader of an outcome (section 20.1). Empty takes `compaction.model`. |
| `off` | `true` turns compaction off for the runs of the profile, before a call and after an overflow. It does not go together with `at`. |

The block `compaction` of the configuration takes `off` too, and there `at: 0` turns compaction off as well. The inline profile of a request overlays the block as it overlays a budget: a set field replaces. The kernel resolves the setting when it queues the run and records it in the resolved profile of `fw.run.queued` (section 12.5): the size, `off`, and the model as a selector. A run therefore keeps its compaction setting for its life, and a change of the profile or of the configuration reaches new runs only. A run from the ledger of an older kernel reads the block of the configuration. `Agent.CheckProfile` and the loader of the configuration refuse a bad value and name the key, such as `compaction.at`.

A model call can also fail because the request does not fit the context window. Then the kernel compacts and calls the model once more in the same turn. A second overflow in the same turn fails the run. The kernel recognizes an overflow from `fw.ErrContextOverflow`, or from the error text of Anthropic, OpenAI, Gemini, vLLM, llama.cpp, and tabbyAPI. A local server with a small window therefore needs no setting. `compaction.at: 0`, `compaction.off: true`, and `off: true` in the block of a profile turn compaction off in both cases.

The default compactor uses simple compaction. It sends the history of the run to the model with the same system prompt, tools, and effort as a turn. It adds one user message. That message asks for a summary inside `<summary>` tags and forbids tool calls. The prefix of the call is the prefix of a turn. The call therefore reads the prompt cache, and the thinking blocks in the history stay valid.

The summary replaces the whole history. The new epoch starts with one user message that holds the summary. Each pinned message of the run follows it (section 5.5), in ledger order and verbatim, then the messages from the caller that the compactor kept, then the messages that arrived during the summary call. A pinned message is never summarized away, and each compaction keeps it once: a pinned message that the compactor also kept from the caller tail moves to the pinned messages and is not kept twice. A pinned message that arrives during the summary call follows as any message that arrives then. The kernel replays no earlier response, no tool call, and no thinking block. The reason is that a provider binds a thinking block to the history before it. A kept tail of old turns therefore fails after a summary. Anthropic rejects such a request with a 400 for accounts created on or after 2026-08-31. The messages from the caller at the end of the history, such as a new question, carry no provider data. They follow the summary word for word.

When the summary call itself does not fit, the compactor summarizes the history up to the last assistant message. That part was the history of an earlier call that fit. The compactor appends the remaining messages after the summary as plain text, and cuts each block to 4,000 characters. `compaction.model`, or `model` of the block of the profile, sends the summary call to another model, which does not read the prompt cache of the run. A message that arrives during the summary call follows the summary in the new epoch.

Compaction appends `fw.context.compacted` with a new epoch number, the summary block, the messages that the compactor kept, the pinned messages among them, and the token usage of the summary calls. The usage, the cost, and the budget of the run count that usage. The `messages` projection starts from the latest epoch. The system prompt is not part of the history, so the profile prompt and the skills list survive every compaction. The data that tools add to the first user message, such as the memory index, comes back at the start of the first message after the summary (section 7.8.2). The ledger before the epoch is unchanged, so rule 5 holds.

### 5.5 Follow-up messages

A run accepts more messages after it finishes or while it waits for input. A follow-up appends `fw.message.received` and starts a new turn on the same run. The mailbox serializes follow-ups, so a run never has two writers. A follow-up can carry a budget, which changes the budget of the run before the message (section 5.2), so a person continues a run that failed at a limit with more room. A follow-up without one to a run that is still over a limit fails again at once.

A follow-up that arrives while the model answers enters the history after that answer. A follow-up that arrives while tool calls run follows their results. So the next request ends with the message, also after a restart, because the history is a function of the ledger. A call that never answers leaves the message for the next request, at the end. A provider takes a request that ends with an answer of the model as a prefill. Newer models, such as Claude 5.5, refuse a prefill with a 400. The kernel sends such a request only to continue a paused turn (`pause_turn`). For any other request that ends with an answer, the kernel fails the run with that reason before the call.

A follow-up can be pinned, with `fw.WithMessagePinned` or the field `pinned` of the route. A pinned message survives every compaction: the new epoch holds it verbatim after the summary (section 5.4). An operator hands a skill or a standing instruction into a running conversation this way, and it does not vanish at the next compaction. The pinned messages of a run hold at most 64 KB together, counting their text, their inline data, and their files. A pinned message past the bound fails with `ErrInvalidRequest` and a text that names the bound. The event line of such a message starts with `pinned`.

Each follow-up records its sender. The field `by` holds the subject of the principal that sent the message, as `fw.budget.changed` records it. A trusted relay that sends the message of a person names that person in `by` (section 8.2). It is empty for a message of the kernel and for the process itself. A trusted caller relays a message for another party with `fw.WithMessageFrom`, and the field `from` names that party: `kind`, which is `person` or `agent`, `name`, `run`, the run of an agent, and `member`, the seat of the party in the work that relays the message, such as a member of a channel. A trusted caller is the process itself, the static token, or a workload. Any other principal that names a party gets `ErrForbidden`, so a person cannot pose as another person. Each field holds at most 256 bytes and no control character. The kernel shows the kind and the name and reads nothing else. The front of a fleet relays the messages of one agent to another this way.

A message from an agent has the role user. The model reads it inside a block that names the agent. The kernel escapes the attributes, and it escapes a closing tag inside the text, as it does for the memory index (section 7.8.2), so the text cannot end its block early:

```
<message from="reviewer" kind="agent">
The tests pass on my branch. Please rebase.
</message>
```

From the first such message on, the system prompt of the run ends with the sentence `fw.MessageNote`. It says that the block is a message from another agent, with the authority of a tool result and not of the operator. It follows the sections of the tools and `fw.ContextNote` (section 12.5). It is not text of the profile, so the digests of the run stay as they are, and a restart records no `fw.drift.system` (section 12.5). A run that never gets a message from an agent keeps its prompt and its cached prefix. The `fw.model.requested` of each call whose prompt ends with the sentence names it in `systemNotes` as `message` (section 4.2). So a consumer of the egress stream sees which calls carried the sentence. The event holds the name only, never the text of the prompt. A message of a person reaches the model as it came. A hosted harness gets the same block and the same sentence (section 19.3). It builds its own prompt, so its `fw.model.requested` has no `systemNotes`. The interventions of the quality metrics do not count a message from an agent as a follow-up of a person (section 10.1).

### 5.6 Process duties

`maxRuns`, or `FW_MAX_RUNS`, bounds working root runs: 16 by default, or 1 under ax. `maxQueuedRuns`, or `FW_MAX_QUEUED_RUNS`, bounds additional admitted root drivers; zero selects the finite default of 64. A direct start and a branch are new work: each reserves its place before it stores an attachment or appends an event, so a start that the bound refuses leaves no file on disk and no event in the ledger. A follow-up and an approval decision continue a run that this process accepted. Each takes a place for its driver, which then waits for a working slot, but the bound never refuses it, so an approval or a follow-up never gets a 429 while the process is full. A follow-up reserves before it stores attachments or records a budget change; the place goes back when an active driver takes the message. A branch that waits for a decision holds no place until the decision starts its driver. A job of the bus intake holds its place from the moment the intake takes it (section 9.3). At capacity, a new start or a branch gets `ErrSaturated` with a `SaturationError`; the native API returns 429 and `Retry-After`, and the A2A server returns the server error -32000 with the reason `SATURATED` and a retry hint in its data, or 429 and `Retry-After` on its REST binding. Retry with the same idempotency key. An already accepted idempotent start returns its run without another admission. Concurrent retries of an initial start wait for its acceptance result, and cancellation of a waiter does not cancel the owner.

A start checks the drain and the storage before it appends `fw.run.queued`. Once that event is in the ledger, the run is accepted: the start returns the run, also when a shutdown or a storage failure begins during the rest of the start, such as the records of its attachments. The run then waits in the ledger, the shutdown suspends it, and the next start of the process resumes it. The bus intake acknowledges such a job, so no other process starts the run again.

An admitted driver can wait for a working slot, so the number of admitted root drivers is at most `maxRuns + maxQueuedRuns` for new work; follow-ups and decisions can add to that number. Recovery resumes work already accepted by the ledger even when a configuration change lowers that limit. Completed and idle run records remain queryable; this limit bounds admission, not lifetime history retention. A child takes no root slot; it uses `spawn.maxRunning` and the tree limits (section 11.9). This separation lets a parent wait for its children without exhausting their slots.

Only a storage fault blocks the process: an append, a flush, or a read of the ledger that fails, or an append after the ledger closed. A driver that stops on another error, such as a broken pipe of a hosted harness or an error of a runner, fails its run alone with `fw.run.finished`, and the process goes on; a follow-up can continue the run. The first storage fault blocks writes and cancels local execution for the process. `Agent.RuntimeError` preserves the cause, `Agent.RuntimeErrorSince` gives its time, the log gets one Error line with the cause, `Ready` becomes false, `/readyz` returns 503, `fw_storage_blocked` becomes 1 (section 10.1), and waiting callers receive `ErrStorageUnavailable`. A fork after the failure returns `ErrStorageUnavailable`, not `ErrClosed`. Reads expose a runtime-only `Run.Blocked` message for open runs while retaining the last recorded state. `GET /v1/status` reports `blocked: true` with `blockedReason`, and `idle: true` with `idleSince` at the time of the failure, whatever the state of the runs, because the process does no work until it restarts. The suspend controller of the workbench can therefore suspend the Task, and the next request restores a fresh process that rebuilds from the ledger. The runtime neither retries an ambiguous append nor invents a terminal ledger event. Repair storage and restart the owning process to rebuild from whatever reached the ledger; unsafe interrupted tools still follow the approval rules of section 4.5. An external effect already in flight can still finish, and the runtime does not roll it back.

On `SIGTERM`, the kernel stops accepting work and waits up to 8 seconds for in-flight model calls. Then it appends `fw.run.suspended` to every active run, flushes the ledger, and exits. A process blocked by storage failure cannot append suspension records. Under ax, `fw` is PID 1. A PID 1 process must reap child processes, so `fw` handles `SIGCHLD` and waits on orphaned children. A suspend under ax sends no `SIGTERM` (section 11.5). Only a stop of the worker pod itself starts this path.

### 5.7 Loop guard

A model that loops makes the same tool call with the same input, or the same short cycle of calls, again and again. The kernel finds this in the log alone. Every tool call of every response has a fingerprint: the tool name and the input as canonical JSON. After each response, the kernel looks at the end of the fingerprint list for a cycle of one, two, or three calls. It counts how many times in a row the cycle appears.

One repeat before `maxRepeats`, the kernel appends `fw.loop.detected` with the action `warned` and adds a system message from the kernel to the history. The message tells the model that it repeats itself and that one more repeat ends the run. At `maxRepeats`, the kernel appends `fw.loop.detected` with the action `failed`. Then it appends `fw.budget.exceeded` with the limit `maxRepeats` and finishes the run with outcome `failed`. The run keeps the text of the last response as its output. `maxRepeats: 0` turns the guard off.

The guard counts calls that ran, calls that the policy denied, and calls that failed alike. Each is one more attempt of the same action. A polling loop that waits on a slow job is a legitimate use that the guard stops at the default of five repeats. A profile that polls on purpose raises `maxRepeats`.

### 5.8 Time

A model does not know the date. It also does not see how much time passes between two of its turns. The kernel gives it both.

The `Clock` interface tells the time. The kernel reads every time from the clock of the agent: the time of each event, the deadline, and the time notes. The default is `SystemClock`, and `fw.WithClock` replaces it. A test passes `fw.NewManualClock` and moves time forward without a sleep. The kernel computes every time from the wall-clock times in the ledger, never from the uptime of the process. The values therefore stay right after a restart, after a resume in a fresh sandbox, and after a restore that moves the clock.

`time.zone`, or `FW_TIME_ZONE`, names the time zone of the agent with an IANA name, for example `America/Chicago` or `UTC`. An empty value means the zone of the system: the zone of `TZ`, else the zone of `/etc/localtime`, else UTC. A sandbox under ax sets neither, so its zone is UTC until `FW_TIME_ZONE` names another one. A profile sets its own zone with `timeZone`, or `FW_AGENTS_<NAME>_TIME_ZONE`. A child run without a zone takes the zone of its parent.

`fw.run.started` records the zone of the run. The run keeps that zone for its life, after a resume, and in a replay. The binary embeds the zone database, so a zone name resolves in an image without zone files. If a zone in the configuration does not resolve, the process stops at start. An inline profile with an unknown zone gets a 400 that names the field.

The `clock` tool returns the exact time in the zone of the run and the start of the run. It also returns the working time of the run and the working time left before its deadline. The tool is read-only, idempotent, and parallel-safe. It is also a `Briefer` (section 7.8). Its section of the system prompt gives the date only, as in this example:

```
Today is Wednesday, 2026-10-14, in the time zone America/Chicago.
```

The section leaves out the time of day, because a prompt that changes on every run cannot share the prompt cache with other runs. The method `RunClock` of the agent gives the same values to Go code.

Before a model call, the kernel measures the gap since the latest model response or the latest time note, whichever is later. If the gap is `time.noteAfter` or more, the kernel adds a time note. The default of `time.noteAfter` is 10 minutes, and 0 turns the notes off. A note looks like this:

```
Time note: it is now Thursday 2026-10-15 09:12 CDT, 21 hours 3 minutes after your last response. This run has 1 hour 40 minutes of working time left before its deadline.
```

The second sentence appears only for a run with a deadline. The first model call of a run gets no note, because the system prompt gives the date. One rule covers a follow-up on the next day, a late approval, a slow tool, and a restart after a suspend. The note is a `fw.message.received` event with the role `system`, the origin `kernel`, and the note kind `time`. The warning of the loop guard (section 5.7) is the same kind of event with the note kind `loop`, a budget note has the note kind `budget`, and the instruction of the final report has the note kind `report` (section 5.2). The note is an event and not text that a projection adds. A history that changes after the fact breaks the prompt cache, and a provider then rejects the thinking blocks of a resumed run (section 5.4).

The deadline of a budget counts working time, and `fw.WorkingTime` computes it from the ledger. Budget notes give the working time that is left at 75% and at 90% of the deadline, and a model call that is in flight when the deadline passes ends at the deadline (section 5.2). A run works from its input, a follow-up, an approval decision, or a resume. If it finishes, waits for a decision or an answer, or is suspended, it stops working. The time that a run waits for a person, for a follow-up, or in a suspended sandbox therefore does not count.

A gap with no event before `fw.run.resumed` means that the process stopped without a word. An example is a sandbox that ax killed on suspend (section 11.5). The working period ends at the last event before the gap.

## 6. Models

### 6.1 Interface

```go
type Model interface {
    Generate(ctx context.Context, req *Request) (*Response, error)
    Stream(ctx context.Context, req *Request, on func(Delta)) (*Response, error)
}
```

`Request` carries the system prompt, the messages, the tool specifications, the effort level, and the output token limit. An adapter fills an empty effort with the `effort` of the configuration that opened it, unless `EffortSet` is true: then the empty effort is the effort of the call: the Anthropic adapter sends its default, `high`, and the Gemini and OpenAI adapters send no effort, so the model uses its own default. The kernel sets `EffortSet` for a run with a resolved profile (section 12.5). `fw.model.requested` records the effort `default` for a call without an effort, because the model then sends the default of its provider. Messages use a small provider-neutral block set: text, image, document, tool use, tool result, and opaque. The opaque block carries provider data such as thinking blocks without interpretation.

### 6.2 Adapters

| Selector prefix | Library | Default model |
|---|---|---|
| `anthropic/` | `github.com/anthropics/anthropic-sdk-go` | `claude-opus-5-5` |
| `gemini/` | `google.golang.org/genai` | `gemini-3.8-flash` |
| `openai/` | OpenAI-compatible chat completions over HTTP | none |

A model selector is the string `provider/model`, for example `anthropic/claude-opus-5-5`. There is no default selector. A configuration that names no model boots and serves, logs one line that says so, and refuses every run with `fw: no model configured` until `model`, `FW_MODEL`, or the profile of the run names one. A process that only serves people, such as the front of a fleet, needs no model and no model credential. The `gemini` adapter exists because ax injects `GEMINI_API_KEY` and the ax `Model` resource defaults to Google. The `openai` adapter exists for OpenAI itself, for proxies, and for local servers such as vLLM and Ollama.

Each provider has a connection block under `providers.<name>` in `fw.yaml`, and every field of it has an `FW_PROVIDERS_<NAME>_*` variable (section 12.1):

| Field | Meaning |
|---|---|
| `baseUrl` | The endpoint of the provider. A local server or a proxy needs no code change. |
| `apiKeyEnv` | The secret that holds the API key, for a key under another name than the default |
| `headers` | Extra headers on every request. A value that names a secret is replaced by the secret. |
| `timeout` | The limit of one call, retries included. |
| `maxConcurrent` | The most model calls of this process to the provider at the same time, for a server with few slots. A call waits for a free slot. `0`, the default, means no limit. |
| `tls` | A CA bundle, a client certificate and key, or `insecure` for development. |
| `options` | Options that one adapter reads. Section 6.6 lists the OpenAI options. |

`fw.model.responded` records the time of each call in three fields. `latencyMs` is the whole call. `queuedMs` is the wait for a slot of `maxConcurrent`. `ttfbMs` is the time from the start of the call at the provider to the first streamed event of the answer. The OpenAI adapter measures it, and the other adapters leave it out. A long `ttfbMs` is a wait for the server, such as a queue at a model slot or a long prompt. The time after it is the work of the model.

### 6.3 Anthropic adapter settings

Unless a profile overrides them, the adapter applies these defaults:

- Thinking stays on. The adapter sends no `thinking` parameter. Depth comes from `output_config.effort`, default `high`.
- Every call streams. `max_tokens` is 64000.
- One prompt cache breakpoint sits on the last system block, so the tool list and the system prompt are cached. Tools are sorted by name for a stable prefix.
- Operator instructions during a run go into a mid-conversation `system` message, not into the top-level system prompt. The cached prefix stays valid.
- Client tools set `eager_input_streaming: true`. The adapter checks each parsed input against the tool schema before the call.
- Refusals use the server-side fallback: `fallbacks: "default"` with the beta header `server-side-fallback-2026-07-01`.
- History is append-only. The adapter never edits an earlier turn, because thinking blocks are bound to the conversation that produced them.

### 6.4 Middleware

```go
type ModelMiddleware func(Model) Model
```

Middleware wraps a model the way `http.Handler` middleware wraps a handler. One line adds one behavior. The built-in set is:

- `Retry` retries on 429 and 5xx with exponential backoff and honors `retry-after`.
- `Fallback` switches to a second model after repeated failure.
- `RateLimit` applies a token bucket per model.

Secrets need no middleware. The kernel removes them from every event before the append (section 4.2).

### 6.5 Credentials

The `Secrets` interface resolves a name to a value. The default chain reads `<NAME>` from the environment, then reads the file named by `<NAME>_FILE`. Drivers exist for HashiCorp Vault and Google Secret Manager. The Anthropic adapter asks for `ANTHROPIC_API_KEY`, the Gemini adapter for `GEMINI_API_KEY`, and the OpenAI adapter for `OPENAI_API_KEY`. The OpenAI key is optional, because a local server often needs none. `providers.<name>.apiKeyEnv` changes the name. `fw doctor` reports whether the key of the configured provider is present, and never prints it.

### 6.6 OpenAI adapter settings

The adapter speaks the chat completions API over `net/http` with no SDK, so one adapter serves OpenAI, Azure OpenAI, OpenRouter, LiteLLM, vLLM, Ollama, and tabbyAPI. Every call streams. The adapter retries a 429, a 5xx, and a network error that happens before the first response byte. A connection that is refused or reset before any byte of the answer arrived is retried five times, about fifteen seconds, because under ax a Task boots before its egress policy takes effect. It waits as `Retry-After` names, or with an exponential backoff. It never retries once a stream started. A stream that ends before a finish reason, ends in the middle of an event, or meets a reset fails with `fw.StreamInterruptedError`, so the kernel can call again (section 5.1). Malformed data in an event that the server finished, an error from the server, and a stall do not count as a cut. A stream with no byte for `idleTimeout` fails, and a response larger than 16 MB fails. A 401 or a 403 names the secret that the key comes from. A 404 that says the model is unknown lists the models that the server offers. The method `Models` lists them for `fw doctor`.

`providers.openai.options` holds these keys:

| Option | Values | Default |
|---|---|---|
| `reasoning` | `auto`, `on`, `off`: send the effort as `reasoning_effort` | `auto`: on for o-series and GPT-5 names. For another model on a server that is not OpenAI or Azure OpenAI, such as vLLM, llama.cpp, or Strata, `auto` sends an effort of the configuration or the profile other than `high`, the default, and keeps `max_tokens`. |
| `maxTokensField` | `auto`, `max_tokens`, `max_completion_tokens` | `auto`: follows `reasoning` |
| `authHeader` | A header name. `Authorization` sends a Bearer token. Any other name sends the bare key, which Azure wants in `api-key`. | `Authorization` |
| `organization`, `project` | The `OpenAI-Organization` and `OpenAI-Project` headers | `OPENAI_ORG_ID`, `OPENAI_PROJECT_ID` |
| `promptCacheKey` | `auto`, `on`, `off`: send `prompt_cache_key` set to the run id | `auto`: only to api.openai.com |
| `parallelToolCalls` | `true` or `false` | not sent |
| `idleTimeout` | A duration. `0` turns the check off. | `120s` |

A server streams reasoning content as `reasoning_content` or `reasoning`. That content goes into an opaque block and back to the server on the next turn unchanged. Usage counts cached prompt tokens apart and never counts reasoning tokens twice. The Responses API is not implemented.

## 7. Tools

### 7.1 Interface

```go
type Tool interface {
    Spec() ToolSpec
    Call(ctx context.Context, input json.RawMessage) (ToolResult, error)
}

type ToolSpec struct {
    Name         string
    Description  string
    InputSchema  json.RawMessage
    ReadOnly     bool // never changes state
    Idempotent   bool // safe to run twice
    Destructive  bool // hard to reverse
    ParallelSafe bool // can run beside other calls
    Title        string // a short phrase for a person, such as "Run shell commands"
    Action       string // the label of the button that allows a call, such as "Run command"
    OptIn        bool   // a profile gets it only when its tool list names it
}

type ToolSource interface {
    Tools(ctx context.Context) ([]Tool, error)
}
```

`ReadOnly`, `Idempotent`, and `Destructive` match the MCP tool annotations `readOnlyHint`, `idempotentHint`, and `destructiveHint`. `ParallelSafe` is local. Annotations from an MCP server are hints, and the policy can override them. An empty tool list and `*` give a profile every tool except an `OptIn` tool. Such a tool reaches only a profile that names it.

`Action` is the label of the button that allows a call of the tool. If one label is true for every call, the tool sets it. Without it, every surface shows a neutral label, such as "Allow". Section 7.2 lists the labels of the built-in tools. Section 7.3 says which MCP tools get a label. `fw` never makes a label from a tool name.

### 7.2 Built-in tools

| Tool | Purpose | Flags |
|---|---|---|
| `bash` | Run a shell command in the workspace | destructive |
| `files` | View, create, and edit files under a root path | none |
| `http` | Fetch a URL on an allowlist | read-only, parallel-safe |
| `memory` | Read and write notes in files under `$FW_HOME/memory`, in the scope of the run (section 7.8) | idempotent for each call of `view`, `create`, `delete`, and `rename` (section 7.8.1) |
| `recall` | Search and read notes, earlier runs, workspace files, and skills (section 7.8.3) | read-only, idempotent, parallel-safe |
| `skill` | List the skills, or read one skill with its files | read-only, idempotent, parallel-safe |
| `clock` | Give the time in the zone of the run and the working time left before the deadline (section 5.8) | read-only, idempotent, parallel-safe |
| `ask_user` | Ask the caller a question and wait for the answer | read-only |
| `spawn_task` | Start a child run with its own task, prompt, tools, and budget | parallel-safe |
| `await_task` | Wait for a child run and return its output | read-only, parallel-safe |
| `send` | Publish a message to a bus subject | none |
| `decide` | Ask a decider typed questions and get the answers with their probabilities. Without an entry in `deciders`, the tool does not exist (section 22.2). | read-only, idempotent, parallel-safe |

The workbench adds the built-in tool `propose_agent` (the workbench architecture, section 6.4).

`bash` answers a non-zero exit code with a normal result, not an error result. `fw.tool.responded` records the exit code in the field `exitCode`, which a tool sets through `ToolResult.ExitCode`, and the event line reads "the tool bash exited 1 in 6952 ms".

Four built-in tools set `Action`:

| Tool | Action |
|---|---|
| `bash` | Run command |
| `http` | Send request |
| `spawn_task` | Hand off task |
| `send` | Send message |

The other built-in tools have no label. `files` and `memory` read, write, and delete, so no one label fits them. `ask_user` asks a question and does not act. `clock`, `decide`, `recall`, `skill`, and `await_task` change nothing, so a person seldom allows their calls.

The `files` tool never opens a path under the paths that `Config.Reserved` lists: the home directory of `fw`, the directory of the file ledger, and the configuration file. The check uses the real path, after symbolic links. A change there changes what `fw` allows at its next start. For example, a profile file under `$FW_HOME/agents` can grant its agent access and read secrets from the environment through `${VAR}`. Every other path under the root needs no approval, except the paths that the rules of the workspace refuse (section 7.9).

When the provider is Anthropic, `bash`, `files`, and `memory` use the Anthropic-defined tool shapes `bash_20250124`, `text_editor_20250728`, and `memory_20250818`. Other providers get the same tools with a plain JSON schema. Each profile lists the tools that it allows. The `bash` shell gets the environment of a tool process (section 7.12): no manifest of ax, no credential of `fw`, and no variable that holds a secret. It starts with `FW_HOME` and with the variables of `tools.bash.env`. A script inside the sandbox therefore reads an endpoint from its environment instead of from a literal in a prompt. `tools.bash.timeout` bounds one command, 120 seconds by default. A tool that implements `io.Closer` is closed at shutdown, which ends the shell. The `bash` tool keeps one shell per run: the working directory, the variables, and the background jobs of one run never reach another run or another person, and the kernel closes the shell when the run ends, through the interface `fw.RunCloser`, which any tool with state per run can implement. A call with `RunInfo.Fresh` set, such as a check of a definition of done (section 24), runs in a new shell of its own instead, and that shell ends with the call.

Each `bash` command starts with `set -e`, `set -u`, and `pipefail` off. A failure under a `set -e` of the command ends the command with its exit code and keeps the shell, its directory, and its variables.

### 7.3 MCP client over HTTP and HTTPS

MCP is the Model Context Protocol. The revision used here is 2026-07-28, in which every request is self-contained and there is no session. `fw` is an MCP client for every server under `tools.mcp` in `fw.yaml` and for every server in the ax Workspace field `spec.mcp.servers`. An entry with `endpoint` uses the Streamable HTTP transport. An entry with `command` and `args` uses the stdio transport, and with a tool user that process runs as the tool user (section 7.12). The client library is `github.com/modelcontextprotocol/go-sdk`.

#### Connection settings

Each HTTP server entry has these fields:

| Field | Meaning | Default |
|---|---|---|
| `endpoint` | The MCP endpoint URL, `http://` or `https://` | required |
| `tls.ca` | Path to a PEM bundle that signs the server certificate | system roots |
| `tls.cert`, `tls.key` | Client certificate and key for mutual TLS | none |
| `tls.insecure` | Skip certificate checks. Development only. | `false` |
| `auth.bearer` | The `Secrets` name of a static bearer token | none |
| `auth.oauth` | OAuth 2.1 client fields: `clientId`, `clientSecret`, `scopes`, and `grant`, which says whose authority a call carries (section 7.11) | none |
| `headers` | Extra headers on every request. Values resolve through `Secrets`. | none |
| `timeout` | Time limit per call | `60s` |
| `skills` | Where the skills of the server come from: `auto`, a list of the URIs of `SKILL.md` resources that the operator pins, or `off` (see Skills of a server) | `auto` |
| `trusted` | Treat the tool annotations and titles of this server as true. A title becomes the label of the button that allows a call. Each call also names the person of the run in `_meta` (see The run behind a call). | `false` |

Use `https://` for every endpoint outside the cluster. `fw` logs a warning at start for an `http://` endpoint whose host is not a loopback address or a cluster-local name.

Every POST carries the headers that the revision requires: `MCP-Protocol-Version`, `Mcp-Method`, and `Mcp-Name` on `tools/call`, `resources/read`, and `prompts/get`. A parameter that a tool marks with `x-mcp-header` is mirrored into `Mcp-Param-<name>`. The body carries the same values in `_meta`. `fw` sends `Accept: application/json, text/event-stream` and reads either one JSON object or a request-scoped SSE stream. SSE is server-sent events, a one-way stream of text events over HTTP.

#### Authentication

`auth.bearer` sends `Authorization: Bearer <token>` with a token from `Secrets`. `auth.oauth` follows the MCP authorization flow. On a 401 with `WWW-Authenticate: Bearer resource_metadata="..."`, `fw` reads the protected resource metadata (RFC 9728). Then it discovers the authorization server (RFC 8414 or OpenID Connect discovery). Then it requests a token with the `resource` parameter (RFC 8707). By default, `fw` uses the `client_credentials` grant with a pre-registered client id and secret, because a sandbox has no browser. With `auth.oauth.grant: user`, the entry acts for the person who started the run, and that person connects in a browser (section 7.11). Tokens stay in memory and refresh before expiry. A 403 with `error="insufficient_scope"` triggers one step-up request with the union of the old and the challenged scopes, and then the call fails. Mutual TLS uses `tls.cert` and `tls.key`. No token appears in the ledger or in a log line.

#### Discovery and tool listing

On first use of a server, `fw` sends `server/discover`. The result gives `supportedVersions`, `capabilities`, `instructions`, and a cache time `ttlMs`. `fw` picks a version from the list and adds `instructions` to the description block of that server. Then it sends `tools/list`, follows `nextCursor` to the end, and caches the list for `ttlMs`. The tools of each server appear to the model as `<server>.<tool>`, sorted by name. A provider that rejects a dot in a tool name, such as Anthropic, sends a sanitized name (`git__commit`) and maps the answer back. Policy rules and profiles therefore always use the dotted name. A tool definition with an invalid `x-mcp-header` value is dropped with a warning, as the revision requires. A refreshed list appends new tools and keeps the old order, so the prompt cache prefix stays valid.

A tool of a server with `trusted: true` takes its `Action` (section 7.1) from the title that the server gives the tool: `title`, else `annotations.title`. The label is one line. Spaces collapse, and characters that do not print go away. A title longer than 40 characters is cut at a word and ends with an ellipsis. A tool without a title has no label. A server without `trusted` never sets a label, because the label speaks for every call.

If the server reports `capabilities.tools.listChanged`, `fw` opens a `subscriptions/listen` stream with `toolsListChanged: true`. On `notifications/tools/list_changed`, `fw` refreshes the list. When the stream drops, it reconnects with backoff.

#### Older servers

Many servers still speak revision 2025-11-25 or earlier, with an `initialize` handshake and an `Mcp-Session-Id` header. `fw` is a dual-era client. It sends a modern request first. If the reply is a 4xx without a recognized modern JSON-RPC error in the body, `fw` falls back to `initialize`. It keeps the session id for that server. `fw` caches the era per endpoint for the life of the process.

#### Calls, input requests, and tasks

`tools/call` runs with the entry timeout and the context of the run. When the run is canceled, `fw` closes the response stream, which is the cancellation signal in this revision. A `notifications/progress` event on the response stream becomes `fw.tool.progress` in the ledger.

A result with `resultType: "input_required"` means the server needs something before it can finish. `fw` handles each entry of `inputRequests` by method:

- `elicitation/create` in form mode becomes `fw.approval.requested` with the requested schema and the `requestState`. The run waits in `input-required`. The decision content goes back as `inputResponses`, with `requestState` echoed unchanged. Because the state is in the ledger, the retry works after a restart or a resume.
- `elicitation/create` in URL mode becomes a message to the caller with the URL. The run waits until the caller answers, and then `fw` retries the call.
- `sampling/createMessage` and `roots/list` are deprecated in this revision. `fw` answers them for older servers only: sampling with the run model under the run budget, roots with the workspace paths.

A result with `resultType: "task"` needs the extension `io.modelcontextprotocol/tasks` in the capabilities that `fw` declared. `fw` stores the `taskId` in the `fw.tool.requested` data, polls `tasks/get` at `pollIntervalMs`, and answers `input_required` through `tasks/update`. If the run is canceled, `fw` sends `tasks/cancel`. A resumed run continues polling from the stored task id. This is how a long upstream tool survives a suspend and a resume.

#### Skills of a server

A server can serve skills through the MCP extension `io.modelcontextprotocol/skills` (SEP-2640, on base revision 2026-07-28). It declares the extension in its capabilities. `skills/list` lists the skills: the URI of each, its front matter, and its files with the digest `sha256:...` and the size of each, or `dynamic` for a skill whose files the server lists on demand. The files are resources that `resources/read` returns. The kernel declares the extension on its side and handles these values of `skills` of an entry:

| Value | The kernel |
|---|---|
| `auto`, the default | lists the skills with `skills/list` when the server declares the extension. A server that declares it and refuses the method with -32601, or a 404 on revision 2026-07-28, as agentgateway does, is remembered as one that does not answer it. |
| a list of `SKILL.md` URIs | pins those skills. The kernel reads each one with `resources/read`, takes the name and the description from its front matter, and computes its digest itself. Use it for a server behind a gateway that refuses the method. With `auto` in the list, the kernel tries `skills/list` first and takes the pins when the server does not declare the extension, does not answer the method, or fails. |
| `off` | reads no skill of the server |

Each skill of a server joins the skills of the agent with the origin `mcp`, the entry name of the server, and the URI of its `SKILL.md`. Its name is `<server>/<name>`, such as `github/review`, so a skill of a server never shadows a skill of the skill directories, and the allowlist `skills` of a profile matches local names and qualified names alike: `github/*` allows every skill of one server. A name that the server lists twice keeps its first entry. The line of the skill in the system prompt names the server: `- github/review: Review a pull request (from the MCP server github)`. A server that acts for the person of a run (section 7.11) gives no skills, because the list must not depend on the person.

The kernel reads the list when the process starts, again when the server sends `notifications/resources/list_changed`, and again when the `ttlMs` of the listing passes. A server that is down keeps its last list, the log says so, and the kernel tries again after 30 seconds. The kernel reads no file of a skill ahead of need. The `skill` tool reads `SKILL.md` of a server skill at the moment of use with `resources/read`, and its field `file` reads a supporting file the same way, so a model reads a reference file of a server skill as it reads one of a local skill. Before it uses a file, the kernel checks its size and its digest against the listing, or, for a pinned skill and a file that the listing does not name, against its own first read, and it checks that the name in the front matter of `SKILL.md` is the name of the listing. A mismatch refuses the file with the reason, and nothing of it reaches the model. A file holds at most 4 MB, and the tool limits of section 7.7 bound the result. A path that leaves the skill, an absolute path, and a hidden file are refused. The tool reads only a skill that the profile of the run allows. `GET /v1/skills` lists the skills of the servers with their origin and their URI. `fw doctor` names, for each server, how many skills it gives and whether it answers `skills/list` or needs a pinned list. `recall` matches a skill of a server by its name and its description only.

#### The run behind a call

Every `tools/call` of a run carries the run in `_meta`, next to the keys that the revision requires:

| Key | Value |
|---|---|
| `io.featherweight/run` | The id of the run |
| `io.featherweight/agent` | The name of the profile of the run |
| `io.featherweight/node` | The name of the process, from the key `node` of the configuration, as the egress stream carries it (section 9.4) |
| `io.featherweight/principal`, `io.featherweight/principalKind` | The subject and the kind of the person of the run. Only a server with `trusted: true` gets them. |

A call without a run, such as a call of a Go caller outside the turn loop, carries none of these keys. A run without a principal carries no principal keys. A server, such as the MCP server of the front of a fleet, reads the keys to know which run and which agent calls it, and for which person. The keys are data for the server. The kernel reads nothing back from them, and they grant nothing: a server still authenticates the caller, because any client can send the same keys. The Go names of the keys are `fw.MCPMetaRun`, `fw.MCPMetaAgent`, `fw.MCPMetaNode`, `fw.MCPMetaPrincipal`, and `fw.MCPMetaPrincipalKind`.

#### Failure handling

A connection error or a 5xx is retried three times with exponential backoff. A 4xx is not retried. After five failures in a row, `fw` marks the server down for 30 seconds. During that time each call to its tools returns `is_error` at once, with a message that names the server. The gauge `fw_mcp_server_up` shows the state. The `http` tool allowlist does not apply to MCP endpoints, because the two are separate plugs.

#### Entries from the ax Workspace

An ax `spec.mcp.servers` entry has `name`, `endpoint`, `command`, and `args`, and nothing else. `fw` merges these entries with `tools.mcp` by name, and the `fw.yaml` entry wins. To secure an endpoint that the Workspace declares, add an entry with the same name to `fw.yaml` and give it the TLS and auth fields. `spec.mcp.registries` entries are logged and skipped, because the ax contract does not define the registry lookup yet.

### 7.4 A2A peers

A2A is the Agent2Agent protocol, version 1.0. A peer is another agent that `fw` reaches by A2A. Each peer becomes one tool named `ask_<peer>`. The tool sends a message, follows the stream, and returns the final text and artifacts. A peer is configured by its agent card URL, or by `ax: <atespace>/<task>` for a Task in the same ax cluster (section 11.4). The client library is `github.com/a2aproject/a2a-go/v2`.

### 7.5 Policy

```go
type Policy interface {
    Decide(ctx context.Context, call ToolCall) Decision // Allow, Deny, or Ask
}
```

The default policy allows read-only tools, asks before destructive tools, and denies nothing. Rules in `fw.yaml` change this per tool name, per flag, or per profile. A profile policy combines with the global policy, and the stricter verdict wins, so a profile can tighten the rules but not loosen them. A child run also carries the rules of every ancestor. A run keeps the rules of the profile that it was queued with (section 12.5), and the rules that its profile has now apply on top. A rule that a person adds to a profile therefore takes effect at once on the runs that are already going, and a rule that they remove stays in force for those runs. A call that waits for an approval meets the rules again when a person allows it: a deny that a profile of the run or of an ancestor gained meanwhile wins over the allow, and the result of the call names the rule. The policy of the process is not asked again. The rules of `fw.yaml` and a policy from a Go option are not part of a profile and apply as they are now. With `policy.external`, a control plane outside the process decides every tool call in place of the rules (section 13.5). The policy of a profile and the rules of the ancestors can still only make its verdict stricter. A security team therefore changes the policy without a rebuild.

`fw.MergeRules` preserves independent policies under `all` clauses. Each clause evaluates its own default, tool patterns, and argument rules; the strictest verdict wins. Flattening allow or ask lists across defaults is not equivalent. The native API and AX child requests carry these clauses intact.

A rule is one of four forms:

| Form | Example | Matches |
|---|---|---|
| a flag | `destructive` | every tool with that flag |
| a tool glob | `git.*` | the tool name |
| a tool and a command pattern | `bash: git push*` | the `command` field of the input, and for a deny or an ask rule each simple command in it |
| a tool, a field, and a pattern | `files: path=*.env` | that string field of the input |

In a command or field pattern, `*` matches any text, slashes included, and `?` matches one character. If the input holds that field, the field form applies. Otherwise the text matches the command, so `bash: FOO=bar*` still matches a command. With the field form, a deployment restricts what a tool call can touch: a path, a URL, a subject, a method.

#### Compound commands

A command pattern matches the whole text of the command from its start. A model often joins commands, as in `cd /work && git push`. So a deny or an ask rule also matches each simple command in the text. A simple command is one command and its arguments, such as `git push origin main`. The kernel splits the text where the shell starts a new command: at `&&`, `||`, `;`, `|`, `&`, and a new line. A separator inside quotes or after a backslash does not split the text. A redirection such as `2>&1` or `&>file` does not split it either. The kernel removes the blanks at the start and the end of each part. The field form on the field `command`, such as `bash: command=git push*`, splits the same way.

| Rule | Command | Verdict |
|---|---|---|
| `ask: bash: git push*` | `cd /work && git push origin main 2>&1` | ask |
| `ask: bash: git push*` | `echo "a && git push"` | no match: the separator is inside quotes |
| `deny: bash: rm -rf*` | `make clean; rm -rf build` | deny |

The shell also runs a command in other places. So the kernel matches these parts too (`fw/command.go`):

- Each simple command inside `( )`, `$( )`, `<( )`, and backquotes, also inside double quotes. So `echo "$(git push)"` matches `git push*`. Single quotes keep their text as data, so `echo '$(git push)'` does not match.
- The text from the command word on. Variable assignments such as `FOO=1` can come before the command word. A function header such as `f()` can come there too. So can the reserved words `!`, `{`, `if`, `then`, and `elif`. The reserved words `else`, `do`, `while`, `until`, and `coproc` can come there too. So `GIT_SSH_COMMAND=x git push` and `if git push; then` match `git push*`.
- After a runner, the text from each later word on. A runner is a command that runs another command from its arguments. The runners are `sudo`, `doas`, `su`, `runuser`, `env`, `time`, `nohup`, and `xargs`. `nice`, `ionice`, `timeout`, `stdbuf`, `setsid`, `chroot`, and `flock` are runners too. So are `watch`, `command`, `builtin`, `exec`, `eval`, `find`, and `ssh`. The last runners are the shells `sh`, `bash`, `zsh`, `dash`, `ksh`, and `fish`. A later word can also hold a whole command line, as the argument of `sh -c` does. When a later word holds a blank, a quote, or a special character, the kernel reads it as a command line too. It removes the quotes of the word first. So `sudo -u ci git push`, `echo origin | xargs git push`, and `bash -c 'cd x && git push'` match `git push*`.
- Each line of a here-document. When its delimiter has no quotes, a body runs its `$( )` parts. A script that a body writes to a file can run later. A quote in a body does not reach past its line.
- Each of these parts without its quotes and backslashes, with one space between its words. So `git 'push'` and `git  push` match `git push*`.

The split errs on the side of a match. A quote that does not close counts as a plain character, so it hides no text. A word after a runner matches also as a plain argument, as in `sudo echo git push`. A line of a here-document that only goes to a file matches too. Such a command asks although it does not have to. A command that the kernel cannot split within its bounds matches every deny and ask rule with a command pattern for its tool. The bounds are 8 levels of nesting and 16 MiB for all the parts together.

An allow rule matches the whole text of the command only. One part of a compound command never allows the other parts. So `allow: bash: git status*` does not allow `cd x && git status`. A deny or an ask rule that matches a part wins over an allow rule that matches the whole text.

A command pattern guards against the plain forms of a command. It is not a sandbox. It matches the words as the model wrote them. So `git push*` does not match `git -C repo push`, a command in a variable such as `$GIT push`, or a script that runs `git push`. A rule for such a form names it too, such as `bash: git -C * push*`. A boundary that must hold does not rest on a command pattern alone. The run then also lacks the credential, or the agent gets a narrower tool in place of `bash`.

A tool can resolve its input before the policy decides (`fw.Resolver`). The `files` tool gives the real path of the file, relative to its root, after `./`, an absolute path, and symbolic links. With `tools.files.perRun`, its root is the directory of the run, so a rule such as `files: path=docs/*` matches `docs` in that directory. A deny or an ask rule matches the path of the model or the resolved path. An allow rule matches only the resolved path, so `docs/../secrets/a` does not pass `allow: files: path=docs/*`. `policy.external` gets the resolved input in the field `resolved`.

### 7.6 Many tools

A large tool list costs tokens, and a changed tool list breaks the prompt cache. When more than 24 tools are loaded, the Anthropic adapter registers the tool search tool and marks the rest as deferred. The model then loads the schemas it needs. A tool source that changes at runtime appends tools instead of replacing the list, so the cached prefix stays valid.

### 7.7 Tool limits

Every tool call goes through the limits middleware, `fw.LimitTools`. The configuration `tools.limits` sets the defaults, and `tools.limits.perTool` overrides them per tool name or glob.

| Limit | Default | Effect |
|---|---|---|
| `maxInputBytes` | 256 KB | A larger input does not run. The model reads an error that names the limit. |
| `maxOutputBytes` | 100 KB | A larger result keeps its head and its tail. A note at the cut says how many bytes are gone, and in a run it names the file with the full text. |
| `timeout` | 10m | A call that runs longer gets an error result and its context ends |

The limits apply to the built-in tools, to MCP tools, to A2A peers, and to tools passed as Go options alike. The reason is that the middleware wraps every tool outermost. The kernel records the bounded result in the ledger, so the history that the next model call sends is bounded too. An image or a document in a result counts with its size. If it does not fit, the kernel drops it with a note. The cut keeps two thirds of the room for the head and one third for the tail. The start of a listing and the end of a log therefore both survive.

The output limit does not cut the result of a step of a check run, because no model reads it. The check keeps at most its own `maxBytes` of each result (section 23.3).

In a run, the kernel also writes the full text of a cut result to `.fw-tool/<call id>.txt` in the directory of the run, with every secret replaced as in the ledger, and records `fw.file.changed` for it. The note at the cut names the file and its size, so the model reads the part that it needs with the `files` tool and a line range. One careless command then costs the context window only the limit, and a lower limit, such as 16 KB for a local model, loses nothing. The `bash` tool keeps up to 1 MiB of the head of each stream and 32 KB of its tail when this limit applies to it, so the file holds the whole output of most commands. Without the limit, `bash` keeps 100 KB of the head and the tail itself. Without `tools.files.perRun`, the directory of the run is the root of the workspace, so a git checkout there wants `.fw-tool/` in its `.gitignore`.

### 7.8 Memory

The memory of an agent is the durable state that a later run can use: the notes that runs keep, the runs in the ledger, the files of the workspace, and the skills. The package `memory` holds the `Store` of the memory. The tools `memory` and `recall`, the routes `/v1/memory` of the native API (section 8.2), and the hosts of harnesses (section 19.10) each build one over the same directories, so every surface reads and writes the same notes with the same scope, limits, and audit. The kernel owns the contract in `fw/memory.go`: `fw.Memory` reads (`Search`, `Get`), `fw.MemoryWriter` writes (`Remember`, `Forget`), and `fw.MemoryScope` names what a call may reach.

#### 7.8.1 Notes and scopes

The `memory` tool keeps notes between runs in Markdown files. The model addresses them as `/memories/...`, with the commands of the Anthropic memory tool. The files are canonical: a person reads, edits, backs up, and moves them as files. `tools.memory.scope` selects the directory:

| Scope | Directory | Who shares the notes |
|---|---|---|
| `agent`, the default outside ax | `$FW_HOME/memory/agents/<agent>` | every person who runs the profile, and every run of the profile without a principal |
| `principal`, the default under ax | `$FW_HOME/memory/principals/<agent>/<hash>` | one person in one profile; the hash names the subject of the principal, so no email address is part of a path. A run without a principal uses the directory of `agent`. A run with a principal also reads the notes of `agent`, under `/memories/.agent`, and never changes them (section 7.8.4). |
| `shared` | `$FW_HOME/memory/shared` | every profile of the process; through the native API, only `access.admins` write these notes (section 8.2) |

A run without a principal is a local `fw run` under `access.default: open` with no control plane, or a job that a publisher put on the bus without a principal. A local `fw run` under `access.default: closed`, or when a control plane decides, and every run of `fw-workbench mcp` carry the principal `local:<user>`, of the kind `local` (section 13.1), so in the scope `principal` they use the directory of that user. A job that the front of the workbench enqueues carries its caller.

The kernel builds the scope from the run (`Store.Scope` of `fw.RunInfo`), never from the input of a call, so a tool or a caller cannot widen it. Under ax one Task serves many people one after another, so `ax.ApplyToConfig` sets the scope `principal` when `tools.memory.scope` is empty, and it says so in the log, as it does for `tools.files.perRun`. A configuration that sets `agent` under ax lets a person read the notes that the runs of another person wrote, and `fw doctor` warns about it (section 12.9). A child run uses the scope of its own profile and principal. Real sharing across Tasks and teams, with access rules, retention, and curation, belongs to the workbench, not to this directory.

A scope keeps the notes of one person apart from the runs of another. It does not hide a run from the people who can view it. The index of the notes is part of `fw.run.started` (section 7.8.2), and the input and the result of each call of the tools `memory` and `recall` hold the text that the call wrote or read. So anyone who can view a run, under `run.view` (section 13.2), reads what that run read and wrote. `fw.memory.changed` holds no text, but the ledger of the run does.

The store keeps these rules for every write, from any surface:

- A note holds at most 128 KB, and one scope holds at most 2,000 notes. A write past a limit fails with a message that the model reads (`memory.ErrLimit`).
- The count of the notes and every walk of a scope, for the index, the listings, the scan, and the FTS index, visit at most 10,000 notes and, apart from them, at most 10,000 other entries, such as directories and hidden files. So directories do not use up the bound of the notes. A walk that stops at the bound of other entries cannot finish its count, so a new note fails closed with `memory.ErrLimit`, and the listings and the search miss the notes past the bound. Each of them says so. The index at the start of a run and the `list` tool of `fw-memory` end with a sentence that names the bound (`memory.CutNote`), a search says it in its notes, `GET /v1/memory/notes` adds `cut: true`, and `fw doctor` warns that its counts are too low.
- A name is UTF-8, has at most 8 parts, such as `a/b/c.md` with 3, and has no part that starts with `.` and no control character, a rune below 0x20 or 0x7f. So no note hides from the index and the listings, and no name adds a line to the index. A walk does not go into a directory that is too deep to hold a note. The index also escapes each name (`memory.EscapeName`), so a file that a person put in the directory with a newline in its name cannot forge a line.
- A write goes to a hidden temporary file, which the store syncs and renames over the note, and then the store syncs the directory of the note, so a stop of the process or of the system leaves the old note or the new one, never a part. The first open of a scope by a `Store` removes the hidden temporary files that a stopped write left there, while it holds the lock of the scope.
- A write takes the lock of its scope directory, a `flock` on the inode of the directory, so the writes of every `Store` and every process on the volume go one at a time. The store tries the lock without blocking, and again after a short pause, until it gets the lock, the context of the call ends, or 30 seconds pass; then the write fails with an error that says why (`memory.ErrLockTimeout`). A compare-and-swap (`Remember` with a hash, `absent`, or `present`) and the count of notes hold the lock from the read to the write, so two writers with the same condition never both succeed.
- Each command of the `memory` tool that writes runs while it holds the lock of the scope (`Store.Locked`), so `str_replace` and `insert` read the note and write it in one step. A concurrent write with `If-Match` through the native API, or an edit of another run, is never lost: it comes before the edit or after it. `Store.Edit` does the same for one note.
- The hash of a note, which `Get` returns, the native API sends as `ETag`, and a compare-and-swap checks, covers the whole file.
- `Forget` removes one note and refuses a directory, so each note that goes has its own record. `Forget`, a `delete` of the tool, and a `rename` take the rows of the note out of the FTS index at once, when the index exists (section 7.8.3).
- Each change that a tool call makes appends `fw.memory.changed` to the ledger of its run: the operation, the ref, the scope, the agent, the size, and the SHA-256, never the text (`fw.RecordMemoryChange`). No run records a write or a removal through the native API, so the log records it: the subject of the caller, the operation, the ref, the scope, the size, and the SHA-256, never the text.

The `memory` tool tells the kernel for each call that the call is safe to run again (`fw.CallIdempotency`). When a restart cuts a call before its result (section 4.5), the call runs again without an approval. A second `view`, `create`, `delete`, or `rename` leaves the notes as the first one did, or fails because the first one already moved or removed the note. An `insert` or a `str_replace` records the call before it writes. A second run then returns the result of the first one and does not edit again (section 7.8.4). Any tool whose calls differ can implement `fw.CallIdempotency`, in place of the flag `ToolSpec.Idempotent`, which classifies every call of a tool together. The memory tool leaves that flag false, so a policy rule `idempotent` does not match it.

#### 7.8.2 The index is data, not an instruction

At the start of a run, the tool adds two texts through two seams:

1. `fw.Briefer` adds the section `# Memory` to the system prompt. The section is the same for every run: it says how the memory works and what to save, a preference of the user, a decision and its reason, or a fact that took effort to find. It holds no note.
2. `fw.ContextBriefer` adds the index of the notes as data. The index lists each note with its path and its first line, newest first, up to 40 notes and about 4 KB. A `description:` field in front matter takes the place of the first line. When notes are left out, the index says how many and points to the tool `recall`.

The kernel records the data in `fw.run.started`, in the field `context`, and puts it at the start of the first user message of every model call, inside a block that names the tool. The system prompt gets one sentence of the kernel, `fw.ContextNote`, that tells the model that such a block holds data and not instructions. The kernel escapes a closing tag inside the text of a tool, so a note cannot end its block early. The first message of a run looks like this:

```
<context source="memory">
Notes under /memories, newest first:
- /memories/build.md: How to build and test this repository
- /memories/user/preferences.md: The user wants short answers
</context>

Fix the flaky checkout test.
```

A note is text that an earlier run, a person, or a harness wrote. Text in the system prompt has the authority of the operator, so a note there turns a planted line into an instruction for every later run. That is the persistence path of the Claude Code memory compromise of April 2026, which Anthropic closed by taking memories out of the system prompt. The kernel keeps the same split.

The data stays the same for the life of the run, so the cached prefix of the prompt stays valid. A compaction replaces the history, and the next call puts the data back at the start of the first message, so the index survives every compaction. A resume and a replay take the data of the recorded run. A note that the model writes during a run appears in the index of the next run. The `clock` tool uses `fw.Briefer` for the date (section 5.8), and the package `env` uses its own section (section 18).

#### 7.8.3 Recall

The tool `recall` reads the durable state of the run. A search takes words and finds records of four kinds:

| Kind | Ref | What it searches |
|---|---|---|
| `note` | `mem://<scope>/<agent>/<path>`, or `mem://shared/<path>` | the notes of the scope of the run: path, first line, and text. In the scope `principal`, the notes of the scope `agent` of the agent too. |
| `run` | `run://<id>` | the earlier runs of the same agent that the same principal started: title, input, output, and metadata. An empty principal equals only an empty principal, so a run of a person never sees a run without a principal, and a run without a principal sees only those. |
| `file` | `file://<path>` | the workspace of the run: path, and the text of each file up to 64 KB, at most 5,000 files and 16 MB for one search |
| `skill` | `skill://<name>` | the skills that the profile of the run allows (`skills`), the same skills that its system prompt lists: name, description, and body |

A hit gives the ref, the title, and a short snippet. A call with `ref` reads one record. A note ref resolves in the scope of the run, so a ref that names another agent or another scope fails. A run of the scope `principal` also reads a ref of the scope `agent` of its own agent, and nothing writes it through such a ref. A run ref and a skill ref follow the same rules as a search. The tool is read-only, idempotent, and parallel-safe, and it treats every record as data. The run itself never matches its own search.

The tool reads the runs with the principal of its run in the context of the call (`fw.WithRunPrincipal`), the way a surface puts the principal of a request there. So `Agent.List` and `Agent.Get` authorize each run for that person (`run.view`, section 13.2), on top of the rule of the same principal, and a run reads no run that the person who started it cannot view. A run without a principal reads with the authority of the process, and the rule of the same principal alone bounds it.

One search reads at most 16 MB from the files of the workspace, and a scan reads at most 16 MB from the notes. A search that reaches a bound, or the bound of 5,000 files, stops that kind and says so in a note of its result (`memory.RecallResult`), which the tool shows after the hits and the native API returns in the field `notes`.

The default build scans the notes for a search. The build tag `memory_sqlite` adds a SQLite FTS5 index of the notes, with BM25 ranking, from the pure-Go driver `modernc.org/sqlite`, so the binary stays static without cgo. The index is derived: one database for each scope under `$FW_HOME/memory/index`, updated from the modification time and the size of each note before each search, and built again from the notes when its file is of another version or not of the index. It uses a rollback journal, because a journal in WAL mode needs shared memory that a sandbox volume does not always give. Every `Store` of the process and every process can open the same file: each connection has a busy timeout, each write runs in an immediate transaction, and one transaction sets the marks and the schema of a new file. A file that is not a database, is corrupt, has the marks of another version, or holds tables that are not of the index goes, and the next search builds it from the notes; a busy lock is never a reason to remove a file. A note that changed is found by its modification time, its size, and its inode, which an atomic write always changes. A removal of a note takes its row out of the index at once, not at the next search, and the database runs with `PRAGMA secure_delete` and the `secure-delete` option of FTS5, so the words and the text of a removed note do not stay in the file. Version 3 of the schema turned the secure delete on, so a file of an earlier version goes and the notes build it again. The tag adds about 4 MB to the binary. A search that the index cannot answer falls back to the scan.

#### 7.8.4 Know-how

Know-how is what an agent learns on the job. `docs/KNOWHOW.md` is its contract. This section says what the kernel does for it.

**Data blocks of the caller.** `RunRequest.Context` holds data blocks: text that earlier runs or other people wrote, such as a page, the summary of a conversation, or the text that a Duty found. Each block has a source, which matches `[a-z][a-z0-9-]{0,31}`, and a text. A run takes at most 8 blocks of at most 16 KiB each (`fw.CheckRunContext`). A request past a bound fails before the ledger holds anything, and the native API answers 400 with the bound. A check run takes no block. `fw.run.queued` records the blocks. At the start of the run, the kernel wraps each block with `fw.ContextBlock`. It puts the blocks after the blocks of the tools, in the field `context` of `fw.run.started`. The system prompt then gets `fw.ContextNote`. A resume, a replay, a branch, and the retry of a bus job keep the blocks. `POST /v1/runs` and a bus job take the field `context`, and `fw.WithRunContext` is the Go option. A hosted harness puts the blocks after the index of the notes, at the start of its first prompt.

**Notes kept at the front.** `tools.memory.front` names the front of a fleet: `url`, and `tokenEnv`, the variable that holds the static token. With it, the front keeps the notes of the scopes `agent` and `principal`, and the files under `$FW_HOME/memory` are a copy. The scope `shared` stays in the files of the process. Without `front`, nothing changes.

- Notes of the agent. The after-work review keeps its notes in the scope `agent`. A run of the scope `principal` reads them too, beside the notes of its principal, and never changes them; its own writes go to its scope. With a front, the store pulls the scope `agent` as it pulls the scope of the run (`GET /v1/knowhow/notes?agent=<agent>&scope=agent`), both at once, so the start of a run still waits 5 seconds at most. Without a front, the run reads the local scope `agent` of the same home. The model sees them under `/memories/.agent` (`memory.AgentPrefix`). A part of a name that starts with `.` is never a note (`memory.CheckName`), so no note of the principal can take this path, and the notes of the agent never hide one. The memory tool reads them with `view`, and a view of `/memories` lists them after the notes of the run. A `create`, a `str_replace`, an `insert`, a `delete`, or a `rename` that names a path under `/memories/.agent` changes nothing: the tool answers with an error that says that the notes belong to the agent and that the run can keep its own note instead (`memory.AgentReadOnly`). In the scopes `agent` and `shared`, `.agent` stays a hidden name that the tool refuses. This is how the scope `principal` works, with no switch. The scopes `agent` and `shared` keep their behavior.
- Pull. At the start of a run, the store asks the front for the list of the scope (`GET /v1/knowhow/notes`), within 5 seconds. The store fetches each note whose hash differs from the copy. It removes each copy that the front put away, holds for a person, or no longer lists. Some notes of the copy were never on the list, such as a note from before the front was set. Each of them goes to the front as a new note with `by: agent`. When the front keeps another text under the same name, such as the note of another Task of the pool, the text of the copy goes first under a free name, such as `gotchas-2.md`, so no note from before the front is lost. A second pull sends nothing. Then the memory tool writes its block from the copy. When the front does not answer, the run uses the copy, and the block says so in one line.
- Push. Each change through `memory.Store` goes to the front inside the lock of the scope, after the write to the copy. The memory tool, the native API, and the hosted `fw-memory` server all change notes through it. A write sends `PUT` with the text, `ifHash`, `by`, the run, and the sources. `ifHash` is the hash of the copy before the write, or `absent` for a new note. A removal sends `DELETE` with `ifHash`. `by` is `agent` for a run and `person:<subject>` for a person through the native API. The kernel builds both from the context, never from the input of a call. The sources come from `MemoryRecord.Links`. A write without links sends an empty list, so the front never keeps the sources of an older text for a new one. A rename sends the note under its new name and then removes the old name. With a front, the memory tool deletes and renames one note at a time, and it refuses a directory.
- A 412 of the front puts the text of the front back in the copy. The change then fails with `memory.ErrFrontConflict`: "This note changed in another run. View it again before you change it." The native API answers 412 with that sentence.
- A front that does not answer within 10 seconds, or that refuses the change, gets the change undone in the copy. The change then fails with `memory.ErrNotKept`, and the native API answers 503.
- `Remember` keeps `Links` and `Metadata` beside the text of the note, in a hidden state file of the scope. `Get` returns them while the text stays the same.
- Sources from the text. A write without links takes the sources that the lines of the note name: `Source: page:<path>` for a page of the wiki, or `Source: file:<path>` for a file of the workspace (`memory.SourcesOf`). The memory tool has the shape of the provider, so a model names a source this way, and the memory section of the system prompt tells it how. With a front, each page gets the hash of the page at the front as its version (`GET /v1/pages/{path}`), so the note goes out of date when the page changes.
- Ask first. A front that holds a change of the agent for a person answers `202` with `waiting`. The store keeps the text in the copy for the run, and `fw.memory.changed` carries `waiting: true` with the sources of the write. The memory tool and `fw-memory` then tell the model that the note waits for a person, not that it is saved (`memory.Answer`).
- The client reads the token from the variable at each request and sends it only in the header `Authorization`. Each holder of a store gives the token to the redactor (`memory.Store.GuardToken`). No error of the client holds the token or the address of a request, which names the principal.

**The block with a front.** The block of the memory tool keeps its place and its bound. With a front, its first line says how many notes it lists and how full it is, such as "Notes: 23 listed, 3.1 of 4 KB shown." It counts only the notes of the list. Then it lists three groups. First come the notes that a person wrote, with the label "(from a person)". Next come the checked notes, with the label "(checked <date>)". Last come the other notes. Inside a group, the notes that runs used most come first, then the newest. Notes that are out of date or resting stay out, and one line counts them. When the front does not answer, the labels come from its last answer. The memory section of the system prompt then names this order (`memory.SystemSectionFront`).

**The block in the scope `principal`.** When the agent has notes to list, the block of a run of the scope `principal` lists two groups, each after one line: the notes of the agent after "Notes of this agent, read-only:" (`memory.AgentNotesHead`), then the notes of the run after "Your own notes:" (`memory.OwnNotesHead`). The rules above hold in each group, with a front or without one. The line that counts the notes left out counts both groups. The bound of about 4 KB and 40 notes covers both groups together: each group first has half of it, and the room that one group leaves goes to the other, first to the notes of the agent (`fitGroups` in `memory/brief.go`). When the agent has no note to list, the block is the block of the principal alone. The memory section of the system prompt adds `memory.AgentSection`, which says where the notes of the agent are and that the run cannot change them.

**Safe to repeat.** The memory tool records an `insert` or a `str_replace` before it writes. The record holds the call id, the hash before, the hash after, and the result, in the state file of the scope. A repeated call whose note has the hash after returns the first result. A repeated call whose note has the hash before edits the note. A repeated call whose note has neither hash fails, because another write came between. So every call of the tool is safe to repeat, and an unattended run never waits for a person on a note.

**The notes that a run read.** A run records `fw.memory.used` for the notes that it read. `brief` names the notes that the block lists, after `fw.run.started`. `view` names one note that the memory tool shows. `recall` names a note that `recall` or `fw-memory` reads by its ref. The event holds refs and hashes, never text or the principal (`fw.RecordMemoryUse`). Without a front, `brief` holds refs without hashes. A note of the agent that a run of the scope `principal` reads has the ref of the scope `agent`, such as `mem://agent/triage/labels.md`, so the front counts the use on that note. The front counts the uses of each note from it.

**Comparison runs.** The metadata `fw.knowhow` with the value `off` makes a run a comparison run. A comparison run goes without what the agent learned, so a person can see what learning is worth. Such a run gets no memory tool and no recall tool, so it gets no memory block, neither the notes of its principal nor those of its agent. The routine runner does not act on it. `fw.run.started` records `knowhow: "off"`. A hosted run gets no memory tools either.

**Recall across Tasks.** With a front, the search of runs also asks the archive of the front (`GET /v1/knowhow/runs`). The archive gives the runs of the same agent and the same principal, within 5 seconds. Recall keeps one hit per run id, the hit of this Task first, and the bound of its hits. A ref of such a run reads the run that the search found. A front that does not answer adds a note to the result.

**Routines bound to a Duty.** A routine with `duty` counts only for a run whose metadata `workbench.duty` (`fw.MetaDuty`) names that Duty. `fw.routine.decided` carries `risk`. If the annotations of every tool of the steps and of the checks say read-only, the risk is `read`. Otherwise it is `write`, and a tool that the run lacks counts as a write.

### 7.9 Workspace

The workspace is the directory that the `files` tool and `bash` work in. It is `tools.files.root`, else the first binding under ax, else the working directory. The kernel owns the workspace as an `fw.Files`. A file tool gets it from the context of its call, so the kernel checks each name and records each change. With `tools.files.perRun`, the tools of each run work in a directory of their own under the workspace (below).

```go
type Files interface {
    fs.StatFS                                 // Open, Stat
    fs.ReadDirFS                              // ReadDir
    WriteFile(name string, data []byte) error // replaces the file and creates its parents
    Remove(name string) error                 // a file or a tree, never the root
    Rename(oldname, newname string) error     // fails when newname exists
}

func DirFiles(dir string) (Files, error)  // the default: a directory through os.Root
func WithFiles(f Files) Option            // another store, such as one in memory
func FilesFrom(ctx context.Context) Files // the workspace of a tool call
func RunFiles(events []Event) []RunFile   // the files that a run changed
```

A name follows `io/fs`: slash-separated, relative, and clean. `DirFiles` does each operation through `os.Root`, so no name leaves the directory, through a symbolic link either. The `memory` tool uses `DirFiles` on its own directory under `$FW_HOME`, outside the workspace, through the store of package `memory` (section 7.8).

The kernel checks the real name of each operation, after symbolic links, against these rules:

| Rule | Key | Default |
|---|---|---|
| Never read or written: the home directory of `fw`, the file ledger, and the configuration file (`Config.Reserved`) | none | always |
| Never read or written | `tools.files.deny` | `.env`, `.env.*`, `*.pem`, `*.key`, `.netrc`, `**/.git/config` |
| Read, never written | `tools.files.readOnly` | `.git` |
| Largest file that is read or written | `tools.files.maxFileBytes` | 8 MiB |

A pattern without a slash matches one part of the path at any depth, as in `.gitignore`. For example, `.git` matches the directory `.git` and every path under it, and `*.pem` matches `certs/web.pem`. A pattern with a slash matches from the root, and `**` matches any number of directories. A list in `fw.yaml` replaces the default list. The variables are `FW_TOOLS_FILES_DENY` and `FW_TOOLS_FILES_READ_ONLY`, with commas between the patterns, and `FW_TOOLS_FILES_MAX_FILE_BYTES`. A listing leaves out each path that the rules refuse.

#### A directory for each run

`tools.files.perRun`, `FW_TOOLS_FILES_PER_RUN` in the environment, gives each run a directory of its own: `<root>/runs/<run id>`, where `<root>` is the root of the workspace. That directory is the root of the `files` tool of the run and the directory where its `bash` shell starts. The shell finds the path in `FW_RUN_DIR`, and `fw.RunInfo.Dir` gives it to a tool. The kernel creates the directory at the first tool call or attachment of the run. A run of a later person therefore starts in an empty directory, never in the clone, the output, or the files of an earlier run.

| Run | Directory |
|---|---|
| A run that a caller starts | `runs/<its id>` |
| A child run in this process (section 11.9) | the directory of its parent, so a whole tree works on the same files |
| A branch in this process (section 4.7) | the directory of its source, like a follow-up of the source |
| A branch or a child in another process | a directory of that process, which starts empty |
| A run with `fw.InFilesRoot`, such as the bootstrap goal of an ax workspace (section 11.3) | the root |

Only the kernel makes a run a child or a branch, so metadata that names `fw.forkOf` in a request does not give a run the directory of another run. Without `tools.files.perRun` every run works in the root, and `FW_RUN_DIR` names the root.

The default is on under ax and off elsewhere. Under ax, one Task serves the runs of many people one after another on one durable volume, one run at a time (`maxRuns` 1, section 11.9). A run then must not read the output of an earlier run, push it, or start from its stale clone. A developer's `fw run` and `fw serve` work in a project directory instead, where the `files` tool must see the project itself. A Task whose Workspace prepares a checkout for every run, with `spec.git` and a goal, either sets `perRun: false` or names the checkout in its prompt: `bash` reaches it, and the `files` tool does not.

The paths in `fw.file.changed` and in the references of attachments stay relative to the root of the workspace, such as `runs/<id>/out/report.md`, so the files of a run read the same through every surface (below). The label of an attachment names the path in the directory of the run, where the tools of the run find it. The kernel removes no directory of a run, because the files of a finished run stay readable. A deployment that keeps a Task for long removes old run directories itself. If the root is a git checkout, add `runs/` to its `.gitignore`.

`bash` is not confined to the directory of its run. A shell can change to any directory that its user can enter, the root and the directories of other runs of the Task included. The directory of a run keeps the runs apart for the tools that respect it: the `files` tool, which opens the directory itself as its root so that no name and no link leads out of it, and a shell that stays where it starts. The boundary against a shell is the Task and the tool user (section 7.12).

#### Changes in the ledger

Each write, removal, or rename through the workspace during a tool call appends one `fw.file.changed` event. The event holds the call id, the tool, the operation, and the real path. It also holds the old path of a rename, the size, and the SHA-256 of the content. The operation is `created`, `changed`, `removed`, or `renamed`. The event never holds the content. The kernel writes the file before it appends the event. A suspend under ax therefore never leaves an event for a file that is not on the volume. `bash` changes files with no event, because the kernel does not see inside a shell.

`fw.RunFiles` reads the events of a run and gives one entry for each path that the run changed, with the last change. A file that the run created and then changed stays `created`.

#### Reading the files of a run

| Surface | Call |
|---|---|
| Native API | `GET /v1/runs/{id}/files` lists the files. `GET /v1/runs/{id}/files/{path}` returns the content, or a view for the dashboard with `Accept: application/json`. With `?download=1`, it returns the content under any `Accept`. |
| MCP | The tool `run_file` with `runId` and `path` returns the view. With `raw` true, it returns the content as an embedded resource. The resource template `fw://runs/{id}/files/{+path}` reads the content of a run of this process. |
| Dashboard | The result of a run lists the files that it changed. A tap opens a file screen with a Download action, which reads the raw content with `download=1` or with `run_file` and `raw`. |

The workbench serves the MCP and Dashboard rows (the workbench architecture, sections 3 and 6).

A caller who can view the run (`run.view`) can read the files that the events of the run list, and nothing else. One workspace serves every run, so a path that no event of the run lists answers 404. The rules of the workspace apply to the read too. The content is the current file. If its SHA-256 differs from the last change of the run, the view says that the file changed after the run. The raw content carries `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, and `Content-Security-Policy: sandbox`, so a file never runs script on the origin of the dashboard. The routes and the tool reach the member that owns the run, as the other run routes do (section 8.6).

`download` takes `1`, `true`, `0`, `false`, or an empty value. Any other value answers 400. The raw content names the file in `Content-Disposition` as RFC 6266 says. Control characters and Unicode format characters, such as U+202E, leave the name. A path separator becomes `_`. The parameter `filename` holds an ASCII form of the name. In that form, `_` replaces each quote, each percent sign, and each character that is not printable ASCII. If that form differs from the name, `filename*=UTF-8''...` holds the name in UTF-8.

The MCP server of the workbench returns the raw content up to 4 MiB (4194304 bytes), through `run_file` with `raw` and through the resource template. The bound of `tools.files.maxFileBytes` applies first. Both reads go through `view.ReadRunFileUpTo` of the package `surface/view`. It refuses a larger file at `Stat`, before the read. The error names the size, the bound, and the native route that serves the file in full. Then it reads at most 4 MiB and one byte, so a file that grows after `Stat` stays bounded. The embedded resource holds the bytes in base64 with the sniffed media type, at the URI `fw://runs/{id}/files/{path}`. The text names the path, the size, and the SHA-256. If the file changed after the run, the text says so. A `resources/read` of the template answers a UTF-8 file without a NUL byte as text and every other file as a blob. The template uses `{+path}`, the reserved expansion of RFC 6570, so both `docs/a.md` and `docs%2Fa.md` match it.

#### What the workspace does not do

The workspace is not a virtual file system. Files stay on the disk, where `bash` and `git` see them, and the ledger holds no content. Under ax, `/workspace` survives a suspend. A file on one fleet member is not on the other members. The kernel does not watch `bash`, keep versions of a file, or search the content of files.

### 7.10 Attachments

An attachment is a file that a person sends with a message, such as a screenshot, a log, or a spreadsheet. The kernel keeps the bytes in the workspace under `attachments/`, in the directory of the run with `tools.files.perRun` (section 7.9), and the ledger holds a reference. A replay, a bus message, or a projection therefore never carries the bytes.

```go
type FileRef struct {
    Path      string // the real path in the workspace, such as attachments/chart.png
    Name      string // the name that the person sent, cleaned
    MediaType string // sniffed from the bytes, never taken from the caller
    Size      int64
    SHA256    string
}

func Attachment(name string, data []byte) Block // a new file for Start or Send
func WithAttachments(files ...Block) RunOption  // the files of the first message
```

A block of type `file` holds a `FileRef`. `Start` and `Send` take the blocks that `Attachment` makes. The kernel writes each file through the workspace and puts the reference in `fw.run.queued` or `fw.message.received`. If another file has the same name, the new file gets a number, such as `chart-2.png`. The same bytes keep their path. Each file appends one `fw.file.changed` event without a call id, so the files of the run list it.

The rules of section 7.9 apply. A denied name, such as `.env`, fails the message as an invalid request, and so does a file over `tools.files.maxFileBytes`. A message holds at most 20 files and 32 MB. Only the kernel makes a reference, so a file block from a caller must hold the bytes.

For each model request, the kernel reads each file again. A label names the file, its type, its size, and its path. The model then gets this:

| The file | What the model gets |
|---|---|
| A type that the model reads, such as PNG or PDF | The label and the bytes, as an image or a document block |
| Text of 256 KB or less | The label and the text |
| Any other file | The label, and the agent reads the file with a tool |

An adapter lists the types that it reads through `fw.MediaTyper`. The Anthropic and Gemini adapters read PNG, JPEG, GIF, and WebP images and PDF. The OpenAI adapter reads the same images, and PDF only from api.openai.com. Its option `mediaTypes` changes the list, and `none` suits a server that reads text only. If a file changed after the person attached it, the label says so. A file that is gone becomes a note, and the run goes on. fw does no text extraction, no OCR, and no image processing on the server.

| Surface | Call |
|---|---|
| Native API | `files` on `POST /v1/runs` and on `POST /v1/runs/{id}/messages`: a list of objects with `name` and `data`, and `data` in base64. A body with files can hold them in base64 and 1 MB more. |
| MCP | `files` on the tools `run` and `send`, in the same shape. The server takes a request of the same size. |
| Dashboard | The + button of the home composer or of the message bar, a paste, or a drop. A draft keeps its files up to about 1 MB across a reload. The page sends a photo over 2 MB as a JPEG of at most 2048 pixels on its long side. The chat shows a picture in the message, and a tap opens any file. |

The workbench serves the MCP and Dashboard rows (the workbench architecture, sections 3 and 6).

A2A and the bus take no attachments yet.

### 7.11 Acting for a person

By default, an MCP server entry with `auth.oauth` acts as the agent itself, through the `client_credentials` grant (section 7.3). One service account then makes every call, and the service never learns who started the run. The setting `auth.oauth.grant: user` changes that. The entry then acts for the person who started the run. Each call carries a token that the authorization server issued for that person, so the service applies the rights of that person.

The kernel holds one primitive for this: a tool call that acts for a person pauses until a credential of that person arrives from outside. The kernel keeps no grant, no refresh token, and no OAuth flow. A linked package keeps the grants of people, runs the OAuth flows, and gives the kernel the access tokens of a person through `fw.PersonTokens`, which it sets with `Agent.SetPersonTokens`. The workbench is that package (the workbench architecture, section 11). A process without one tells the model that it keeps no connections of people.

#### Configuration

| Key | Variable | Meaning | Default |
|---|---|---|---|
| `tools.mcp[].title` | `FW_TOOLS_MCP_<NAME>_TITLE` | The name of the service for a person, such as `Tickets` | the entry name |
| `auth.oauth.grant` | `FW_TOOLS_MCP_<NAME>_AUTH_OAUTH_GRANT` | `client` acts as the agent. `user` acts for the person who started the run. | `client` |
| `auth.oauth.clientId`, `auth.oauth.clientSecret` | `..._AUTH_OAUTH_CLIENT_ID`, `..._AUTH_OAUTH_CLIENT_SECRET` | With `grant: user`, an empty client id uses the client of the browser sign-in (`auth.oidc`). A value gives the entry a client of its own. The secret is a `Secrets` name. | the sign-in client |
| `auth.oauth.issuer` | `..._AUTH_OAUTH_ISSUER` | The authorization server of a client of its own | discovered |
| `auth.oauth.authorizeUrl`, `auth.oauth.tokenUrl` | `..._AUTH_OAUTH_AUTHORIZE_URL`, `..._AUTH_OAUTH_TOKEN_URL` | The two endpoints. A value skips discovery. | discovered |
| `auth.oauth.scopes` | `..._AUTH_OAUTH_SCOPES` | The scopes of the grant | none |
| `auth.oauth.offline` | `..._AUTH_OAUTH_OFFLINE` | Ask for the scope `offline_access` | `true` |
| `auth.oauth.authParams` | `..._AUTH_OAUTH_AUTH_PARAMS_<KEY>` | More parameters of the authorization request, such as `access_type: offline` | none |
| `auth.oauth.exchange.audience`, `auth.oauth.exchange.scopes` | `..._AUTH_OAUTH_EXCHANGE_AUDIENCE`, `..._AUTH_OAUTH_EXCHANGE_SCOPES` | Get the token for this server through token exchange (RFC 8693), with this audience | off |

The kernel checks `grant` and keeps the other keys for the package that keeps the connections, which reads them from the configuration of the agent. The store of the grants is configured there too (the workbench architecture, section 11).

| Key | Variable | Meaning | Default |
|---|---|---|---|
| `tokens.url` | `FW_TOKENS_URL` | The address of a token broker, the front of the workbench. Empty keeps the tokens of people off, unless a linked package sets them. | empty |
| `tokens.credential` | `FW_TOKENS_CREDENTIAL` | The `Secrets` name of the bearer token that this process presents to the broker | `auth.tokenEnv` |
| `tokens.subject` | `FW_TOKENS_SUBJECT` | The bus subject on which the broker announces a finished connection and a disconnect | `fw.connections` |

#### How a call pauses

1. A call needs a token of the person. `PersonTokens.Token` returns an error that wraps `ErrNotConnected` or `ErrConnectionExpired`, or the service refused the tokens of the person. The MCP client calls `fw.RequestConnect`. The kernel appends `fw.approval.requested` with `connect`: the server, its title, and the reason, `missing` or `expired`. The run moves to `input-required`.
2. Every surface shows the request. The dashboard and the MCP server of the workbench show a Connect card and a URL-mode elicitation (the workbench architecture, sections 2.1 and 3.4).
3. When the connection of the person ends, the package calls `Agent.Connected` with the subject and the services that are now connected. The kernel decides every pending connect of that person for those services as the process, and the paused calls go on.

A decision from a surface that allows a connect request is refused, because a click is not a grant. A deny skips the service. The call then fails with a plain message to the model, and the run does not ask again for that service.

#### Tokens for a server

The MCP client keeps one MCP session per person and server, because a server can bind a session to the person of its first request. Before each request, it asks `PersonTokens.Token` for the token of the person. A 401 from the server drops that token through `Reject` and sends the request once more with a new one. When the server refuses again, the client forgets the grant through `Forget`, closes the session, and asks the person to sign in again. Such a failure never counts toward the circuit breaker of the entry.

The tool list of a server is shared. Until a person with a grant lists the tools, the server shows one tool, `<server>.connect`. A call of that tool asks the person to connect, and the real tools appear in the next step.

A run without a person gets an error from such a tool, never the identity of the agent. A workload, the static token, a bus intake, and an anonymous caller have no person.

#### The token broker

A grant has one holder, and a Task is the wrong holder: ax saves the volume of a Task on a suspend, and a restore brings back a refresh token that the server already replaced. In a fleet, the front keeps every grant and runs every refresh, and each Task gets the tokens of a person from it. The kernel package `tokens` is that client. With `tokens.url` set, it is the `PersonTokens` of the process, and it defines the protocol, which the package `connections` of the workbench serves (the workbench architecture, section 11.9).

The client sends `POST /v1/tokens` to the broker with the subject of the person, the server, and the id of the run in the context of the call. It presents the credential of `tokens.credential`, by default the static token of `auth.tokenEnv`, so the broker sees a member of the fleet and never a person. The broker finds the run in its fleet records, checks that the person started it, and answers with the token and its expiry. The client keeps the token until shortly before the expiry, and for five minutes at most. A refusal carries a reason: `not-connected` and `expired` become `ErrNotConnected` and `ErrConnectionExpired`, so the call asks the person to connect, and every other reason is an error for the model. `POST /v1/tokens/reject` drops a token that the service refused, and `POST /v1/tokens/forget` drops the grant when the service kept refusing.

When a connection of a person ends at the broker, the broker publishes `fw.connection.connected` on the bus subject of `tokens.subject`, with the subject and the services that are now connected. The client of each member turns it into `Agent.Connected`, and the paused calls of that person go on.

A person can disconnect a service, and a service can keep refusing a grant. In both cases the grant ends, and the broker publishes `fw.connection.disconnected` on the same subject, with the subject and the services. The client of each member then drops every cached token of that person, because the services that share a grant lose it together. The next call of a run asks the broker again, and the run asks the person to connect. Without the notice, a Task kept acting for the person until the cached token expired. The refresh token never enters a Task. An access token does, and it lives an hour.

#### Restarts

A call that waits for a connection survives a restart. The service never saw the call, so the call runs again after the restart, without the question of section 4.5. If the person is still not connected, the call asks again. A skip before the restart still ends the call. The grants of people live in the package that keeps the connections, so the kernel loses none of them (the workbench architecture, section 11).

### 7.12 Tool processes

A tool process is a process that a tool starts and that a model drives: the shell of `bash`, and a stdio MCP server (section 7.3). It runs untrusted input: a file that a run reads can hold a prompt injection that asks for a command. This section says what such a process gets.

#### The tool user

`tools.user` names the user that runs every tool process, `FW_TOOLS_USER` in the environment. The value is a user name or a uid, with an optional `:group` or `:gid`, such as `nonroot`, `65532`, or `65532:65532`. A name, a bare uid, and a group name must exist in `/etc/passwd` and `/etc/group`. A `uid:gid` with two numbers needs no entry. The user and the group must not be root.

| Environment | Default |
|---|---|
| An ax Task, with `fw` as root | `65532`, the user `nonroot` of both images |
| An ax Task where root lacks a capability of the list below, such as gVisor under Agent Substrate | the user of `fw`, with a warning in the log |
| An ax boot without root, such as `fw serve --task-file` on a developer machine | the user of `fw`, with a warning in the log |
| Every other process | the user of `fw`, as before |

`none` runs the tool processes as the user of `fw` in every environment. A user that does not exist stops the start with an error that names `tools.user` and what to set. The start also stops when `fw` does not run as root, because only root starts a process as another user, and when the tool user is the user of `fw`, because that user keeps nothing from the tools.

Root also needs these capabilities in its effective set, which `fw.MissingToolCaps` reads from `/proc/self/status`:

- `CAP_CHOWN` gives files to the tool user.
- `CAP_DAC_OVERRIDE` reads and writes the files of the tool user.
- `CAP_KILL` stops a tool process at its time limit.
- `CAP_SETGID` and `CAP_SETUID` start a process as the tool user.

A sandbox can take these capabilities from root. The gVisor sandbox of Agent Substrate leaves root `CAP_KILL`, `CAP_NET_BIND_SERVICE`, and `CAP_AUDIT_WRITE` only, so a chown fails there, even in `/tmp`. If a capability is missing, an explicit `tools.user` stops the start with an error that names the capabilities. The ax default then gives the user of `fw`, with a warning that names them. On such a sandbox a shell can read FW_HOME, so a Task that reads untrusted input gets no shell. `fw` never falls back to its own user without a word. `fw doctor` names the tool user on the line `tools user`.

A tool process of a tool user runs with the uid and the gid of that user and with no other group. It gets the `HOME`, `USER`, and `LOGNAME` of the user. If `tools.userHome` is set, the home comes from it. Else it comes from `/etc/passwd`, and it is `/` for a `uid:gid` without an entry. The process cannot read the memory of `fw`, its environment in `/proc/<pid>/environ`, or its open files. The kernel of the sandbox checks the user for each of them. The `fw` binary also makes its process non-dumpable at start (`prctl PR_SET_DUMPABLE 0`), for every verb. Then a process of the same user without `CAP_SYS_PTRACE` cannot read these entries either, and `fw` writes no core file. A debugger therefore attaches only to an `fw` that it starts.

A stdio MCP server of a tool user also gets the environment of a tool process and starts in the root of the workspace, so a server such as `server-filesystem` cannot read the state of `fw` either. Such a server gets no secret from the environment of `fw`. Give a credential to an MCP server over HTTP instead, through `auth.bearer` or `auth.oauth`: the kernel calls that server and keeps the credential. Without a tool user, a stdio server keeps the whole environment of `fw`, as before.

The images create the user. `fw:static` has `nonroot`, uid and gid 65532, from its distroless base, and `Dockerfile.toolbox` creates the same user with the home `/home/nonroot`. The tool user cannot install system packages, and a bootstrap goal (section 11.3) cannot install them either. An agent gets Python packages and programs of conda-forge through the block `environment` (section 18). A heavy toolchain goes into an image built from `fw:toolbox`.

#### The home of the tool user and the root directory

`tools.userHome`, `FW_TOOLS_USER_HOME` in the environment, names the home of the tool user: an absolute path. At the start, `fw` creates the directory and gives it to the tool user with the mode `0700`. A link or a file in its place goes first, because the tool user can put one there in an earlier start. `fw` sets the mode first, while it owns the directory, and then changes the owner. Root changes the mode of a file of another user only with `CAP_FOWNER`, which `fw` does not need and a Task does not have. A home of the tool user with the mode `0700` stays as it is at the next start. The home must not overlap FW_HOME, the file ledger, or the configuration file, and it must not hold a directory that stays with `fw`. Each part of the kernel that starts a process as the tool user reads the user through `fw.ToolUserOf`. The shell, a stdio MCP server, and the managers of section 18 therefore get the same `HOME`. The hosted programs of section 19 keep their own home.

Under ax, with a tool user, `tools.userHome` is `/workspace/.home` by default, on the durable volume beside FW_HOME. The home of the image loses each write at the next suspend. Agent Substrate also shows a file of the image as a file of root (agent-substrate/substrate#2036), so the tool user cannot write `/home/nonroot`.

Agent Substrate gives the root directory `/` the mode `0700` at every boot (agent-substrate/substrate#2035). The tool user then reaches no workspace, and the start stops with `/ has the mode 700`. With a tool user and `fw` as root, the ax boot therefore opens `/`. It adds the read and search bits for the group and for other users, and it logs the change once. Root owns `/`, so the change needs no capability. Without a tool user, as in a sandbox that lacks the capabilities above, the boot changes nothing: `/`, `HOME`, and the owners of the files stay as they are.

#### The home of fw and the workspace

With a tool user, `fw` prepares the files at start, before it opens the ledger:

1. The home directory of `fw` and the directory of the file ledger belong to the user of `fw` and have the mode `0700`. The tool user can neither read nor write the ledger, `fw.yaml`, the profile files under `agents`, the memory, the skills under the home, or the markers of the ax boot.
2. Once, every file and directory of the workspace except those paths gets the tool user as its owner. The workspace is the root of the files tool, and under ax also `/workspace` and the path of each binding (`Config.Workspaces`). The walk follows no symbolic link and stays on one file system. A file with more than one hard link keeps its owner, because a link can lead to a file outside the workspace. The marker `$FW_HOME/tooluser` records the user, the roots, and the reserved paths, so a later start skips the walk. A change of any of them walks again, and so does an ax boot that prepared a workspace again, because the new files belong to `fw`.
3. Each directory from a root of the workspace down to a path of `fw`, such as `/workspace` for `/workspace/.fw`, stays with the user of `fw` and gets the mode `1775` with the group of the tool user. So do the root of the files tool and the directories above it in the workspace, and with `tools.files.perRun` the directory `runs` under that root. The kernel creates the directory of each run with the user of `fw`, the group of the tool user, and the mode `0775`, and refuses one that belongs to the tool user. A tool process adds and removes its own files there, but the sticky bit keeps it from renaming or removing `.fw` or the root of the files tool. It therefore cannot put a home of its own in place for the next start, or a link to FW_HOME in place of the root or the run directory that the `files` tool opens at each call.
4. A configuration file in the workspace, such as `./fw.yaml`, gets the mode `0600` and the user of `fw`. A configuration file elsewhere must not be writable by the tool user.
5. The start stops when the tool user cannot reach and write a root of the workspace, when it can rename a path of `fw` or a directory above it, or when it can read the file of the API token (below).

The kernel writes as the user of `fw`, so every file and directory that it creates in the workspace for a tool, such as an attachment or a file of the `files` tool, goes to the tool user at once. A new file goes to that user through its descriptor, so a link that a shell puts at the name meanwhile changes nothing.

The walk costs one `lstat` for each entry and one `lchown` for each entry that changes. In a container with runc on a local disk, it gave 200,000 files to the tool user in 1.1 seconds, so a checkout with a million files takes about 6 seconds. A file system call in gVisor costs several times more, so expect such a checkout to take from half a minute to a few minutes in an ax sandbox. The walk happens once per volume, because the marker and the owners are on the volume and survive a suspend. An ax Workspace that clones a large repository therefore pays it once, at the first start of its Task, and again only when the boot prepares that workspace again.

#### What the tool user does not cover

- A process that `fw` starts for the operator still runs as the user of `fw`: `spec.command` of a Task (section 11.3) and the processes of the guest services of `ax ssh` (section 11.12). A Task with `spec.debug: true` lets any caller that reaches port 80, a tool process included, start a process as the user of `fw`, which undoes the tool user.
- With FW_HOME under the root of the files tool, as with `tools.files.root: /workspace` under ax, and without `tools.files.perRun`, the `files` tool checks the real path of each name before it opens it. A shell that changes a directory into a link between the check and the open can lead the tool into FW_HOME. `fw` logs a warning at start for this layout. Put FW_HOME outside the root of the files tool for untrusted input.
- Every tool process of one Task runs as the same user. A shell of one run reads and changes the files of another run of the Task, also with `tools.files.perRun`. The boundary is the Task and the tool user.
- A credential that the egress gateway of the platform adds to a request reaches every process of the sandbox (below).

#### Credentials that the egress gateway adds

This is a known limit, and the kernel cannot remove it alone. The egress policy of an ax Task can tell the egress gateway of the platform to add a credential to each request to a host, such as a GitHub token in the `Authorization` header of every request to an MCP server. The gateway sees the sandbox, not the process that sends the request. It therefore adds the credential to a request from any process of the sandbox, a tool process of the tool user included. A shell can then call the credentialed service with `curl`, and no rule of the policy of `fw` asks a person first. A rule such as `ask: [github.create_pull_request]` guards the MCP tool that the model calls through `fw`. It does not guard the service against a shell in the same sandbox. A prompt injection in a file that a run reads is enough to start such a call. The sandbox has one network identity, so the kernel cannot tell the gateway which process sent a request. Until the platform tells the processes of a sandbox apart, treat every host with an injected credential as reachable by every tool process of the Task.

#### A Task for untrusted input

Input is untrusted when a run reads text that nobody checked: an upstream repository, an issue, a web page, a reply of a tool, or a file that a person attached. Such text can carry a prompt injection. For a Task that reads untrusted input, use this pattern:

1. Give the profile no shell tool. Leave out `bash` and every stdio MCP server whose tools run commands. With `tools.files.perRun`, the `files` tool works in the directory of the run alone.
2. Put the write tools on `ask`, so a person allows each change: the writes of the `files` tool, such as `ask: ["files: command=create", "files: command=str_replace", "files: command=insert"]`, and every MCP tool that changes a remote system, such as `ask: ["github.create_*", "github.merge_*", "github.push_*"]`.
3. Give credentials only to MCP servers that the kernel calls: an entry with `auth.bearer`, `auth.oauth`, or `auth.spiffe`. Its credential stays in the memory of `fw`, and every call through it meets the policy of `fw`. Do not let the egress gateway add a credential for such a Task, because then every process of the sandbox carries it.
4. Keep the tool user on, which is the default under ax, keep `spec.debug` off, and keep FW_HOME outside the root of the files tool.

A profile with `bash` and a credential that the gateway adds is the opposite of this pattern: a shell reaches every credentialed host without a question.

#### The environment

`fw.ToolEnv` builds the environment of a tool process. From the environment of `fw`, it leaves out these variables:

- `AX_TASK_YAML` and `AX_WORKSPACES_YAML`. The Task holds the value of every `spec.env` entry, such as `FW_API_TOKEN`, and a Workspace can hold a token in the URL of a repository.
- The credentials that the configuration names: `auth.tokenEnv`, `tokens.credential`, the `apiKeyEnv` of each provider, the key variables of the model adapters, `auth.bearer` and `auth.oauth.clientSecret` of each MCP server, and `auth.oidc.clientSecret`. Each name also goes with the suffix `_FILE`.
- The names in `FW_BASH_HIDE`, a list with commas between the names, and `FW_BASH_HIDE` itself.
- Every variable with a secret name, by the rule of the redactor (section 4.2), such as `GITHUB_TOKEN` or `DB_PASSWORD`.
- Every variable whose value holds a secret by the rules of the redactor: a value that `fw` resolved through `Secrets`, the value of a variable with a secret name, or text with the shape of a key, such as a JWT or a URL with a password.

Then it adds the variables that the configuration gives the tool on purpose, `tools.bash.env` for the shell. Such a variable wins over the same name from the environment. It can have a secret name, because an operator named it, and `fw` logs a warning at start for each such name. It never has a name from the list above or a value that `fw` resolved through `Secrets`, so a configuration cannot pass a credential of `fw` to a tool under another name. The redactor still replaces its value in every event. The shell also gets `FW_HOME`.

The metadata endpoints of section 11.1 answer every process in the sandbox. They replace with `***` the value of each `spec.env` entry with a secret name and the user information of each `spec.git` URL.

#### The API token in a file

The default `Secrets` reads `<NAME>` from the environment, then the file that `<NAME>_FILE` names (section 6.5). The static token can therefore come from a file: set `FW_API_TOKEN_FILE` instead of `FW_API_TOKEN`, or `<auth.tokenEnv>_FILE` for another name. Every reader of the token reads it this way: the `token` mode of `auth.mode`, the `ax` spawner, the token client of section 7.11, `fw doctor`, and the fleet of the workbench. A file keeps the token out of the environment of `fw` and out of a Task manifest. Make the file readable by the user of `fw` alone, with mode `0400` or `0600`. With a tool user, `fw` refuses to start when the tool user can read the file, for example a file of a Kubernetes Secret volume with the default mode `0644`; set `defaultMode: 0400` on such a volume.

## 8. Surfaces

### 8.1 One listener

Every surface mounts on one `http.ServeMux`. The listener speaks HTTP/1.1 and h2c, so gRPC and HTTP share the port. h2c is HTTP/2 without TLS. The routes are:

| Path | Surface | Present |
|---|---|---|
| `/v1/*` | Native API | always |
| `/.well-known/agent-card.json`, `/a2a/*` | A2A server | default on |
| `/metrics` | Prometheus (section 10) | always |
| `/healthz`, `/readyz` | Health | always |
| gRPC guest services of `ax ssh` (section 11.12) | ax debug | under ax with `spec.debug: true` |

The workbench mounts more routes on the same listener: `/mcp`, `/ui`, the browser sign-in, and the routes for people (the workbench architecture, section 2). Under ax, the metadata endpoints of the runner contract answer on a loopback listener of their own, `127.0.0.1:8099`, and not on this one (section 11.3).

```go
type Surface interface {
    Mount(mux *http.ServeMux, a *Agent) error
}
```

### 8.2 Native API

The native API is small. Every call returns JSON. Every error is an RFC 9457 problem document.

| Method and path | Purpose |
|---|---|
| `POST /v1/runs` | Create a run. Body fields: `agent`, `input`, `title`, `instructions`, `profile`, `budget`, `metadata`, `wait`, `files` (section 7.10), `check` (section 23), and `context`, the data blocks of the caller (section 7.8.4). A block past a bound gets a 400 that names the bound. A run with `check` calls tools and no model, and its `input` can be empty. A trusted caller, the static token or a workload, also sets `principal` and `principalKind`, the person that the run is for, and `id`, the id of the run (section 11.9). |
| `GET /v1/runs` | List the runs of this process. `?parent={id}` lists the children of a run. `agent`, `state`, `principal`, `batch`, `kind`, and `limit` narrow the list. `batch` matches the metadata key `fw.batch`. Without `kind`, the list leaves out the check runs, and `kind=check` lists the check runs alone (section 23). `GET /v1/fleet/runs` of the workbench lists the runs of a whole fleet. |
| `GET /v1/runs/{id}` | Get one run. |
| `GET /v1/runs/{id}/live` | Stream the text of the run as the model produces it, as server-sent events without ids (section 5.1). For the model call in flight and every later one while the client stays connected: `delta` with `{turn, text}` for each piece of the answer, joined to at most one event per 40 ms; `tool` with `{turn, name, callId}` when the adapter yields a tool call of the response; `done` with `{turn, stopReason}` when the model call ends, `error` for a call that failed; and `idle` with `{state}` when the run leaves the running state. A client that connects in the middle of a call gets the pieces from then on. A comment line every 15 seconds keeps proxies open. The stream ends when the run finishes or the client goes, and it drops a client that falls 64 events behind. It needs the viewer role on the run, as the events route does. A run takes 16 clients; one more gets a 429. The relay of the workbench forwards it as it forwards the events route. |
| `GET /v1/runs/{id}/events` | Stream the ledger as server-sent events. `Last-Event-ID` resumes from a sequence number. Under `Accept: application/json`, one page of events with `lines`, the short line of each event, as the MCP tool `run_events` of the workbench gives. |
| `POST /v1/runs/{id}/messages` | Send a follow-up message. Body fields: `content`, `files`, `budget`, which changes the budget of the run before the message (section 5.2), `from`, the party that the caller relays the message for (section 5.5), and `pinned`, which keeps the message through every compaction; any caller who can send sets it (section 5.5). The message records the subject of the caller in `by`, or the person of the header `Fw-On-Behalf-Of` (see On behalf of a person below). Only a trusted caller, the static token or a workload, sets `from`, as for `id` of `POST /v1/runs`; anyone else gets a 403. |
| `POST /v1/runs/{id}/reactions` | React to an event of a run, or to the run as a whole (see Reactions below). Body fields: `seq`, the event, `0` or absent for the run; `reaction`, `up`, `down`, or `heart`; `note`, at most 1 KB; `by`, the person; and `remove`, true to take the reaction of the person back. Only a trusted caller, the static token or a workload, sets `by`, as for `from` of a message; anyone else gets a 403. Without `by`, the caller reacts as itself, or for the person of the header `Fw-On-Behalf-Of`. The answer is 200 with the run object. A `seq` that the run does not have, an unknown reaction, and a longer note are a 400. It needs the operator role on the run. |
| `POST /v1/runs/{id}/quality` | Record the receipt of a system of record, such as a ticket that a person closed or a deploy that passed its checks (section 10.1). The body has four fields. `id` is a stable id of the receipt, at most 128 bytes. `verification` is `accepted`, `satisfied`, `rejected`, or `unknown`. `completion` is the sequence of the `fw.run.finished` that the receipt verifies, and `0` or absent means the latest completed end. `duplicateEffects` counts the effects that the system saw more than once. The kernel records the receipt as `fw.run.annotated` through `Agent.ObserveQuality`, and the answer is 200 with the run object. A retry with the same body writes nothing. A second verification of the same end, an unknown verification, and a run that never completed are a 400. It needs the operator role on the run. |
| `PATCH /v1/runs/{id}/budget` | Change the budget of a run (section 5.2). The body is a budget; each field that it sets replaces the same field for the work that follows. The answer holds `budget`, the effective budget after the change, and `run`, the run object. It needs the operator role on the run, as a decision does. The change records the caller, or the person of the header `Fw-On-Behalf-Of`, in `by`. |
| `POST /v1/runs/{id}/approvals/{approval}` | Decide a pending approval. Body fields: `decision`, `note`. The decision names the caller, or the person of the header `Fw-On-Behalf-Of`, as its decider. |
| `POST /v1/runs/{id}/cancel` | Cancel a run. Its `fw.run.finished` names the caller, or the person of the header `Fw-On-Behalf-Of`, in `by`. |
| `POST /v1/runs/{id}/fork` | Branch a run (section 4.7). Body fields: `seq`, the last event to keep, every event when absent; `input`, a message that follows the kept events; `model`, a selector for the branch; `metadata`; `spawn`, which runs the branch in another process through the spawner. The answer is the branch. With `spawn` the status is 202, and the branch is queued on its member until that process takes it. The branch belongs to the caller, or to the person of the header `Fw-On-Behalf-Of`. |
| `POST /v1/runs/fork` | Start a branch from the events that another process kept (section 4.7). Body fields: `id`, the id of the branch, a new one when absent; `events`; `input`; `model`; `metadata`. The same branch sent again under its id answers with the branch that the process has. For the static token or a workload alone. |
| `GET /v1/runs/{id}/files` | List the files that the run changed (section 7.9). |
| `GET /v1/runs/{id}/files/{path}` | The content of a file that the run changed, or a view of it under `Accept: application/json`. `?download=1` always returns the content (section 7.9). |
| `GET /v1/agents` | List agent profiles with their descriptions, instructions, and skills. |
| `GET /v1/tools` | List tools with their specifications. Each entry also has `text`, a plain phrase that says what the tool does. |
| `GET /v1/skills` | List skills, each with `origin`, `local` or `mcp`, and for a skill of an MCP server `server` and `uri` (section 7.3). |
| `GET /v1/skills/{name}` | The `SKILL.md` text, or JSON with the file list under `Accept: application/json`. The name of a skill of a server has a slash, such as `/v1/skills/github/review`; the kernel reads its text from the server and checks it (section 7.3). |
| `GET /v1/status` | The state of the agent at a glance: the usage of the runs that the caller can view, the oldest approval that waits, and the counts of runs by state (section 11.8). |
| `GET /v1/memory` | Search the memory of an agent (section 7.8.3). Query: `agent`, `default` when absent; `q`, the words; `kinds`, a comma list of `note`, `run`, and `skill`; `limit`, 10 by default and 50 at most. The answer holds `scope` and `hits`, and `notes` when a bound stopped a search early. The kind `file` belongs to the tool of a run, so the route refuses it. |
| `GET /v1/memory/notes` | List the notes of the scope of the caller in an agent, newest first, without their text. Query: `agent`. The answer holds `cut: true` when the walk stopped at its bound (section 7.8.1), so notes can be missing. |
| `GET /v1/memory/notes/{path}` | One note with its `hash`, the SHA-256 of the whole file, which the header `ETag` repeats. Under `Accept: text/markdown`, the text alone. A directory answers 400. |
| `PUT /v1/memory/notes/{path}` | Write the body as the text of a note. `If-Match` with the hash of the current note, quoted or weak, makes the write a compare-and-swap, `If-Match: *` writes only a note that exists, and `If-None-Match: *` writes only a new note; each answers 412 when the note differs. The store keeps its limits (section 7.8.1). With a front, a 412 of the front gives a 412 with the sentence of the contract. A front that does not answer gives a 503, and the copy stays as it was (section 7.8.4). |
| `DELETE /v1/memory/notes/{path}` | Remove a note, and its rows of the FTS index. A directory answers 400. |

The routes of the memory take the scope from the agent of the query and the principal of the request, never from a field (section 7.8.1). An agent that does not exist and an agent that the caller cannot view (`agent.view`) both answer 404 with the same text, as `GET /v1/agents/{name}` does, so the routes do not tell which agents exist; a caller who can view the agent and lacks the action gets 403. Reading needs `agent.run` on the agent. Writing or removing a note of the scope `principal` needs `agent.run`. Writing or removing a note of the scope `agent`, which every person of the agent reads, needs `agent.edit`. Writing or removing a note of the scope `shared`, which every profile of the process reads, needs the admin role of the process (`access.admins`), and the 403 says so. Each write and removal goes to the log with the subject of the caller, the operation, the ref, the scope, the size, and the SHA-256, never the text. These routes let a person, a script, or another harness seed and curate the memory of an agent without a run.

`profile` is an inline profile (section 12.5). Its fields are `system`, `instructions`, `skills`, `tools`, `model`, `effort`, `policy`, `budget`, and `compaction`, which takes `at`, `model`, and `off` (section 5.4). `budget`, at the top level, inside the profile, in a follow-up, and in `PATCH /v1/runs/{id}/budget`, takes the seven limits and the three fields of the final report of section 5.2. `deadline` and `reportTimeout` are duration strings, such as `"90s"`, or numbers of nanoseconds. Examples of a request that the kernel refuses are an unknown model name and a child past the spawn limits. Such a request gets a 400 or a 409 problem document that names the field.

`POST /v1/runs` with `wait: true` blocks until the run finishes or needs input. With the header `Accept: text/event-stream`, the same call streams events as they happen and then closes. The header `Idempotency-Key` makes the call safe to retry. A run object looks like this:

```json
{
  "id": "01JA3Q9Z6K2M4N8P0R1S2T3U4V",
  "agent": "reviewer",
  "title": "Review pull request 42",
  "state": "input-required",
  "input": "Review pull request 42",
  "output": null,
  "pending": { "id": "ap_7f3", "tool": "bash", "input": { "command": "git push" }, "reason": "rule: ask destructive", "requestedAt": "2026-10-13T09:14:31Z" },
  "usage": { "inputTokens": 48210, "outputTokens": 3120, "cacheReadTokens": 41000, "cacheWriteTokens": 2200, "turns": 7 },
  "seq": 61,
  "createdAt": "2026-10-13T09:12:04Z",
  "updatedAt": "2026-10-13T09:14:31Z"
}
```

`usage` sums the work of the run: the input, output, cache read, and cache write tokens of its model calls, its turns, its tool calls, and `costUSD`, the cost of the calls whose model has a price (section 5.2). `usage` never holds the work of a child run. `unpricedTokens` counts the tokens of the calls whose model has no price, because their cost is unknown, not zero. A price of zero is a price. The kernel records the usage of the run so far as `totals` on every `fw.run.finished` and `fw.run.suspended`, across follow-ups, so a run that finishes again carries more. The projection takes those totals in place of its own sum, and `usageSeq` is the sequence number of the latest event that carried them. The ledger gives the same usage with and without the totals, because they are the sum of the events before them. After a restart with another price table, the work before the latest totals keeps the cost at the prices of its time. A consumer that receives only the lifecycle events of a run therefore knows its usage each time the run finishes or is suspended (section 9.4). A branch counts its own work only (section 4.7).

`activity` says what a run that works does now: `turn`, the number of the open turn; `tool` and `callId`, the tool call that the run waits for, absent while the model works; and `since`, the time of the event that began it. `fw.turn.started` sets the turn and clears the tool. `fw.tool.requested` sets the tool and the call id. `fw.tool.responded` of that call clears the tool. `fw.turn.finished` and `fw.run.finished` clear the field. `fw.approval.requested` and `fw.run.suspended` clear it too. A decision of that approval, or a resume of a run that waits for no decision, brings it back while the turn is open. A call that asked a question then goes on with its tool. A run that waits or finished has no field. The ledger never holds it. The kernel keeps the words out of the run object: `view.ActivityPhrase` words the activity with the step phrase that a result lists (`view.StepPhrase`), such as `Ran "go test ./..."`, from the input of the `fw.tool.requested` with the same call id, or "Thinking" while the model works, with no number of the turn.

```json
"activity": { "turn": 7, "tool": "bash", "callId": "toolu_01", "since": "2026-10-13T09:14:02Z" }
```

`reactions` counts the reactions of people on the run and its events by reaction, such as `{"up": 2, "heart": 1}`, and holds nothing more. A run without a reaction has no field. The front of a fleet keeps the list of the reactions in its own store.

A run that started a child also has `treeUsage`, the usage of the run and of every run under it, as the process that holds the run knows it: its children in this process as they are now, and its children in other processes as each last reported its own tree. `treeOpen` counts the runs under it that did not finish, and `treeWorking` the runs of those that work: queued or running. `treeUsage` includes `usage`, so a sum over several runs, such as the usage of a job, adds `usage` and never `treeUsage`. The ledger never holds either field: the kernel sets them when it reports a run, so a parent in another process reads the whole tree of a child in one read (section 11.9).

The run states are `queued`, `running`, `input-required`, `completed`, `failed`, `canceled`, and `suspended`. Authentication defaults to a bearer token read from `FW_API_TOKEN`. The setting `auth.mode: none` must be explicit. The modes `oidc` and `spiffe` give every caller an identity, and section 13 says what an identity can do. The native API also serves `GET /v1/agents/{name}`. A relay can answer it first, for a profile that another process runs (section 12.8). The workbench serves `PUT` and `DELETE /v1/agents/{name}` and `GET /v1/whoami`, the caller with its roles and its teams.

An extension adds routes to this surface (section 12.8). The workbench adds the page, the browser sign-in, home, results, transcripts, the builder, the fleet, usage, and connections (the workbench architecture, section 2).

#### Run titles

`title` names the work in a few words, so a list reads "Fix checkout retries" and not only "reviewer". The metadata key `fw.title` (`fw.MetaTitle`) sets it. The field `title` of `POST /v1/runs` puts a trimmed title into that key. Such a title wins over a `fw.title` entry in `metadata`. A title of more than 200 characters is an invalid request.

Without the key, the run takes `fw.TitleFrom` of its input. That is the first sentence of the first line with text, without Markdown marks, cut at a word to 64 characters (`fw.MaxTitleRunes`). An input without text gives no title. The `status` projection computes this title from `fw.run.queued`, so a run in an older ledger gets a title too.

The kernel makes no title call. A title that a model writes comes from the workbench, which records it on the run as an annotation (the workbench architecture, section 9).

#### Annotations

`Agent.Annotate` adds metadata to a run after the kernel queued it, as the event `fw.run.annotated`. The metadata merges into the metadata of the run, and a later entry replaces an earlier one. A new `fw.title` entry changes the title of the run. The data can carry the usage, the provider, and the model of the model call that produced the metadata, such as a title call. The usage and the cost of the run then include that usage, and the metrics count it (section 10.1). With a principal, the caller needs the operator role on the run. The entries `idempotencyKey`, `fw.drift.system`, and `fw.drift.instructions` (section 12.5), and `fw.kind` of a check run (section 23), are the kernel's and cannot change. A request cannot set them either, on any surface. The metadata after the merge must fit the bounds of `fw.CheckMetadata`. The entries that the kernel sets, also `fw.forkOf` and `fw.forkSeq` of a branch (section 4.7) and the `fw.intake.*` entries of a job from the bus intake (section 9.3), do not count, so an entry of the kernel never makes a later annotation fail.

The kernel annotates a run itself when the text of its profile changed across a restart, with the keys `fw.drift.system` and `fw.drift.instructions` (section 12.5).

#### Reactions

`Agent.React` records the reaction of a person on an event of a run, or on the run as a whole, as the event `fw.run.reacted`: a thumb up, a thumb down, or a heart, with a note of at most 1 KB. With a principal, the caller needs the operator role on the run, as for an annotation. `by` is the subject of the caller, or the person that a trusted relay names (see On behalf of a person below). The process itself, the static token, or a workload can name another person in `by`, so the front of a fleet records the reaction of a person on a run of a Task; any other principal that names another person gets `ErrForbidden`. A reaction that the same person already put on the same event appends nothing, also with another note. With `Removed`, `remove` in the body of the route, the person takes the reaction back: the kernel appends `fw.run.reacted` with `removed` when the person has the reaction on the event, and nothing otherwise. A quality receipt that the thumb recorded stays, because a receipt is final. The run object counts the reactions in `reactions`, and a removal takes one from the count. The metadata of a run is one flat map with a bound on its entries, so a reaction is an event of its own and not an annotation: a run with fifty reactions can still take its next annotation.

A thumb on the `fw.run.finished` of a completed episode is also a quality receipt (section 10.1): `React` calls `Agent.ObserveQuality` with `accepted` for `up` or `rejected` for `down`, that completion, and the receipt id `reaction:<run>:<seq>:<by>`. A subject that makes the id longer than 128 bytes gives its place to the first 16 bytes of its SHA-256. The reaction then records `receipt: true`. When `ObserveQuality` refuses the receipt, because a receipt of another person or another thumb of the same person already verified the completion, the reaction still records, with `receipt: false`, and the log says why. A heart records no receipt, and neither does a thumb on any other event.

A reaction changes the state of a run in no way, as an annotation does. One that arrives after `fw.run.finished` leaves the run finished: the metrics and the traces open no run for it, the file ledger closes the file of the run again after it, and a restart does not resume the run. The file ledger syncs the event before `React` returns, as it does for an annotation.

An annotation changes the state of a run in no way. One that arrives after `fw.run.finished` leaves the run finished, as the record and the report of a child do (section 11.9): the metrics and the traces open no run for it, the file ledger closes the file of the run again after it, and a restart does not resume the run, because the rebuild reads the state of a run and not its last event (section 4.5).

#### On behalf of a person

People act on the runs of a fleet through its front, and the workers hold the runs. A trusted relay names the person with the header `Fw-On-Behalf-Of`. The ledger records that person. The relay decides for the person before it relays.

The front authenticates the person. It decides the action with the rules that decide what the person sees. Then it sends the request with the static token and the header `Fw-On-Behalf-Of: <subject>`, the subject of the person. The worker authorizes the static token, as for any request. It never authorizes the named person, so the header neither widens nor narrows what the caller can do. `fw.PrincipalFrom` still returns the caller. The bearer token of a person therefore never reaches a worker.

Every route of the native API reads the header after the authentication, and so does every route of an extension. The header names the person in these records:

| Route | Record |
|---|---|
| `POST /v1/runs/{id}/approvals/{approval}` | `decider` of `fw.approval.decided` |
| `POST /v1/runs/{id}/messages` | `by` of `fw.message.received`, and `by` of `fw.budget.changed` for a budget in the body |
| `PATCH /v1/runs/{id}/budget` | `by` of `fw.budget.changed` |
| `POST /v1/runs/{id}/cancel` | `by` of `fw.run.finished`. A run under the canceled run records no person. |
| `POST /v1/runs/{id}/reactions` | `by` of `fw.run.reacted`. A `by` in the body wins. |
| `POST /v1/runs/{id}/fork` | the principal of the branch, with the kind `user` |

Only the static token or a workload names another person, as for `from` of a message. Any other caller that names another subject gets a 403. A caller that names its own subject changes nothing. An empty header changes nothing too. The value holds at most 256 bytes and no control character, and it is UTF-8. A longer value, a control character, a value that is not UTF-8, and a second header are a 400. `POST /v1/runs` takes the person of a new run from the body fields `principal` and `principalKind`, not from the header. A Go caller names the person in a context with `fw.WithActingFor` and reads it with `fw.ActingFor`.

### 8.3 A2A server

The agent card is served at `/.well-known/agent-card.json`. The card takes `name` and `description` from the default profile and lists one A2A skill per profile. The field `supportedInterfaces` lists the JSON-RPC binding at `/a2a/jsonrpc` and the REST binding at `/a2a`. `SendMessage` creates a run or appends a follow-up. `SendStreamingMessage` streams the `a2a` projection. `GetTask`, `ListTasks`, `CancelTask`, and `SubscribeToTask` read projections. Run states map to A2A task states as follows:

| Run state | A2A task state |
|---|---|
| `queued` | `TASK_STATE_SUBMITTED` |
| `running` | `TASK_STATE_WORKING` |
| `input-required` | `TASK_STATE_INPUT_REQUIRED` |
| `completed` | `TASK_STATE_COMPLETED` |
| `failed` | `TASK_STATE_FAILED` |
| `canceled` | `TASK_STATE_CANCELED` |
| `suspended` | `TASK_STATE_WORKING` |

Push notification configurations are stored as run metadata. The `bus` projection or a webhook delivers them. The server library is `github.com/a2aproject/a2a-go/v2`.

### 8.4 MCP server

The MCP server is part of the workbench. `fw-workbench` serves it at `/mcp` over Streamable HTTP, and `fw-workbench mcp` serves it over stdio (the workbench architecture, section 3). The kernel keeps the origin `mcp` in its events and its metrics, so a run that comes through the MCP server is an ordinary run.

### 8.5 Health and metrics

`GET /healthz` returns 200 once the process is up. `GET /readyz` returns 503 until the workspaces are prepared, then 200. After a storage failure, `GET /readyz` returns 503 again until the process restarts, `GET /v1/status` reports the process blocked and idle since the failure, and the gauge `fw_storage_blocked` is 1 (section 5.6). `GET /metrics` serves the Prometheus registry (section 10.1). The route carries no authentication, because a scraper has no token. Keep it inside the monitoring perimeter. Under ax, `/readyz` drives the `WorkspaceReady` condition.

### 8.6 Fleet

The fleet is a feature of the workbench (the workbench architecture, section 4), and the kernel knows no fleet. The kernel gives a linked package three hooks for it. A service runs beside the agent for as long as the agent lives (section 12.4). A relay of the native surface answers a request about a run that another process holds (section 12.8). It can also answer for a profile that another process runs. A collector adds gauges to the metrics surface (section 10.1). A fleet member reads the credential of a caller from `fw.CredentialFrom` and acts on the caller's behalf at the member that owns a run. A front can also decide for a person itself and relay the action with the static token and the header `Fw-On-Behalf-Of` (section 8.2). Then no credential of a person leaves the front. When another process holds a run, the field `member` of the run object names that process. The kernel never sets it.

## 9. Message queues

### 9.1 Interface

```go
type Bus interface {
    Publish(ctx context.Context, subject string, e Event) error
    Subscribe(ctx context.Context, subject string, h func(Event) error) (stop func(), err error)
    Consume(ctx context.Context, queue string, h func(Event) error) (stop func(), err error)
}
```

`Subscribe` is fan-out. `Consume` is a durable queue with at-least-once delivery. The handler returns `nil` to acknowledge a message and an error to request redelivery. Every message on the bus is a CloudEvents JSON object with the envelope from section 4.1.

A driver can also implement `GatedBus`, a queue consumer that asks a gate before it takes messages:

```go
type GatedBus interface {
    Bus
    ConsumeGated(ctx context.Context, queue string, g Gate, h func(Event) error) (stop func(), err error)
}

type Gate interface {
    Admit(ctx context.Context, max int) (int, error)
    Unused(n int)
}
```

Before each read of the queue, the consumer calls `Admit` with the number of messages that it can handle at that moment, reads at most the admitted number, and gives back with `Unused` the admissions that it got no message for. A message that the consumer drops without the handler, such as one that does not decode, gives its admission back too. The owner of the gate frees the admission of a message when the handler returns. The messages that a consumer does not take stay on the queue for the other consumers. The bus intake uses it to take a job only for a free run slot (section 9.3). The `nats` and `redis` drivers implement it. The stop function of a gated consumer ends a wait in `Admit`.

The bus has no other contract. An earlier revision let a driver keep the last event of each subject for the records of the fleet; the fleet reads the platforms now (the workbench architecture, section 4), so that contract went.

### 9.2 Drivers

| Driver | Library | Notes |
|---|---|---|
| `mem` | standard library | Default. In-process only. |
| `nats` | `github.com/nats-io/nats.go` with JetStream | Recommended. One binary gives streams, a key-value store, and an object store. |
| `redis` | `github.com/redis/go-redis/v9` with Streams | Reuses the Redis that ax already deploys. No new infrastructure. |
| `kafka` | `github.com/twmb/franz-go` | Not implemented yet (section 16). |

A `nats` bus opens without its server, such as in a golden boot without network, and connects in the background. Until it connects, every publish fails at once with `nats: not connected yet`, on the stream path and on the core path. Before its first connection, the client of NATS refuses every message with headers, and the error then names the headers and not the cause. The caller therefore tries again later, as the egress (section 9.4) and the environment (section 18.6) do.

### 9.3 Intake

The setting `bus.intake` names a queue, for example `fw.jobs.reviewer`. Each message is a run request with the same body as `POST /v1/runs`. A request with `check` starts a check run (section 23). A request from the queue keeps the `principal` and the `principalKind` that it names, because the bus is a channel of the process, so the front of a fleet sends the work of a person to a worker and the run acts as that person (the workbench architecture, section 4.9). If the bus is out of reach when the process boots, the consumer does not start, and the kernel logs a warning and tries again in the background, with a pause that doubles up to 30 seconds, while the process serves its API and its health. A sandbox whose egress policy takes effect after the first packet, and a bus server that restarts, both need this.

#### One job for each free run slot

The intake takes a message only when a slot of `maxRuns` is free. Under ax that is one slot, so a Task holds one job at a time, and the other Tasks of the queue take the other jobs. The drivers of top-level runs fill the slots, and a driver that waits for a slot counts too. A run that waits for the decision of a person on a held tool call has no driver, so its slot is free, and the Task takes its next job while the person decides. A tool that waits for an answer inside its call keeps its driver and its slot. The intake consumes through `GatedBus` (section 9.1), so a message that the Task cannot start stays on the queue. Each message that the gate admits also holds a reserved place in the backlog of `maxRuns + maxQueuedRuns` (section 5.6) until its handler returns, and the start of its run takes that place over. A start from another surface cannot take the place, so a job that the intake took is never refused for saturation. A run that another surface starts at the same moment can still take the free working slot first. The run of the job then waits in this process for the next slot. A bus driver without `GatedBus` can deliver faster than work starts, so its handler first waits for a place in the backlog and takes it the same way: a full backlog never makes such a bus drop a job, as the in-memory bus does after three failures of a handler. A shutdown, a storage failure, and the end of the kernel context during a start return an error for redelivery rather than dead-lettering a valid job.

The option `concurrency` of the `nats` and `redis` drivers is the number of messages that one consumer handles at a time. Its default is `maxRuns`: 16 when the configuration leaves `maxRuns` at 0, and 1 under ax. `bus.options.concurrency` sets another value.

#### The acknowledgement

The handler acknowledges a message as soon as `fw.run.queued` of its run is in the ledger. The file ledger syncs that event for every origin before the append returns. The handler does not wait for the end of the run. A run that waits for a person therefore holds no message, the front that suspends an idle Task leaves no message whose acknowledgement times out, and a shutdown waits for no handler.

From then on the ledger holds the job, and the bus does not deliver the message again. A shutdown or a storage failure that begins after `fw.run.queued` is in the ledger does not refuse the start (section 5.6): the handler acknowledges the job, and the run waits in the ledger. A run that a shutdown, a suspend, or a crash interrupts resumes from the ledger at the next start of the same process (sections 4.5 and 11.5), and no other process starts a second run of it. The message id is the idempotency key when the request names none, so a message that comes back to the same process returns the first run. Two cases still deliver a message again. A message that arrives while the kernel shuts down starts no run, and the handler returns an error, so another process can take it. A process that stops between the append and the acknowledgement leaves the message on the queue, and another process can then start a second run of the job, because the idempotency key holds in one process only.

#### Retries

`bus.intakeRetries` is the number of times that the kernel puts a job on its queue again after the run of the job failed. The default is 0, because each retry spends model tokens again. A retry is a new message of the type `fw.run.request` on the same queue, so any process of the queue can take it. It carries the request of the failed run, with the bytes of its files read again from the workspace, an idempotency key of its own, `<failed run>.retry`, and two metadata entries: `fw.intake.attempt`, the number of the attempt, 2 for the first retry, and `fw.intake.retryOf`, the id of the failed run. A file that changed or went after the run started stops the retry, and the job goes to the dead letter with that error. A canceled run and a completed run get no retry.

#### Dead letter

The subject `<intake>.dead`, for example `fw.jobs.reviewer.dead`, receives a record for every job whose run failed after its last attempt, and for every request that cannot start: a message that is not a run request, a profile that does not exist, a run without a model, and every other refusal of the start. The record is the event `fw.intake.dead`, which is not a ledger event. Its data has these fields:

| Field | Value |
|---|---|
| `queue` | The intake queue. |
| `request` | The run request. For a failed run, the request as `fw.run.queued` recorded it, with the idempotency key and the attempt metadata, and with references in place of the bytes of its files. For a request that cannot start, the request as the message carried it, without the bytes of its files, or the data of the message when it is not a run request. |
| `run` | The id of the failed run. A request that cannot start has none. |
| `message` | The id of the bus message of a request that cannot start. |
| `outcome` | `failed` for a run that failed after its last attempt, `rejected` for a request that cannot start. |
| `error` | Why, such as `budget exceeded: deadline`. |
| `attempts` | The attempts of the job, this one included. |
| `node` | The process that sent the record (the key `node`). |

The envelope has the same `node`, the run as `subject`, and the id `<run>.dead`, or `<message>.dead` for a request that cannot start, so a bus that drops a repeated message also drops a repeated record. A record costs nothing, so the dead letter is on by default, and `bus.deadLetter: false` turns it off. The `nats` driver adds the subject to its stream when the stream does not capture it already, so the stream stores the records. The `redis` driver keeps every subject in a stream of its own. A queue with a wildcard has no subject for a retry or a dead-letter record, and the kernel logs a warning when the intake starts.

If the dead letter refuses the record of a request that cannot start, the handler returns an error, so the bus delivers the message again. Otherwise the handler acknowledges such a message, because the bus delivers it again and again until the handler does.

#### What a failed job owes

The intake marks each run that it starts with the metadata entry `fw.intake.queue`, the queue of the job. When the run fails, the kernel sends the retry or the record, and then annotates the run with `fw.intake.outcome`: `retried`, `dead-lettered`, or `dropped` when the dead letter is off. A failed run with `fw.intake.queue` and without `fw.intake.outcome` still owes its retry or its record. The kernel tries again while the bus refuses, with a pause that doubles up to 30 seconds, and a restart sends it from the ledger. A run from the bus of an older kernel has no `fw.intake.queue`, so an upgrade sends nothing for its old failures. A branch of a job (section 4.7) is no job: it gets no retry and no record. A request cannot set `fw.intake.outcome`, because the intake removes the entry. The `fw.intake.*` entries do not count against the bound of the metadata (section 8.2).

A person can continue a run that failed on its budget, with a follow-up or a change of its budget (section 5.2). The intake acts at the failure, so the retry or the record of such a run is already out when the person continues it. The dead letter then names a run that can still finish, and a retry works on the same job next to it. A consumer of the dead letter reads the state of the run before it acts on a record. An operator who continues failed runs by hand keeps `bus.intakeRetries` at 0.

### 9.4 Egress

The setting `bus.egress` names a subject prefix, for example `fw.events`. The `bus` projection publishes each ledger event to `<prefix>.<atespace>.<agent>.<type>`. The setting `bus.egressTypes` limits the published types. Each event carries `node`, the name of the process from the key `node` of the configuration, so a consumer that follows many processes knows which one holds the run; the front of a fleet reads the stream this way (the workbench architecture, section 4.8). Consumers include audit, billing, dashboards, and other agents.

With the file ledger, the ledger is the queue of the egress. The observer only names the run of each new event. One goroutine reads the events after the cursor of each run from the ledger, publishes them in order, and saves the cursor, so a ledger append never waits for the bus. A cursor counts only the events that the bus confirmed or that the filter skipped. If the bus refuses, the goroutine tries again with a pause that doubles up to 30 seconds. A boot without the bus loses no event, and a shutdown sends what waits while the bus answers. The boot after a crash or a suspend sends what the bus did not confirm, such as the final `fw.run.finished` of a run that a suspend cut. A custom ledger gets the same behavior when it implements `fw.EgressCursors`. A ledger without cursors, such as the memory ledger, keeps the events in a queue in memory instead: past 10,000 events the queue drops the oldest, and a crash loses what is queued.

The cursors are files under `<ledger>/.egress/`, one for each route and run, apart from the runs. A route is the prefix, the atespace, and the filter of the types. A change of any of them starts new cursors, so the new route sends the history of the ledger again. The first boot of a kernel with cursors on the ledger of an older kernel sends the history too, final events included. The events behind a cursor reach the disk before the cursor does, so a crash can repeat an event but never skips one. A cursor that no longer matches the ledger, for example after a recovery cut the run, goes, and the egress sends the run again from the start. The delivery is therefore at least once: a crash, a failed save of a cursor, or a confirmation lost on the way sends an event again under the same id. A consumer drops an id that it has seen, and applies the events of a run by their sequence number.

A pass of the egress reads only the runs with new events, from the line of their cursor, and publishes at most 128 events of one run before it goes on to the next run, so a long run cannot hold back the others. A pass over every run comes at the boot and then once a minute. A run whose ledger cannot be read waits for that pass, and the other runs go on.

A consumer that applies the events of a run with `Run.Apply` gets the `activity` of the run object (section 8.2) only when the stream carries `fw.turn.started`, `fw.tool.requested`, and `fw.tool.responded`. With `fw.turn.finished` too, the activity ends with the turn; without it, it ends at the next approval, suspend, or end of the run. `fw.tool.requested` carries the input of each call, so a filter that adds it puts the inputs on the stream, each string cut to `bus.egressMaxBytes` when it is set (below).

The setting `bus.egressMaxBytes` (`FW_BUS_EGRESS_MAX_BYTES`) bounds each string value in the `data` of a published event. A longer value goes out cut to its first `bus.egressMaxBytes` bytes, at a rune boundary, followed by a note of the shape ` [cut 1234 of 5678 bytes]`: the number of bytes cut and the length of the whole value. The egress walks nested objects and arrays, and leaves the keys of an object and every value that is not a string as they are. The envelope stays whole: the id, the type, the time, the source, the subject, the sequence number, and the node. The `raw` provider blocks of `fw.model.responded` are data too, and the egress cuts their strings like any other. The cut happens on the copy that goes to the bus, so the ledger, the native API, and a replay keep every event whole. The default, 0, publishes the data whole. An event with no string literal past the bound goes out unchanged without a decode, after one scan of its bytes; only an event with a long string is decoded and encoded again, and the keys of its objects then come in sorted order. The front of a fleet sets the bound on its workers, so it can put `fw.tool.requested` and `fw.tool.responded` on the stream without whole tool inputs and outputs. A cursor of the egress does not depend on the bound, so a change of it sends no history again.

The stream is not a ledger. A consumer that adds up the tokens of `fw.model.responded` can count an event twice after a crash, and can miss some with a ledger without cursors. A consumer also does not need `fw.model.responded`, which carries the content of the run: `fw.run.finished` and `fw.run.suspended` carry the totals of the run (section 8.2), and the projection sets the usage from them. A consumer of the lifecycle events alone, such as the front of a fleet, therefore knows the tokens, the turns, the tool calls, and the cost of every run that finished or was suspended. A lost event costs nothing once a later `fw.run.finished` or `fw.run.suspended` of the same run arrives. With the file ledger, the next boot of the process sends a final event that the bus did not confirm. Until then, or with a ledger without cursors, the consumer keeps the usage of the event before it until it reads the run again, as the front of a fleet does when a Task wakes. Between two such events, the usage of a run on that stream is not final, and a consumer must show it as partial.

The egress publishes the events of a run in order, and a retry sends the oldest event of the run that the bus did not confirm before any later one. The runs share no sequence, so with cursors the events of two runs can reach the bus in another order than the ledger recorded them. The queue in memory keeps the order of the process. The `nats` and `redis` drivers deliver the messages of one connection in order. The events of a run therefore reach a subscriber in order, unless two processes publish events of the same run, a crash repeats a part of the run, or the driver itself reorders. The `mem` driver reorders: it hands each event to a goroutine of its own, within one process. Two processes publish for one run, for example, when the source of a branch records the branch as failed under its id while the process of the branch took it after all (section 4.7). A consumer must not let an older event of a run undo the state or the totals of a later one. For a consumer that meets a reorder, a `fw.run.queued` that arrives after a later event of its run gives the run its identity, such as its agent, its person, and its metadata, and leaves its state and its usage as the later events made them; a run that no later event gave a state becomes queued.

The cost in the totals is the cost at the prices of the process that ran the calls. The totals do not name the model of each call, so a consumer cannot price them again. A consumer with a price table of its own therefore loses its own pricing at every totals event: its prices apply only to the increments that it receives after the totals, and the cost that it shows can go down at an end of the run, to $0 with every token unpriced when the worker has no price table. A later change can add a breakdown by model to the totals, so a consumer can price them with its own table. The tokens that the process had no price for are `unpricedTokens`, so a consumer shows an unknown cost and not a cost of zero.

### 9.5 Request and reply

A decision point on the bus uses request and reply (section 13.5). `fw` publishes a `fw.decision.requested` event to the subject of `policy.external.bus`, or of the control plane of the workbench. The event names a reply subject of the process, and a responder publishes `fw.decision.answered` to it. The default timeout is 5 seconds. Without an answer in time, a tool call is denied, and a request from a caller is refused with a 503.

<a id="observability"></a>

## 10. Observability

Operators watch `fw` with Prometheus and Grafana. `fw` has no usage page of its own. The kernel keeps the exact record of usage in the ledger.

### 10.1 Metrics

The `metrics` projection maintains this registry. The route `/metrics` carries no authentication, so keep it inside the monitoring perimeter (section 8.5).

| Metric | Type | Labels |
|---|---|---|
| `fw_runs_total` | counter | `agent`, `outcome` |
| `fw_runs_queued_total` | counter | `agent`, `origin` (`http`, `a2a`, `mcp`, `bus`, `cli`, `tool`, `other`), `batch` |
| `fw_runs_active` | gauge | `agent` |
| `fw_run_duration_seconds` | histogram | `agent` |
| `fw_run_turns` | histogram | `agent` |
| `fw_task_completions_total` | counter | `execution`, `grader_judgment` |
| `fw_task_verifications_total` | counter | `result` |
| `fw_operator_interventions_total` | counter | `kind` |
| `fw_operator_interventions_per_run` | histogram | `coverage` |
| `fw_recovery_duration_seconds` | histogram | `result` |
| `fw_confirmed_duplicate_effects_total` | counter | none |
| `fw_ambiguous_tool_retries_total` | counter | none |
| `fw_successful_task_cost_usd` | histogram | `basis` |
| `fw_successful_task_cost_excluded_total` | counter | `reason` |
| `fw_budget_exceeded_total` | counter | `agent`, `limit` (`maxTurns`, `maxTokens`, `maxCostUSD`, `maxToolCalls`, `deadline`, `maxRepeats`, `other`) |
| `fw_done_passes_total` | counter | `agent`, `result` (`passed`, `weak`, `retried`, `failed`): the passes of a definition of done (section 24) |
| `fw_model_requests_total` | counter | `provider`, `model`, `outcome` (`ok`, `error`, `canceled`) |
| `fw_model_request_duration_seconds` | histogram | `provider`, `model` |
| `fw_model_tokens_total` | counter | `provider`, `model`, `kind` (`input`, `output`, `cache_read`, `cache_write`) |
| `fw_model_cost_usd_total` | counter | `provider`, `model` |
| `fw_model_unpriced_tokens_total` | counter | `provider`, `model` |
| `fw_tool_calls_total` | counter | `tool`, `outcome` (`ok`, `error`, `denied`) |
| `fw_tool_call_duration_seconds` | histogram | `tool` |
| `fw_mcp_server_up` | gauge | `server` |
| `fw_approvals_pending` | gauge | `agent` |
| `fw_approvals_oldest_timestamp_seconds` | gauge | `agent` |
| `fw_approvals_total` | counter | `decision` |
| `fw_loops_total` | counter | `agent`, `action` (`warned`, `failed`) |
| `fw_bus_messages_total` | counter | `driver`, `direction` (`in`, `out`) |
| `fw_ledger_events_total` | counter | `type` |
| `fw_ledger_append_duration_seconds` | histogram | `driver` |
| `fw_build_info` | gauge | `version`, `commit`, `go` |
| `fw_storage_blocked` | gauge | none |

The origin of a child run is `tool`. `fw_storage_blocked` is 1 after a storage failure stopped the process (section 5.6), and 0 otherwise. It stays 1 until the process restarts, because the process then writes nothing and runs nothing. The metrics read it from `Agent.RuntimeError` at each scrape and at each OTLP push. `fw_approvals_oldest_timestamp_seconds` is the Unix time of the request of the oldest approval that waits. An agent without such an approval has no series.

`fw_model_tokens_total` and `fw_model_cost_usd_total` also count the summary calls of a compaction, from `fw.context.compacted`, and the model call of an annotation, from `fw.run.annotated`, such as the title call of the workbench. The kernel adds these tokens to the usage of the run, so the metrics match the ledger. These calls do not count in `fw_model_requests_total`. A branch (section 4.7) counts its own work only, as its usage does: the metrics and the traces count no model call, token, cost, turn, or tool call of the events that it kept from its source, and they open no span for them. A branch that its copies end counts as a run that ended at its last copy. `fw_model_cost_usd_total` counts priced tokens only. The tokens of a model without a price go to `fw_model_unpriced_tokens_total`, because their cost is unknown, not zero. A price of zero is a price. Without a price table, every token is unpriced.

The model latency histogram has buckets up to 900 seconds, because one turn on a large model can run for minutes. The Go runtime and process collectors are on. The library is `github.com/prometheus/client_golang`.

#### Task quality and recovery

Execution completion is not verification. `fw_task_completions_total` counts each own completion episode, including failure and cancellation, and labels the model grader judgment separately. `unverified` means that no grader judgment was recorded. A follow-up starts another episode. Copied branch history contributes no completion or intervention.

A trusted integration calls `Agent.ObserveQuality` with a stable receipt ID and the sequence of an own completed `fw.run.finished`. It records a final `accepted`, `satisfied`, `rejected`, or `unknown` verification as `fw.run.annotated`. Accepted means that a person accepted the result; satisfied means that an external verifier checked it. Model grading alone creates neither. Identical receipt retries append nothing, including after restart; a conflicting receipt or second final verification of the same completion is refused. The API needs operator access when a principal is present and is not a model tool. A system of record that holds no Go code sends the same receipt to `POST /v1/runs/{id}/quality` (section 8.2). A thumb of a person on that `fw.run.finished`, through `Agent.React` or `POST /v1/runs/{id}/reactions`, records such a receipt for the person: `accepted` for a thumb up and `rejected` for a thumb down, with the receipt id `reaction:<run>:<seq>:<by>`. The first thumb on a result verifies it, and a later thumb records the reaction without a receipt (section 8.2, Reactions).

`fw_task_verifications_total` counts those receipts. Use all execution episodes as the denominator for an overall verified-success rate, or completed episodes for verification coverage. Completions without receipts remain unverified; an explicit unknown receipt is not success. A late receipt can refer to a completion outside the query window, so short-window ratios need that interpretation.

Interventions count attributed approval decisions, interactive user follow-ups, and attributed interactive budget changes. A message that a trusted caller relays from another agent (section 5.5) is not a follow-up of a user. These are concrete actions recorded by the runtime, not proof that a human sat at the keyboard. The per-run histogram counts actions per completion episode, including zero, and labels whether historical coverage is complete. Recovery latency starts at `fw.run.resumed` only for a run that was queued or running at the interruption. A run that waited for input, with a pending approval, waits for a person before and after the restart, so it gets no sample, and its next suspend or resume adds no sample either. Recovery ends at the first model response, tool response or progress event, approval request, or terminal or suspend event. Its result label distinguishes progress, `input_required` (the run reached a question for a person), terminal outcomes, and interruption. It does not measure total outage time, and it never counts the time that a person takes to decide.

Confirmed duplicate effects require application evidence. `QualityObservation.DuplicateEffects` records the number of additional external effects that the integration confirmed; an effects-only receipt needs no completion. Repeated outstanding tool requests count separately as ambiguous retries. Neither a retry nor a prevented duplicate increments the confirmed-effects counter.

Cost is the own model usage of a completion episode, excluding copied history and child usage. The success-cost histogram accepts only final accepted or externally satisfied receipts with complete, priced usage. Missing usage and unpriced tokens go to the exclusion counter. Submit the receipt after the cost annotations of that episode: the receipt freezes its cost snapshot, and later annotations cannot revise an exported histogram. This is not total fleet spend divided by successful root tasks, nor a tree-cost measure. The observability examples include queries, an application receipt example, and OTLP export of these same series.

#### Labels

No label holds a principal, a run id, a prompt, a path, or a URL. Tests feed a principal, a run id, and secret text into the events and make sure that no label holds them. These labels hold names that the configuration bounds:

- `agent`: the profiles of `fw.yaml` and the profiles that authorized people create. The kernel refuses a run of an agent that does not exist.
- `tool`: the tools that the kernel resolved. A tool name that the model invents becomes `unknown`.
- `server`: the names of the MCP servers under `tools.mcp`.
- `provider`: the registered model drivers.
- `model`: the models of the configuration, and at most 32 others.
- `driver`: the bus and ledger drivers.
- `member`: the names of the fleet members, which the deployment sets. The fleet gauges forget a member that is gone (see below).

The other labels hold values from fixed lists: `outcome`, `origin`, `limit`, `kind`, `decision`, `action`, `direction`, `state`, `type`, and `result`. An origin or a limit that this version does not know becomes `other`. The labels of `fw_build_info` have one series.

A run request can name any model in an inline profile, so the model label has a bound. The models of the configuration always keep their names: `model`, the values of `models`, `compaction.model`, and the model of each profile at mount time. Up to 32 other provider and model pairs keep their names. Each pair after that gets the model label `other`. A name longer than 128 bytes always gets `other`.

#### Stores and MCP servers

An observer that also implements `fw.StoreObserver` measures the stores of the agent:

```go
type StoreObserver interface {
    ObserveLedgerAppend(driver string, d time.Duration)
    CountBusMessage(driver, direction string)
}
```

The kernel calls `ObserveLedgerAppend` after each append to the ledger. It calls `CountBusMessage` for each message that the bus intake receives (`in`) and each message that the bus egress publishes (`out`). `driver` is `ledger.driver` or `bus.driver`. If the configuration names no driver, it is `custom`. The metrics projection implements the interface, which feeds `fw_ledger_append_duration_seconds` and `fw_bus_messages_total`. At each scrape, the metrics surface reads `Agent.ToolSources` and sets `fw_mcp_server_up` for each MCP server.

#### Collectors of linked packages

A linked package adds its own series to the registry of the metrics surface with `metrics.RegisterCollector`, a constructor that gets the agent at the first mount of the surface. The fleet of the workbench adds the gauges `fw_fleet_member_up`, `fw_fleet_member_last_seen_timestamp_seconds`, `fw_fleet_runs`, and `fw_fleet_approvals_oldest_timestamp_seconds` this way (the workbench architecture, section 4.3). The rules and the dashboard of `examples/observability` name them, so they expect `fw-workbench` on the processes that Prometheus scrapes.

#### Prometheus is not the usage record

Use Prometheus for trends, errors, and latency. Do not use it to bill people or to audit usage. A scrape misses what a process does between its last scrape and its exit. Prometheus never sees a process that it does not scrape. A counter starts again at zero after a restart, so a total over a long range is an estimate.

The ledger of each process records every model call with its tokens, and `fw events <run> --format jsonl` exports it. Bus egress (section 9.4) sends the same events to a consumer for billing or audit, and the totals on `fw.run.finished` give the usage of each run without the content of its calls. The kernel enforces each budget from the ledger, not from Prometheus. The dashboard of the workbench therefore shows no usage page. `GET /v1/usage` and the MCP tool `usage` of the workbench still answer, and both are deprecated. The result of a run keeps the usage of that run beside its limits (the workbench architecture, section 6.3).

#### Scrape configuration, alerts, and a dashboard

The directory [examples/observability](../examples/observability/README.md) holds a supported setup. `prometheus.yml` has the scrape jobs, `alerts.yml` has operational and task-quality alert rules, and `grafana/fw.json` is one dashboard for Grafana 10 or later. The job `fw-pods` keeps only the pods with the label `fw.dev/scrape: "true"`. The examples put that label on the always-on fronts only, never on an ax Task. The README tells how to load each file.

### 10.2 Traces

The `trace` projection emits OpenTelemetry spans. A run is one span. Each turn is a child span, and each model call and tool call is a child of its turn. The service `Export` of the package `telemetry` starts the projection when a collector is named (section 10.5). The exporter is OTLP, configured by the standard `OTEL_EXPORTER_OTLP_ENDPOINT` variable. The `traceparent` value travels in every ledger event, in every bus message, in the `spawn_task` environment, and in outbound A2A and MCP requests.

The spans carry the attributes of the OpenTelemetry GenAI agent conventions, so a trace store that knows them, such as Grafana, Langfuse, or Braintrust, reads the traces without a mapping. The run span keeps the name `fw.run` and carries `gen_ai.operation.name` `invoke_agent`, `gen_ai.agent.name` and `gen_ai.agent.id`, both the profile, and `gen_ai.conversation.id`, the run id. A model span, `chat <model>`, carries the chat attributes and the conversation. A tool span, `execute_tool <tool>`, carries `gen_ai.tool.name`, `gen_ai.tool.call.id`, the agent, the conversation, and `gen_ai.tool.type`: `function` for a built-in tool or a tool of a Go option, and `extension` for a tool of an MCP server or an A2A peer. `fw.tool.requested` names the source of the tool for this. A tool of an MCP server also carries `mcp.method.name` `tools/call`, and a call of the `skill` tool carries `gen_ai.skill.name`.

A span names its run in `gen_ai.conversation.id`, and the run span also in `fw.run.id`. No span attribute and no attribute of a log record (section 10.5) holds a person, the id of another run, or free text of an integration. These attributes name who acted as a kind only:

| Attribute | Where | Value |
|---|---|---|
| `fw.approval.decider_kind` | event `fw.approval.decided` | `person` (a user, or the person that a trusted relay names), `parent` (a run: the parent of a child, or the run that a grader grades), `token` (the static token), `workload` (a SPIFFE identity), `local` (the local user or `fw run`), `anonymous`, `kernel`, `none` (no decider), or `unknown` (a decider without a kind, such as in the ledger of an older kernel) |
| `fw.budget.by_kind` | event `fw.budget.changed` | the same kinds |
| `fw.outcome.graded` | event `fw.outcome.evaluated` | true when a grader run started; the id of the grader run stays in the ledger |
| `fw.quality.receipt_sha256` | event `fw.run.annotated` and span `fw.quality.observation` | the SHA-256 of the receipt id in hex, so a trace store matches one receipt without reading its id |

The kind comes from the principal, never from the shape of the subject. The kernel records it in the event beside the subject, as `deciderKind` and `byKind`: the kind of the principal of the caller, with `person` for a user, `person` for the person that a trusted relay names, `parent` for a run that decides for its child or its grader, `local` for `fw run`, and `kernel` for the kernel. So a person whose subject looks like a run id or a SPIFFE URI stays a person. A principal of no known kind records no kind. The ledger keeps the subject itself.

### 10.3 Logs

Logs are JSON lines on standard error from `log/slog`, so that `fw-workbench mcp` keeps standard output for the protocol. Each line carries the `run`, `turn`, and `traceparent` that apply. `FW_LOG=debug` adds request and response bodies with secrets redacted.

### 10.4 Trajectories

A trajectory is the full record of what an agent did. The ledger is the trajectory. The command `fw events <run> --format jsonl` exports it. The ax roadmap names trajectory collection at the runner layer as a goal, and the ledger is that record.

### 10.5 Export over OTLP

A Prometheus scrape is a request, and a request wakes an ax Task that sleeps, so a worker must not be scraped. A worker pushes instead. The package `telemetry` registers the service `Export`, and the standard variables turn it on: `OTEL_EXPORTER_OTLP_ENDPOINT` for every signal, or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, and `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` one by one. The transport is OTLP over HTTP, and the exporters read the other `OTEL_EXPORTER_OTLP_*` variables, such as the headers and the certificate. Without a variable, the service starts nothing, and the Prometheus route stays as it is.

The metrics are the series of the Prometheus registry of section 10.1, pushed as they are: the same names, the same labels, and the labels `service.name` and `service.instance.id` of the resource, the latter the `node` of the process. The push runs at the interval of `OTEL_METRIC_EXPORT_INTERVAL`, 60 seconds by default, through the Prometheus bridge of the OpenTelemetry SDK, and it starts as soon as the metrics surface mounts. A Task that sleeps stops pushing, and a collector that keeps the last value of a gauge shows it as it was.

The logs are the ledger: every event of every run goes out as one log record. The body is the data of the event, the event name is its type, and the attributes carry the run id, the sequence number, the agent, the node, and the source. The record carries the trace id and the span id of the run from the `traceparent` of the event, so a trace store and a log store meet on the same ids. A failed run and a budget stop are errors, a loop and a suspend are warnings, and everything else is information. The ledger is the trajectory of section 10.4, and this is how it reaches a collector with no change to ax.

The traces of section 10.2 start through the same service, so one set of variables carries all three signals. `fw doctor` reports which signals go where. The directory `examples/observability` has a collector configuration that receives the three and hands the metrics to Prometheus, the logs to a log store, and the traces to a trace store; Grafana reads the fleet from those, and a front needs no gauge of its own for it.

## 11. Running inside google/ax

### 11.1 What ax expects from a runner

ax starts `/usr/local/bin/ax-task-runner` as PID 1 in every Task container. The contract from `docs/runner.md` in the ax repository is:

| ax provides or expects | Detail |
|---|---|
| `AX_TASK_YAML` | The Task as YAML, without status |
| `AX_WORKSPACES_YAML` | Every bound Workspace as a multi-document YAML stream, in binding order |
| `spec.env` | Set directly in the environment |
| `GEMINI_API_KEY` | Set when the atespace has a Gemini credential |
| `/workspace` | A durable volume, restored on resume |
| `GET /readyz` on port 80 | 503 until workspaces are prepared, then 200 |
| `GET /healthz` on port 80 | 200 once alive |
| `GET /metadata/v1alpha1/ax/task` on loopback | The Task as YAML (optional). Since ax commit 22954f6, the metadata endpoints answer on a loopback address only, `127.0.0.1:8099` in the stock runner, and not on port 80. |
| `GET /metadata/v1alpha1/ax/workspaces` on loopback | The Workspaces as YAML (optional) |
| `spec.command` | Started as a supervised child with `AX_METADATA_URL` set to the loopback address |
| `SIGTERM` to PID 1 | Stop or suspend. Ten seconds of grace, then `SIGKILL`. |
| `spec.debug: true` | Guest services over gRPC on port 80, multiplexed with h2c |

### 11.2 How fw meets the contract

`fw` implements the contract itself instead of embedding the ax `runner` package. The reason is port 80. The ax runner owns the port 80 mux, and `fw` needs that port for its own surfaces. The `ax` package defines the manifest types from the YAML schema of the ax repository. `fw` therefore parses the same YAML that the stock runner parses, without an import of the ax module. The container image contains a symlink from `/usr/local/bin/ax-task-runner` to `/usr/local/bin/fw`.

Two images are built. `fw:static` is `gcr.io/distroless/static` plus the binary, for agents that only use HTTP, MCP, and A2A. `fw:toolbox` is a Debian slim image for coding agents that use `bash`. It holds `git`, `curl`, `python3`, network and file programs, and `uv` and `pixi` for the environments of section 18. Both images run `fw` as root. They hold the tool user `nonroot`, uid and gid 65532, which runs the processes of tools under ax (section 7.12). The stock ax runner image installs Python and the Antigravity agent. `fw` needs neither.

### 11.3 Boot sequence under ax

1. Read `AX_TASK_YAML` and `AX_WORKSPACES_YAML`. Set `FW_HOME` to `/workspace/.fw`.
2. Listen on port 80. Answer `/healthz` with 200. Serve the metadata endpoints on `127.0.0.1:8099`, or on the port of `--metadata-port`. If the Task sets `spec.debug: true`, start the guest services of `ax ssh` (section 11.12).
3. For each workspace binding, in order, write `spec.files`, clone `spec.git`, and create `spec.skills.path`. Skip a binding whose marker exists.
4. Write the marker at `$FW_HOME/ax/initialized-<path>`. With a tool user, the next step builds the agent. The agent gives the prepared files to that user once and keeps `$FW_HOME` to `fw` (section 7.12). Before that, the boot opens the root directory for the tool user, and the agent creates the home of the tool user (section 7.12).
5. If the binding has a `goal` and a model credential is present, run the `bootstrap` profile with that goal. The time limit comes from `AX_BOOTSTRAP_TIMEOUT`, default 10 minutes. The marker is written first, so a container killed during the goal does not run the goal again on resume.
6. Answer `/readyz` with 200.
7. Resume unfinished runs (section 4.5).
8. If `spec.command` is empty, serve. If `spec.command` starts with `fw`, run that subcommand in process. Otherwise, start the command as a supervised child with `AX_METADATA_URL` set to the loopback address of step 2.

The stock runner writes its marker under `/ax`, which is not on the durable volume. `fw` writes its marker under `/workspace/.fw`, so a resumed container does not repeat setup. If `fw` finds a marker under `/ax`, it also honors that marker.

The `bootstrap` profile replaces the Antigravity bootstrap. It is a normal `fw` run with the `bash` and `files` tools, a fixed system prompt, and the goal as input. It needs a model credential. If no credential is present, `fw` logs a warning and skips the goal, which is what the stock runner does. For the same reason, `fw.New` treats a missing model credential as a warning: the process boots, serves health and metadata, and reports the missing credential on the first run.

### 11.4 Addressing through the atenet router

ax gives a Task no Kubernetes Service. Requests reach a Task through the Service `atenet-router.ate-system.svc.cluster.local` on port 80 with the header `ate-target-actor: <atespace>/<task>`. The router resolves the actor and proxies the request to port 80 inside the sandbox. If the actor is suspended, the router resumes it first. `fw` listens on that port, so every surface of every `fw` Task is reachable from inside the cluster with no extra configuration:

```
curl -H 'ate-target-actor: default/reviewer' \
     -H 'Content-Type: application/json' \
     -d '{"input": "Review pull request 42", "wait": true}' \
     http://atenet-router.ate-system.svc.cluster.local/v1/runs
```

An A2A message or an MCP call to a suspended agent wakes it. An MCP request in revision 2026-07-28 carries no session, so the woken container answers it from the ledger with nothing to rebuild. This turns an ax Task running `fw` into a durable actor. The actor sleeps while it is idle and wakes on demand. The `fw` A2A client adds the header for every peer configured with `ax:`. A third-party A2A client cannot learn the header from the agent card. Until ax offers per-task addresses, an external A2A client needs a proxy that adds the header.

### 11.5 Suspend and resume

Agent Substrate v0.4.0 suspends an ax Task in three steps. It pauses the sandbox, saves `/workspace`, and then kills `fw` with `SIGKILL`. No `SIGTERM` comes first, although the ax runner document says so. `fw` therefore writes no `fw.run.suspended` event, and an event that `fw` writes after the save is lost. Later, Substrate restores `/workspace` into a fresh container.

On resume, `fw` boots again, finds the markers, skips setup, and resumes runs from the ledger. A run that was waiting for an approval is still waiting. The bus intake acknowledged the job of each run when the run was queued (section 9.3), so the bus gives no job of a sleeping Task to another Task. A run that was in the middle of a model call sends the call again. If the lost events include a tool call that ran, the resumed run asks the policy again and can run the call a second time. The working time of a run ends at the last event before the suspend (section 5.8). The deadline therefore does not count the time that the Task sleeps. A grant that `fw` refreshed after the save comes back as its old copy. The authorization server can then refuse it, and the person connects again (section 7.11).

On each boot, `fw` takes the writer lock of its file ledger. Another `fw` process can hold the lock for a while, for example `fw run` that a person started through `ax ssh` during a slow boot with `spec.debug`. The boot then tries the open again for up to 30 seconds, with a log line every five seconds, so PID 1 does not exit at once, and after that it exits with a message that names the other holder. `fw run` and `fw replay` beside a running server still fail at once with `fw.ErrLedgerLocked`.

A Task that a storage failure blocked (section 5.6) does no work until it restarts. Its `GET /v1/status` reports it blocked and idle since the failure, so the suspend controller can put it to sleep, and the next request restores a fresh process that rebuilds from the ledger.

`fw` still follows section 5.6 on `SIGTERM`. If the worker pod itself stops, Substrate sends `SIGTERM`. After a grace period of 30 minutes, it sends `SIGKILL`.

### 11.6 Using the Workspace spec

The ax Workspace spec carries everything `fw` needs for a profile. `spec.files` ships `fw.yaml` and profile files into the workspace. `spec.git` is cloned by `fw`. After the clone, `fw` removes the user information of an http or https URL from the remote. A token in `spec.git` therefore does not stay in `.git/config`. A later fetch or push needs a credential from the egress policy or from a credential helper. `spec.mcp.servers` become MCP clients. `spec.skills.path` is read for `SKILL.md` folders, which become MCP prompts and on-demand context. An operations team ships an agent by applying a Workspace. See `examples/ax/reviewer.yaml`.

### 11.7 Credentials

If the atespace has a Gemini credential, ax injects `GEMINI_API_KEY`. ax does not deliver the `Model` resource to the runner today, and `spec.env` holds plain values. For another provider, pass the key through `spec.env` or through a `Secrets` driver that the sandbox can reach. Do not write a key into a Workspace file, because the file lands on the durable volume.

### 11.8 Status for the control plane

ax has no RPC for a runner to report its state, and ax commit f0f16ff removed the fields `usage` and `pendingApproval` from `TaskStatus`, because nothing set or read them. ax keeps the meaning of an agent out of the Task. That layer is the kernel. `fw` serves `GET /v1/status` on its listener, behind the authentication of the native API, with a JSON body:

```json
{
  "usage": { "promptTokens": 48210, "completionTokens": 3120 },
  "pendingApproval": { "id": "ap_7f3", "action": "bash: git push", "requestedAt": "2026-10-13T09:14:31Z" },
  "runs": { "active": 1, "inputRequired": 1, "total": 12 },
  "idle": true,
  "idleSince": "2026-10-13T09:14:31Z"
}
```

The body counts the runs that the caller can view. `pendingApproval` is the oldest approval that waits, or `null`. `idle` is true when no run is queued or running: every run waits for input, is suspended, or is finished, and a process without runs is idle. `idleSince` is the newest update of a run, or the start of the process when no run worked yet, and it is absent while a run works. The front of a fleet reads the body through the atenet router before it suspends a Task that the egress stream shows as idle (the workbench architecture, section 4.12). The `ax` driver reads it before it removes a child Task (section 11.9). A suspended Task costs nothing and wakes on the next request through the router. If ax adds a reporting RPC, a `Reporter` plug pushes the same data.

### 11.9 Child runs and child tasks

An agent creates other agents with the `spawn_task` tool. The tool starts a child run in the same process with a task of its own. If the parent gives them, the child also gets a system prompt, instructions, tool and skill subsets, a model, an effort, and a budget. These form an inline profile on top of the named profile of the child. The tool is parallel-safe. A model that needs three things done therefore starts three children in one response, and the kernel runs them at the same time. With `wait` false the tool returns the run id at once, and `await_task` collects the result later.

The kernel keeps a child inside its parent:

- A child uses only tools that its own profile and every ancestor allow. The tools and the rules of an ancestor are those of its resolved profile (section 12.5).
- A child carries the policy rules of every ancestor, and the stricter verdict wins.
- A child shares the budget of its parent: its tokens, its cost, and its tool calls count toward the tree of its parent and of every ancestor (section 5.2). Its own limits are capped by the budget of its parent, and its deadline is the earliest deadline of its ancestors.
- `spawn.maxDepth` (default 3) bounds how deep the tree goes, `spawn.maxChildren` (default 16) bounds how many children one run starts in its life, and `spawn.maxDescendants` (default 32) bounds how many runs one tree holds under its root in its life, at every depth and in every process together. Zero leaves a limit unbounded. `spawn.disabled` forbids children, including direct Go child creation; AX uses it for exhausted depth and for a child whose share of the tree has no room for more runs. `spawn.driver` names a `Spawner` that runs the children in other processes (below). Past any bound, `spawn_task` returns an error result that names it, and the model goes on without the child.
- A canceled parent cancels every run under it that is still open, at any depth, also below a child that finished, in this process and in other processes. Cancel of a finished run cancels the open runs under it, and leaves the run as it is. A branch is a run of its own, and the cancel of its source leaves it alone.
- A child shares the trace of its parent, so one trace shows the whole tree.
- A child takes no slot of `maxRuns`, because its parent holds one while it waits. Otherwise parents that wait for children can fill every slot. A child takes a slot of `spawn.maxRunning` instead (default 8): at most that many child runs of the process work at the same time, and the others stay queued until a slot is free. A run that waits for another run, in `spawn_task` with `wait`, in `await_task`, or in any `WaitFor`, and a run whose tool waits for the answer of a person, gives its slot up until the wait ends and then takes one again. A run that holds a slot therefore never waits for a run below it, so a tree of any depth makes progress with one slot. The slots also bound the model calls of children in flight at once, and with them how far a tree can pass a shared limit (section 5.2).
- `maxRuns` is 16 when the configuration leaves it at 0, and 1 under ax: a Task is one agent with one working copy, so one run at a time holds `/workspace` and its shell. A Task that must run more sets `maxRuns` in its configuration. A child run in the same process is not such a run: it works at the same time as its parent, in the same workspace and with the same files, and with `tools.files.perRun` in the directory of its parent (section 7.9). A child that must have a workspace of its own runs in a child Task (`spawn.driver: ax`, below).

When a child needs an approval or asks a question, `spawn_task` relays it to the caller of the parent. It arrives as a question on the parent run. Every surface shows it, and the answer goes to the child. The parent run therefore stays the one place to watch.

A `Spawner` runs the children in other processes, and `spawn.driver` selects one. Without one, `spawn_task` starts the child in this process, as above. With one, the kernel fills the request of the child and hands it to the spawner: the id of the child is one that the kernel chooses, and the other process holds the child under it; the principal and its kind are those of the parent, so the child acts as the same person; the budget is the budget of the parent, capped, with the working time that the parent has left as the deadline, and with each of `maxTokens`, `maxCostUSD`, and `maxToolCalls` lowered to what the tightest tree of the lineage has left at that moment, so the child alone can never spend more than was left, and with `finalReport: false` when the parent turns the final report off (section 5.2); and the metadata key `fw.parent` holds the id of the parent, and the key `fw.batch` of the parent, the name of its job, passes to the child, as it does for a child in this process. The kernel reads the key from the metadata of the parent as it is now, after every annotation (section 8.2), so a front that annotates a run with a batch after the run started gives that batch to every child that the run starts after the annotation. The spawner returns the child and the name of the process that holds it. The kernel counts the child toward `spawn.maxChildren`, and from then on `Get`, `WaitFor`, `Decide`, `Cancel`, and `Children` reach the child through the spawner, with `Run.Member` set. A restart reads the children back from the ledger. A branch in another process is recorded in the same event, first when it is placed and again when it starts (section 4.7). A branch is a run of its own: it shares no budget and no count with the tree of its source. A canceled parent cancels its remote children too.

The parent counts a child in another process in its tree each time it reads the child: in the wait loop of `spawn_task`, in `await_task`, in `Get`, and in `Children`. The other process reports `treeUsage`, the usage of the child and of every run under it there (section 8.2), so one read covers the whole tree of the child. The kernel also reads, every five seconds and up to four at a time, each child that works and that no wait read in that time: a child that is queued or running, or a finished child with a run under it that works (`treeWorking`). A child that `spawn_task` started with `wait` false, or whose parent finished, therefore still counts while it works. The kernel never reads a child in the background that does not work: one that waits for a person, one that sleeps, or one whose tree ended. Under ax, a read goes through the router, which wakes the Task of the child, so such reads would keep a sleeping child awake. While a child works, the status of the process is not idle (`Agent.Following`), so the suspend controller of the workbench does not put the parent to sleep while its children spend. A child that its process does not know any more, or that does not answer a read for ten minutes, counts as lost; only reads count toward the ten minutes, so a child that answers reads is never lost, whatever its cancels do. A lost child stays in the tree of its parent with its last usage, because spend is spend, and it counts as neither open nor working there, so it keeps no run above it awake. Nothing reads or cancels it again, and the parent records it as lost in `fw.child.reported`, so a restart does not follow it either. A read that reaches it later, from a wait or a person, takes it back. Each read keeps the most that any read reported, field by field, and a child counts at least its own `usage`, whatever its `treeUsage` says. When a read shows that the tree of the child ended, that the child stopped or started to work, and at most once a minute while its usage grows, the kernel appends `fw.child.reported` to the parent. A restart of the parent rebuilds its tree from its own ledger: the children in this process from their `fw.run.queued`, the children in other processes from `fw.run.child`, and their usage and their state from the latest `fw.child.reported`. The new process reads in the background only a child that worked when the parent knew it last; a child that an older kernel recorded has no known state, and the new process does not read it. When a read shows that the tree of a run here ran out, the kernel cancels the open runs under that run (section 5.2).

A process without a spawner, for example after `spawn.driver` left the configuration or when the ledger of a Task runs on a developer machine (section 11.10), cannot reach its children in other processes. It says so once in its log, counts them as neither open nor working, answers `Get`, `WaitFor`, `Decide`, and `Cancel` of such a child with `fw.ErrRunNotFound` and the reason, and cancels and stops their parent without them. Their last usage still counts in the tree.

What this leaves is bounded and stated: a child that does not work is read again only when something reads it for another reason, a wait of `spawn_task` or `await_task`, `Get`, or `Children`. A child that a person wakes later, by a decision or a follow-up in its own process, spends without a read of its parent until such a read, and so does a child that an older kernel started. Each such child stays within its own cap, which was what its tree had left when it started.

The record and the report of a child, `fw.run.child` and `fw.child.reported`, can arrive after the parent finished, for example for a child that ends after its parent or for a branch of a finished run. Like an annotation (section 8.2), they leave the parent finished: the file ledger closes the file of the run again, the metrics count no active run, the traces open no run span, and the stream of the events of the run ends after them (`fw.LeavesFinished`).

`spawn.maxDescendants` counts the runs of a tree when they start, so children that start at the same time never hold more runs together than the limit: the kernel counts and starts each child under one lock. A child in this process counts as one run, and the runs under it count as they start. A child in another process reserves a share of the tree, itself and the runs that it can start under it, before its process exists, because the parent cannot count those runs as they start. The share is an equal part of the room that the tree has left among the children that the parent can still start under `spawn.maxChildren`, so later siblings get at least as much as earlier ones; a child at the last depth reserves one run, and with `spawn.maxChildren` 0 the number of siblings is unknown and every share is one. The share travels to the child in the metadata key `fw.maxDescendants`, the runs that the child can have under it, and the kernel of the child holds the tree of the run to it, or to its own `spawn.maxDescendants` when that is less. The share is recorded in `fw.run.child` and counts for the life of the tree, as the children count toward `spawn.maxChildren`; a child that the kernel knows holds no run, because no process has its run or its process is gone, gives its share back, and a child that a look kept keeps it. The bound is strict: a tree never holds more than `spawn.maxDescendants` runs under its root, in every process together. The default of 32 lets a root start its 16 children and as many runs again deeper; before it, the depth and the children of one run let a root start 16 + 256 + 4,096 runs.

The start of a child in another process takes a while: the `ax` driver creates a Task, waits until it runs, and then starts the run there. The parent therefore records the child before anything exists, so after a restart at any point of the start it knows every process that the spawner created or might have created. A spawner that is a `Planner` names the process of a child before it creates it (`Plan`). The kernel appends `fw.run.child` with the id of the child, that member, the kind `spawn`, which tells it from a placed branch whatever metadata the request names, and the facts of the child: the person, the metadata, the profile name, the input, and the idempotency key of the start. The file ledger syncs every `fw.run.child` event, so the record is on disk before `Spawn` runs. Once the child has its run, the kernel appends `fw.run.child` again with `started`. If the ledger refuses that record three times, the kernel cancels the child, abandons its process, and records the child as failed, and `spawn_task` gets an error: a parent that cannot record its child cannot follow it. A start that the spawner refuses, and a start that `Cancel` ends, can still leave a process, for example a Task that the ax server created while the call ended, so the kernel abandons the process that `Plan` named first. Then it records the child in this process under the id of the child as a failed or a canceled run with the reason, as a branch is. When the abandon fails, the child stays a child of the parent, and a look takes over at once (below). The caller already learned that the child did not start, so nothing reads the child, even if it took its run before the start failed. The parent therefore records the outcome on the child in `fw.run.child` (`ended` and `reason`), and the look, also after a restart, cancels a child that it finds and removes its process, with retries, before it records the child with that outcome. When the look cannot tell whether the child runs, for example because the router does not answer while the ax API does, it removes the process at its bounds (below) all the same, because nothing reads the child. Only a start that the spawner refused or that `Cancel` ended abandons the process. A shutdown or a storage failure of the parent process that cuts a start is different: the child can have taken its run, so the kernel keeps its process and records nothing, because the ledger is closing or failing. The first record of the child stays the truth, and the look after the restart finds the child and follows it, or records it as lost (below); the approved rerun of the call then takes that child instead of a second one. Until the spawner returns, the parent answers `Get` for the child as a queued run, and `Cancel` ends the start; a `Cancel` that comes after the child took its run goes on to the child, and the end of the parent process does not cancel the child. The request that `Spawn` gets carries no idempotency key: the kernel keeps the key of the start, and the id names the child.

The record of a child that never got its run stands in for the child. The parent lists it among its children, and it is no part of the tree of the parent (section 5.2): it adds no usage, holds no share of `spawn.maxDescendants` and no slot of `spawn.maxRunning`, and does not count toward `spawn.maxChildren`. A start that fails gives its share of the tree back. Nothing runs it in this process: it takes no message, no decision, and no branch, and a restart does not resume it. A record that a restart cut before its end ends then as failed. The record of a branch that never started stands in for the branch the same way, without a parent.

After a restart, the kernel looks for each child whose start has no `started` record: `Find` of the `Planner` asks for the child run at its member. A child that exists is recorded as started and followed as any other child. A parent that ended cancels it, because the call that started the child never returned and nothing reads its result; its process stays, as the process of any child that ended does. For a child that does not exist, `Abandon` removes its process first, and then the kernel records the child as a failed run with the reason. A restart between the two looks again and finds nothing to remove, and a failed abandon is tried again. While the spawner cannot tell, for example because the router does not answer, the child stays a child of the parent, and the kernel asks again, first after a second and then up to once a minute. The look gives up only when two bounds hold: the record of the child, which its id dates, is 15 minutes old, so the bound holds over restarts, and this process tried at least five times over at least three minutes of its own looking. A look that begins long after the record, as after a long stop of the parent, therefore still tries a passing error, such as an outage of the ax API, several times. A look that gives up keeps the process of the child, because the child can still run there and a delete would end its work, and the budget and the deadline that the child got from its parent bound what it spends. The kernel cannot know that such a child never ran, so the child stays a child of the parent: it keeps its share of the tree, its place under `spawn.maxChildren`, and the key of its start. The parent records why in `fw.run.child` (`keptBecause`), with the reason, such as "this process could not reach ... to look for the child", so a person sees what happened, and the next start of the parent looks for the child again. The approved rerun of the call takes the child once it answers; while it does not, the rerun starts no second child and answers that the child of the call can still run in its process, which it names. A look for a child that nothing reads removes its process instead (above); only a process that its removal cannot reach stays, and its child is kept the same way. Until the look ends, the parent answers `Get` for the child as a queued run, and `WaitFor`, `Cancel`, and `Decide` wait for the end. A look does not count in `Agent.Starting`: it goes on at the next start, so a sleep of the process loses nothing. A spawner that is not a `Planner` gets the first record without a member; after such a restart the kernel cannot look for the child, and it records the child as failed with the reason. A child with a member stays a child of the parent when the spawner of the next process is not a `Planner`. The records that a restart makes go out on the egress of the bus, because the kernel makes them once the egress runs.

No run of this process takes the id of a child in another process. `Start` and `ForkEvents` refuse an id that names a run of this process, a child in another process, also one whose start is open, or a run of the ledger, with `fw.ErrRunExists`, so the native API answers 409. A run of this process under the id of a child that started in another process, as an older version let a caller make, does not displace the child after a restart: the child wins, and the run is set aside.

`spawn_task` keys the start of each child by its tool call, with the idempotency key `spawn_task:<parent>:<call>`. The kernel adds a digest of what the child is asked to do: its profile name, input, instructions, inline profile, budget, metadata, and files. A start under the key of an earlier start of the same parent and with the same digest returns the child of that start, and first waits for a start or a look of that child that is still open. A provider that repeats the id of a tool call for another task therefore gets a new child. The approved rerun of an open `spawn_task` call after a restart (section 4.5) therefore takes the child of the first attempt instead of a second child. A child that never got its run is no longer a child of the parent, so the rerun starts a new one. A child in this process uses the same key through the idempotency of `Start`, so the rerun takes the child that resumed with its parent.

The remote request carries the intersection of the child's effective named and inline tool profile with every ancestor's tools, resolved to literal patterns over the tools currently available to the parent. Opt-in tools require permission at every level. New or disconnected tools are not granted later to that child. An empty restricted intersection is refused; a tool-free unrestricted actor can delegate reasoning with a deny-all tool policy. The request also carries the conjunction of the base rules, ancestor rules, and the child's rules, plus its time zone. The receiving process applies its own policy on top. Remote spawning with a custom Go policy or `policy.external` is refused until a protocol can guarantee that policy's enforcement in the child; local children continue to use those policies.

The driver `ax` runs each child in a child Task. It creates the Task through the `ax.v1alpha1.AX` gRPC service at `ax.server`, with the image, the command, the resources, the environment, and the workspaces of this Task, without the goals and without the debug services. The child gets `FW_PARENT_RUN`, `FW_PARENT_MEMBER`, the name of this Task as `ax:<atespace>/<task>`, `FW_SPAWN_MAX_DEPTH`, the remaining depth when positive or explicitly unlimited, and `FW_SPAWN_MAX_DESCENDANTS`, the runs that its share of the tree leaves under it, or the limit of this process for a branch. The child also gets the `egress` of this Task, so it reaches what its parent reaches and nothing more; ax creates the egress policy of the child actor from it before the child first runs (google/ax issue 449). An exhausted child gets `FW_SPAWN_DISABLED=true`, because a zero depth alone means unlimited; so does a child whose share has no room for more runs. Such a child cannot start a child, so the driver also removes from its egress every host with the name and the port of `ax.server`, whatever its scheme. The ax API has no authentication (section 11.11), and a process that reaches it can create, delete, and enter any Task. A wildcard host that covers the server stays, because it names other hosts too. Inherited depth variables of either spelling, the inherited share, and the inherited disabled flag are removed before setting the new values. The child loses the fleet name and URL of the parent, because it derives its own. ax creates every Task suspended, so the driver resumes the Task, waits until it runs, then starts the child run through the atenet router at `ax.router` with the static token of the fleet, the person of the parent, the id of the child run, and an idempotency key, and sends the request again while the child boots. It follows the run through the native API of the child, reads it once a second while a tool waits, and relays decisions and a cancel the same way. A child Task that never gets its run is deleted at once. A child Task whose runs ended stays for a while, so a person can still read their ledger, and the suspend controller of the workbench puts it to sleep. Then the parent removes it (below). The parent Task needs the ax server and the router in its egress policy. The name of a child Task is the name of this Task, cut to fit a DNS label, a hyphen, and the id of the child run in lower case, so `Plan` names the Task before it exists, and the idempotency key `spawn:<parent>:<atespace>/<task>` is the same for every start of one child. After a restart, `Find` reads the child Task on the ax API: a Task that does not exist, that ax deletes, or that failed holds no run that answers, and a Task that does not run is resumed for the look, as `Spawn` would have done; then `Find` reads the run through the router. `Abandon` deletes the child Task, and a Task that is gone already is no error. A create that fails can still have made the Task, for example when the call ends while the ax server works on it, so the driver deletes the Task then, with a context of its own.

ax deletes no Task by itself: a Task has no owner, no label, and no time to live (section 11.11). So the process that creates the child Tasks also removes them, after `ax.childRetention` (`FW_AX_CHILD_RETENTION`), 24 hours by default. A negative retention keeps every child Task. A service of the `ax` package runs in a process that serves in a Task with the `ax` driver and that can start children. It lists the Tasks of the atespace of its Task 10 seconds after the start of the process and then every 15 minutes. It takes each Task whose `FW_PARENT_MEMBER` names its own Task. These are the children of this process, and also the children of an earlier process of the same Task whose ledger is gone, such as the Task before a roll of its pool. A child Task goes when it and every child Task below it, by the same variable, are due. The service deletes the lowest Task first, so a Task that stays never loses its parent. A child Task is due in three cases:

- ax deletes it already. The service sends the delete again, because Substrate can refuse a delete while an actor resumes.
- It failed before it ran, and it is older than the retention. A Task that failed after it ran can hold runs that nobody can read, so it stays, and a person decides.
- It is older than the retention, and its `GET /v1/status` (section 11.8), read through the router with the static token, shows no open run, no run that works, and no storage failure. Its runs must also have ended at least the retention ago, by `idleSince`, unless it holds no run at all, because it never got its run.

A read through the router wakes a Task that sleeps. So the service reads such a Task only after it slept for the whole retention, by the time that ax gives the condition `Ready` at a suspend. It resumes the Task through the ax API first, as `Find` does, so the phase on the ax API follows. A Task that stays while no run works there, for example because a run waits for a person, goes back to sleep at once, and the service reads it again only after another retention. One pass wakes at most eight Tasks. From the first wake to the end of the pass, the process holds itself busy (`Agent.Hold`), so the suspend controller does not put it to sleep in the middle. The clocks are the creation and the suspend of each Task on the ax API and the newest update of a run in the ledger of the child, so a restart of the parent loses none of them. The status counts the runs that the static token can view, so that token must see every run of a child, as it sees every run of a pool.

The parent keeps what it recorded about a child that goes. `fw.run.child` and the latest `fw.child.reported` stay in its ledger, so the tree usage of the parent run still counts the child, and so does the usage of a spawn bridge, which is the tree usage of its manager run. `Children` of the parent leaves out a child that it cannot read, as for any child whose process does not answer, and a read of the child through the parent fails. The result of the child stays where the parent collected it, in the result of `spawn_task` or of `await_task`. The view of the workbench drops the child Task with its runs, as it drops any Task that leaves the Task list (the workbench architecture, section 4.1). Only a process of the parent Task removes its children, so the child Tasks of a Task that never runs again, for example after its pool shrank, stay until a person deletes them, and so do the children of a Task with an older kernel until its next start with this one.

The native API lets a trusted caller, the static token or a workload, name the principal of a run in the body: `principal` and `principalKind`. The child Task gets the person of the parent this way. A person who names a principal starts the run as themself. A trusted caller also names the id of the run with `id`, an id of the shape that the kernel makes, so the child holds its run under the id that its parent recorded. The native API refuses `id` from anyone else, and answers 409 for an id that the process knows already.

### 11.10 Local testing without a cluster

`fw serve --task-file task.yaml --workspace-file ws.yaml` reads the manifests from files instead of the environment. These flags match the stock runner flags. The `--port` flag moves the listener off port 80 for a developer machine, and `--metadata-port` moves the metadata endpoints off `127.0.0.1:8099`.

### 11.11 Gaps in ax today

These gaps exist in ax at commit 8d2ccf3, read on 2026-10-17. Each one maps to an item on the ax roadmap.

- A Task has no goal field. `fw` takes the goal from `FW_GOAL`, from `spec.command`, from a message through the router, or from a queue.
- The `Model` resource is not delivered to the runner. `fw` reads credentials from the environment (section 11.7).
- There is no runner-to-control-plane status RPC. `fw` serves a status endpoint instead (section 11.8).
- A Task has no address of its own. External A2A clients need a header-adding proxy (section 11.4).
- The stock marker directory `/ax` is not durable. `fw` writes its marker under `/workspace` (section 11.3).
- Agent Substrate v0.4.0, read on 2026-10-14, suspends a Task with `SIGKILL` after it saves `/workspace`. No `SIGTERM` comes first. `fw` writes no `fw.run.suspended` event, and the working time of a run handles the missing event (section 11.5).
- The egress gateway adds a credential of the egress policy to a request from any process of the sandbox, so a tool process reaches a credentialed host as `fw` does. The policy of `fw` does not bind such a call. Section 7.12 gives the pattern for untrusted input.
- The ax API has no authentication. A process that reaches `ax.server` can create, delete, suspend, and enter any Task. So only a Task that starts child Tasks needs the API in its egress, and a child that cannot start children does not get it (section 11.9).
- A Task has no owner, no label, and no time to live, and ax deletes no Task by itself. The `ax` driver removes its own child Tasks (section 11.9).

### 11.12 Guest services and `ax ssh`

`ax ssh <task> -- <command>` runs a command in the sandbox of a Task and prints its output. The command goes to the Agent Substrate guest services, which answer gRPC on port 80. As the runner contract asks, `fw` serves them only for a Task that sets `spec.debug: true`. `ax ssh` refuses a Task that does not set it.

The package `ax/guest` puts the upstream services from `github.com/agent-substrate/env` on one gRPC server, as the stock runner does:

- The process service starts, follows, feeds, and signals processes. `ax ssh` uses it.
- The file system service reads and writes files under the first workspace.
- gRPC reflection describes both services, so a tool such as `grpcurl` needs no proto files.

If a request uses HTTP/2 and its content type starts with `application/grpc`, the listener sends it to the services. Every other request goes to the routes of section 8.1. The services start with the listener in step 2 of section 11.3, before the workspaces are ready. You can therefore inspect a boot that hangs on a clone or a goal. Until the first workspace exists, a process with no working directory starts in the closest parent directory that exists.

A process starts in the first workspace by default. It gets `AX_METADATA_URL`, as the command of the Task does, and it inherits the rest of the environment of `fw`. It runs as the user of `fw`, not as the tool user of section 7.12. Its output stays in `/var/log/ate-jobs` for about one hour after it exits. That directory is not on the durable volume. If `fw` cannot write to it, the output goes to `ate-jobs` under the temporary directory. A process stops after one hour or after the timeout of its request, and ten processes can run at the same time.

Each call writes the log line `fw: ax guest call` with the method, the process id, and the name of the program. A call that starts or signals a process, or reads or writes a file, logs at the info level. The line never holds the arguments, the input, or the output of a process, because they can hold a secret. The process service collects the exit status of its own processes at once. The zombie reaper of section 5.6 waits five seconds before it collects a child, so the two never take the same exit status.

Do not set `spec.debug` on a Task whose environment or workspace holds data that other callers must not read. The services carry no authentication, as the runner contract expects, and `ax ssh` sends no credential. The authentication, the access roles, and the policy of `fw` do not apply to these calls. Any caller that reaches port 80 of the sandbox, directly or through the atenet router, can run a process. That process can read every `spec.env` entry and every key in the environment of `fw`. A tool process inside the sandbox reaches port 80 too, so the guest services undo the tool user of section 7.12. If the services are on, `fw` logs a warning at boot.

The `fw:static` image holds no shell, so `ax ssh <task>` with no command cannot start `/bin/sh`. The `fw` binary is on the PATH of both images, and `fw events` finds the ledger under `/workspace/.fw` without a flag. This command prints the ledger of a run in either image:

```
ax ssh reviewer -- fw events <run-id>
```

Since ax commit 92d297e, `ax ssh` forwards standard input and Ctrl-C. A bare `ax ssh <task>` therefore opens a shell in the `fw:toolbox` image, and the process service takes the input through `WriteProcessInput`.

## 12. Malleability

### 12.1 Three configuration layers

Configuration resolves in this order, and the last value wins:

1. Go options passed to `fw.New`, for library users.
2. The file `fw.yaml`, found at `$FW_HOME/fw.yaml`, at `./fw.yaml`, or at `FW_CONFIG`.
3. Environment variables with the prefix `FW_`, for example `FW_MODEL` and `FW_BUS_URL`.

A key in `fw.yaml` maps to an environment variable in uppercase, with `_` between the parts. `bus.url` becomes `FW_BUS_URL`, and a list of strings is comma-separated. See `examples/fw.yaml` for every key with its default. Run `fw env` to print every variable, and the value of each variable that is set.

Every key is reachable from the environment, maps and named lists included. An operator inside a sandbox therefore sets an endpoint through `spec.env` and never writes a literal URL into a file. A map takes the key between the prefix and the field, and a list of named entries takes the entry name:

| Variable | Sets |
|---|---|
| `FW_MODELS_FAST=anthropic/claude-haiku-5-5` | `models.fast` |
| `FW_PROVIDERS_OPENAI_BASE_URL=http://vllm:8000/v1` | `providers.openai.baseUrl` |
| `FW_TOOLS_MCP_GIT_ENDPOINT=http://git-mcp:8080/mcp` | the `endpoint` of the `tools.mcp` entry named `git`. The variable creates a missing entry. |
| `FW_TOOLS_PEERS_REVIEWER_AX=default/reviewer` | the `ax` field of the peer named `reviewer` |
| `FW_AGENTS_REVIEWER_MODEL=fast` | `agents.reviewer.model` |
| `FW_TOOLS_BASH_ENV_NATS_URL=nats://nats:4222` | `tools.bash.env.NATS_URL` |
| `FW_TOOLS_LIMITS_PER_TOOL_HTTP_MAX_OUTPUT_BYTES=1048576` | `tools.limits.perTool.http.maxOutputBytes` |
| `FW_TIME_ZONE=America/Chicago` | `time.zone` |
| `FW_AGENTS_REVIEWER_TIME_ZONE=Europe/Berlin` | `agents.reviewer.timeZone` |

The entry name ends where the longest known field starts, so `FW_TOOLS_MCP_GIT_MCP_ENDPOINT` names the entry `git_mcp`. An entry from the file matches its variable with case ignored and `-` equal to `_`. A key segment becomes a name in lowercase or a header name in canonical form. It can also become an option name in lower camel case, or a variable name as given. A variable with a known prefix and an unknown field is an error that lists the fields that exist. A typo therefore stops the process, and the process never ignores it.

A package that a program links in can own a top-level block of `fw.yaml`. It claims the key with `fw.RegisterExtension` in `init`, the loader keeps the block in `fw.Config.Extensions`, and `Config.Extension` decodes it into a type of the package. The loader still refuses every unknown key that no package claimed. No variable sets a block, so the package reads its own variables. `fw config` prints the block as it is, so a block holds no secret. The workbench claims the block `workbench` (the workbench architecture, section 8).

A value in `fw.yaml` and in a profile file expands `${NAME}` and `${NAME:-default}` from the environment before the file is read. `$$` is one `$`. A reference to an unset variable with no default is an error that names the variable, the key, and the file. Prompt text read from a file does not expand.

### 12.2 What you can replace

| Interface | Default | Alternatives | Swap with |
|---|---|---|---|
| `Model` | none; a run needs `model:`, `FW_MODEL`, or a profile model | `anthropic/*`, `gemini/*`, `openai/*`, your own | `model:` or `fw.WithModel` |
| `Tool`, `ToolSource` | `bash`, `files`, `http`, `memory`, `recall` | MCP servers, A2A peers, your own | `tools:` or `fw.Tools` |
| `Policy` | allow read-only, ask on destructive | rules, an external MCP or bus decision point, your own | `policy:`, `policy.external:`, or `fw.WithPolicy` |
| `Ledger` | JSONL files | memory, your own | `ledger.driver:` or `fw.Ledger` |
| `Bus` | `mem` | NATS, Redis, Kafka, your own | `bus.driver:` or `fw.Bus` |
| `Compactor` | a summary by the model of the run | your own | `compaction:` sets when, `fw.WithCompactor` replaces it |
| `Secrets` | environment and files | Vault, Google Secret Manager | `secrets.driver:` or `fw.Secrets` |
| `Surface` | native, A2A, metrics | MCP from the workbench, any subset, your own | `surfaces:` or `fw.Surfaces` |
| `Observer` | messages, status, metrics, bus, a2a, trace | your own | `fw.Observe` |
| `Authn` | bearer token | none, oidc, spiffe, a chain of modes, your own | `auth.mode:` or `fw.WithAuthn` |
| `Authz` | rules from `access` | your own, or the control-plane authorization of the workbench | `fw.WithAuthz` or `fw.WithAuthzFrom` |
| Profiles at runtime | the configuration | a linked package that writes files, such as the workbench | `Agent.SetProfiles` |
| TLS provider | plain HTTP | `file`, `spiffe`, your own | `tls.provider:` or `fw.WithTLS` |
| `Prices` | empty | a price table | `prices:` or `fw.Prices` |
| `Clock` | the system clock | a manual clock for tests, your own | `fw.WithClock` |
| `ToolLimits` | 256 KB in, 100 KB out, 10 min | your own numbers per tool | `tools.limits:` |
| `PersonTokens` | none | the package `tokens`, a client of a token broker, or a linked package that keeps the grants of people, such as the workbench | `tokens.url:` or `Agent.SetPersonTokens` |
| `Spawner` | children in this process | `ax`, a child Task per child; your own | `spawn.driver:` or `fw.WithSpawner` |
| `Decider` | none, and a routine with questions and the tool `decide` need `deciders:` | `systemone`, the client of a System One server such as Clef or Jev, or your own | `deciders.<name>.driver:` or `fw.WithDecider` |

### 12.3 Middleware

`ModelMiddleware` wraps a model (section 6.4). `ToolMiddleware` wraps a tool with the same shape. Built-in tool middleware covers timeouts, output size limits, and output redaction. Middleware is the main way to add a behavior without touching the kernel.

### 12.4 Hooks

A hook is an observer (section 4.4), a policy (section 7.5), a run annotator, or a service. An observer sees everything and changes nothing. A policy decides before a tool runs. A run annotator, `fw.RunAnnotator`, adds metadata to a run when the kernel queues it. The kernel calls it after the authorization of the request, with the context of the caller, the request, and the parent run of a child. The kernel knows nothing about the entries, which a linked package gives a meaning. An entry of the request wins, and a result out of the bounds of `fw.CheckMetadata` adds nothing. A package registers an annotator with `fw.RegisterRunAnnotator` in `init`, and a program passes one with `fw.WithRunAnnotators`. `WithoutDefaults` leaves the registered annotators out. A service, `fw.Service`, runs beside the agent for as long as the agent lives: `New` starts it after the kernel starts and gets back what stops it, and shutdown stops the services first, before the decision points, the tools, the bus, and the ledger. A package registers one with `fw.RegisterService`, a program passes one with `fw.WithServices`, and `WithoutServices` starts none, which the verbs that run one piece of work pass. The fleet of the workbench is such a service. Both run in process by default. An out-of-process observer reads the egress subject. An out-of-process policy is a decision point: an MCP tool or a bus responder that answers each decision request (section 13.5). The same form decides authorization.

### 12.5 Profiles, instructions, and skills

A profile is a named agent configuration. It holds a description, a system prompt, and a list of instruction texts. It also holds a skill allowlist, a tool allowlist with globs, a model selector, an effort level, a time zone, a policy, and a budget. Profiles live under `agents:` in `fw.yaml` or as one file each under `$FW_HOME/agents/`. The system prompt and each instruction entry is either the text or the path of a file that holds it. The file must end in `.md` or `.txt` and must not be under `/proc`, `/sys`, or `/dev`, so a profile that arrives through the API or a rendered workspace cannot read a file of the process into its prompt. A request names a profile with `agent`. One binary serves many profiles, and the A2A card lists each one as a skill.

Instructions reach the model in three layers, and every layer is a plain text:

1. The system prompt of the profile. A profile without one gets `fw.DefaultSystemPrompt`. That prompt says what the agent is and how it works, with the rules that keep a run from looping.
2. The instructions of the profile, under a heading, so one profile composes a base prompt with house rules.
3. The instructions of the run, from the field `instructions` of the request, under their own heading.

After the three layers, the system prompt lists the skills that the profile allows (see below). Last come the sections that tools add at the start of the run, such as the memory section (section 7.8), the sentence `fw.ContextNote` when a tool or the caller added data to the first user message (sections 7.8.2 and 7.8.4), and the sentence `fw.MessageNote` from the first message from another agent on (section 5.5). The digests below cover the text of the profile alone, so none of these parts changes them.

A request can also carry an inline profile in the field `profile`. It overlays the named profile for that run. A set system prompt, model, effort, or time zone replaces the value. Its instructions follow the instructions of the named profile. A tool, skill, or routine list replaces the list. The policy becomes the stricter of the two, and the budget and the block `compaction` take the set fields. The kernel stores the inline profile and the instructions in `fw.run.queued`, so a replay sees the same inline profile and the same instructions. A tool list in an inline profile can give a run tools that the tool list of its agent leaves out. The kernel names them in `extraTools` of `fw.run.queued` and of the run object, and leaves out a tool that an ancestor does not allow. Every surface tells the person. Under the first message, the conversation says "For this run only, reviewer can also run shell commands". The result of the MCP `run` tool ends with the same fact, and `fw run` prints it on standard error. The command line builds an inline profile from flags such as `--system`, `--tools`, and `--max-turns`.

The kernel resolves the profile of a run once, when it queues the run, and records it in `fw.run.queued` as `resolved`, next to the inline profile of the request. The kernel reads the profile set once for the request, so a change in the middle of the start cannot split a run between two versions. The run uses the resolved profile for its life: in every turn, after a follow-up, after an approval, after a suspend, after a restart, and in a branch. A change to the profile through `Agent.SetProfiles`, in a file under `$FW_HOME/agents`, or in `fw.yaml` therefore reaches new runs only, and each run names the configuration that it ran with. The fields are these:

| Field | In the resolved profile |
|---|---|
| model | the selector, through `models` and the `model` of the configuration, so a change of either does not reach the run |
| effort | the overlay, else the `effort` of the configuration; a run queued without one asks for the default of the provider, also after the configuration names an effort (section 6.1) |
| skills, tools | the allowlists of the overlay |
| time zone | the overlay, else the zone of the parent, else `time.zone`; `fw.run.started` records the zone that wins (section 5.8) |
| budget | the budget of the overlay, with each zero field taken from `budget:` in `fw.yaml`; the budget of the request stays in its own field and wins over it |
| compaction | the block `compaction` of the overlay over the block of the configuration: `at`, the size that starts a compaction, `off` when the run never compacts, and `model` as a selector, empty for the model of the run (section 5.4) |
| policy | the rules of the overlay, with the rules of the named profile now on top as a tightening (section 7.5) |
| routines | the routines of the overlay, whole (section 22.1) |
| system prompt, instructions | not the text, only its digests, `systemDigest` and `instructionsDigest` (below) |
| description, access | never part of it |

The resolved profile holds no configured text, because a system prompt or an instruction can hold a value that a variable expanded (section 12.1). The event goes to the ledger, the bus egress, the OTLP export, the events of the native API, and a branch in another process. It goes to anyone who can view the run, also to a person who cannot view the agent of a child run. The text of the inline profile stays in the `profile` field of the event, as it always was, and reaches the model as the event stores it.

The process that queues a run keeps the text of the run in memory: the system prompt of the inline profile, else that of the named profile, else `fw.DefaultSystemPrompt`, and the instructions of the named profile, then those of the inline profile. A change through `Agent.SetProfiles` therefore does not reach the text of a run. The model reads the text of the named profile as the configuration holds it, and the kernel replaces no secret in it, because that text never passes through an event. A branch in the same process takes the text of its source, or of the nearest run up its chain of sources that holds the text, so a branch of a branch that made no model call yet keeps it too.

Each digest is `sha256:` and the hex SHA-256 of the text as the model reads it: the system prompt without the spaces at its start and end, and the instructions, each without the spaces at its start and end, the empty ones left out, joined by a blank line. The text of the inline profile counts as the event stores it. After a restart, or in a branch in another process, no process holds the text in memory, so the kernel takes the text of the profile at the first model call and compares its digests with the digests of the run. When they are the same, the run goes on. When one differs, the configuration changed since the run was queued: the run goes on with the text now, and the kernel records the change on the run as `fw.run.annotated` with the metadata key `fw.drift.system` or `fw.drift.instructions` and the value `<old digest> -> <new digest>`. The kernel logs a warning with the same facts. The metadata of the run object then shows the change, the annotation passes the egress filter of the fleet, and a later restart compares with the new digest, so each change is recorded once. The change does not fail the run. An annotation keeps the title of a run (section 8.2).

The kernel reads some parts of a run as they are now, because they are not configuration of the run or because they decide authorization:

- Access. Who can view or operate the agent and its runs follows the access block of the profile now and the authorization of the process (section 13.2). The resolved profile never grants access that the configuration took away.
- The policy of the process, from `policy:` in `fw.yaml`, a Go option, or `policy.external`. A change to the tool list of a profile reaches new runs only, so a person who must take a tool away from a run that is already going denies the tool in the policy of the profile.
- The tools that are connected, such as the tools of an MCP server, and the skills that are installed. The kernel reads them at each model call, so the skill index in the system prompt matches what the `skill` tool serves.
- The settings of the process, such as `compaction`, `time.noteAfter`, `tools.limits`, and `spawn`.

A run from the ledger of an older kernel has no resolved profile and reads the named profile at each step. A fleet can mix kernels. A process of an older kernel that takes a branch through `POST /v1/runs/fork` ignores `resolved` and runs the branch with its own profile, as it reads the named profile at each step.

A skill is a directory with a `SKILL.md` file whose front matter names and describes it. The description is one line of at most 300 bytes. The kernel reads a YAML block scalar, folded (`>`) or literal (`|`), as its text. The skills live under `$FW_HOME/skills`, or under the directories in `tools.skills`, which replace it, and under the path `spec.skills.path` of each Workspace of an ax Task, relative to its binding when it is not absolute. The kernel reads them in that order, and the first directory that holds a name wins. A skill of the process therefore shadows a skill of a Workspace with the same name: the configuration of the operator wins over the files that a Task brings, so a Workspace cannot replace a skill that the operator vetted. An MCP server adds skills under the names `<server>/<name>` (section 7.3, Skills of a server). The system prompt lists the skills that the profile allows, one line each: the skills of the directories, sorted by name, then the skills of each server in the order of `tools.mcp`, each with the name of its server. The `skill` tool returns the body of one skill and the list of its other files, and its field `file` returns one of those files. It reads only a skill that the profile of the run allows. The MCP server of the workbench serves the same files as resources under `fw://skills/<name>`. A profile with no `skills` entry sees every skill, and a profile with an empty list sees none.

### 12.6 Driver registry

Each driver registers itself by name at init, for example `fw.RegisterBus("nats", nats.New)`. A third-party Go module adds a driver by being imported. A blank import in `cmd/fw/drivers.go` selects the drivers that the default binary includes. A custom build leaves out a driver by leaving out its import.

A linked package can also change the default configuration. It calls `fw.RegisterDefaults(func(*fw.Config))` in `init`. `DefaultConfig` applies the changes after the kernel defaults, in the order of registration. A file or a variable still overrides the result. The workbench uses it to add the surface `mcp` and the tool `propose_agent` (the workbench architecture, section 1.1). In the same way, `fw.RegisterExtension` claims a configuration block (section 12.1), `fw.RegisterRunAnnotator` adds a run annotator, `fw.RegisterService` adds a service (section 12.4), `fw.RegisterSpawner` adds a spawner driver (section 11.9), `fw.RegisterDecider` adds the client of a decider (section 22.2), `metrics.RegisterCollector` adds series to the metrics surface (section 10.1), `cli.RegisterDoctorCheck` adds a line to `fw doctor`, `Kit.SetRelay` of the native surface sets the relay for the runs and the profiles of other processes (section 12.8), and `Agent.SetPersonTokens` gives the tool sources the tokens of people (section 7.11).

### 12.7 Out-of-process extension

Three protocols extend `fw` without Go code:

- MCP adds tools and policy checks through the MCP client. The MCP server of the workbench also brings work in through its `run` tool (the workbench architecture, section 3).
- A2A adds peers that the agent can ask.
- The bus adds intake, egress, and request and reply hooks.

### 12.8 Public Go API

The package `fw` is the main public package. It holds the interfaces above, the option functions, and four entry points. A program that builds on the kernel, such as the workbench, also uses two more public packages:

- `cli` holds the command line. `cli.Run` runs a verb. A binary sets function variables such as `cli.NewLogger`, `cli.CardJSON`, and `cli.ServeMCPStdio` for the packages that it links. A nil variable turns off what needs it.
- `surface/native` is the extension point of the native API. `native.Extend` registers an `Extension`, and `native.New` mounts each one after the run routes. `native.NewWith(exts...)` mounts only the given ones.

An extension gets a `*native.Kit`. The kit has `Agent`, `Handle` (behind authentication), `HandlePublic`, `SetSigninPath`, and `SetRelay`. It also has the helpers of the run routes: `Remote`, `Start`, `Fail`, `Decode`, and `DecodeFiles`. The package also exports `WriteProblem`, `WriteJSON`, `Accepts`, `Cause`, `Attachments`, the type `FileBody`, and the limits `DefaultEventsLimit` and `MaxEventsLimit`. The names of its headers are `HeaderIdempotencyKey` and `HeaderOnBehalfOf` (section 8.2).

A relay (`native.Relay`) answers a request about what another process holds. An extension sets it with `Kit.SetRelay`, and the last one to set it wins. The surface calls its method `Take(w, r) bool` before the handler of each route of one run, and before `POST /v1/runs`. When `Take` answered the request, it returns true. `Kit.Remote` wraps a route of an extension in the same way.

A relay can also answer `GET /v1/agents/{name}` for a profile that another process runs, such as the profile of a pool of workers of a fleet. The process of the surface holds no such profile, so its own lookup answers 404. For this route the relay implements the optional interface `native.AgentRelay`:

```go
type AgentRelay interface {
    TakeAgent(w http.ResponseWriter, r *http.Request, name string) bool
}
```

The surface checks the relay for this interface. It calls `TakeAgent` after the authentication of the caller and before its own lookup, with the path value `name`. When `TakeAgent` answered the request, it returns true. With false, the kernel answers as it does without a relay. The kernel checks no right before the call. So the relay checks the rights of the caller itself. It answers a name that the caller cannot view with a 404, as the kernel does. Without a relay, or with a relay that does not implement `AgentRelay`, the route does not change.

Semantic versioning applies to these three packages. A sketch of `fw`:

```go
package fw

// New builds an agent from options, fw.yaml, and FW_* variables, in that order.
func New(opts ...Option) (*Agent, error)

// Run starts one run and blocks until it reaches a terminal state or needs input.
func (a *Agent) Run(ctx context.Context, input string, opts ...RunOption) (*Run, error)

// Start starts a run and returns at once. Watch it through Run.Events.
func (a *Agent) Start(ctx context.Context, input string, opts ...RunOption) (*Run, error)

// WaitFor blocks until done reports true for the status of the run.
func (a *Agent) WaitFor(ctx context.Context, id string, done func(*Run) bool) (*Run, error)

// Children lists the child runs of a run.
func (a *Agent) Children(ctx context.Context, id string) ([]*Run, error)

// SpawnChild starts a child of a run: in this process, or through the
// Spawner of the agent in another one (section 11.9). A request with the
// idempotency key of an earlier start of the same parent returns the
// child of that start. NewRunRequest builds its request from an input and
// options.
func (a *Agent) SpawnChild(ctx context.Context, parent string, req RunRequest) (*Run, error)
func (a *Agent) Spawner() Spawner
func NewRunRequest(input string, opts ...RunOption) RunRequest

// AuthorizeAll asks the Authz about several checks for the principal in ctx.
// An Authz that implements BatchAuthz gets every check in one call.
func (a *Agent) AuthorizeAll(ctx context.Context, checks []AuthzCheck) []error

// RunClock returns the time of a run: the time now and the start in the zone
// of the run, its working time, and the working time left before the deadline.
func (a *Agent) RunClock(ctx context.Context, id string) (RunClock, error)

// RunBudget returns the effective budget of a run: its own budget, merged
// with the budget of its profile and of the configuration as the kernel
// resolved them when it queued the run, with every change of the budget on
// top, and capped by the budget of its parent. MaxTokens, MaxCostUSD, and
// MaxToolCalls hold for the run and every run under it together (section
// 5.2); Run.TreeUsage tells what the tree used.
func (a *Agent) RunBudget(ctx context.Context, id string) (Budget, error)

// ChangeBudget changes the budget of a run: each field that change sets
// replaces the same field for the work that follows (section 5.2). It
// needs the operator role on the run, records fw.budget.changed, and
// returns the effective budget after the change.
func (a *Agent) ChangeBudget(ctx context.Context, id string, change Budget, origin string) (Budget, error)

// Send adds a follow-up message to a run (section 5.5). WithSendBudget
// changes the budget of the run before the message. WithMessageFrom names
// the party that a trusted caller relays the message for, and
// WithMessagePinned keeps the message through every compaction.
func (a *Agent) Send(ctx context.Context, id string, msg Message, origin string, opts ...SendOption) error
func WithSendBudget(b Budget) SendOption
func WithMessageFrom(f MessageFrom) SendOption
func WithMessagePinned() SendOption

// WithActingFor names the person for whom the caller in ctx acts, so the
// records of the action name that person (section 8.2). Only the process,
// the static token, or a workload acts for another person. The kernel
// still authorizes the caller alone. ActingFor reads the person back.
func WithActingFor(ctx context.Context, subject string) (context.Context, error)
func ActingFor(ctx context.Context) string

// Live streams the text of a run as the model produces it: delta, tool,
// done, and idle events, never in the ledger (section 5.1).
func (a *Agent) Live(ctx context.Context, id string) (<-chan LiveEvent, error)

// React records the reaction of a person on an event of a run as
// fw.run.reacted. A thumb on the end of a completed episode is also a
// quality receipt (section 8.2).
func (a *Agent) React(ctx context.Context, id string, d ReactionData) error

// Following counts the children in other processes that work; the
// process is not idle while one does (section 11.9).
func (a *Agent) Following() int

// TitleFrom makes a short title from the input of a run (section 8.2).
func TitleFrom(input string) string

// WithCheck makes a run a check run: the kernel makes the tool calls of
// the check under the policy of the agent and calls no model (section
// 23). Resolved fills the defaults of a check and names its first
// problem. Kind is RunKindCheck for a check run.
func WithCheck(c Check) RunOption
func (c Check) Resolved() (Check, error)
func (r *Run) Kind() string

// Run options: AsAgent, WithInstructions, WithRunProfile, WithRunBudget,
// WithParent, WithMetadata, WithIdempotencyKey, WithOrigin, and
// InFilesRoot, which keeps the tools of a run in the root of the files
// tool with tools.files.perRun (section 7.9).

// Serve mounts every enabled surface on one listener and blocks.
func (a *Agent) Serve(ctx context.Context) error


// Files returns the workspace with the rules of tools.files (section 7.9).
func (a *Agent) Files() Files

// ToolUser returns the user of the tool processes (tools.user), or nil
// when they run as the user of fw (section 7.12). Apply starts an
// exec.Cmd as that user, and ToolEnv builds the environment of such a
// process.
func (a *Agent) ToolUser() *ToolUser
func ToolEnv(ctx context.Context, cfg *Config, inherited, configured []string) []string

// PersonTokens returns the tokens of people that a linked package set with
// SetPersonTokens, and Connected lets every paused call of a person go on
// whose service is now connected (section 7.11).
func (a *Agent) PersonTokens() PersonTokens
func (a *Agent) SetPersonTokens(p PersonTokens)
func (a *Agent) Connected(subject string, servers ...string) int

// RequestConnect pauses the tool call in ctx until the person of the run
// connects a service. A tool source that acts for a person calls it when
// PersonTokens has no token of the person.
func RequestConnect(ctx context.Context, req ConnectRequest) error

// Ask is the one-call form for the smallest cases.
func Ask(ctx context.Context, input string, opts ...Option) (string, error)

// Options. Each one starts with "With", because the plain names are the
// interface types.
func WithConfig(c *Config) Option
func WithConfigFile(path string) Option
func WithModelName(selector string) Option
func WithModel(m Model) Option
func WithTools(t ...Tool) Option
func WithToolSources(s ...ToolSource) Option
func WithPolicy(p Policy) Option
func WithLedger(l Ledger) Option
func WithBus(b Bus) Option
func WithSecrets(s Secrets) Option
func WithCompactor(c Compactor) Option
func WithSurfaces(s ...Surface) Option
func WithObservers(o ...Observer) Option
func WithRunAnnotators(fns ...RunAnnotator) Option
func WithModelMiddleware(m ...ModelMiddleware) Option
func WithToolMiddleware(m ...ToolMiddleware) Option
func WithProfile(p AgentProfile) Option
func WithBudget(b Budget) Option
func WithListen(addr string) Option
func WithAuthn(a Authn) Option
func WithAuthz(z Authz) Option
func WithTLS(c *tls.Config) Option
func WithPrices(p Prices) Option
func WithLogger(l *slog.Logger) Option
func WithClock(c Clock) Option
func WithServices(s ...Service) Option
func WithSpawner(s Spawner) Option
func WithoutServices() Option
func WithFiles(f Files) Option
func WithoutDefaults() Option
```

`WithAuthz` wins over `WithAuthzFrom` and the rules of `access`, and `WithPolicy` wins over `policy.external` (section 13.5).

A complete library program. Each driver registers itself at import, so the blank imports select the drivers:

```go
import (
    "github.com/bdubs00/featherweight-kernel/fw"
    _ "github.com/bdubs00/featherweight-kernel/bus/nats"
    _ "github.com/bdubs00/featherweight-kernel/model/anthropic"
    _ "github.com/bdubs00/featherweight-kernel/surface/native"
    _ "github.com/bdubs00/featherweight-kernel/tools/builtin"
    _ "github.com/bdubs00/featherweight-kernel/tools/mcp"
)

cfg := fw.DefaultConfig()
cfg.Bus.Driver = "nats"
cfg.Bus.URL = os.Getenv("NATS_URL")
cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "git", Endpoint: "https://git-mcp.default.svc.cluster.local:8443/mcp"}}

agent, err := fw.New(fw.WithConfig(cfg), fw.WithListen(":8080"))
if err != nil {
    log.Fatal(err)
}
log.Fatal(agent.Serve(ctx))
```

### 12.9 The command line

The command line mirrors the API. Each command is one verb.

| Command | Purpose |
|---|---|
| `fw init [dir]` | Write a starter `fw.yaml`, a default prompt, and the `agents` and `skills` directories. The starter sets no model: it shows one commented `model` line for each provider, and `fw init` prints that a model must be set, because a paid model is never a built-in choice. |
| `fw doctor` | Report on the configuration, credential, model, tool sources, connections, bus, ledger, skills, memory, and time zone, one line each |
| `fw run "goal"` | Run once in the terminal and print the output |
| `fw serve` | Serve every enabled surface |
| `fw tools` | List tools from every source |
| `fw skills [name]` | List the skills, or print one skill |
| `fw events <run>` | Print or follow the ledger of a run |
| `fw replay <ledger>` | Replay a ledger against a model |
| `fw card` | Print the A2A agent card |
| `fw config` | Print the resolved configuration with secrets masked |
| `fw env` | Print every configuration variable, and the value of each one that is set |
| `fw agents [name]` | List the profiles or print one. `put` and `delete` come with `fw-workbench`. |
| `fw whoami` | Print the local principal that the commands use |
| `fw routine replay --routine r.yaml [--ledger dir] [--agent name] [--decide] [--json]` | Check a routine against the runs of a ledger (section 22.4) |
| `fw routine propose --agent name [--ledger dir] [--runs ids] [--out r.yaml] [--name n] [--decider d] [--json]` | Make a routine from the runs of an agent (section 22.4). A build with the tag `lean` leaves it out, and the tag `propose` brings it back. |

`fw-workbench` takes every verb above. It adds `fw-workbench mcp`, which serves the MCP server over stdio (the workbench architecture, section 3). In `fw`, the verb `mcp` fails with a message that names `fw-workbench mcp`.

`fw doctor` starts each line with `ok`, `warn`, or `FAIL`. A `FAIL` line is a problem, and the exit code is then 1. A `warn` line names a risk of a setup that works, and the exit code stays 0. Three checks warn: no model in the configuration, because the process then refuses every run, the memory scope `agent` inside an ax Task, because each person then reads the notes that the runs of other people wrote (section 7.8.1), and a memory directory whose walk stops at its bound, because the counts are then too low. The memory line names the scope, the index, and the notes of the profile `default` in the directory of that scope: under `principal`, the notes of all people together and the notes of the runs without a principal. Inside a Task, an empty `tools.memory.scope` counts as `principal`, as `ax.ApplyToConfig` sets it for `fw serve`.

`fw run` shapes the run from flags without a configuration change. `-system` and `-i` take a text or `@path` to a file, and `-tools` and `-skills` take globs. `-model`, `-effort`, `-time-zone`, `-max-turns`, `-deadline`, and `-max-tool-calls` set the rest. The flags build an inline profile (section 12.5). When a run stops at a budget, `fw run` names the limit and the flag or key that raises it.

`fw run` and `fw events --format text` print one line for each event. The line of `fw.run.queued` names the work first, for example `fw.run.queued title="Fix checkout retries" agent=reviewer origin=http`. The title is the title of the run (section 8.2). An input without text gives no `title` field. `fw replay` names the source run with its title on standard error. The JSON of `fw run --json` and `fw replay --json` is the `fw.Run` object, so it gains the field `title`.

## 13. Security

### 13.1 Who the caller is

Every surface authenticates every request with the `Authn` plug and puts the principal in the request context. A principal has a subject, a name, a kind, an issuer, groups, and scopes. The kinds are `user`, `workload`, `token`, `local`, and `anonymous`. A `user` is a person, from an OIDC token. A `workload` is a service or another agent, from a SPIFFE identity. `token` is the static bearer token of the process, and `local` is the user of the process, over stdio. `anonymous` has no credential, and it comes from `auth.mode: none`.

`auth.mode` names one mode, or several separated by commas that are tried in order, for example `oidc,spiffe,token`. The first mode that accepts the credential wins, and the first mode writes the challenge.

A refused request gets a 401 with the challenge, and the log gets one line at level info, `request refused without a valid credential`. The line holds the method, the route pattern, the path, the reason, and `node`, the name of the process, so the log of several replicas says which one refused. The reason is `no credential` for a request without an `Authorization` header and without a client certificate. Otherwise it names the reason of each mode, such as `oidc: token expired; token: the bearer token is not the static token`. No token and no other value of a header enters the line. For each method, route, and reason, at most one line goes out per minute, and the next line counts the refusals that the log left out in `skipped`. A client that loops on a 401 therefore writes one line a minute.

| Mode | Credential | Subject |
|---|---|---|
| `token` | the static bearer token from the secret `auth.tokenEnv` | `token` |
| `oidc` | a bearer token from the issuer `auth.oidc.issuer`, checked against its keys, its issuer, its expiry, and `auth.oidc.audience` | the `sub` claim. The name comes from `email` or `auth.oidc.nameClaim`, and the groups from `auth.oidc.groupsClaim`. |
| `spiffe` | a JWT-SVID bearer token. With `auth.spiffe.x509` and a listener that verifies clients, also an X.509-SVID client certificate. | the SPIFFE ID, for example `spiffe://example.org/ns/prod/sa/reviewer` |
| `none` | nothing | `anonymous` |

Both drivers are packages of the kernel, `authn/oidc` and `authn/spiffe`, because a worker that people reach through the front verifies their tokens itself; a lean build leaves out the one it does not use (section 15). The `oidc` driver reads the discovery document of the issuer and refreshes its keys. Its challenge names the protected-resource document at `/.well-known/oauth-protected-resource`, so an MCP client finds the authorization server and does the OAuth flow itself. The `spiffe` driver reads the trust bundle from the Workload API at `auth.spiffe.socket`. Without an agent, it reads the bundle from the file or URL `auth.spiffe.bundle`. The same driver is the TLS provider `spiffe`, which serves the X.509-SVID of the workload and verifies client certificates against the bundle. An MCP server entry with `auth.spiffe` sends a JWT-SVID of this workload on every request, so one agent calls another with no shared secret.

TLS on the listener comes from `tls.provider`: `file` uses `tls.cert` and `tls.key`, with `tls.clientCA` and `tls.clientAuth` for client certificates, and a driver such as `spiffe` builds the configuration itself. Without a provider the listener speaks plain HTTP with unencrypted HTTP/2, which is the right choice behind the atenet router.

The workbench adds a browser sign-in to the page at `/ui`, and it serves the protected-resource document (the workbench architecture, sections 2 and 5).

### 13.2 What the caller can do

An agent profile carries an `access` block with three lists: `owners`, `operators`, and `viewers`. An owner manages the profile and does everything below. An operator starts runs, sends follow-ups, approves, cancels, and changes the budget of a run. A viewer reads runs and their ledgers. Whoever starts a run can always view and operate that run. A child run belongs to the one who started its parent, so the same people see the whole tree.

Each entry names principals in one of five forms:

| Entry | Matches |
|---|---|
| `alice@example.com` or `spiffe://example.org/ns/prod/*` | the subject or the name, as a glob where `*` matches any text |
| `group:platform-team` | a group of the principal, as a glob |
| `kind:workload` | every principal of that kind |
| `issuer:https://idp.example.com` | every principal from that issuer |
| `*` | every principal |

The process-wide block `access` holds `admins`, who own every agent, `creators`, who can add agents, and `default`. With `default: open`, which is the default, an agent that has no `access` block is open to every authenticated principal as an operator. That is how fw behaved before access existed. With `default: closed`, such an agent is reachable by its admins only. A deployment therefore turns identity on in two steps: set `auth.mode` and `access.admins`, then switch `access.default` to `closed` once every agent names its owners.

The kernel enforces this through the `Authz` plug on every call that carries a principal. A run on an agent needs the operator role, and a read of a run needs the viewer role or the run itself. A decision, a cancel, or a change of the budget needs the operator role or the run itself, and a profile change needs the owner role. A trusted relay can name the person it acts for (section 8.2). The kernel still decides for the relay, and the named person reaches no decision. A call that carries no principal comes from the process itself, such as a tool that starts a child run, and is trusted. A tool that reads runs for the run that called it, such as `recall`, puts the principal of that run in the context of its reads (`fw.WithRunPrincipal`), so the kernel decides them as reads of the person who started the run, not as calls of the process (section 7.8.3). The routes of the memory follow the same roles, and a note of the scope `shared`, which every profile reads, needs an admin (section 8.2). A target can name another process that holds the agent and the run, a member of the fleet of the workbench (the workbench architecture, section 4). The default rules refuse such a target, because they do not know the rules of another process. The default `Authz` is the rules above, and `fw.WithAuthz` replaces it with a policy of your own. `fw.WithAuthzFrom` builds one from the agent once the agent has its configuration and its bus; the workbench uses it to hand the decisions to a control plane outside the process (section 13.5 and the workbench architecture, section 12). A worker kernel in a fleet decides with the rules above alone.

### 13.3 Managing agents

The kernel reads its profiles from `fw.yaml`, from the `FW_AGENTS_*` variables, from Go options, and from the files under `$FW_HOME/agents`, and it writes none. A linked package that manages agents for people, the workbench, writes a file and hands the kernel the new set with `Agent.SetProfiles`, so a run can name the new profile at once. A run that is already queued keeps the profile that it was queued with (section 12.5). `SetProfiles` checks every profile with `Agent.CheckProfile`, which judges the name and the fields of a profile and changes nothing. A profile that the configuration defines is fixed: `Agent.ProfileLocked` reports it, and `SetProfiles` refuses a change to it, because the configuration wins over a file at the next start. The loader names the profiles that it read from files in `Config.FileProfiles`, so the kernel knows which profiles are fixed.

The actions `agent.create`, `agent.edit`, and `agent.delete` stay in the kernel, because they define the roles of section 13.2. The workbench authorizes a write with them, gives a new profile to its creator as the first owner, and serves `PUT` and `DELETE /v1/agents/{name}`, `PUT /v1/agents/{name}?dryRun=true`, the MCP tools, and the verbs `put` and `delete` of `fw agents` (the workbench architecture, section 10). `Agent.ProfileGrants` gives the verdict of each tool of an agent, and the view of one agent carries it in plain words as `abilities`.

### 13.4 Rules

- Authentication is on by default. A missing `FW_API_TOKEN` with `auth.mode: token` stops the process at start.
- Secrets never enter the ledger. Before each append, the kernel replaces each known secret value and each text with the shape of a key (section 4.2). Give a secret to an agent through `Secrets` or the egress policy. A secret in the input of a run reaches the model as `[redacted]`.
- Tool results are data, not instructions. The kernel passes them to the model as tool result blocks only.
- Operator instructions use system messages. User and tool content never reach the system prompt.
- Destructive tools ask by default. A deployment that wants no prompts sets the policy to allow, in writing, in `fw.yaml`.
- An approval binds the tools that `fw` runs, not a shell that calls a host with a credential that the egress gateway adds (section 7.12). For untrusted input, give the profile no shell and give credentials only to MCP servers that the kernel calls.
- The `files` tool is jailed to its root path and never opens the home directory of `fw`, the file ledger, or the configuration file. The `http` tool uses an allowlist. The sandbox of the Task, which is ax under ax, keeps `bash` inside the Task. Inside the Task, the tool user keeps `bash` and the stdio MCP servers away from the state, the memory, and the secrets of `fw` (section 7.12). Under ax the tool user is on by default.
- Annotations from an MCP server are hints. The policy trusts only the servers that `fw.yaml` lists as trusted.
- Budgets cap every run. A run with no budget gets the profile budget. A profile with no budget gets the default: 200 turns, 2 hours of working time, 1000 tool calls, 32 calls per turn, and 5 repeats of one call.
- At a limit of turns, tool calls, or working time, the default budget spends one bounded model call past the limit: the final report, with at most 1,024 output tokens and 2 minutes. It never spends past a limit of tokens or cost. `budget.finalReport: false` turns it off (section 5.2).
- Tool limits bound every call: 256 KB in, 100 KB out, and 10 minutes, per tool and per configuration. A large data source cannot flood the context window.
- A child run never has more than its parent: fewer tools, stricter rules, a smaller budget, and a bounded depth and count.
- An inline profile on a request never carries an `access` block, so a request cannot grant itself a role. Its tool list can give a run more tools than its agent lists, and every surface says so (section 12.5).
- The principal of a run and the decider of an approval are in the ledger, so every action has a name on it. For an action that a trusted relay makes for a person, the name is that person (section 8.2).
- A control plane that does not answer never opens access. With `policy.external`, the tool call is denied. An `Authz` that reports `fw.ErrAuthzUnavailable`, such as the control-plane authorization of the workbench, gets a 503 for the caller.

Do not set `auth.mode: none` on a listener that is reachable from outside the cluster. Anyone who reaches the port can start runs and approve tool calls. Do not set `access.default: closed` without an entry in `access.admins`, or nobody can manage the agents until the configuration changes.

### 13.5 External decisions

A deployment with many agents often keeps its rules in one control plane. With `policy.external`, that control plane decides which tool calls run. `fw` sends the facts of each decision and enforces the answer. It makes no decision of its own for that part, so the control plane is the single source of truth. A security team changes a rule without a change to `fw.yaml` and without a rebuild.

Who can do what is a question of the front, not of a worker. The workbench asks a control plane with `workbench.access.external` (the workbench architecture, section 12), and a worker kernel decides with the rules of `access` alone (section 13.2). The kernel defines the protocol of both decisions, the requests, the answers, and the transports, so one control plane serves both, and it builds a decision point for a linked package with `Agent.NewDecisionPoint`.

The block `policy.external`, and the block of the workbench, have the same fields:

| Field | Meaning | Default |
|---|---|---|
| `mcp` | The name of a `tools.mcp` entry. The endpoint, the TLS, and the authentication of that entry apply. | none |
| `tool` | The tool on that server | `decide` for `policy.external`, `authorize` for the authorization of the workbench |
| `bus` | The bus subject that receives the requests | none |
| `timeout` | The limit of one request | `5s` |

A block sets exactly one of `mcp` and `bus`, or the process stops at start. Every field has a variable, for example `FW_POLICY_EXTERNAL_MCP=control` and `FW_POLICY_EXTERNAL_BUS=fw.policy`. The option `fw.WithPolicy` wins over the block. This configuration of a front sends both kinds of decision to one MCP server:

```yaml
tools:
  mcp:
    - name: control
      endpoint: https://control.example.com/mcp
      auth: { bearer: CONTROL_TOKEN }
policy:
  external: { mcp: control }
workbench:
  access:
    external: { mcp: control }
```

#### The requests and the answers

Every request is a JSON object with `kind` and `version`. The kind is `authz` or `policy`, so one tool can serve both. The version is 1. An authorization request, which the front of the workbench sends, asks about one principal and one or more checks:

```json
{
  "kind": "authz",
  "version": 1,
  "principal": {"subject": "bob@example.com", "kind": "user", "issuer": "https://idp.example.com", "groups": ["platform-team"]},
  "checks": [
    {"action": "run.operate", "agent": "reviewer", "access": {"owners": ["alice@example.com"]},
     "run": {"id": "01JA3Q9Z6K2M4N8P0R1S2T3U4V", "agent": "reviewer", "principal": "alice@example.com",
             "state": "input-required", "origin": "http",
             "lineage": [{"id": "01JA3Q8X1C2D3E4F5G6H7J8K9M", "agent": "planner", "principal": "alice@example.com"}]}},
    {"action": "agent.view", "agent": "reviewer", "access": {"owners": ["alice@example.com"]}}
  ]
}
```

The actions are the actions of section 13.2: `agent.view`, `agent.run`, `agent.edit`, `agent.delete`, `agent.create`, `run.view`, `run.operate`, and `admin`. `access` is the access block of the profile, as data. A check about an agent or a run of another process also carries `member`, the name of that process in the fleet of the workbench (the workbench architecture, section 4). Then `access` and `lineage` come from the record of that member. `run` is set for an action on a run. Its `principal` is the subject that started the run, and `lineage` lists the ancestors of a child run, the parent first. The answer holds one decision per check, in the same order:

```json
{"decisions": [{"allow": false, "reason": "only the owner of the root run operates it"}, {"allow": true, "ttlMs": 30000}]}
```

A policy request describes one tool call: the tool and its flags, the call id, the input, the resolved input (section 7.5), and the run.

```json
{
  "kind": "policy",
  "version": 1,
  "tool": {"name": "bash", "source": "builtin", "destructive": true, "trusted": true},
  "callId": "toolu_01",
  "input": {"command": "git push origin main"},
  "run": {"id": "01JA3Q9Z6K2M4N8P0R1S2T3U4V", "agent": "reviewer", "principal": "alice@example.com", "state": "running", "origin": "http"}
}
```

The answer is one verdict, `allow`, `deny`, or `ask`, with an optional reason:

```json
{"verdict": "ask", "reason": "a push to main needs a second person"}
```

The Go types are `fw.AuthzRequest`, `fw.AuthzAnswer`, `fw.PolicyRequest`, and `fw.PolicyAnswer`.

#### Over MCP and over the bus

With `mcp`, `fw` calls the tool by its plain name with the request as the arguments. It reads the answer from `structuredContent`, or else from the first text block that holds a JSON object. A result with `isError` counts as no answer. A server that is down after repeated failures (section 7.3) also counts as no answer. The MCP tool source leaves out every server that a block names, so the model never sees or calls a decision tool; a linked package names its servers with `fw.RegisterControlServers`. The MCP driver registers this transport with `fw.RegisterDecisionPoint`.

With `bus`, `fw` publishes a `fw.decision.requested` event to the subject. Its data holds `replyTo`, a reply subject of the process, and `request`, the request above. A responder publishes a `fw.decision.answered` event to `replyTo`. Its data holds `inReplyTo`, the id of the request event, and `answer`. Any process that subscribes to the subject can answer. A Go responder reads a request with `fw.DecisionRequestOf` and builds the answer event with `fw.AnswerDecision`.

`fw doctor` prints one line for each decision point. For an MCP server, it makes sure that the server lists the tool. A bus subject has no probe, because a responder only answers a request.

#### Authorization

A worker kernel authorizes with the rules of section 13.2. The control-plane authorization is a plug of the workbench (its architecture, section 12): the front builds an `Authz` over a decision point through `fw.WithAuthzFrom`, and `AccessMode` reports `external` for it. Such an `Authz` is a `fw.LineageAuthz`: it sends the lineage of a run in each check and decides about a child run itself, so the kernel does not ask again for the ancestors after a refusal. `Agent.RunFacts` gives it the lineage of a run of this process.

If the control plane does not answer in time, the plug refuses the request with `fw.ErrAuthzUnavailable`, and every surface of the kernel tells that apart from a refusal. The native API answers 503 with `Retry-After: 5`, and the MCP surface returns an error result that says that the service did not answer. A caller therefore tells a refusal, a 403, from a control plane that is down.

A2A has no error for a service that is down, so the A2A surface sends the server error of the protocol. Its details carry the reason `AUTHZ_UNAVAILABLE` and `retryAfterSeconds: 5`. The metadata of its error info carries `fwReason: AUTHZ_UNAVAILABLE` and `retryAfter: "5"`. The JSON-RPC binding sends this error with the code -32000 and HTTP 200, as JSON-RPC defines. The REST binding sends HTTP 503 with `Retry-After: 5`, and the error document has the code 503 and the status `UNAVAILABLE`. If the first event of a stream on the REST binding is this error, the stream gets the same 503. The surface holds the headers of a stream until its first event to make this possible, and that event comes right away.

A call from the process itself carries no principal and never asks. Runs, child runs, resumes, and the bootstrap goal therefore keep working during an outage. The verbs `fw agents`, `fw whoami`, and `fw-workbench mcp` act as the local principal (section 13.1) when a control plane decides, which the hook `cli.ExternalAccess` of the workbench tells the command line; the control plane therefore also decides for the local user.

#### Policy

`policy.external` replaces the rules of `policy:`. The control plane decides every tool call. `allow` runs the call, and `deny` refuses it with the reason. `ask` asks the caller for an approval (section 5.3), and the `Authz` of the process authorizes the decision on that approval as the action `run.operate`.

The policy of a profile and the rules of the ancestors of a run still apply, and the stricter verdict wins. They can therefore only make the verdict of the control plane stricter. If the control plane does not answer, or answers with an unknown verdict, the call is denied. The model reads the reason in the tool result.

After an approval, the kernel asks again about the calls of the response that are still open. The verdict therefore holds at the moment that a call runs.

Do not add a second `tools.mcp` entry with the endpoint of a decision point. `fw` hides the entry that a block names from the model, but the model can call the decision tool through the second entry.

### 13.6 Teams

A team is a group of people and agents that work together. Teams group the view of the workbench and grant no access, so the kernel knows no team. It records the principal that started a run, in `fw.run.queued`, and a run annotator of the workbench adds the teams of that person to the metadata of the run (section 12.4). The workbench architecture, section 8, describes the teams, the configuration block `workbench`, and `by` on the runs lists of the workbench.

## 14. Repository layout

Drivers live in public packages, not under `internal/`, because a Go program outside this module selects a driver by importing it.

```
cmd/fw/                 the binary and the drivers that it links
cli/                    the command line, public so that another binary reuses it
authn/oidc/             the oidc authentication driver
authn/spiffe/           the spiffe authentication driver and TLS provider
fw/                     the public contract and the kernel: interfaces, types,
                        configuration, options, registry, turn loop, projections,
                        resume, replay, compaction, clock, external decisions,
                        file ledger ("file"), in-memory ledger and bus ("mem"),
                        rules policy, grant stores and connections
config/                 fw.yaml and FW_* loading
model/anthropic/        default adapter
model/gemini/           Gemini adapter
model/openai/           OpenAI-compatible adapter
tools/builtin/          bash, files, http, memory, recall, skill, clock, ask_user,
                        send, spawn_task, await_task, decide
memory/                 the memory store: notes, scopes, limits, audit, recall,
                        and the optional FTS5 index (tag memory_sqlite)
routine/                replay and propose of routines over recorded runs (section 22.4),
                        propose only without the tag lean, or with the tag propose
decider/systemone/      the client of a System One server, the decider driver "systemone"
tools/mcp/              MCP client as a ToolSource, a decision point, and a Connector
tools/a2a/              A2A peers as a ToolSource
bus/nats/ bus/redis/
surface/native/         the native API for runs and agents, and its extension point
surface/a2a/ surface/metrics/
surface/axmeta/         the ax metadata endpoints on a loopback listener
surface/view/           the shapes that the native routes return
ax/                     runner contract: manifests, workspace setup, supervision,
                        and the child Tasks of spawn.driver ax with their sweep
ax/guest/               the guest services of ax ssh
telemetry/              Prometheus registry, OpenTelemetry setup, slog
examples/               fw.yaml, ax manifests, observability, prompts, skills
docs/ARCHITECTURE.md    this document
Dockerfile              fw:static on distroless
Dockerfile.toolbox      fw:toolbox on Debian with git, python3, uv, and pixi
```

The workbench architecture, section 1.3, gives the layout of the workbench.

## 15. Dependencies

The default binary carries these modules. The blank imports of `cmd/fw/drivers.go` and `cmd/fw/driver_*.go` link the drivers. Without a build tag the binary carries every driver. With the tag `lean` it carries the core alone, the configuration loader, the native API, the built-in tools, the MCP client, the token client of the fleet, the System One client of the deciders, and the telemetry, and each driver comes back with its own tag: `nats`, `redis`, `anthropic`, `openai`, `gemini`, `spiffe`, `oidc`, `metrics`, and `a2a`. A worker image for one fleet therefore builds with the drivers it uses inside ax, for example `go build -tags lean,nats,openai,oidc ./cmd/fw`, or `docker build --build-arg TAGS=lean,nats,openai,oidc`, and the Makefile takes `TAGS`. The full binary is about 82 MB; the lean core is about 38 MB, and a worker with NATS, OpenAI, and `oidc` about 41 MB. A configuration that names a driver that the build left out stops at start with the name of the driver, except a surface or a built-in tool, which the kernel skips with a warning. One tag adds a capability and no driver: `memory_sqlite` adds the SQLite FTS5 index of the memory (section 7.8.3) from the pure-Go `modernc.org/sqlite`, in the full and in the lean build alike, for about 4 MB; without it, the memory scans the notes. The tag `lean` also leaves out `fw routine propose`, which reads many runs, the work of the workbench or of a librarian over a ledger, not of a worker. The tag `propose` brings the verb back (section 22.4). `fw routine replay` stays in every build. The ax manifest types are defined in the `ax` package from the YAML schema, so the binary does not import the ax module. Unencrypted HTTP/2 on one port comes from the standard library `http.Server.Protocols` field.

| Module | Used for |
|---|---|
| `github.com/anthropics/anthropic-sdk-go` | Anthropic adapter |
| `github.com/modelcontextprotocol/go-sdk` | MCP client |
| `github.com/a2aproject/a2a-go/v2` | A2A client and server |
| `github.com/prometheus/client_golang` | Metrics |
| `go.opentelemetry.io/otel` and the OTLP exporter | Traces |
| `github.com/nats-io/nats.go` | NATS bus and ledger |
| `github.com/redis/go-redis/v9` | Redis bus |
| `google.golang.org/genai` | Gemini adapter |
| `github.com/agent-substrate/env` | Guest services of `ax ssh` (section 11.12) |
| `github.com/go-git/go-git/v5` | Clone without a git binary in `fw:static` |
| `gopkg.in/yaml.v3` | Manifests and `fw.yaml` |

## 16. Delivery order

Each milestone ends with a working binary. As of 2026-10-13, every milestone below is implemented and tested except these parts:

- the Kafka driver
- the NATS and SQLite ledger drivers
- the OAuth resource-server mode of the MCP server
- the MCP task shape, because the Go SDK has no Tasks extension yet
- `spawn_task` as a child ax Task, because it starts a child run in process
- persistence of A2A push configurations across a restart
- the Vault and Google Secret Manager drivers
- the OpenAI Responses API transport

The intake revision of 2026-10-20 changed the bus intake (section 9.3). The intake takes a job only for a free slot of `maxRuns`, through the new `fw.GatedBus` and `fw.Gate` of the `nats` and `redis` drivers, and the option `concurrency` of both drivers defaults to `maxRuns`, so one Task under ax holds one job. The handler acknowledges a job when `fw.run.queued` of its run is in the ledger, which the file ledger syncs, and no longer waits for the end of the run. A run that waits for a person therefore holds no message, a suspend gives no job to a second Task, and a shutdown with a job in flight does not hang. The configuration gained `bus.intakeRetries` (default 0) and `bus.deadLetter` (default on), the dead-letter subject `<intake>.dead` with the event `fw.intake.dead`, the metadata entries `fw.intake.queue`, `fw.intake.attempt`, `fw.intake.retryOf`, and `fw.intake.outcome`, and `Config.RunSlots`.

The sandbox revision of 2026-10-20 made the tools of a Task safe for untrusted input as far as one sandbox allows (section 7.12). A tool process gets the environment of `fw.ToolEnv`, without the manifests of ax and without any variable that holds a secret. `tools.user` runs the shell and the stdio MCP servers as another user, 65532 by default under ax, and `fw` keeps FW_HOME to itself with mode `0700` and makes its process non-dumpable. `tools.files.perRun`, on by default under ax, gives each run a directory of its own (section 7.9), and `fw.InFilesRoot` keeps the bootstrap goal in the root. The kernel gained `fw.ToolUser`, `fw.LookupToolUser`, `Agent.ToolUser`, `fw.ToolEnv`, `fw.IsSecretName`, `fw.ToolTreeMarker`, `RunInfo.Dir`, and `RunQueuedData.FilesRoot`, and `fw doctor` the line `tools user`. The credentials that the egress gateway adds remain a known limit.

The export revision of 2026-10-17 added the service `Export` of the package `telemetry` (section 10.5): with the standard `OTEL_EXPORTER_OTLP_*` variables, a process pushes its Prometheus series over OTLP, sends every ledger event as a log record with the trace of its run, and starts the trace projection, which no verb started before. The run counters gained the label `batch`, and `fw doctor` gained the line `telemetry`. The fleet gauges of the workbench and the Operator view of the page go, because Grafana reads the fleet from the collector.

The jobs revision of 2026-10-17 added the metadata key `fw.batch`, the name of the job of a run, which a child inherits, and the filter `batch` of `GET /v1/runs`. A request from the bus intake keeps the principal that it names. The front of the workbench sends a job with `enqueue` and follows it with `batch` (the workbench architecture, section 4.9).

The node revision of 2026-10-17 gave every ledger event the field `node`, the name of the process from the key `node` of the configuration: `ax:<atespace>/<task>` under ax, which the package `ax` sets, else `<host>:<port>`. The front of a fleet reads the egress streams of its workers and places each run by it (docs/ROADMAP.md, section 2).

The spawner revision of 2026-10-17 put `spawn_task` behind the `Spawner` plug (docs/ROADMAP.md, section 3). `Agent.SpawnChild` starts a child in this process or through the spawner, the ledger of the parent records a remote child in `fw.run.child`, and the run methods of the agent reach a remote child through the spawner. The driver `ax` of the package `ax` creates a child Task over the ax gRPC API and drives its run through the atenet router. The native API lets a trusted caller name the person of a run with `principal` and `principalKind`, and `RunRequest` gained `PrincipalKind`. The module depends on `github.com/google/ax` for the generated API.

The token broker revision of 2026-10-17 added the kernel package `tokens` (docs/ROADMAP.md, section 3). With `tokens.url`, a process gets the access tokens of people from a broker, the front of the workbench, through `fw.PersonTokens`, and a notice on the bus resumes its paused calls when a connection ends. The configuration gained the key `tokens`, and `fw doctor` gained the line `tokens`. A `cli.DoctorCheck` of a linked package gets the secrets of the process since this revision.

The records revision of 2026-10-18 removed `fw.Retainer` from the bus contract, with the retained events of the `mem`, `nats`, and `redis` drivers and the options `retainBucket` and `retainKey`. The fleet of the workbench reads the ax API and the egress stream, and a worker publishes nothing about itself (the workbench architecture, section 4).

The authorization revision of 2026-10-18 moved the control-plane authorization to the workbench, the first item of stage 5 (docs/ROADMAP.md). The kernel lost `access.external`, its variables, and the authorization over a decision point. It gained `fw.WithAuthzFrom`, `fw.LineageAuthz`, `Agent.NewDecisionPoint`, `Agent.RunFacts`, `fw.RegisterControlServers`, and the hook `cli.ExternalAccess`. `policy.external`, the decision protocol, and the transports stay (section 13.5).

The connections revision of 2026-10-17 moved the grants and the OAuth flows out of the kernel, the fifth and last step of stage 3. The kernel lost `fw.Grant`, `fw.GrantStore`, the stores, `fw.Connector`, `fw.Connection`, `fw.ConnectStart`, `Agent.Connections`, `Agent.BeginConnect`, `Agent.FinishConnect`, `Agent.Disconnect`, `Agent.Grants`, `fw.WithGrantStore`, the key `grants` of the configuration, and the `connections` line of `fw doctor`. The MCP client keeps the sessions of people and the placeholder tool, and gets the tokens of a person from `fw.PersonTokens`, which a linked package sets with `Agent.SetPersonTokens`. `Agent.Connected` resumes the paused calls when a connection ends. The package `tools/mcp` exports its OAuth helpers for such a package. The workbench package `connections` keeps the grants under `workbench.grants` and `FW_WORKBENCH_GRANTS_*` (the workbench architecture, section 11). The file `fw/grants.go` stays: it projects the tool grants of a profile for the surfaces and has nothing to do with OAuth.

The fleet revision of 2026-10-17 moved the fleet out of the kernel, the fourth step of stage 3. The kernel lost the fleet types, `Agent.Fleet`, `fw.WithFleet`, `fw.RegisterFleet`, the key `fleet` of the configuration, the forwarding of the native routes, the fleet-wide run listing of the view, the fleet gauges, and the fleet line of `fw doctor`. It gained four hooks for every linked package: services (`fw.RegisterService`, `fw.WithServices`, `fw.WithoutServices`), the relay of the native surface (`native.Relay`, `Kit.SetRelay`), collectors of the metrics surface (`metrics.RegisterCollector`), and doctor checks (`cli.RegisterDoctorCheck`). `fw.Retainer`, `fw.WithCredential`, `fw.CredentialFrom`, and `Agent.AccessMode` stay, because a bus, a surface, and the authorization of the kernel define them. The fleet of the workbench reads its block `workbench.fleet` and the variables `FW_WORKBENCH_FLEET_*`.

The profiles revision of 2026-10-17 moved the profile writes to the workbench, the third step of stage 3. The kernel lost `PutProfile`, `DeleteProfile`, `fw.ProfileStore`, `fw.WithProfileStore`, `fw.ProfileLocker`, the routes `PUT` and `DELETE /v1/agents/{name}`, and the verbs `put` and `delete` of `fw agents`. It gained `Agent.SetProfiles`, `Agent.ProfileSet`, a `CheckProfile` that changes nothing, `Config.FileProfiles`, and the hooks `cli.AgentsPut` and `cli.AgentsDelete` (section 13.3). The store of the config package writes the files and judges nothing.

The titles revision of 2026-10-17 moved the model titles to the workbench, the second step of stage 3. The kernel lost `titles`, `fw.WithTitles`, `fw.CleanTitle`, the title call, and the event `fw.run.titled`. It gained `Agent.Annotate` and the event `fw.run.annotated`, metadata for a run after its start, with the usage of the model call that wrote it (section 8.2). The rebuild at start now reads the state of a run and not its last event, so a finished run with a late annotation is not resumed, and the file ledger closes the file of a finished run again after an annotation. The workbench asks for a title the first time that it shows a run (the workbench architecture, section 9).

The teams revision of 2026-10-17 moved the teams to the workbench, the first step of the hollowing of `fw` (docs/ROADMAP.md, stage 3). The kernel lost `Agent.TeamsOf`, `Agent.AgentTeamsOf`, `access.teams`, the teams in `fw.run.queued` and in the answers of `access.external`, `by` on `GET /v1/runs`, and `GET /v1/whoami`. It gained two hooks for any linked package: a claimed configuration block, through `fw.RegisterExtension` and `Config.Extension` (section 12.1), and the run annotator, through `fw.RegisterRunAnnotator` and `fw.WithRunAnnotators` (section 12.4). The workbench records the teams of a run in its metadata through an annotator, and serves `GET /v1/whoami` and `GET /v1/home/others` (the workbench architecture, section 8).

The ax revision of 2026-10-17 followed upstream ax to commit 8d2ccf3. The metadata endpoints moved to a loopback listener at `127.0.0.1:8099`, and `AX_METADATA_URL` names it (sections 11.1 and 11.3). The status of the agent moved to `GET /v1/status` on the native API (sections 8.2 and 11.8). The manifest types took the keys `createTime` and `ResourcesSpec` of ax commit f0f16ff. The stdin gap of `ax ssh` closed with ax commit 92d297e (section 11.12).

The split of 2026-10-17 divided featherweight-agent into two repositories. This repository, featherweight-kernel, keeps the agent runtime for one ax Task, and its binary is `fw`. The repository featherweight-workbench holds the workbench for people, and its binary is `fw-workbench`. The workbench took the page, the browser sign-in, the routes for people, and the MCP server. It also took the fleet driver, the `oidc` driver, and the tool `propose_agent`. The kernel gained `fw.RegisterDefaults` (section 12.6) and the extension point of the native surface (section 12.8). The command line moved to the public package `cli`. The package `surface/dashboard` became `surface/view`, which holds the shapes that the kernel routes return. The kernel defaults are now the surfaces native, A2A, and metrics, and `titles: input`. The contract of the package `fw` did not change. It still holds the hooks that the workbench drivers use, and the next step moves them out (the workbench architecture, section 7).

The connections revision of 2026-10-16 let an MCP server entry act for the person who started a run (section 7.11). A person connects a service once, in the dashboard or through an MCP client. `fw` keeps the grant in an encrypted store and refreshes it under the rules of RFC 9700. A call that needs a grant pauses the run until the person connects. The native API added `/v1/connections` and the callback `/ui/connect/callback`. MCP added URL-mode elicitation for a connection and the tools `connections` and `disconnect`. `fw doctor` reports the connections.

The workbench revision of 2026-10-16 named the work and gave its result a place. Each run got a title from its request, from a model (`titles: model`, the default), or from its input (section 8.2). Each tool got `ToolSpec.Action`, the label of the button that allows a call (section 7.1). The shared views of home, the result of a run, and the agent builder moved to the server (the workbench architecture, section 6.3). The native API added `GET /v1/home`, `GET /v1/runs/{id}/result`, `POST /v1/builder`, and backward transcript pages. MCP added `run_result`, `agent_builder`, `capabilities`, raw file reads, and a file resource. A host that shows only text therefore does the work of the page. The page became a calm workbench: a composer on home, the work by title, and a run that opens at its result (the workbench architecture, section 6.2). It keeps drafts and forms across polls, loads earlier messages on demand, works from the keyboard, and moves the fleet to an Operator view. Save as agent takes only a completed top-level run that the caller started. The dashboard gained browser sign-in with OIDC and PKCE (the workbench architecture, section 5) and lost its usage screen. `GET /v1/usage` and the MCP tool `usage` are deprecated. Operators read new metrics, the fleet gauges, and the files of `examples/observability` instead (section 10). `fw.StoreObserver` and `Agent.ToolSources` feed the metrics of the ledger, the bus, and the MCP servers. The fleet gauges forget the members that are gone, and a late title never reopens a run in the metrics or the traces.

The fleet revision of 2026-10-14 added the fleet (the workbench architecture, section 4). Each `fw` process publishes a record of itself and of its runs on the bus, and any process serves one view of the whole fleet. A request about a run of another process goes to that process with the credential of the caller. The revision also added `fw.Retainer` to the bus contract (section 9.1). No leader and no shared ledger are needed.

The external decisions revision of 2026-10-14 added `access.external` and `policy.external` (section 13.5). A control plane outside the process, behind an MCP tool or a bus subject, decides who can do what and which tool calls run. The revision also added batch authorization for listings, and `fw.ErrAuthzUnavailable`, which the native API answers with a 503. This document described `policy.external` before this revision, but no code read it.

The time revision of 2026-10-14 added the `Clock` plug and a time zone for the agent and for each profile (section 5.8). It also added the `clock` tool, which puts the date in the system prompt, and time notes after a gap. The deadline now counts working time. Before, it counted the wall time since the creation of a run. A follow-up or an approval after the deadline, two hours by default, therefore failed the run at once.

The context revision of 2026-10-13 came on top of the identity revision. It added context estimates from the usage that the provider reports, and compaction at a configurable size and after an overflow error. It also added simple compaction that reads the prompt cache (section 5.4). It also added a memory directory per agent profile and the memory index in the system prompt through `fw.Briefer` (section 7.8).

The identity revision of 2026-10-13 added the principal model and the `oidc` and `spiffe` modes, with their drivers under `authn/`. It added the access roles, the kernel-side authorization, and profile storage and management from every surface. It also added the MCP App dashboard, TLS on the listener, and the verbs `agents` and `whoami` (section 13).

The safeguards revision of 2026-10-13 came on top of the milestones. It added the loop guard and the three shape limits of the budget (sections 5.2 and 5.7). It added the tool limits middleware (section 7.7) and field rules in the policy (section 7.5). It added child runs with bounds and an approval relay (section 11.9), and instructions, inline profiles, and skills (section 12.5). It added environment variables for every key and `${VAR}` expansion in files (section 12.1). It added provider connection blocks and the complete OpenAI adapter (sections 6.2 and 6.6). Last, it added the verbs `init`, `doctor`, `skills`, and `env` (section 12.9).

1. Kernel, file ledger, Anthropic adapter, `bash` and `files` tools, and `fw run`.
2. `fw serve`, the native API with server-sent events, metrics, and health.
3. ax runner mode, the metadata surface, the two images, and the example manifests.
4. MCP client and server, A2A client and server, and peers.
5. Bus drivers `mem`, `nats`, and `redis`, with intake and egress.
6. Resume, replay, compaction, budgets, approvals through every surface, and traces.
7. Gemini and OpenAI adapters, the Kafka driver, guest services, and secret drivers.

## 17. Assumptions and decisions

- The language is Go. ax is Go, the ax runner embedding path is Go, and a static Go binary makes the smallest image.
- The module path is `github.com/bdubs00/featherweight-kernel`. The workbench module is `github.com/bdubs00/featherweight-workbench`.
- The Go version is 1.27, to match the ax module.
- The ax contract comes from the ax repository at commit 8d2ccf3, read on 2026-10-17. ax is pre-release and expects breaking changes.
- The A2A version is 1.0. The MCP revision is 2026-07-28. The Go MCP SDK from v1.7.0 targets that revision. It states that it keeps the deprecated features for at least twelve months, and the dual-era behavior depends on that.
- `fw` implements the ax runner contract itself instead of embedding the ax `runner` package (section 11.2).
- NATS is the recommended bus. Redis is the zero-new-infrastructure choice on an ax cluster.

## 18. Environments

### 18.1 Why

ax builds every actor template with the snapshot fidelity `VOLUMES`. At every resume, Agent Substrate cold-boots the container from its image and restores `/workspace` only. A `pip install` or an `apt-get install` of a run goes to the root file system of the sandbox, and the next suspend loses it. A run that needs a package therefore installs it again after each wake, and it spends its calls to find out what the Task holds.

The package `env` puts the packages on the durable volume instead. The block `environment` of `fw.yaml` names the packages of two managers that need no root: `uv` for Python, and `pixi` for everything else from conda-forge, such as Node, git, compilers, Ansible, and PostgreSQL. For each manager, the block names a manifest with a lock file next to it, or a plain list of packages. With a list, the kernel writes the manifest and the lock file itself. The kernel adds what the managers do not know. It gives the durable placement, the lock enforcement, and the install in the background. It also gives the variables of every tool process, the brief in the system prompt, the tool `env`, the Dockerfile, and the events.

`uv` and `pixi` are single static binaries. The kernel never downloads them: the image must carry both on PATH. `fw:toolbox` carries uv 0.13.1 and pixi 0.82.0 from the same step as `fw env dockerfile` (section 18.10), and `python3` for uv. If one is missing, `fw doctor` says so.

The package registers itself through the seams of section 12.6: `fw.RegisterExtension` for the block, `fw.RegisterService` for the install, `fw.RegisterTool` for the tool, `fw.RegisterToolEnv` for the variables, `fw.RegisterPromptSection` for the brief, `cli.RegisterDoctorCheck` for its line of `fw doctor`, and `fw.RegisterDefaults` for the tool list and the policy. The default binary links it. A lean build links it with the tag `env`, for example `-tags lean,nats,openai,env`.

### 18.2 The block

```yaml
environment:
  dir: /workspace/.environment     # under ax; elsewhere ./.environment in the root of the workspace
  cache: /workspace/.cache         # under ax; elsewhere ./.cache in the root of the workspace
  python: pyproject.toml           # optional; uv.lock must be next to it
  tools: pixi.toml                 # optional; pixi.lock must be next to it
  prepare:                         # optional; after the install, as the tool user, once per digest
    - ansible-galaxy collection install community.general -p /workspace/.environment/ansible
  placement: durable               # durable | image
  brief: true                      # the Environment section of the system prompt
```

A list takes the place of a manifest, and mirrors name the package index of each manager:

```yaml
environment:
  packages:
    python: [ansible-lint, "pyyaml>=6"]   # requirements for uv, in place of python
    tools: ["nodejs>=22", jq]              # packages of conda-forge for pixi, in place of tools
  mirrors:
    python: http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple
    tools: http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge
```

| Key | Default | Meaning |
|---|---|---|
| `dir` | `/workspace/.environment` when `/workspace` exists, else `.environment` in the root of the workspace; `/opt/environment` with `placement: image` | The directory of the environment |
| `cache` | `/workspace/.cache` when `/workspace` exists, else `.cache` in the root of the workspace | The caches of uv and pixi |
| `python` | none | A `pyproject.toml`, relative to the root of the workspace. `uv.lock` must be next to it. |
| `tools` | none | A `pixi.toml`, or a `pyproject.toml` with a `[tool.pixi]` table. `pixi.lock` must be next to it. |
| `prepare` | none | Shell commands (`/bin/sh -c`) that run in the root of the workspace after the install |
| `placement` | `durable` | `durable` installs under `dir` at run time. `image` says that the image holds the environment at `dir`, built from `fw env dockerfile`; the kernel installs nothing then. |
| `brief` | `true` | `false` leaves the Environment section out of the system prompt |
| `on` | `true` | `false` turns the packages off: the lists are empty, the next install removes the environment, the prompt has no Environment section, and the tool `env` only lists (section 18.7) |
| `ask` | `true` | `false` keeps the agent from a change of its packages: the commands `add` and `remove` of the tool `env` refuse |
| `packages.python` | none | A list of requirements for uv, such as `pyyaml>=6`, in place of `python`. The kernel writes the manifest and the lock file under `dir` (section 18.4). |
| `packages.tools` | none | A list of packages of conda-forge for pixi, in place of `tools`. An item is a name, with an optional version and build, such as `nodejs>=22` or `nodejs 22.*`. |
| `mirrors.python` | none | The index of uv and pip, such as a cache in the cluster. Every tool process and every install gets it (section 18.5). |
| `mirrors.tools` | none | The mirror of `https://conda.anaconda.org/conda-forge` in the configuration of pixi |

A list suits a pool of several Tasks. The configuration holds the list, so every worker gets the same packages, and nobody writes a manifest or a lock file. An empty list is valid. The tool `env` can then add a package, and the state is ready with no package and no network.

The decision for `/workspace` is the one of `fw.DefaultHome`. A relative path is relative to the root of the workspace (`tools.files.root`). Another key in the block is an error, as everywhere in `fw.yaml`. The start stops when the block does not fit:

- `dir` and `cache` must not be FW_HOME, a path under it, or a path above it. The environment is a sibling of FW_HOME, never part of it.
- They must not hold the root of the workspace, and with `tools.files.perRun` they must not be under the directories of the runs, which move with each run.
- Under ax, a durable `dir` must be under `/workspace`, because every suspend loses the rest.
- With `placement: image`, `dir` must not be under `/workspace`, because the volume of the Task hides the image there.
- `packages.python` and `python` cannot both exist. The same holds for `packages.tools` and `tools`.
- A requirement must not start with a dash, hold a control character or a quote, or hold more than 200 characters. A list holds 100 items at most.
- A Python requirement is a name of PEP 508 with optional extras and version specifiers, such as `pyyaml`, `pyyaml>=6`, or `ansible-lint[yaml]==24.9.2`. A URL, an environment marker after a semicolon, and a space inside the name are refused. A requirement of `tools` is a conda name with an optional version and an optional build, such as `jq`, `nodejs>=22`, `nodejs 22.*`, or `openssl 3.* *_0`, without a channel. The kernel writes each requirement into a manifest of TOML, so the check keeps every quote and line break out of it. The tool `env` and `PUT /v1/environment` check a requirement by the same rules.
- A mirror is an `http` or `https` URL with a host. It must not hold a user, a password, a query, or a fragment, because every tool process can read it.
- A list needs `placement: durable`. An image needs a manifest with its lock file.

The package `env/pkglist` holds the checks of a list and of a mirror. It has no `init` function, so the workbench can import it and check a list before it writes the list into a pool.

`placement: image` is the target for a heavy environment, such as PyTorch. A durable volume ships at every suspend and comes back at every resume. The lab measured 0.13 to 0.2 seconds for a small workspace; a 2 GiB environment on `/workspace` changes that cost. Measure on the homelab before you choose: the time of `uv sync` for `torch` from the CPU index with a cold and a warm cache, the size of the volume after it, and the suspend and resume time of the Task with that volume.

### 18.3 The layout

| Path | Owner | Use |
|---|---|---|
| `<dir>` | fw, mode `1775` with the group of the tool user | The tool user adds and removes its own entries, but the sticky bit keeps it from renaming a file of fw |
| `<dir>/.fw-env.json` | fw | The marker of a ready environment |
| `<dir>/.fw-env-failed.json` | fw | The last failure |
| `<dir>/.fw-env.lock` | fw | The `flock(2)` lock of one installer |
| `<dir>/.fw-env-local.json` | fw | The changes of the tool `env` to the lists (section 18.7) |
| `<dir>/.fw-env-index.json` | fw | The mirror of each lock file that fw made for a list |
| `<dir>/.fw-env-outbox.json`, `<dir>/.fw-env-outbox.lock` | fw | The events that the bus refused, and the lock of that file (section 18.6) |
| `<dir>/.fw-env-pushed.json` | fw, mode `0600` | The push of the front, the base in place of the lists and the switch of the block (section 18.13) |
| `<dir>/src` | fw | The parent of the directories of the lists |
| `<dir>/src/python`, `<dir>/src/tools` | tool user | The manifest of a list (`pyproject.toml`, `pixi.toml`), which fw writes, and its lock file, which the manager writes next to it |
| `<dir>/venv` | tool user | The virtual environment: `UV_PROJECT_ENVIRONMENT` and `VIRTUAL_ENV` |
| `<dir>/python` | tool user | The interpreters of uv: `UV_PYTHON_INSTALL_DIR` |
| `<dir>/bin` | tool user | The links to those interpreters and the programs of `uv tool install`: `UV_PYTHON_BIN_DIR`, `UV_TOOL_BIN_DIR` |
| `<dir>/uv-tools` | tool user | The tools of uv: `UV_TOOL_DIR` |
| `<dir>/pixi` | tool user | `PIXI_HOME`, with the file `config.toml` that fw writes and the programs of `pixi global` |
| `<dir>/pixi-envs` | tool user | The environments of pixi (`detached-environments`), so an environment lives under `dir` and not next to its manifest |
| `<cache>/uv`, `<cache>/pixi` | tool user | `UV_CACHE_DIR`, `PIXI_CACHE_DIR` |

Without a tool user, as under Agent Substrate today (section 7.12), every path belongs to the user of fw, and nothing changes owner. With a tool user, a state file that fw does not own counts as absent, because the tool user could have written it.

The managers run as the tool user with the `HOME` of section 7.12, which is `/workspace/.home` under ax. When a Task turns the tool user on, the walk of the workspace gives the files under `dir` to that user. A state file then counts as absent. The next start therefore installs the environment once more, from the cache, and a list loses its local changes. `fw` takes `dir` and `<dir>/src` back at each install.

The tool user can put a link in place of an entry that it owns under `dir`. `fw` therefore writes a manifest and the `config.toml` of pixi through an `os.Root` of `dir`. No link then leads a write or a read of `fw` out of `dir`. A state file gets a temporary file with a random name before its rename. `fw` opens the lock file without a follow of a link. A directory of a manifest that is a link becomes a real directory again.

### 18.4 The install

The install runs these commands, each as the tool user, with the environment of a tool process (`fw.ToolEnv`, section 7.12) and the variables of section 18.5:

1. `uv sync --frozen --no-install-project --project <dir of python>`, then `uv pip list --format json --python <dir>/venv` for the versions. `--frozen` installs the lock as it is and never resolves again. The interpreter comes from `requires-python` and `.python-version`. Without a Python in the image that fits, uv downloads one into `<dir>/python`.
2. `pixi install --frozen --manifest-path <tools>`, then `pixi info --json` for the directory of the environment and `pixi list --json --frozen` for the versions.
3. Each `prepare` command, with `/bin/sh -c`, in the root of the workspace, with the programs of steps 1 and 2 on PATH.

For a list, steps 1 and 2 write the manifest first and lock it. Each manifest holds the effective list (section 18.7) in sorted order:

1. For Python, fw runs `python3 --version` with the `python3` of its own PATH, which is the Python of the image. It writes `<dir>/src/python/pyproject.toml` with `requires-python` set to the minor version of that Python, such as `==3.11.*`. Then it runs `uv lock` and `uv sync --locked --no-install-project`, both with `--project <dir>/src/python --python <python3>`. `UV_PYTHON_DOWNLOADS=never` keeps uv from a download of an interpreter, so a list needs no host for interpreters. Without a mirror, the install sets `UV_DEFAULT_INDEX` to `https://pypi.org/simple`.
2. For tools, fw writes `<dir>/src/tools/pixi.toml` with the channel `conda-forge` and the platform of the machine, such as `linux-64`. Then it runs `pixi lock` and `pixi install --locked`, both with `--manifest-path <dir>/src/tools/pixi.toml`. pixi runs no link script of a package.
3. An empty list runs no manager and needs no network. It removes the environment of an earlier list.

The lock file of a list stays between two installs, so a change of one package keeps the versions of the others. uv.lock holds the absolute URL of the index for each file. When the mirror of a manager changes or goes away, the install removes the lock file first and locks again from scratch. `<dir>/.fw-env-index.json` records the mirror of each lock file. If the digest of a list is ready, a cold boot does not lock it again, because the marker matches.

The marker records the digest: the sha256 of the manifests, the lock files, and the prepare commands, with a label for each part. Another file does not count. For a list, the digest covers the effective list in sorted order, the mirrors, and the prepare commands. It does not cover the manifest and the lock file that the kernel writes. A block without lists and mirrors keeps the digest of the files alone. A new digest, such as a lock file from a new revision of a pool, installs again, and the caches make it incremental. The marker also holds the versions, the directory of the pixi environment, and the time that the install took. A Python package that a list names is explicit (`is_explicit`) in the marker. A ready environment has a marker with the digest of now, a virtual environment, and a pixi environment.

`fw serve` builds the agent, the kernel starts, and then the service of the package starts the install in the background. It never delays `/readyz`. A failure records the step, the error, and the last 4 KB of the output of the manager. The next try comes after 5 seconds, and the pause doubles up to 5 minutes. A golden boot has no network, so its first try fails. The next tries succeed once the egress policy lands. A failure that an earlier process recorded is due at once, so the cold boot after a resume tries without waiting. At its start, the service sends the state that it finds once: `fw.env.ready` with the marker, or `fw.env.failed` with the failure. A Task that is ready at boot therefore says so, and the front can keep one event for each Task. After that, a retry that fails with the same error sends nothing. With `placement: image`, the service installs nothing and sends the state of the image. After a ready environment, the service ends. The output in a failure loses every secret of the process (section 4.2). It also loses the password of every URL, such as the token of a private index. During the install, the process holds its status at not idle (`fw.Agent.Hold`). The front therefore does not suspend the Task in the middle of a download.

The service lives as long as the process. After an install that is ready, it reads the block again every 2 seconds. It installs a new digest, such as after a push of the front (section 18.13). A change of the digest also ends a pause before the next try. A failure that is an answer of the resolver, such as for a package that does not exist, comes again with the same lists. Its next try therefore waits 5 minutes, the longest pause, at once. A failure of the network keeps the short pause that doubles. The output of uv for such an answer holds `No solution found when resolving`, and the output of pixi holds `failed to solve the environment` or `Cannot solve the request`.

A verb that starts no service, such as `fw run`, installs the environment in the first tool process of a run. A laptop therefore tests a profile with the packages of its Task before the Task exists: `fw run` and `fw serve` outside ax install the same environment under `./.environment`. The time limit of the tool call (`tools.limits.timeout`, 10 minutes) bounds that install. A cut install goes on from the cache at the next try.

One lock file per environment keeps one installer at a time, across processes too. The lock is `flock(2)`, so it ends with its process. On a system without `flock(2)`, two processes can install at the same time.

### 18.5 Tool processes

`fw.RegisterToolEnv` adds a hook that `fw.ExtendToolEnv` applies after `fw.ToolEnv`. The shell of `bash` and a stdio MCP server call it, so both get the same environment:

- `PATH` with these directories in front, in order: `<dir>/venv/bin`, the `bin` directory of the pixi environment, `<dir>/bin`, and `<dir>/pixi/bin`.
- `VIRTUAL_ENV` and `UV_PROJECT_ENVIRONMENT` (with `python`), `CONDA_PREFIX` (with `tools`), `UV_CACHE_DIR`, `UV_PYTHON_INSTALL_DIR`, `UV_PYTHON_BIN_DIR`, `UV_TOOL_DIR`, `UV_TOOL_BIN_DIR`, `PIXI_HOME`, and `PIXI_CACHE_DIR`.
- `UV_LINK_MODE=copy` when `dir` and `cache` are on two file systems (the device numbers differ), because uv links the files of a package from its cache by default.
- With `mirrors.python`, `UV_DEFAULT_INDEX` and `PIP_INDEX_URL` with the URL of the mirror. pip ignores an index on plain http unless it trusts the host. An `http` mirror therefore also gives `PIP_TRUSTED_HOST` with the host of the URL, without the port. uv needs no such variable for plain http.

`mirrors.tools` is part of the file `config.toml` under `PIXI_HOME`, which every pixi process reads. A Python list without a package has no virtual environment, so `VIRTUAL_ENV` and `UV_PROJECT_ENVIRONMENT` stay out.

A tool process never fails because of an install. Until the environment of the current digest is ready, a tool process gets the last environment that installed and still exists. After a failed install, it does the same. Without such an environment, it starts without one. Only the first install, before any environment exists, holds a tool process of a run, bounded by the context of its call. Then `fw run`, which starts no service, installs there. A run that needs a missing package learns of it from the Environment section and from the shell. A tool process outside a run, such as the listing of `fw tools`, gets the variables and waits for nothing.

`UV_PROJECT_ENVIRONMENT` holds for every project. A `uv sync` in another project of the workspace therefore installs into the same environment. A shell unsets it for a project that needs a virtual environment of its own. pixi runs no activation script for a tool process: the process gets `PATH` and `CONDA_PREFIX` alone.

### 18.6 Events

The events of the environment belong to the agent, not to a run. As with `fw.connection.connected` (section 7.11), the package publishes them on the bus, on the subject `fw.environment`, and logs each one. The subject of the event is empty, and `node` names the process. The ledger of the run that changed the environment records its call of the tool `env`.

The bus can refuse an event. A golden boot has no network, and a Task that restores from that snapshot goes on as the same process. An event that the golden boot lost therefore never comes again by itself. A refused event waits in `<dir>/.fw-env-outbox.json`, and the service sends it for the life of the process:

- `fw.env.ready` and `fw.env.failed` wait as a flag. A retry sends the state of that moment, so the newest state wins.
- `fw.env.changed` waits as it is, in order, because the front writes each change into the configured lists. These events go before the state.
- After a refusal, the service waits 1 second, and the pause doubles after each refusal in a row, up to 30 seconds. Otherwise it looks every 10 seconds, and at once after an install of the service.
- The start event is a flag of the outbox too. Once the state is ready or failed, the service sends it. A retry that fails the same way sends nothing itself, so the start event carries that failure. An install that sends a newer state first clears the flag, so the front gets no copy.

A process without the service, such as `fw run`, leaves a refused event to the next start. A file of the outbox that `fw` does not own counts as empty while a tool user exists.

| Type | Data | When |
|---|---|---|
| `fw.env.ready` | the marker: `digest`, `placement`, `ready`, `seconds`, `uv`, `pixi`, `python`, `tools`, `prepare`, and the switch | an install succeeded, or the service found a ready environment at its start |
| `fw.env.failed` | `digest`, `time`, `step`, `error`, `output`, `attempt`, `retryAt` | an install failed, or the service found a failure at its start. A retry that fails with the same error records no new event. |
| `fw.env.changed` | `digest`, `previous`, `manager`, `command`, `package`, `run`, `agent` | the tool `env` changed a manifest or a list and installed the result |

The switch is `on`, `ask`, and, for a block with lists, `lists`: `python` and `tools`, the base with the local changes of the tool `env`. `fw.env.failed` carries the switch too, next to the failure. In the marker, `python` holds `manifest`, `version`, `env`, and `packages`, and `tools` holds `manifest`, `prefix`, and `packages`. Each package is `name`, `version`, and `is_explicit`, the names of `uv pip list` and `pixi list`.

### 18.7 The tool `env`

The tool takes `command`: `list`, `add`, or `remove`. `add` and `remove` also take `manager`, `python` or `tools`, and `package`, a requirement such as `pyyaml>=6` or `nodejs>=22`. A package that starts with a dash is refused, so the model cannot pass an option to a manager.

The switch of section 18.2 decides first, before the policy. `list` always works, and `add` and `remove` refuse with these answers:

- With `on: false`: `A person turns packages on for this agent on the page.`
- With `ask: false`: `This agent cannot ask for packages. A person adds them on the page.`

The ask rules of the policy stay. Each call reads the block again, so a push of the front applies to the next call.

| Call | Runs |
|---|---|
| `add` with `python` | `uv add --no-sync --project <dir of python> -- <package>` |
| `remove` with `python` | `uv remove --no-sync --project <dir of python> -- <package>` |
| `add` with `tools` | `pixi add --no-install --manifest-path <tools> -- <package>` |
| `remove` with `tools` | `pixi remove --no-install --manifest-path <tools> -- <package>` |

The manager changes the manifest and the lock file. Then the tool installs the new digest under the lock of the environment and records `fw.env.changed`. A failure of the manager leaves the files as they were. With `placement: image`, `add` and `remove` refuse, because the next cold boot would lose the change: change the manifest and build the image again.

With a list, `add` and `remove` change the list and not a manifest. The tool works under the lock of the environment:

1. It makes the new list. An `add` takes the place of a requirement of the same package, so `pyyaml>=6` replaces `pyyaml`. A `remove` takes out every requirement of the package, and it fails for a package that the list does not hold. A list holds 100 packages at most.
2. It writes the manifest of the new list and locks it, as `uv add --no-sync` does. If the lock fails, the list and the environment stay as they were, and the result holds the end of the output of the manager.
3. It records the change in `<dir>/.fw-env-local.json`, installs the new list, and records `fw.env.changed`.
4. If that install fails, the tool puts the previous list back and records no failure. A package that does not build therefore never leaves the agent without its environment.

The effective list is the configured list with the changes of `<dir>/.fw-env-local.json`. The file names the digest of the configured lists. If the configured lists change, the file no longer counts, and the next install removes it. The front puts the approved changes into the configured lists of the pool, so the new configuration holds them already. The same effective list then gives the same digest, and the Task stays ready without an install.

The policy of section 7.5 names a tool and a field of its input. `env` is one tool, and its field `command` names the action, so a rule names an action with the field form:

| Rule | Effect |
|---|---|
| `ask: ["env: command=add", "env: command=remove"]` | the default: a person allows each change of the packages, and `list` runs |
| `deny: ["env: *"]` or `deny: [env]` | a locked profile: no call of the tool runs |
| `ask: ["env: command=add"]` | ask before an add only |

A dotted name such as `env.add` is a tool glob, and no tool has that name, so the rule matches nothing. The package adds the default rules to `policy.rules.ask` and `env` to `tools.builtin` through `fw.RegisterDefaults`. A file that sets `policy.rules.ask` or `tools.builtin` replaces the default list, so it names them again. Without the block, the tool is opt-in (section 7.1), and no profile gets it unless its tool list names it.

### 18.8 The Environment section

`fw.RegisterPromptSection` adds a function that `SystemPrompt` calls after the skills, at each model call. The section of the package reads the state files and runs no command. It names `dir`, the virtual environment, the manifests, the manager of each, the version of Python, and the packages with their versions: every Python package, and the pixi packages that the manifest names, 30 of each at most. It tells the model to call the tool `env` for a package, or, for a profile without the tool, that the packages are fixed. Inside a Task (`AX_TASK_YAML`), it also names what the Task does not have: a change outside `/workspace` is lost at the next suspend, apt and sudo are not available to a run, there is no GPU, so PyTorch runs on the CPU, and there is no init system or service manager.

The text changes only when the state changes: once from installing to ready, or to failed. The cached prefix of the prompt therefore stays valid between those changes. A name or a version with characters other than those of a package name stays out of the text, because the state files are on a volume that other processes can write.

For a list, the section names the list in place of the manifest and the lock file. It names the Python packages that the list names, and not the packages that they bring. A list without a package gets one short line. The section says that a person allows each change. It also tells the model not to edit the files under `<dir>/src`, because fw writes them again. With a mirror, it says that the packages come from the mirror of the deployment, and that other registries can be out of reach.

With `on: false`, the prompt has no Environment section. With `ask: false`, the section says that the agent cannot change its packages. Until an install of the current digest is ready, the section names the packages of the last environment that installed. A tool process gets the same packages. The section names a failure in one sentence, such as `The last change of the packages failed to install at the step python, so the shell has the packages of the last install that worked` after a failed change.

### 18.9 `fw doctor`

The line `environment` names:

- the state: `none` without the block, not installed yet, `materializing`, `ready` with the digest and the time of the install, or `failed` with the step, the error, the attempt, and the next try;
- whether `uv` and `pixi` are on PATH, with their versions, for the managers that the block needs;
- the hosts that the egress policy of the Task must let through, from the URLs in the lock files: `tls pypi.org` for an https host, which needs a rule with tls passthrough, and `http deb.debian.org` for a plain http host. Without a lock file it names the default registry: `pypi.org` and `files.pythonhosted.org` for uv, `conda.anaconda.org` for pixi. Without `python3` on PATH it adds `releases.astral.sh`, where uv downloads a Python. `download.pytorch.org` appears when the lock of uv uses the PyTorch index.

With a mirror, the host of the mirror is the egress need of its manager, with the port of the URL, such as `http pkg-cache.fw-fleet.svc.cluster.local:8080`. pixi takes every download of conda-forge from the mirror. uv takes every download of a list from the mirror too, because the kernel locks the list against the mirror. A manifest of uv keeps the hosts of its lock file, because `uv sync --frozen` downloads from those URLs. For a Python list, the line checks the `python3` of the image, and it never names `releases.astral.sh`.

The kernel cannot add an egress host. A person adds the hosts to the Task. A failed state, a missing manager, or a missing `python3` for a Python list fails the line.

### 18.10 `fw env dockerfile`

`fw env dockerfile --from <image>` prints a Dockerfile for `placement: image`, for the pools pipeline to build and pin. It builds nothing. The build context is the root of the workspace. The Dockerfile:

1. Installs uv 0.13.1 and pixi 0.82.0 from their release URLs on GitHub (the static musl builds for amd64 and arm64), and checks the sha256 of each archive. A new version needs new checksums.
2. Sets the variables of section 18.5 with `dir` inside the image, except the caches: at run time the kernel gives a tool process the cache of the configuration.
3. Copies the manifests and the lock files, and `.python-version` when it exists, to `<dir>/src/python` and `<dir>/src/tools`.
4. Runs `uv sync --frozen --no-install-project` and `pixi install --frozen`.
5. Runs each `prepare` command in `dir`.
6. Writes the marker with the digest and the versions.

Steps 4 to 6 keep the caches in a BuildKit cache mount, so no cache lands in the image.

`Dockerfile.toolbox` installs uv and pixi with the text of step 1, and a test of `env/dockerfile_test.go` compares the two. A new version therefore changes `env/dockerfile.go` first. The image also carries `python3` for the lists. It has the network programs `ss`, `ip`, `dig`, `ping`, and `nc`. It has the file programs `unzip`, `xz`, `less`, `file`, `lsof`, and `rsync`.

`fw env dockerfile` refuses a block with lists, because an image needs a manifest with its lock file. The variables of a mirror stay out of the image. At run time, the kernel gives them to each tool process.

A block with `placement: image` keeps its `dir`. A durable block gives `/opt/environment`, and the comment at the top of the file names the block that the image needs. The base image must have `/bin/sh`, `curl`, `tar`, and `sha256sum`, which `fw:toolbox` has. apt packages stay an image concern: add them to the file before the managers. When the workspace of the Task holds the manifests, `fw doctor` compares the digest of the marker in the image with the digest of those files, and fails when they differ.

### 18.11 What this is not

It is not a container inside the container: gVisor does not nest. It is not a root shell: apt and system services stay out of a run. It does not replace the goal of a Workspace (section 11.3), which stays the fallback for an environment that nobody wrote down. No GPU reaches a Task, so a GPU package is not a reason for an environment.

### 18.13 A push of the front

A pool of the workbench gives a worker a new Task for a change of its configuration. A new Task starts with an empty `/workspace`, which holds the runs of the agent. The front therefore changes the packages of a running worker in place, through two routes of the native surface. Without the block `environment`, both answer 404.

`GET /v1/environment` needs the role that views each profile of the process (`agent.view`). It answers 200:

```json
{"on": true, "ask": true, "lists": {"python": ["pyyaml", "six"], "tools": ["jq"]},
 "state": "ready", "digest": "sha256:...", "marker": {"digest": "sha256:...", "...": "..."},
 "failure": {"digest": "sha256:...", "step": "python", "...": "..."}}
```

`lists` holds the base with the local changes of the tool `env`. `state` is `pending`, `materializing`, `ready`, or `failed`. `digest` is the digest of now. `marker` is the last environment that installed, and `failure` is the failure of the digest of now. Each of the two is absent without one.

`PUT /v1/environment` needs the role that operates each profile of the process (`agent.run`). The front calls it with its static token. It takes this body:

```json
{"on": true, "ask": false, "python": ["pyyaml", "six"], "tools": ["jq"]}
```

`on` and `ask` must be present. Each list follows the checks of section 18.2, and a list that does not fit answers 400. A block that names a manifest, or that has `placement: image`, answers 409. The kernel keeps the body in `<dir>/.fw-env-pushed.json`, with the mode `0600`, and answers 202 with the shape of the GET. The body is the base in place of the lists and the switch of the block. A change of the base drops the local changes of the tool `env`. The service installs a new digest in the background. The notifier sends the state with the new switch, also without an install.

The file lives on the volume of the Task, so a suspend keeps the push. A new Task starts from the block, and the front pushes again.

### 18.12 Limits

- The `files` tool writes as the user of fw. With its root at `/workspace` and without `tools.files.perRun`, a run can therefore write the state files under `dir`. The brief takes only names and versions from them, in the characters of a package name.
- A tool process of the tool user can change the environment, as `pip install` into the virtual environment does. The marker then no longer describes it, until the next install of a new digest.
- The time of a long install in `fw run` counts against the time limit of the first tool call of the run.
- On a system without `flock(2)`, two processes can install one environment at the same time.
- The manager of a list runs as the tool user and writes the lock file in a directory of the tool user. A tool process can therefore change the lock file of a list. The next install writes the manifest again, but it keeps the lock file.
- A change of the tool `env` on a list stays on its Task. Another worker of the same pool gets it only after the front writes it into the configured lists.
- `mirrors.python` changes the downloads of a manifest of uv only through its lock file, because `uv sync --frozen` downloads from the URLs of the lock file.
- No measurement of a heavy environment exists yet. Section 18.2 names the three numbers to take on the homelab before a default moves between `durable` and `image`.

## 19. Hosted harnesses

A harness is an agent program that people use on their laptops, such as opencode or Claude Code. The block `harness` of `fw.yaml` makes the kernel host one. Every run of the process then becomes a session of the harness instead of a run of the turn loop. The kernel keeps what makes a run durable: one ledger, one policy, one approval surface, and an idle run while a person decides, so a Task suspends at no cost and a cold boot continues the session. The package `harness` holds the hosts, one file for each kind, and `cmd/fw/driver_harness.go` links it behind the build tag `harness` (section 15).

### 19.1 Configuration

```yaml
harness:
  kind: opencode            # opencode | claude-code
  home: /workspace/.harness # HOME, and CLAUDE_CONFIG_DIR for Claude Code
  command: []               # the program and its first arguments
  port: 0                   # opencode only: the loopback port of opencode serve
```

| Key | Meaning | Default |
|---|---|---|
| `kind` | The harness: `opencode` or `claude-code`. A block without it stops the start. | none |
| `home` | The home of the harness: its sessions, its transcripts, and its credentials. It must be on the durable volume, beside the home of `fw` and never inside it, and never inside the directory of a run. | `.harness` beside the home of `fw`: `/workspace/.harness` under ax, `./.harness` elsewhere |
| `command` | The program of the harness and its first arguments. The kernel adds its own flags after them. | `opencode serve`, or `claude` |
| `port` | The loopback port of `opencode serve`. `0` picks a free port. A port for `claude-code` stops the start. | `0` |

The block names no model and no key. The harness uses the model of the run, the model that the turn loop would use, with the endpoint and the key of its provider: `providers.<provider>.baseUrl` and the secret that `apiKeyEnv` names. A run without a model fails, as in the turn loop. A block that `fw config` prints holds no secret. A run that the turn loop started before the block was set stays with the turn loop.

### 19.2 The runner

The kernel holds one seam for the hosts, in `fw/runner.go`. A package registers a constructor with `fw.RegisterRunner(func(ctx context.Context, a *fw.Agent) (fw.Runner, error))`, and a program can pass one with `fw.WithRunner`. `New` builds the runner before it resumes the runs of the ledger, so a resumed run goes to it. `Agent.Runner` returns it.

```go
type Runner interface {
    Drive(ctx context.Context, h *Hosted) (bool, error)
}
```

The driver of a run asks the runner first, each time it starts: for a new run, after a follow-up, after a decision, and after a restart. `Drive` does the work that the events of the run leave, and returns when the run is finished, waits for a decision, or its context ends. Like the turn loop, the kernel releases the run only when no event arrived while `Drive` worked; otherwise it calls `Drive` again. It also releases a run that is finished or waits for a decision, so a runner that recorded the end of a run is not called again for it. A shutdown ends the context at once. `Drive` reports false for a run that it does not drive, and the turn loop drives that run.

`Hosted` gives the runner what the turn loop uses: `Events`, `Emit` through the append path of the kernel (the secrets of the data are replaced, the status and the observers see the event, and an approval or a terminal event is synced), `Finish`, which records the end of the run unless a message came that the runner did not handle, `Decide` with the policy of the run, `Over` and `Budget` for the budget of the run, `Usage`, `Model`, `Queued`, and `Dir`, the directory of the tools of the run, which it creates with `tools.files.perRun`. Every exported signature of `fw` stays as it was.

### 19.3 The session in the ledger

The host records the steps of a session with the events of the turn loop, so the budget, the status, the views, `fw events`, the OTLP export, and the front work unchanged. A turn is one model call of the harness, as in the turn loop.

| From the harness | In the ledger |
|---|---|
| opencode: a part `step-start`. Claude Code: the first `assistant` line of a message id. | `fw.turn.started`, `fw.model.requested` |
| opencode: the parts `text` and `tool` of the step, and `step-finish` with its tokens. Claude Code: the `assistant` lines of one message id, with the usage of the message. | `fw.model.responded`, with the text, the tool calls, the stop reason, and the usage |
| a tool call: the permission request of opencode, or the `tool_use` block of Claude Code | `fw.tool.requested`, before the call runs |
| opencode: a part `tool` that is `completed` or `error`. Claude Code: a `user` line with a `tool_result`. | `fw.tool.responded` |
| the end of the step, or the next model call | `fw.turn.finished`, then `fw.file.changed` for the files that the turn changed (19.7) |
| a call that the policy asks a person about | `fw.approval.requested` (19.4) |
| opencode: `session.idle`. Claude Code: the `result` line. | `fw.run.finished`, with the latest text as the output |
| every other event of the harness | `fw.harness.event` |

`fw.harness.event` holds `harness`, the kind, `kind`, the type of the event of the harness, such as `message.part.updated` or `result`, and `data`, a compact payload: ids, states, tool names, the stop reason, and the tokens, never the text of a file or a key. The high-frequency events of opencode, `message.part.delta` and `server.heartbeat`, are left out. The same event also records the steps of the kernel in the session. Their kinds start with `fw.`:

| Kind | Meaning |
|---|---|
| `fw.start` | The run is a session of a harness. |
| `fw.session` | The session has the id `session`: the id of the session of opencode, or the session UUID of Claude Code. |
| `fw.brief` | The first prompt with text of the session goes to the harness next. The field `context` is true when that prompt starts with the memory index (19.10). The host records it before it sends the prompt. |
| `fw.prompt` | The harness got the messages of the run up to the sequence number `seq`. The field `context` is true when that prompt started with the memory index (19.10). |
| `fw.answer` | The harness got the decision of the approval `approval`. |
| `fw.idle` | The harness finished the exchange that the latest `fw.prompt` or `fw.answer` started. |

An exchange without its `fw.idle` was cut by a stop of the process. The next driver continues it (19.5).

A message without text, such as a blank follow-up or a note of the kernel, gives the harness nothing: the host records `fw.prompt` and `fw.idle` for it at once, and no `fw.brief`, and finishes the run again. A message with attachments alone gets a text that names the files, such as `attachments/notes.txt`, which the harness reads in the directory of the run. A message from another agent (section 5.5) reaches the harness inside its `<message>` block, and from that message on the added system text of the harness ends with `fw.MessageNote`. A finished run stays finished until a follow-up reopens it. A run with a rubric (section 20) records `fw.outcome.evaluated` with the verdict `skipped` and the reason `a hosted harness has no grader` before it completes, once for each answer.

opencode reports the usage of a model call when the call ends, after its tool calls ran, while the kernel records a call that asks a person before it runs. Such a model call therefore gets its `fw.model.responded` with no usage at the first permission request, and its usage follows as `fw.run.annotated` with the usage, the provider, and the model. The usage, the cost, the budget, and the metrics count it in the same way (section 8.2). The model calls of a subagent count the same way and are not turns of the run.

### 19.4 Policy and approvals

The kernel names the tools of a harness in lower case, such as `bash`, `read`, `write`, `edit`, and `webfetch`, so one rule fits both kinds, for example `bash: git push*` or `write: filePath=*.env` (section 7.5). `bash` and the guards `external_directory` and `doom_loop` of opencode are destructive, so the default policy asks a person about them, as for the `bash` tool of the kernel. `read`, `glob`, `grep`, `list`, `ls`, `webfetch`, `websearch`, `skill`, and the to-do tools are read-only. Every other tool has no flag.

opencode asks the kernel about every call. The kernel sets `OPENCODE_PERMISSION` to `ask` for every permission key, and the inline configuration `OPENCODE_CONFIG_CONTENT` sets `ask` for the agents `build`, `plan`, `general`, and `explore`, which win over the configuration of the project. The kernel prompts with the agent `build`. For each `permission.asked` event of the session or of a subagent, the kernel asks the policy of the run. It answers `allow` with `once` and `deny` with `reject` and the reason, on `POST /permission/{id}/reply`, or on `POST /session/{id}/permissions/{id}` of an older server.

Claude Code asks the kernel through a `PreToolUse` hook of the type `http`. Each process gets the hook in `--settings`, with the URL `http://127.0.0.1:<port>/v1/harness/hook/<secret>` on the native API and `onFailure: block`, so a hook that fails blocks the call. The same settings set `disableAllHooks` to false, allow the URL of the hook in `allowedHttpHookUrls`, set `permissions.defaultMode` to `default`, and reject a server named `fw-memory` in the `.mcp.json` of the repository with `disabledMcpjsonServers` (19.10). `--settings` wins over `.claude/settings.json` and `.claude/settings.local.json` of the repository, which the model can write, and the lists of `allowedHttpHookUrls` merge, so a file of the repository cannot turn the hook off or block it. The secret is new for each process and leaves with it. The route answers only a caller on the loopback address, and a secret that no process holds gets `deny`. The hook answers `allow` and `deny` from the policy, and `defer` where the policy asks. Claude Code ignores `defer` when one response makes several tool calls, and its own permission flow then runs a read inside the working directory without asking. The hook therefore answers `deny` with the reason `this call needs an approval; make it alone` for such a call. It counts the calls of a response from the lines that claude wrote before it called the hook, and waits up to two seconds for the driver to read them.

Each harness reports the tools of the memory server (19.10) under a name of its own: `mcp__fw-memory__recall`, `mcp__fw-memory__list`, `mcp__fw-memory__remember`, and `mcp__fw-memory__forget` for Claude Code, and `fw-memory_recall`, `fw-memory_list`, `fw-memory_remember`, and `fw-memory_forget` for opencode. The ledger and the approvals keep these names. The policy sees one name for each tool in both harnesses, `fw-memory.recall`, `fw-memory.list`, `fw-memory.remember`, and `fw-memory.forget`, with the canonical input of the call (19.10), so one rule fits both, such as `fw-memory.remember: path=/memories/private/*`, and the rule `*fw-memory*` matches each tool. The host also asks the policy about the call under the name of the tool of the turn loop that does the same work: `memory` for `remember`, `forget`, and `list`, with the field `command` set to `create`, `delete`, or `view`, and `recall` for `recall`. A verdict under that name counts when a rule gave it, not the default: an ask or a deny that is stricter than the verdict under `fw-memory.<tool>` wins, so `ask: [memory]` and `deny: ["memory: command=delete"]` hold for a hosted call, and an allow wins over a verdict that only the default gave under `fw-memory.<tool>`. `recall` and `list` are read-only; `remember` and `forget` have no flag. When the profile of the run does not allow the tool `memory`, or `recall` for a search beyond the notes, the host denies the call before it asks the policy, and so it does for a call whose input has no canonical input (19.10). A tool whose name differs from a tool of the memory server only by case, such as `mcp__FW-Memory__remember`, or that has the prefix of the server but is not one of its four tools, such as `fw-memory_view`, belongs to another server and gets a deny.

Before it asks the policy, the host denies a call whose path resolves inside the home of the harness, the home of `fw`, or a reserved path: the paths that the file tracker skips (19.7). The fields are `file_path`, `filePath`, `path`, `notebook_path`, the pattern of `glob` when it is absolute, and `ref` when it holds a file ref, such as `file://src/main.go` of `recall`, which names a path under the root of the workspace. A relative path starts in the directory of the run, `~` is the home of the harness, and links are followed. A read of one file in which the harness keeps the full output of a tool call that it cut, `projects/<project>/<session>/tool-results/<file>` of Claude Code or `.local/share/opencode/tool-output/<file>` of opencode, still passes; a listing or a search of that directory does not.

A call that the policy asks about becomes `fw.approval.requested` with the tool, the call id, the input, and the reason of the rule. The run moves to `input-required`, the driver returns, and `GET /v1/status` reports the Task as idle (section 11.8), so the front can suspend it. A decision from any surface starts the driver again. A deny that the rules of the run gained meanwhile wins over an allow, as in the turn loop. A decision answers only the call that the person decided: the same call id and the same tool, and for a tool of the memory server the same canonical input (19.10). The hook of Claude Code uses a decision once, in the process that resumes the session after it.

### 19.5 Waiting, suspend, and a cold boot

opencode waits inside its server. When the decision comes while the process lives, the kernel answers the permission that waits, and the session goes on. When the Task slept meanwhile, the server and its permission are gone, but the session is in the database of opencode under the home of the harness. The new driver starts `opencode serve` again, gives the open tool calls an error result, `not finished: the harness stopped before the call ended`, and sends the session a note: the process stopped and started again, the person allowed or denied the call, and the session continues from where it stopped. The driver records `fw.answer` for the decision after the prompt with the note, so a stop before it gives the note again. When the session makes the same call again, with the same tool and the same input, the kernel answers it from the decision and asks no person again. A session that worked when the process stopped gets the same note without a decision.

Claude Code does not wait. On `defer` it exits with `stop_reason: tool_deferred` and the call in `deferred_tool_use`, and the kernel records the approval from it. After the decision, also after a cold boot, the kernel runs `claude -p --resume <session>` with the same flags and a new hook, the same call fires the hook again, and the hook answers it from the decision in the ledger. A process that a stop cut is resumed with the note of opencode.

The process of a harness runs in a process group of its own and ends with `fw`. A cancel of a run that waits aborts its session of opencode.

### 19.6 Budget

The host asks `Hosted.Over` before each turn and after each usage, with the limits of the run, of its tree, and the deadline of its lineage (section 5.2), and every 15 seconds while it follows the harness, also when the harness sends nothing. The hook of Claude Code asks it before each call and denies the call at a limit. At a limit the host records `fw.budget.exceeded`, gives the open calls an error result, records the changed files, finishes the run as `failed` with the budget error, and stops the harness: it aborts the session of opencode or stops the process of Claude Code. A hosted run makes no final report. Claude Code also gets `--max-turns` with the turns that the run has left, and `--max-budget-usd` with the cost that it has left when the budget sets `maxCostUSD`.

A harness that sends nothing for a long time counts as lost. opencode sends a heartbeat every 30 seconds, and its stream counts as lost after five minutes without any event, a heartbeat included: the driver aborts the session and starts again, as for a stream that ended, three times at most. Claude Code writes nothing while a tool call runs, so a process that writes no line for 30 minutes is stopped, and the run fails.

### 19.7 Files

A harness writes files without kernel events. The host keeps the size, the time, and the mode of each file under the directory of the run, and at the end of each turn with a tool that can write, and at the end of each exchange, it records each file that changed as `fw.file.changed`, with the SHA-256 of the content and the tool `opencode` or `claude-code`, as the `files` tool does (section 7.9). `run_file` and the result view then show the files. The comparison leaves out the home of `fw`, the reserved paths, the home of the harness, `.git`, and `node_modules`. A directory with more than 50,000 files gets no record and a warning in the log. A cold boot starts a new comparison, so a change that the harness made before the stop and after the last record is not in the ledger.

### 19.8 Processes and their environment

The host starts each process as the tool user, with the environment of a tool process (`fw.ToolEnv`) and the variables of `tools.bash.env`, in the directory of the run, as the shell of `bash` (section 7.12). `HOME` is the home of the harness, and `FW_RUN_DIR` names the directory of the run. The hooks of the linked packages (`fw.ExtendToolEnv`) get the run info of the run, as for a tool call, so the environment of section 18 waits for its install, or fails the run, before the process starts. `opencode serve` gets the environment of the first run that starts it, and starts again when the environment changed and no session works; a run that starts while a session works gets the server with the environment from before the change. When `opencode serve` ends, also by a crash, the host kills its process group. Then it adds what the harness needs on purpose:

| Kind | Command | Variables |
|---|---|---|
| `opencode` | `opencode serve --port <port> --hostname 127.0.0.1`, once for the process, in the root of the workspace. A session for each run, created with `POST /session?directory=<directory of the run>`; a prompt with `POST /session/{id}/prompt_async` and the model `fw-<provider>/<model>`; the events from `GET /event`. | `OPENCODE_SERVER_PASSWORD`, a random password for the basic authentication of the kernel; `OPENCODE_CONFIG_CONTENT`, one provider of opencode for each provider of the kernel that a model of the configuration uses, with `baseURL` and `apiKey: {env:<apiKeyEnv>}`, and the server `fw-memory` in the block `mcp` when a run has memory (19.10); `OPENCODE_PERMISSION`; `OPENCODE_DISABLE_MODELS_FETCH=1`; `OPENCODE_DISABLE_AUTOUPDATE=1`; the key under its `apiKeyEnv` |
| `claude-code` | `claude -p --input-format stream-json --output-format stream-json --verbose --model <model> --settings <hook> --session-id <uuid>` for the first exchange, `--resume <uuid>` for each later one, and `--max-turns`, `--max-budget-usd`, and `--append-system-prompt` from the run; `--mcp-config` with the server `fw-memory` of the type `http` when the run has memory (19.10) | `CLAUDE_CONFIG_DIR` (the home), `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL` from `providers.anthropic.baseUrl`, `DISABLE_AUTOUPDATER=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1` |

The instructions of the run go to the harness as its added system text: `system` of the prompt of opencode, and `--append-system-prompt` of Claude Code. The hooks, the skills, `CLAUDE.md`, `AGENTS.md`, `.mcp.json`, and `opencode.json` of the repository reach the harness unchanged, except a server named `fw-memory` (19.10). The output of a process goes to the log of `fw` at the level `debug`, with every secret replaced before the line is cut to 2000 bytes. The redactor knows the password of each `opencode serve` and the secret of the hook of each process of Claude Code (`Agent.AddSecret`), so a tool result that prints them reaches the ledger without them.

### 19.9 Limits

- The driver of opencode speaks the HTTP API of opencode 1, which `https://opencode.ai/docs/server/` documents. It was tested with opencode 1.19.3. opencode 2 has a new API under `/api` and needs a driver of its own.
- Claude Code needs a model of the provider `anthropic` and its key. Its hook needs the native API on a plain HTTP listener, so `fw run`, which serves nothing, and a listener with TLS cannot host it. A call that the policy asks about must be the only call of its response (19.4). `AskUserQuestion` needs an answer in `updatedInput`, which the hook does not give yet.
- A branch of a run whose session started (section 4.7) fails with `harness: a branch of a hosted run is not supported`, because the harness holds one session for the source and the branch. A branch from a point before the session started gets a session of its own.
- The path check of 19.4 reads the path fields of a call. A search of a directory above a private path, such as the root of the workspace, can still list the files there when the tool does not skip hidden directories, and `bash`, which the default policy asks about, reaches every path of the tool user.
- The key of the model reaches the harness, and so every process that the harness starts. The fix is a placeholder key and a credential that the egress gateway adds (docs/REVIEW_2026-10-20.md, section 6.2). The password of opencode and the secret of the hook are also readable by the processes of the harness, which run as the same user. The policy decides each call that the harness asks about; a shell that the policy allowed can do what the harness can. The boundary is the Task and the tool user, as for `bash`.
- An agent that a repository defines for opencode under another name can loosen its own permissions; the kernel prompts with `build`. A policy that denies `task` keeps such an agent out.
- The system prompt and the instructions of a profile do not reach the harness, only the instructions of the run. Attachments stay files in the directory `attachments` of the run: a message with attachments alone names them, and a message with text does not. The MCP servers of `tools.mcp` do not reach the harness yet. The terminal route of docs/REVIEW_2026-10-20.md, section 4.9, for `opencode attach` and `claude --resume` by a person, is not built yet.
- The usage of a model call that a stop of the process cut is lost.
- The memory server (19.10) is a route of the native API on a plain HTTP listener. A process that cannot serve it, such as `fw run`, which serves nothing, a listener with TLS, or a configuration without the surface `native`, gives opencode the memory section and the index without the server (19.10).
- A memory call of Claude Code runs only after its `PreToolUse` hook arms it. A version of Claude Code that does not send the hook for an MCP tool gets an error for each memory call, never an unchecked call. The settings of each process reject an entry named `fw-memory` in the `.mcp.json` of the repository with `disabledMcpjsonServers`. The documentation of Claude Code names this setting as the guard that blocks a server of `.mcp.json` in every permission mode, also in `-p`, where Claude Code loads such a server without asking. A check with Claude Code 2.1.309 in `-p` mode, with an API on the loopback address that answers no model call, confirms it. The server of `--mcp-config` connects with the source `dynamic`, and a server named `fw-memory` in `.mcp.json`, in the local scope, or in the user scope never starts, with the setting or without it, also when the settings of the repository approve the `.mcp.json` server. Without `--mcp-config`, the `.mcp.json` server connects, and the setting rejects it. The documentation does not state the precedence of `--mcp-config`, so the setting stays. `--strict-mcp-config` would hold too, but it drops every other server of the repository, so the harness does not pass it.
- A search of the memory server reaches the files of the workspace only when the run works in the root of the workspace, because the host has no workspace of a run directory to hand to the store yet.
- opencode merges the configuration of the project and of its `.opencode` directories with the inline configuration key by key, so the host cannot remove an entry `fw-memory` of the project before the merge. opencode 1.19 merges the inline configuration last, so its keys win, and a key that only the project sets stays. The host therefore checks the entry of each session instead (19.10).
- `fw.tool.requested` records the input of a memory call, the text of a note included, as the turn loop does for the tool `memory`; `fw.memory.changed` holds no text.

### 19.10 Memory

A hosted run gets the memory of section 7.8 as a run of the turn loop does, when its profile allows the tool `memory` or `recall`. The host reads that from `tools.builtin` and the tool list of the resolved profile, with the globs of the turn loop, because the turn loop builds the tools of a run at each turn and a host cannot ask it.

- **Ambient.** The added system text of the harness gets, after the instructions of the run, the memory section (`memory.SystemSection`, or `memory.SystemSectionFront` with a front, section 7.8.4), one sentence that names the tools of the memory server when the session has the server, and `fw.ContextNote`. A run with data blocks of its caller gets `fw.ContextNote` too, and the blocks follow the index. It never holds a note. The index of the notes goes as data, in a `<context source="memory">` block, to the start of the first prompt with text of the session, never to the system text. When the index leaves notes out, its last line says how many, and with the server, that the tools `list` and `recall` of `fw-memory` find them. Before that prompt goes out, the host records `fw.brief`, with `context: true` when the prompt holds the index, so a stop, a resumed session, and a cold boot never send the block again; the text of a prompt that a stop kept from the harness goes again, without the block. A message without text records no `fw.brief`, so the block waits for the first prompt with text. The `fw.prompt` of the prompt with the block also records `context: true`.
- **Active.** The native API serves an MCP server, `fw-memory`, on the route `/v1/harness/memory/<secret>`, stateless, with answers in JSON. It has four tools: `recall` searches and reads, `list` lists the notes, `remember` writes the whole text of a note, with an optional hash for a compare-and-swap, and `forget` removes one. Claude Code gets the server in `--mcp-config` and reuses the secret of its hook; each opencode serve gets the server in the block `mcp` of `OPENCODE_CONFIG_CONTENT` and a secret of its own, and the same configuration asks the kernel about each tool of the server by name, so a configuration of the project cannot turn that off. The redactor knows each secret. The route answers only a caller on the loopback address, and an unknown or ended secret gets 404. opencode serve waits up to 30 seconds for the native API to mount the route, as Claude Code waits for its hook route, so a run that a cold boot resumes before fw serves still gets the server; a process that runs no service, such as `fw run`, waits one second. A process that cannot serve the route (19.9) gives opencode no server: the host logs a warning once, and the added system text and the index name no tool of the server.
- **The name.** The name `fw-memory` belongs to fw, because a server of that name gets the text of each note and each query. The settings of Claude Code reject an entry of that name in the `.mcp.json` of the repository (`disabledMcpjsonServers`, 19.4). For each session of opencode, the host reads the configuration that opencode resolved for the directory of the session (`GET /config`): when its entry `fw-memory` is not the inline entry of the kernel and nothing more, because a configuration of the project changed a key or added one, such as `command` or `headers`, the session gets a deny for each memory call, its added system text names no tool of the server, and the host logs a warning. A tool whose name differs from a tool of the server only by case gets a deny (19.4).
- **Canonical input.** The MCP SDK does not check the arguments of a call against the input schema of its tool, and a field rule of the policy reads a key by its exact name. Before it asks the policy, the host therefore turns the input of a memory call into its canonical input: a JSON object with only the fields of the input schema of the tool, by their exact, case-sensitive names, each once and of its type, with the path of a note in its full form, such as `/memories/notes.md` for `notes.md`. A call without an input, with an input that is not an object, with a key that the schema does not name, also one that differs from a field only by case, with a key twice, or with a value of the wrong type gets a deny that names the key. A null value of an optional field counts as absent. The policy decides on the canonical input.
- **Arming.** The server makes only the calls that the policy of a run allowed. When the host allows a call of a memory tool, in the hook of Claude Code or in the answer to a permission of opencode, it arms the call for its run with its canonical input. It never arms a call whose input it did not see: a hook without `tool_input` gets a deny, and so does a permission of opencode that comes before the input of its call, with a reason that asks the harness to make the call again. The server turns the arguments of each call into their canonical input too, and answers arguments that have none with an error that names the key. It takes the armed call with the same tool and the same canonical input, in every harness, so a process that reads the secret cannot make an allowed call with other arguments. Before it makes the call, the server checks the rights of the run again: `remember`, `forget`, and `list` need the tool `memory`, and `recall` needs `memory` or `recall`. For a call that a person decided, the host arms the approved call, the tool and the input of the approval, and only when the call of the request is that call (19.4). The server makes a matched call in the memory scope of that run, which the kernel builds from the run info, never from the caller. opencode serves the sessions of many runs from one process and an MCP call of opencode carries no session, so the armed call is what tells the runs apart: two runs never hold the same armed call. A run that asks for a call that another run armed and the server did not take yet gets a deny at once, with a reason that asks the harness to make the call again. A call that no run armed gets an error. An armed call ends with its run, when the run finishes or is canceled, and expires after two minutes.
- **Refs.** `recall` reads a file ref, such as `file://src/main.go`, under the root of the workspace. Before the policy, the host denies a file ref with a hidden part, one that starts with a dot, which the search of the files skips too, and a file ref that resolves inside the home of the harness, the home of `fw`, the ledger, or the configuration file, links followed. The server refuses the same refs, and a search leaves such a file out. When the run works in the root of the workspace, the home of the harness can be below it, and it holds the transcripts of every run.
- **Ledger.** Each write appends `fw.memory.changed` to the ledger of its run, with the tool, the call id of the harness, and no text.
- **Notes of the agent.** A run of the scope `principal` reads the notes of the agent scope too (section 7.8.4). The index at the start of its session lists them first, and its added system text holds `memory.AgentSection`. `list` names them first, under `/memories/.agent`, after the line of `memory.AgentNotesHead`. `recall` reads `/memories/.agent/<name>` and `mem://agent/<agent>/<name>`, and a search finds them. `remember` and `forget` of a path under `/memories/.agent` change nothing and answer the error of the memory tool (`memory.AgentReadOnly`). The canonical input keeps such a path in its full form in every scope, so the policy decides on it, and the server decides what the run does with it: a run of the scope `agent` or `shared` gets the error of a hidden name, as before.

## 20. Outcomes

A run can claim work that nobody checked. Batch scenario1-a found pull request bodies with claims that no command showed. An outcome asks a second model context, the grader, to check the answer of a run against a rubric before the run finishes. When the grader finds a gap, the run gets one more turn to fix it. The kernel code is `fw/outcome.go`, and one call at the end of a successful run in `endTurn` (`fw/turn.go`) starts it.

### 20.1 Configuration

An agent profile and a run request can carry an `outcome` block with three keys:

| Key | Default | What it does |
|---|---|---|
| `rubric` | empty | Plain text that the grader applies. A block without a rubric is off. |
| `grader` | empty | The model of the grader, as a `provider/model` selector or a name from `models`. Empty means the model of the run. There is no other default, so an outcome never picks a paid model by itself. |
| `iterations` | 1 | The most grader passes. 1 grades once and never asks for a revision. 2 or more give the run one more turn with the notes of the grader after each `needs_revision`, up to this number of passes. 0 means 1. The most is 10. |

```yaml
agents:
  migrator:
    outcome:
      rubric: |
        Every claim of the pull request body names the command that shows it.
        The playbook runs twice without a change.
      grader: ""        # the model of the run
      iterations: 2
```

The block of a request replaces the block of the profile as a whole, so a request with `outcome: {}` turns the outcome of its profile off. An inline profile can carry the block too, below the block of the request. A child run never takes the outcome of its profile, neither a child of `spawn_task` nor a child in another process (`fw.parent`); only a block in its own request turns grading on for it. This rule keeps each child of a graded profile from paying for a grader of its own.

The kernel resolves the outcome once, when it queues the run, and records it in the field `outcome` of `fw.run.queued`, with the grader resolved through `models`. A run therefore keeps its outcome for its life, as it keeps its resolved profile (section 12.5). A run without a rubric records no field, and its ledger is the same as before.

The kernel refuses a block with more than 10 iterations or fewer than 0, a grader or a number of iterations without a rubric, a grader that is neither a selector nor a name from `models`, and a rubric longer than 256 KiB. A request gets `fw.ErrInvalidRequest`, which the native API answers with 400. A profile in `fw.yaml` stops the process at load with the key, such as `agents.migrator.outcome.iterations`. `Agent.CheckProfile` refuses such a profile from a linked package. The keys follow the rule of the variables (section 12.1), so `FW_AGENTS_MIGRATOR_OUTCOME_ITERATIONS=3` sets one.

A Go caller passes `fw.WithOutcome(fw.Outcome{...})` or sets `RunRequest.Outcome`. The body of `POST /v1/runs` takes the field `outcome` with the same keys.

### 20.2 The grader run

The grade happens at the end of a successful run only: the last turn ended with an answer, and the run is about to finish as `completed`. A run that fails, stops at a limit with its final report, ends with a refusal, or is canceled gets no grade. A follow-up that arrives before the grade starts goes first, and the grade comes at the end of the turn that it starts.

One pass takes these steps, each one from the ledger:

1. The kernel closes the turn with `fw.turn.finished`.
2. It checks the budget (section 20.4).
3. It starts the grader run as a child of the run, in this process, through the same path as a child of `spawn_task` in this process. The grader never runs through `spawn.driver`, because it reads the files of the run, which only this process holds.
4. It waits for the grader run and reads its verdict.
5. It records `fw.outcome.evaluated` on the run, and for a revision the notes in the same step (section 20.3).

The grader run is a run of the same agent profile with an inline profile on top:

- The system prompt of the grader below replaces the system prompt of the profile. The instructions of the profile still follow it, as for any inline profile (section 12.5).
- The model is `outcome.grader`, else the model of the run.
- The only tool is `files`, and only when the profile and every ancestor allow it. Its rules deny the commands `create`, `str_replace`, and `insert`, so the grader can only view.
- It has no skill and no outcome of its own, so a grader is never graded.
- Its budget is 6 turns, 24 tool calls, 10 minutes of working time, and no final report. The budget of the run caps it, and its deadline never passes the deadline of the run, because a child counts the working time of each ancestor (section 5.2).
- Its origin is `kernel`, its title is `Grade <n> of <title of the run>`, and its idempotency key is `outcome:<run>:<seq>`, where `<seq>` is the sequence number of the answer that it grades. A restart during a pass therefore takes the same grader run instead of a second one.
- The grader works alone. The kernel denies every approval and every question of the grader run.

`spawn.maxDepth`, `spawn.maxChildren`, `spawn.maxDescendants`, and `spawn.disabled` hold for the grader as for any child, and each pass counts as one child. A grader run shows among the children of the run on every surface.

The input of the grader run is one message with these parts, each with a bound:

- The rubric.
- The task: the input of the run and every follow-up message that did not come from the kernel, up to 16 KiB together.
- The final answer of the run, up to 32 KiB, and the number of the pass.
- The notes of the earlier passes of the same task.
- The files that the run changed, from its `fw.file.changed` events, with the content of each text file, up to 64 KiB for all files together and 200 files. A path is relative to the directory of the run (section 7.9), where the files tool of the grader works. A file that does not fit gets the start that fits or a line that says that it is not shown, and the input then tells the grader to read such a file with the files tool. A change that a shell command made has no `fw.file.changed` event and does not show.

The text says what it cut, with the size before the cut. The system prompt of the grader is this text:

```
You are the grader of a run of an agent. You did not do the work of the run, and you do not do it now. You check it.

The message holds the rubric of the run, the task that the run got, the final answer of the run, and the files that the run changed. Apply each criterion of the rubric to the work. Check each claim of the answer against the files and the facts in the message. A claim that nothing in front of you supports counts as not done. If you have the files tool, you can read a file of the run with its view command. Do not change a file.

Write your notes for the agent that did the work. Name each criterion that the work does not meet, and say what must change. Keep the notes short and specific.

The last line of your answer must hold one JSON object and nothing else, without a code fence:
{"verdict": "satisfied", "notes": "..."} when the work meets every criterion of the rubric, or
{"verdict": "needs_revision", "notes": "..."} when it does not.
```

The kernel reads the last line of the answer that is not empty. The line must hold one JSON object with exactly the keys `verdict` and `notes`, both strings, and nothing after it, and the verdict must be `satisfied` or `needs_revision`. A code fence, another key, or another verdict does not parse. The kernel keeps up to 8 KiB of the notes.

### 20.3 The event and the loop

`fw.outcome.evaluated` records one pass on the run:

| Field | Value |
|---|---|
| `verdict` | One of the verdicts below |
| `notes` | The notes of the grader |
| `iteration` | The number of the pass, from 1 |
| `iterations` | The most passes of the outcome |
| `grader` | The id of the grader run, empty when none started |
| `continue` | `true` when the run gets one more turn with the notes |
| `reason` | Why the kernel recorded `max_iterations`, `unparsed`, or `skipped`, or why a `needs_revision` does not go on |
| `answer` | The sequence number of the `fw.model.responded` whose answer the pass graded |

| Verdict | Who says it | What follows |
|---|---|---|
| `satisfied` | the grader | The run finishes with its answer. |
| `needs_revision` | the grader | Before the last pass, the run gets one more turn. When the budget leaves no room for that turn, the verdict stays, the reason says why, and the run finishes. |
| `max_iterations` | the kernel | The grader asked for a revision at the last pass. The event keeps its notes, and the run finishes. |
| `unparsed` | the kernel | The answer of the grader does not parse, or the grader run failed or was canceled. The loop ends, and the run finishes. |
| `skipped` | the kernel | No grader run started (section 20.4). The run finishes. |

For a revision, the kernel appends a `fw.message.received` with the role `user`, the origin `kernel`, and the note kind `outcome` in the same step as the verdict. It holds the notes, the number of the pass, and the instruction to fix what the notes name and to end the turn with the complete final answer. The next turn starts from it, as from a follow-up (section 5.5), and the end of that turn starts the next pass. A restart between the verdict and the notes gives the notes at the next step, so each verdict is recorded once and each revision gets its notes.

The passes of a task count from the latest message that did not come from the kernel. A follow-up of a person to a finished run therefore starts again at pass 1, and the grader grades the new answer.

The run finishes with its last answer as `completed`, whatever the verdict. The verdict is a grade next to the answer, not a failure of the run. The output is the answer of the last turn, so after a revision it is the revised answer.

### 20.4 Budget

A grader run is a child, so its tokens, its cost, and its tool calls count in the tree of the run and of every ancestor (section 5.2). Before a pass, the kernel checks the run: when the run or a tree of its lineage used up `maxTokens`, `maxCostUSD`, or `maxToolCalls`, or the lineage used up its working time, no grader starts. The kernel records `skipped` with the limit in the reason, and the run finishes with its answer. A grader run that `Start` refuses, for example at a spawn limit, gives `skipped` with the error as the reason. Before a revision turn, the kernel checks the turn as the check before the next model call does, `maxTurns` included, so a revision never starts a turn that the budget stops at once. A run of a hosted harness (section 19) starts no grader: it records `skipped` with the reason `a hosted harness has no grader`.

### 20.5 The run object

The run object has the field `outcome` with the latest pass: `verdict`, `notes`, `iterations` (the passes so far), `grader` (the id of the latest grader run), and `reason`. It is absent for a run without a rubric. During a revision turn it shows the `needs_revision` of the pass before. A follow-up to a finished run clears it together with the output. Every surface that returns the run object therefore shows the verdict next to the answer. The event lines of the native API and of `run_events` read `graded <verdict> in pass <n> of <m>` with the notes, and the event summary of `fw` gives the verdict, the pass, and the grader run without the notes.

A front that builds the run object from the lifecycle events of its workers, as the fleet of the workbench does, sees the field only when `bus.egressTypes` of the workers includes `fw.outcome.evaluated`.

### 20.6 What the ledger holds

Unlike the system prompt of a profile, the rubric of a profile enters the ledger: `fw.run.queued` of the run holds the resolved outcome, and the input of each grader run holds the rubric, because the grader reads it from there. The content of the files in the input, up to 64 KiB for each pass, enters the ledger of the grader run too. The kernel replaces every secret in both, as in every event (section 4.2). A rubric therefore must hold nothing that a viewer of the run must not read.

A replay (section 4.6) is a new run and resolves its outcome again: it takes the outcome of the named profile now, and the outcome of the request of the recorded run does not travel to it. Its grader runs call the model live, because a replay answers only the tool calls of the recorded run.

## 21. fw ax and the doctor in a Task

### 21.1 One Task from one profile file

A developer without a front wants one Task from one profile file. Before `fw ax`, that took five steps: write the profile, render a Workspace and a Task by hand, apply them with `ax`, resume the Task, and wait for `/readyz` through the router. The verb `fw ax` in `cli/ax.go` does them in one command, with the manifest types of the ax API and the gRPC client that the spawner of section 11.9 uses:

```
fw ax render -f agents/reviewer.yaml --name reviewer --image <image@sha256:...> --egress deb.debian.org:80
fw ax up     -f agents/reviewer.yaml --name reviewer --image <image@sha256:...> --server 127.0.0.1:8080 --router 127.0.0.1:8001
fw ax status --name reviewer
fw ax events --name reviewer <run-id>
fw ax down   --name reviewer
```

| Flag | Verbs | Meaning |
|---|---|---|
| `--name` | all | The name of the Task, required. The Workspace is `<name>-ws`. |
| `--atespace` | all | The atespace, `default` by default. |
| `-f` | render, up | The profile file: a fragment of `fw.yaml`, such as `policy`, `access`, and `agents`, in the shape of `e2e/fleet/ax/profiles/*.yaml` of the lab. |
| `--image` | render, up | The image by digest, `name@sha256:...`, required, because Substrate pulls an image by digest only. |
| `--egress` | render, up | One more destination, repeated: `host:port` for an `http` rule, `tls:host:port`, `https:host:port`, or a URL. |
| `--env` | render, up | One more `spec.env` entry, `NAME=VALUE`, repeated. It wins over a derived entry. |
| `--workspace-file` | render, up | One more Workspace file, `path=localpath`, repeated. The path stays inside the workspace and is not `.fw/fw.yaml`. |
| `--debug` | render, up | Sets `spec.debug`, which turns on the guest services of `ax ssh` (section 11.12). They carry no authentication. |
| `--server` | up, status, down | The ax API. Else `AX_SERVER`, the variable of the ax command line, else `ax.server`. |
| `--router` | up, status, events | The atenet router. Else `ax.router`. |
| `--timeout` | up | How long `up` waits for `/readyz`, 10 minutes by default. |
| `--format` | events | `jsonl`, the default, or `text`. |

The verbs dial the addresses directly and never start a port-forward. The defaults of `ax.server` and `ax.router` are the addresses inside the cluster, so a laptop forwards the ports first and passes them. Each verb prints the addresses that it uses:

```
kubectl -n ax-system port-forward svc/ax-server 8080:8080
kubectl -n ate-system port-forward svc/atenet-router 8001:80
```

### 21.2 What render writes

`render` prints the Workspace and the Task as one stream of YAML documents, in the order of apply, with a comment that holds the warnings. The output is the same objects that `up` applies, with a placeholder `<secret: NAME>` in place of each secret value. It refuses a name that ax refuses, an image without a digest, a profile file with a key that the kernel does not know, a file path outside the workspace, and a Task that names no model. `fw ax` never chooses a model.

The Workspace holds `.fw/fw.yaml`, which the kernel reads under ax (section 11.3). The file starts from the configuration of the caller, reduced to the keys that a Task shares with it: `model`, `models`, `effort`, `providers`, `bus`, `tokens`, and `tools.mcp`. A provider and an MCP server leave their headers behind, because a header can hold a secret, and each one that does gives a warning. A TLS file that a key names must reach the Task with `--workspace-file`. Each `$` of these values is doubled, so a value that the caller already expanded reaches the Task unchanged. The profile file goes on top as written: a key of the file replaces the same key, two mappings merge key by key, and a `${NAME}` of the file expands in the Task, from `spec.env`.

The Task gets `spec.env` in this order: the static token when `auth.mode` holds `token`, the key of each model provider, the bearer of each MCP server, the credential for the token broker, the OTLP endpoints of the caller (`OTEL_EXPORTER_OTLP_ENDPOINT` and the endpoint of each signal), and the entries of `--env`. A secret takes its value from `--env`, else from the secrets driver of the caller. `render` prints a placeholder for each one, and for each `--env` entry with a secret name, and warns when the caller does not have a value. `up` refuses to start until every value is there.

The hosts of `spec.egress` come from the endpoints of the configuration of the Task, so none is forgotten: the base URL of each model provider, or the default endpoint of the adapter; each URL of the bus; `tokens.url`; the OTLP endpoints; and the endpoint of each MCP server. A child Task of `spawn.driver: ax`, an A2A peer, and an OIDC issuer need a host from `--egress`. A plain `http` or `ws` URL gives an `http://` host, and an `https`, `wss`, or `rediss` URL a plain host, which is TLS that the gateway forwards without decryption, each with its port. `--egress` adds more: an entry as `spec.egress.hosts` takes it, a host for TLS on port 443, `http://host[:port]`, or `https://host[:port]`, or a `ws`, `wss`, or `rediss` URL. The same host appears once. An endpoint that no host can carry gives a warning instead: a scheme such as `nats://` or `redis://`, because the egress gateway carries only HTTP and TLS, and `localhost` or an IP address, because a host is a DNS name.

### 21.3 up, status, events, and down

`up` renders with the values, checks that the router answers, and only then changes anything. It applies the Workspace with `UpdateWorkspace`, creates the Task, and resumes it, because ax creates every Task suspended. Then it reads `/readyz` through the router until it answers 200. While it waits, it reads the phase of the Task and resumes a Task that ax reports as `Suspended` or `Failed`, up to three times, as the lab does after a crash at boot. Three misses of the router in a row stop the wait. A Task that exists already stops `up` with the advice to run `down`, because a Task does not change. At the end, `up` prints the address: the router and the header `ate-target-actor: <atespace>/<name>`.

`status` prints the phase and the conditions of the Task from the ax API. While the Task runs, it also prints the body of `GET /v1/status` of the kernel, read through the router (section 11.8), as JSON, with `idle` and `idleSince`. It does not read the kernel of a Task that sleeps, because a request through the router wakes it. `events` reads the ledger of a run through the router, with `GET /v1/runs/{id}/events` and `Accept: application/json`, a page of 1,000 events at a time, and prints the lines of `fw events`. Both send the static token of the caller. `down` deletes the Task, tries again for a minute while Substrate refuses, waits until ax forgets the Task, and deletes the Workspace.

### 21.4 The doctor in a Task

A sandbox hides facts that the configuration does not show. `cli/doctor_ax.go` adds four lines to `fw doctor` through `cli.RegisterDoctorCheck`. They run inside the Task, through `ax ssh <task> -- fw doctor` or a Task with `spec.debug`, and need no cluster client. The process decides at its start: with `AX_TASK_YAML` or `AX_METADATA_URL` it registers the four lines, and otherwise one line, `ax: not under ax`. Without `AX_TASK_YAML`, the lines read the manifests from the metadata endpoints.

| Line | What it checks | Verdict |
|---|---|---|
| `ax reach` | Each endpoint of section 21.2, with the MCP servers of the Workspaces. An `http` endpoint gets one GET without a credential, and any answer but the refusal of the egress gateway, a 403 with the body `egress denied`, passes. A `tls` endpoint gets a TLS handshake, which the gateway ends when no rule covers the name. Each probe has five seconds at most, and the probes run at the same time. | FAIL when an endpoint does not answer, with each endpoint and the fix |
| `ax trust bundle` | The bundle of the egress gateway at `/run/ate/egress-trust-bundle/trust-bundle.pem`, where a SystemInfo volume puts it in `docs/egress-trust-bundle.md` of Agent Substrate. ax mounts no such volume yet, and an `https://` host needs the bundle, because the gateway terminates its TLS. | ok with the limit while the file is absent, because nobody in the Task can change it; ok with the number of certificates; FAIL when the file holds no certificate |
| `ax tools user` | The user that runs the tools, as `ax.ApplyToConfig` chooses it, and whether it is a separate user from the one of `fw`. | ok with the reason when no separate user is in effect; FAIL when `tools.user` names a user that root cannot become |
| `ax durable` | `FW_HOME` is under `/workspace` and writable, `/workspace` is a mount in `/proc/self/mountinfo`, and every binding is under it. | FAIL with each problem |

Example lines for a Task of the lab in gVisor, with one egress rule missing:

```
FAIL  ax reach: 1 of 5 endpoints does not answer: mcp github github-mcp.fw-fleet.svc.cluster.local:8082 (the egress gateway denies it); fix the egress of the Task (fw ax render derives a host for each endpoint); these answer: model openai ax-model.fw-fleet.svc.cluster.local:8080, bus nats.fw-fleet.svc.cluster.local:8080, tokens fw-front.fw-fleet.svc.cluster.local:8080, telemetry opentelemetry-collector.otel-system.svc.cluster.local:4318
ok    ax trust bundle: absent at /run/ate/egress-trust-bundle/trust-bundle.pem; https:// hosts of spec.egress need the SystemInfo volume there, so use http:// and plain hosts
ok    ax tools user: the tools run as root (0), the user of fw, because root lacks CAP_CHOWN, CAP_DAC_OVERRIDE, CAP_SETGID, CAP_SETUID in this sandbox; no separate user is in effect, so a tool process can read the files of fw
ok    ax durable: FW_HOME /workspace/.fw is writable on the volume at /workspace, with 1 binding on it
```

The trust bundle line counts the certificates once an ax patch mounts the SystemInfo volume at that path. The line of the kernel `tools user` names the default of ax only for root with the capabilities of section 7.12, as `ax.ApplyToConfig` does, and the `ax tools user` line adds whether the user is a separate one.

## 22. Routines

A routine is a vetted sequence of tool calls that the kernel runs in place of the model. It has a decision schema, steps, checks afterward, and an answer. `docs/ROUTINES.md` is the contract between the kernel, the workbench, the page, and the lab. The workbench stores the routines, collects the evidence, and changes the standing. The kernel runs what the profile says and records what happened.

### 22.1 The routine and the profile

A profile carries its routines in `routines`, as `fw.Routine` values. A routine has these fields:

| Field | Meaning |
|---|---|
| `name`, `version` | 1 to 64 of `a-z`, `0-9`, and `-`, unique per agent, and a positive integer |
| `standing` | `shadow` or `live` |
| `canary` | for a live routine, the share of the runs that the agent handles anyway. Without it, the share is 0.05. |
| `decider` | a key of `deciders`, for a routine with questions |
| `questions` | the decision schema: a type (`choice`, `noul`, or `score`), instructions, criteria, the band `act`, the expected answers `expect`, and `observe`. The criteria of a `noul` are absent or name `true` and `false` only. |
| `steps` | the tool calls in order, each with an input, an optional `as`, and an optional `compare`, the keys that a replay compares |
| `after` | the checks afterward: a call, with the substrings that its result holds (`contains`) and lacks (`lacks`) |
| `answer` | after a routine that completes, the output of the run |

The inputs, the answer, and the strings of `contains` and `lacks` are Go `text/template` texts. The data holds `.input`, `.json`, `.meta`, one field per question, and one field per step with `as`. The functions are `find`, `json`, and `lower`. The built-in functions of `text/template`, such as `printf`, `if`, and `eq`, stay. A missing field is an error. A `find` that matches nothing is an error too, so a routine never calls a tool with a missing value.

An inline profile with a `routines` list replaces the routines of the named profile, as a tool list does. An empty list leaves the run without routines, and the JSON of a profile keeps an empty list, as it keeps the skills. The resolved profile holds the routines, so a run keeps the routines that it was queued with (section 12.5).

With `routines.front`, each run reads the routines of its agent from the front of a fleet at its start instead (docs/ROUTINES.md section 3.1, `fw.RoutineSource`). The client keeps the list of each agent with its version and asks the front each time with that version, so a stale copy is never applied. When the front holds the agent, its list replaces the routines of the profile. When it does not, the run takes the routines of its profile. When it does not answer, or its list does not pass the checks below, the run applies no routine. `fw.run.started` records the list as `routines` with its version, and the runner, a restart, and the comparison at the end read that record. A move of a routine at the front then reaches the next run, and no configuration of a worker changes for it.

When the kernel loads a profile, it checks the routines. It also checks the routines of each inline profile. It refuses these:

- more than 64 routines in a profile
- more than 16 steps, more than 16 checks, more than 16 questions, or a template over 4 KiB
- a routine with questions and no decider, or a decider that `deciders` does not have
- a template that does not parse, or a regular expression that does not compile
- a name that is not valid, or a name that an earlier routine has
- any routine on a process that hosts its runs (section 19), because no turn loop runs there

Each error names the key, such as `agents.triage.routines[0] (ack).steps[1].input`, in words for an operator: "no model answers questions under the name clef". `fw.CheckRoutineName` checks a name with the same rule, for the workbench.

### 22.2 Deciders

The block `deciders` names the servers that routines and the tool `decide` ask:

```yaml
deciders:
  clef:
    driver: systemone                # optional, the client of the decider, systemone by default
    baseUrl: http://127.0.0.1:5100   # POST {baseUrl}/v1/systemone
    apiKeyEnv: CLEF_API_KEY          # the secret of the bearer token; empty sends no token
    model: clef                      # required; the response names the model served
    timeout: 30s                     # one call; 0 is 30s
```

Every field has a variable: `FW_DECIDERS_<NAME>_DRIVER`, `FW_DECIDERS_<NAME>_BASE_URL`, `FW_DECIDERS_<NAME>_API_KEY_ENV`, `FW_DECIDERS_<NAME>_MODEL`, and `FW_DECIDERS_<NAME>_TIMEOUT` (section 12.1). A variable creates the entry when the file lacks it.

Package `fw` holds the interface `fw.Decider`, the configuration, the tool `decide`, and a registry of the clients. A driver package registers its client in `init` with `fw.RegisterDecider(name, ctor)`, as a model provider does, and `driver` names it. Package `decider/systemone` is the one driver of the kernel, and every build links it, the lean build too, because it needs only `net/http`. A worker image therefore serves a configuration with `deciders` without a rebuild. A `driver` that the binary does not link stops the load with the drivers that it links. `fw.WithDecider` gives an agent a decider of its own, such as a fake one in a test.

No decider exists by default, and a decider has no default model. The System One client posts `{model, state, questions}`, with at most 64 questions in one call. The runner splits more questions of one decider into several calls. It reads the answers of Clef and Workers AI: `choice`, `noul`, and a `score` with a legend. It also reads the answers of TypeSafe: `answer`, `probability`, and a score by level. Any other shape is an error that names the field. The token comes from `Secrets` at each call and goes only in the `Authorization` header. The redactor knows the token, so no event and no log line holds it. A tool process never gets the variable of `apiKeyEnv` (section 7.12).

### 22.3 The runner

The runner acts once per run, after `fw.run.started` and before the first model turn. It acts on the routines with the standing `shadow` or `live`. A routine with `duty` counts only for a run whose metadata `workbench.duty` names that Duty (section 7.8.4). It never acts on a replay, a branch, a check run (section 23), or a comparison run (section 7.8.4). After a restart, it never runs a routine again.

1. The runner asks each decider once for the routines that share it. It calls the tool `decide` of the kernel, `fw.DecideTool`, through the tool path of the model, whatever `tools.builtin` holds. The call and the probabilities are therefore in `fw.tool.requested` and `fw.tool.responded`. The question ids are `<routine>.<question>`, and the state is the input of the run.
2. It records one `fw.routine.decided` per routine, in the order of the profile. A shadow routine has the mode `shadow`. If the canary share picks the run, a live routine has the mode `canary`. Otherwise it has the mode `live`. The pick is a stable hash of the run id. If every question passes its band and its expected answers, the routine applies. When a decider fails, each of its routines does not apply, with the reason.
3. The first routine in the mode `live` that applies runs its steps and then its checks. Each call goes through the path of a call of the model: the budget of the run and of its tree, the policy with its approval flow, and the limits of the tool. Each call records its events with `routine` set. These end the routine: an error result, a failed command, a policy deny, the deny of a person, a template error, and a failed check. A failed command exits with a code other than 0. A tool such as `bash` reports a failed command as a normal result for the model, so the runner reads the exit code itself. A step `test -f notes.txt` therefore fails when the file is missing, and so does a check with that command. The context block names such a call as an error. The runner records `fw.routine.applied`.
4. A run with a definition of done (section 24) checks the work of a routine that completed, as it checks an answer of the model. The kernel runs the checks before `fw.routine.applied` and records the pass as `fw.done.checked` with `answer` 0, because no model answered. The passes of the model leave that pass out, so the model keeps every attempt. When a check fails, the routine did not complete: `fw.routine.applied` has `ok: false`, `done: failed`, the failed step one place after its steps and its checks, and a reason that starts with `done:` and names the failed checks. When the routine completes, the run finishes with the rendered answer and makes no model call. With a definition of done, its `fw.run.finished` carries `"done": "checked"` and the stop `checked`, so a run of a routine counts as checked only as any run does (`fw.CheckedDone`). A weak pass (section 24) gives `fw.routine.applied` and `fw.run.finished` `"doneWeak": true`, and the stop is `answered`. Otherwise the model runs. When the run has a decision in the mode `live`, the first user message starts with one block of data, as below. A run in the mode `shadow` or `canary` gets no block, because the model must not see the answers of the routine.
5. At the end of a run that the model completed, the runner compares. It takes each routine in the mode `shadow` or `canary`, and each live routine that did not complete. For each question with `observe`, it records one `fw.routine.compared` before `fw.run.finished`. A finished run therefore stays finished. A run that fails is not compared.
6. A routine in the mode `shadow` or `canary` that applies also gets one comparison of its calls, `fw.routine.compared` with `calls: true` and no question. The runner renders the steps and the checks of the routine on the run, with the answers of the routine and with the results of the calls of the agent, and compares them with those calls as a replay does (section 22.4, `fw.CompareRoutineCalls`). `agree` is true when every step and every check with a call matched. For the first step that differs, `step` is its place, `said` and `observed` are the input of the routine and of the agent, each bounded to 200 bytes, and `reason` says how they differ, such as `step 2 files: the key file_text differs`. A routine without questions therefore gains agreement too. A run on which a `find` of the routine finds no value is one that the routine hands to the agent: it gets no comparison of the calls. The calls of the kernel, of the runner and of the checks of done (`fw.KernelCall`), are no calls of the agent.

A restart can cut a live routine off. At the next start, the runner records `fw.routine.applied` with the reason "the process stopped during the routine", and the model starts with the block. The kernel resumes a run that waits for an approval of a call of its routine. It denies the approval as `kernel` first, so a later answer of a person gets `ErrNoPendingApproval`. After a routine that completes, the next step of the turn loop finishes the run. A restart between the two events takes the same step. A replay keeps the mark `replayOf` in its metadata, so it runs no routine after a restart either. A branch never compares the decisions that it kept from its source.

The block names the source `routine`, so the sentence `fw.ContextNote` of the system prompt covers it (section 7.8.2):

```
<context source="routine">
A routine of this agent looked at this run before you and did not complete it, so the run is yours. What it decided and did:
- Routine ack, version 2: applies. Answers: beyond: the probability of yes is 0.01; type: bug with probability 0.93.
Calls of routine ack:
1. gitea.label_read, ok: [{"id":11,"name":"type/bug"},...]
2. gitea.issue_write, ok: ok
3. gitea.issue_write, error: gitea: 503 Service Unavailable
It stopped: step 3 gitea.issue_write: error: gitea: 503 Service Unavailable.
</context>
```

### 22.4 Replay and propose

Package `routine` reads recorded runs. The calls of a run are the calls of the model, without the calls of the kernel (`fw.ModelCalls`). `routine.Replay` checks a routine against them. For each run, it reads the answers of the agent with `observe`. A run whose answer a question does not expect, or whose `find` finds no value, is one that the routine hands to the agent: Replay counts it as handed. For every other run, Replay renders each step and compares it with the call of the agent to the same tool at the same place. It compares each key of `compare`, or else each key of the rendered input, as canonical JSON. A key that only the agent sets is extra and is counted. A check matches the next call of its tool and the recorded result. A check that the agent never made is unchecked and is counted. With a decider, Replay also scores the decider per question: the agreement, the Brier score, and a table of reliability at the bands 0.5, 0.7, 0.85, and 0.95.

`routine.Propose` makes a routine from the runs that completed and that nobody sent back. The calls of the checks of done are no steps. The most common sequence of tool names gives the steps, and a list takes the length that most runs give it. The runs with another sequence or another length are not used, with the reason. Each value of an input, and of a JSON answer, gets one kind:

- A value that every run shares and that the agent read in an earlier result stays a `find` over that result: a text with a space in it, such as a line of a file, or a number after the word that labels it, such as `pens=` before `120`. So a routine that copies a line writes the line of its own run.
- Any other value that every run shares is a constant: a name, a path, a key, a number without a label, or a value that only the input holds.
- A key that some runs leave out and the others set to one value is an optional key, which the routine leaves out.
- A list goes element by element.
- A value from the input or from an earlier result is a `find`. Its context is the word that labels the value, such as `Number: (\d+)`, else the shortest context that reads every run. A text ends at the next character or at the end of the text.
- A value from a small set, at most 8 values over at least 10 runs, is a question with the option `other`. The values of the answer become questions first.
- A value whose context in a result holds the value of a question is a `find` with `printf`.
- Any other value is free, `{{/* free <place> */}}`. The routine stays incomplete until a person writes it, and a replay of the step says "the routine cannot fill the value <place> yet".

The proposal also lists each problem that the kernel refuses in the routine. The verbs `fw routine replay` and `fw routine propose` run both over a ledger (section 12.9). Without `--ledger`, they read the ledger of the configuration.

A proposal reads many runs. That is the work of the workbench or of a librarian over a ledger, not of a worker. A build with the tag `lean`, such as a worker image, therefore leaves out `routine.Propose` and `fw routine propose`, and the verb then says so. The tag `propose` brings them back. `fw` without the tag and `fw-workbench` carry them. A replay stays in every build, because it needs the rendering of the kernel.

### 22.5 Turn routines on

A process without deciders and without routines spends nothing on them. No run asks a decider, the tool `decide` is off, and the runner returns at once. The rendered Task of a profile without routines does not change. These steps turn routines on for one profile:

1. Name a decider in `deciders`, or with the variables `FW_DECIDERS_<NAME>_*`, and add `decide` to `tools.builtin`. A routine with questions needs both. A routine without questions needs neither.
2. Give the profile its routines under `agents.<name>.routines`, in `fw.yaml` or in a profile file, with the standing `shadow` first. The workbench gives the runs of a pool the routines of its store instead, through `routines.front` (section 22.1, and the workbench architecture, section 21.3).
3. Read the decisions and the comparisons in the ledger, or run `fw routine replay --decide` over the past runs.
4. Set the standing to `live` once the routine agrees with the agent. `canary` keeps a share of the runs with the agent.

To turn a routine off, take it out of the list, or send an inline profile with `routines: []`. Without a decider, the load refuses a routine with questions, so a half setup never runs.

## 23. Check runs

A check run makes tool calls and calls no model. The front of a fleet starts one for the check of a duty, and it wakes the model of the duty only for work. A quiet queue therefore costs no tokens.

### 23.1 The request

A run request takes `check`. The body of `POST /v1/runs`, a message of the bus intake, and `RunRequest.Check` of the Go API take the same shape:

```json
{"agent": "triage", "input": "", "check": {"steps": [{"tool": "servicenow.query_incidents", "input": {"query": "priority<=2"}}], "timeout": "30s", "maxBytes": 1048576}}
```

| Field | Rule |
|---|---|
| `steps` | 1 to 5 tool calls, in order. Each step has `tool`, the name of a tool of the agent, and `input`, a JSON object. An empty input is `{}`. |
| `timeout` | The time limit of the whole check, as a duration string such as `"30s"`, or a number of nanoseconds. The default is 30 seconds. The limit is 60 seconds. |
| `maxBytes` | The most bytes that the output keeps of the result of each step. The default is 1 MiB. The limit is 4 MiB. |

The input of a check run can be empty. The kernel refuses a check outside these bounds with a 400 that names the field. A Go caller, such as the front of a fleet, gets the same answer from `fw.Check.Resolved` before it saves a duty.

The kernel sets the metadata `fw.kind` to `check` on a check run (`fw.MetaKind`, `fw.RunKindCheck`, `fw.Run.Kind`). Only the kernel sets this key. A request or an annotation with this key gets a 400.

### 23.2 The calls

After `fw.run.started`, the turn loop makes the calls of the steps in order. Each call takes the path of a call of the routine runner (section 22.3). The budget of the run and of its tree, the policy of the agent, and the limits of the tool apply. The ledger records `fw.tool.requested` and `fw.tool.responded` for each call, as for a call of the model. The call ids start with `check_`.

- If the policy asks before the tool of a step, the run fails at once, because nobody answers a check. The error names the step, the tool, and the rule, such as `check step 1 (rm): the policy asks before this tool (rule: ask destructive), and a check runs only tools that the policy allows without a question`.
- If the policy denies the tool of a step, the run fails with the reason of the policy.
- If the agent has no tool with the name of a step, the run fails. A tool that the tool list of the profile leaves out counts as such a tool.
- After a step that fails the run, no step runs.
- An error result of a tool does not fail the run. The result of the step then has `"isError": true`, and the next step runs.
- A tool of a check gets no asker and no connecter. A question of the tool therefore gets `fw.ErrNoAsker`, and the run never waits for a person.
- At the time limit, the call in flight ends. The ledger records its error result, and the run fails. A step with no time left does not start.
- Each call counts against `budget.maxToolCalls` of the run, as any call does.

The `fw.run.started` of a check run names no model, and no tool writes a brief or a context for it. A process without a model therefore takes a check run. On a process whose runs a `Runner` drives (section 19.2), the turn loop runs the check runs. The runner of routines does not act on a check run.

### 23.3 The output

The output of a completed check run is JSON with one result per step, in the order of the steps (`fw.CheckOutput`):

```json
{"results": [{"tool": "servicenow.query_incidents", "output": {"result": {"records": [{"number": "INC0012"}]}}, "isError": false}]}
```

`output` holds the text of the result of the tool. If the text parses as JSON, `output` is that JSON. If it does not and the result has structured content, as an MCP tool that writes words for a model and JSON for a program does, `output` is the structured content, so a check reads the JSON of such a tool. Otherwise, `output` is a JSON string.

If the text is longer than `maxBytes`, the step keeps the first `maxBytes` bytes of the text, on a rune boundary. Its `output` is then a JSON string, and the result has `"cut": true`. The ledger records the cut result. A result with an image or a document keeps only its text, and it also has `"cut": true`. The output limit of the tools (`tools.limits.maxOutputBytes`, section 7.7) does not cut the result of a step, because no model reads it.

A failed check run has the reason in `error`. Its output holds the results of the steps before the failure. A check run that failed at its first step has no output.

The usage of a check run has no tokens and no cost. It counts the tool calls.

### 23.4 Limits of a check run

- A check run takes no follow-up message and no branch (section 4.7), because a message starts the model. `Agent.Send` and `Agent.Fork` refuse with `ErrInvalidRequest`, and the routes answer 400.
- A check run has no outcome (section 20), because no model writes an answer to grade.
- After a restart, a check run that made no call yet runs its check. A check run that a restart or a shutdown cut off after its first call fails with the error "the process stopped during the check". The kernel never runs a check twice, because it cannot know that the tools of the check only read.

### 23.5 The lists and the intake

`GET /v1/runs` leaves out the check runs. The query `kind=check` lists the check runs alone. The A2A method `tasks/list` and the search of recall over runs (section 7.8.3) also leave out the check runs. `GET /v1/runs/{id}` reads a check run as any run. A linked package, such as the front of a fleet, filters its own lists with `view.FilterKind`. The status of the process and the metrics count the check runs, so a Task with a check in progress is not idle.

A check run comes over the bus intake as any run request (section 9.3). The retry of a failed check run keeps the check, so the retry is a check run too. The dead-letter record also holds the check.

## 24. Definition of done

A run that completes today is a run whose model stopped. Nothing in the kernel says that the work is done. A definition of done closes that gap. It names checks that the work must pass, and the kernel runs them before the run can complete. This is the rule of lean work. A defect stops the line at the step that made it. So the person at the end of the line does not need to inspect every result. The code is `fw/done.go`, and one call at the start of `endTurn` (`fw/turn.go`) starts it.

### 24.1 Configuration

An agent profile can carry a `done` block:

| Key | Default | What it does |
|---|---|---|
| `checks` | none | 1 to 8 checks. Each check has a `tool` and an `input`, a JSON object as text (empty is `{}`). It can have a `name` for a person, and `contains` and `lacks` lists of substrings. A block without a check is off. |
| `attempts` | 3 | The number of failed passes that end the run. Each failed pass before it gives the model one more turn. 1 gives no other turn. The most is 10. |
| `timeout` | 5m | The time limit of each check. The most is 10m, the time limit of a tool call. |

```yaml
agents:
  fixer:
    done:
      checks:
        - name: the tests pass
          tool: bash
          input: '{"command": "go test ./..."}'
        - name: no stray files
          tool: bash
          input: '{"command": "git status --porcelain"}'
          lacks: ["??"]
      attempts: 3
```

A check passes on four conditions. Its call runs. Its result is not an error. Its exit code is zero, for a tool that reports one, such as `bash`. Its text holds every string of `contains` and no string of `lacks`.

The tool name `answer` is reserved (`fw.DoneAnswer`). A check with that tool makes no tool call, also when the agent has a tool of that name. It tests the final answer of the run: the answer must not be empty, and it must hold every string of `contains` and no string of `lacks`. It takes no input. The answer is the last answer of the model, the rendered answer of a live routine (section 22), or the final report of a run that stopped at a limit (section 24.2). A job that only reads, such as a status check, changes nothing, so a check of a call shows only what was there before the run. Its product is its answer, and a check of the answer tests that product:

```yaml
agents:
  stock-reader:
    done:
      checks:
        - name: the answer names the stock
          tool: answer
          contains: ["pens="]
          lacks: ["could not"]
```

A check is weak when it names no `contains` and no `lacks`, and either it only reads a file or a page by its path, or it checks the answer (`DoneCheck.Weak`). Such a check passes for any path that exists, also a file that was there before the run. "Only reads a path" means that the input names a path (the key `path`, `file`, `file_path`, `filePath`, or `filename`), and either its `command` is `view` or `read`, as for `files` and `memory`, or the first or the last word of the tool name is `read` or `view`, as for `read_file` or `front.pages_read` (`fw.ReadsPath`). The profile editor of the workbench warns a person who saves a weak check, with the same rule.

The kernel resolves the block once, at the queue of the run. It records the block with its defaults in the field `done` of `fw.run.queued`. A run keeps its definition of done for its life, as it keeps its outcome (section 20). A child run never takes the block of its named profile, as for an outcome. An inline profile adds its checks to the checks of the named profile and removes none. So a request cannot weaken the definition of done of the owner of an agent. A process that hosts another agent program (section 19) refuses the block, because the checks need the turn loop.

### 24.2 A pass

When the model ends a turn with its answer, the kernel closes the turn. Then it runs every check once, in order, with no model. The calls go through the tool path of the routine runner. So the policy, the limits of each tool, and the ledger treat them as calls of the model. Their call ids start with `done_`, and they never enter the history of the model. A check never asks a person: a call that the policy asks for fails the check, as in a check run (section 23).

The checks have a tool budget of their own: at most 8 calls a pass, one per check, each within the `timeout` of the block. The limits of tool calls, tokens, and cost of the run and of its tree (`budget.maxToolCalls`, `maxTokens`, `maxCostUSD`) neither refuse a check nor count it, because a check calls no model. The usage of the run (`usage.toolCalls`) leaves the calls of the checks out. So a run that used its tool calls still has its work checked, and a failed pass takes no call from the turn that fixes the work. Only the deadline of the run, the limit of a person's time, stops a check.

A run that stops at a limit of turns, tool calls, tokens, or cost runs a pass too, after its final report (section 5.2). That pass has `limit` set to the limit and `answer` 0, and a check of the answer reads the final report, the output of the run. A pass that passed gives the end of the run `"done": "checked"`, and one that failed `"done": "failed"`. The run still fails at its limit, with the stop `budget:<limit>`, and the model gets no other turn. At the deadline, no pass runs.

A comparison run goes without what the agent learned (`fw.knowhow: off`, docs/KNOWHOW.md section 4.6), so it has no `memory` and no `recall` tool. A check of one of these tools is skipped there, not failed. Its result has `"passed": true` and `skipped` with the reason.

Each call of a check gets fresh tool state. The kernel sets `RunInfo.Fresh` on the call. `bash` then runs the command in a new shell in the directory of the run, and it kills that shell after the call. The shell of the run keeps its directory and its variables. So a `cd` or an `export` of the model cannot change a check, and a check cannot change the shell of the model. A check that needs a directory below the directory of the run names it in its command.

The kernel records the pass as `fw.done.checked`:

```json
{"passed": false, "checks": [{"name": "the tests pass", "tool": "bash", "passed": false, "why": "the exit code is 1: ..."}],
 "attempt": 1, "attempts": 3, "answer": 42, "continue": true}
```

- If every check passed, the run goes on to the grader of its outcome, when it has one, and then completes. Its `fw.run.finished` carries `"done": "checked"`.
- A pass that passed is weak when it shows nothing about the work of this run. That is so in two cases. No check of the pass that ran is strong: each one is weak or was skipped. Or the run changed nothing: it made no call of its own that ran without an error result to a tool that is not read-only, that is not a view or a read of a path, and that is not `memory` or `recall`, and no such call changed a file of the workspace. Then each check of a call saw only what was there before the run, such as a draft of an earlier run. A strong check of the answer is the exception: a pass with one is never weak, also in a run that changed nothing, because the answer comes from this run. So a job that only reads, with such a check, counts as checked. The pass carries `"weak": true`, and the run completes with `"done": "checked"`, `"doneWeak": true`, and the stop `answered`, not `checked`. So it lands with the runs that a person checks. A consumer counts a run as checked with `fw.CheckedDone(done, doneWeak)`.
- If a check failed and attempts are left, the model gets a message from the kernel with the note kind `done`. The message names each failed check with the head and the tail of its result. The model gets one more turn to fix the work.
- If a check failed at the last attempt, or the budget leaves no room for another turn, the run fails. Its output is the last answer of the model, its error starts with `not done:` and names the failed checks, and its `fw.run.finished` carries `"done": "failed"`.

The checks run before the grader, because a check is cheap and certain, and a grader is neither. A follow-up of a person starts the attempts again.

### 24.3 What a consumer sees

The run object has `done`: the state `checked`, `fixing`, or `failed`, `weak` for a weak pass, and the attempt and the results of the latest pass. The field `done` of `fw.run.finished` carries the state too, with `doneWeak`, beside the stop `checked`, `answered` for a weak pass, `not-done`, or the stop of a limit after a pass at that limit (section 25). Checked work is `fw.CheckedDone(state, weak)`: the state `checked` and a pass that is not weak. `fw_done_passes_total` counts the passes by agent and result: `passed`, `weak`, `retried`, or `failed`. The front of a fleet reads the lifecycle events of the egress stream. So it tells a checked result from a run whose model stopped, with no change to the egress types of a pool. The workbench also puts `fw.done.checked` on the egress of its pools, so its view and its archive of the runs show which check ran.

### 24.4 Restarts

Every step reads the ledger. A restart that cut a pass off runs the whole pass again. So a check must be safe to run twice, as a test or a read is. The author of the profile writes the checks, not the model, so the kernel does not ask before it runs one again. A restart between a failed pass and its message gives the message once. A restart between the last failed pass and the end of the run ends the run at the next step.

## 25. Typed reasons of waits and stops

A flow view counts where work waits and why it stops. Before this section, the kernel said most of that in free text: the reason of an approval, the error of a run, and the reason of a suspend. A view therefore had to match strings, such as "rerun after resume" or "budget exceeded:". Two fields now carry the reason in one word. Both ride on events that the egress stream of a fleet carries already (`fleet.EgressTypes` of the workbench). So the change adds no event type and changes no egress list. The code is `fw/reasons.go`.

### 25.1 The wait of an approval request

`fw.approval.requested` carries `wait`:

| Value | The run waits for |
|---|---|
| `approval` | a person to allow or deny a tool call that the policy asks about |
| `question` | a person to answer a question of a tool, with a form |
| `connect` | a person to connect a service, so a tool can act as that person (section 7.11) |
| `rerun` | a person to decide whether a call that a restart cut off runs again (section 4.5) |

The pending approval of the run object carries the same `wait`.

### 25.2 The stop of a run

`fw.run.finished` and `fw.run.suspended` carry `stop`. The run object carries it too, while the run is finished or suspended. A run that works or waits has none, and a follow-up or a resume clears it.

| Value | The run stopped because |
|---|---|
| `answered` | the model ended its turn with an answer, or a live routine or a check run gave the output, or the checks of the definition of done passed with a weak pass (section 24.2) |
| `checked` | the model answered, and every check of the definition of done passed, with a pass that is not weak (section 24) |
| `not-done` | the checks of the definition of done failed at the last attempt |
| `budget:<limit>` | the run reached a limit of its budget, such as `budget:maxTurns` or `budget:maxCostUSD` |
| `loop` | the model repeated the same call, or the same cycle of calls, up to `budget.maxRepeats` |
| `refusal` | the model refused the task |
| `max-tokens` | the model stopped at its output limit again after a continuation |
| `model-error` | the call of the model failed after the retries of its adapter |
| `canceled` | a person or a process canceled the run |
| `interrupted` | the process stopped while the run worked: a shutdown suspended it, or a restart cut off a call that cannot run twice |
| `error` | the kernel failed the run for another reason, such as a check run whose step failed |

The limit `maxRepeats` is a loop, so its stop is `loop` and not `budget:maxRepeats`. `fw.StopLimit` returns the limit of a `budget:` stop.

### 25.3 Old ledgers

An event of an older kernel has neither field, and it still loads. `fw.WaitOf` and `fw.StopOf` derive the reason of such an event from its other fields. For an approval request, these are the connect request, the form schema, and the reason. For an end, these are the outcome, the definition of done, and the start of the error. The projection uses them, so the run object of an old run has the same fields as a new one. The kernel also sets each field on a new event that a site or a linked runner (section 19) left empty.

### 25.4 What a flow view adds

A count of the waits by `wait` and of the stops by `stop` answers where work waits on people and which limits stop it. The time of a wait is the time from `fw.approval.requested` to `fw.approval.decided`. Two waits stay outside these fields. A wait for a free slot shows as the state `queued`. A wait for a rate limit of a provider stays inside the adapter, with no event.
