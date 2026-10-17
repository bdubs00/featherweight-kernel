# Featherweight Kernel architecture

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

Work enters through a surface or the bus and lands in a mailbox. A mailbox is a queue with one consumer goroutine per run. With the `mem` bus, it is an in-process channel. When several replicas share a ledger, it is a bus subject. That goroutine is the single writer for the run. It calls the model, runs tools, and appends events to the ledger. Projections subscribe to the ledger and never write to it. Surfaces read projections and write to mailboxes. The workbench adds the MCP server as one more surface (the workbench architecture, section 3).

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
| `fw.run.queued` | agent, input, instructions, inline profile, budget, metadata, origin, principal and its kind, parent, depth, file references (section 7.10), extra tools (section 12.5) |
| `fw.run.started` | model, tool names, brief (the sections that tools add to the system prompt), time zone |
| `fw.run.titled` | title, usage, provider, model (section 8.2) |
| `fw.message.received` | role, content, origin (`http`, `a2a`, `mcp`, `bus`, `cli`, `kernel`), note kind |
| `fw.turn.started` | turn number |
| `fw.model.requested` | provider, model, effort, estimated input tokens |
| `fw.model.responded` | content blocks, raw provider blocks, stop reason, usage, latency |
| `fw.tool.requested` | tool, call id, input |
| `fw.tool.responded` | call id, output, `is_error`, latency |
| `fw.tool.progress` | call id, progress, total, message |
| `fw.file.changed` | call id, tool, operation, path, old path, size, SHA-256 (section 7.9) |
| `fw.approval.requested` | approval id, tool, call id, input, reason, form schema of a question, connect request (section 7.11) |
| `fw.approval.decided` | approval id, decision, decider, note |
| `fw.context.compacted` | epoch, summary block, kept messages, usage of the summary calls |
| `fw.budget.exceeded` | limit name, value |
| `fw.loop.detected` | tool, count, period, action (`warned`, `failed`) |
| `fw.error` | message, retryable |
| `fw.turn.finished` | turn number |
| `fw.run.suspended` | reason |
| `fw.run.resumed` | from seq |
| `fw.run.finished` | outcome (`completed`, `failed`, `canceled`), output, error |

The raw provider blocks in `fw.model.responded` are stored unchanged. They include thinking blocks. The kernel sends them back to the same provider on the next turn, because the provider requires the blocks it produced.

Before the kernel appends an event, it replaces each secret in the data with `[redacted]`. A secret is one of these:

- a value that `fw` resolved through `Secrets`, such as a model API key or `FW_API_TOKEN`
- the value of a variable with a secret name, such as `OPENAI_API_KEY`, `GITHUB_TOKEN`, or `DB_PASSWORD`
- text with the shape of a secret: a PEM private key, a provider or GitHub token, an AWS access key, a JWT, a bearer token, or the user information of a URL

The model builds its history from the stored events, so the model does not see the secret either. The raw provider blocks stay unchanged, because the provider signs them. The bytes of an image or a document also stay unchanged. A value of fewer than eight characters is not replaced. `a.Redact` applies the same rules to the text of a log line.

### 4.3 Storage

The default ledger driver writes one file per run at `$FW_HOME/runs/<run>/ledger.jsonl`. JSONL is one JSON object per line. The driver appends and never seeks back. It calls `fsync` after every approval event and after every terminal event. An index file at `$FW_HOME/runs/index.jsonl` lists each run and its last known state.

When `/workspace` exists, `$FW_HOME` defaults to `/workspace/.fw`. Otherwise it defaults to `./.fw`. Under ax, `/workspace` is the durable volume that survives suspend and resume. Alternative drivers write the same events to a NATS JetStream stream, to a Redis stream, or to SQLite.

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

On start, `fw` reads the index and finds every run whose last event is not terminal. For each such run, it rebuilds the message history and appends `fw.run.resumed`. Then it continues from the last complete point:

1. If the last event is `fw.model.requested`, send the request again.
2. If the last event is `fw.tool.requested` and the tool is idempotent, run the tool again.
3. If the last event is `fw.tool.requested` and the tool is not idempotent, append `fw.approval.requested` and wait.
4. If the last event is `fw.approval.requested`, wait for the decision.

A call that waited for a connection is an exception to step 3. The service never saw such a call, so the call runs again (section 7.11).

### 4.6 Replay

The command `fw replay <ledger> --model <selector>` runs a recorded run again against a different model. It answers every tool call from the recorded `fw.tool.responded` events instead of running the tool. It uses the brief of the recorded run (section 7.8), so the replay sees the memory index that the recorded run saw. The result is a second ledger. A diff of the two ledgers is a regression test for a model change or a prompt change. This covers the common evaluation case with no extra framework.

## 5. The kernel

### 5.1 Run and turn

A run is one conversation with one agent profile, one budget, and one ledger. A turn is one model call and the tool calls that it requests. The loop for a run is:

1. Build the request from the profile system prompt, the tool specifications, and the message history.
2. Call the model with streaming. Append `fw.model.requested` and `fw.model.responded`.
3. If the stop reason is `end_turn`, append `fw.run.finished` with the final text and stop.
4. If the stop reason is `tool_use`, ask the policy about each call (section 7.5).
5. Run the allowed calls. Run parallel-safe calls at the same time. Append one `fw.tool.responded` per call.
6. Append all tool results in one user message. Go to step 1.

Other stop reasons end the turn as follows. `max_tokens` appends `fw.error` and retries once with a continuation message. `refusal` appends `fw.run.finished` with outcome `failed` and the refusal category. `pause_turn` goes to step 1 at once.

### 5.2 Budget

A budget has seven limits. Four bound the size of a run, and three bound its shape.

| Limit | Default | What it stops |
|---|---|---|
| `maxTurns` | 200 | A run that calls the model again and again |
| `maxTokens` | 2,000,000 | A run that reads more than the context can hold |
| `maxCostUSD` | 0 (off) | Spend above a price. It needs a price table. |
| `deadline` | 2h | A run that works too long. It counts working time, not the time that the run waits (section 5.8). |
| `maxToolCalls` | 1000 | A run that calls tools without end |
| `maxCallsPerTurn` | 32 | One response that fans out into too many calls |
| `maxRepeats` | 5 | The same call, or the same cycle of calls, again and again (section 5.7) |

The kernel compares usage with the budget before every model call. When a limit is reached, it appends `fw.budget.exceeded` and finishes the run with outcome `failed`. A call past `maxCallsPerTurn` or `maxToolCalls` does not run. The model reads an error result that names the limit, and the run ends at the next model call. Cost needs a price table. The price table is a plug with an empty default, so `maxCostUSD` is off until you supply prices.

A run takes its budget from the request, then from the profile, then from `budget:` in `fw.yaml`. A zero field means no limit for that field. A child run (section 11.9) is capped by the budget of its parent. If a child or any of its ancestors uses up its working time, the child stops.

### 5.3 Approvals

A tool call that the policy marks `Ask` moves the run to the state `input-required`. The kernel appends `fw.approval.requested`. Every surface shows the pending approval, and every surface accepts a decision. A decision appends `fw.approval.decided`, and the run continues. Approvals survive a restart and a suspend because they are ledger events.

### 5.4 Context management

Long runs outgrow the context window of the model. The `Compactor` interface handles this, and one path serves every provider: the default compactor asks the model of the run for a summary. No adapter uses the compaction of a provider, such as the server-side compaction of Anthropic. Such a compaction fits behind the same interface as a plug of the future.

Before each model call, the kernel estimates the size of the context. It starts from the usage that the provider reported for the latest response of the epoch: input, cache read, cache write, and output tokens. It adds an estimate for the messages after that response, about one token per four bytes of text and 1,600 tokens per image. Without a reported usage, the estimate covers the system prompt, the tool specifications, and the whole history.

When the estimate reaches `compaction.at`, the kernel compacts before the call. The default is 150,000 tokens.

A model call can also fail because the request does not fit the context window. Then the kernel compacts and calls the model once more in the same turn. A second overflow in the same turn fails the run. The kernel recognizes an overflow from `fw.ErrContextOverflow`, or from the error text of Anthropic, OpenAI, Gemini, vLLM, llama.cpp, and tabbyAPI. A local server with a small window therefore needs no setting. `compaction.at: 0` turns compaction off in both cases.

The default compactor uses simple compaction. It sends the history of the run to the model with the same system prompt, tools, and effort as a turn. It adds one user message. That message asks for a summary inside `<summary>` tags and forbids tool calls. The prefix of the call is the prefix of a turn. The call therefore reads the prompt cache, and the thinking blocks in the history stay valid.

The summary replaces the whole history. The new epoch starts with one user message that holds the summary. The kernel replays no earlier response, no tool call, and no thinking block. The reason is that a provider binds a thinking block to the history before it. A kept tail of old turns therefore fails after a summary. Anthropic rejects such a request with a 400 for accounts created on or after 2026-08-31. The messages from the caller at the end of the history, such as a new question, carry no provider data. They follow the summary word for word.

When the summary call itself does not fit, the compactor summarizes the history up to the last assistant message. That part was the history of an earlier call that fit. The compactor appends the remaining messages after the summary as plain text, and cuts each block to 4,000 characters. `compaction.model` sends the summary call to another model, which does not read the prompt cache of the run. A message that arrives during the summary call follows the summary in the new epoch.

Compaction appends `fw.context.compacted` with a new epoch number, the summary block, the messages that the compactor kept, and the token usage of the summary calls. The usage, the cost, and the budget of the run count that usage. The `messages` projection starts from the latest epoch. The system prompt is not part of the history, so the profile prompt, the skills list, and the memory index (section 7.8) survive every compaction. The ledger before the epoch is unchanged, so rule 5 holds.

### 5.5 Follow-up messages

A run accepts more messages after it finishes or while it waits for input. A follow-up appends `fw.message.received` and starts a new turn on the same run. The mailbox serializes follow-ups, so a run never has two writers.

### 5.6 Process duties

The kernel runs one goroutine per active run, up to `FW_MAX_RUNS` (default 16). On `SIGTERM`, it stops accepting work and waits up to 8 seconds for in-flight model calls. Then it appends `fw.run.suspended` to every active run, flushes the ledger, and exits. Under ax, `fw` is PID 1. A PID 1 process must reap child processes, so `fw` handles `SIGCHLD` and waits on orphaned children. A suspend under ax sends no `SIGTERM` (section 11.5). Only a stop of the worker pod itself starts this path.

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

The second sentence appears only for a run with a deadline. The first model call of a run gets no note, because the system prompt gives the date. One rule covers a follow-up on the next day, a late approval, a slow tool, and a restart after a suspend. The note is a `fw.message.received` event with the role `system`, the origin `kernel`, and the note kind `time`. The warning of the loop guard (section 5.7) is the same kind of event with the note kind `loop`. The note is an event and not text that a projection adds. A history that changes after the fact breaks the prompt cache, and a provider then rejects the thinking blocks of a resumed run (section 5.4).

The deadline of a budget counts working time, and `fw.WorkingTime` computes it from the ledger. A run works from its input, a follow-up, an approval decision, or a resume. If it finishes, waits for a decision or an answer, or is suspended, it stops working. The time that a run waits for a person, for a follow-up, or in a suspended sandbox therefore does not count.

A gap with no event before `fw.run.resumed` means that the process stopped without a word. An example is a sandbox that ax killed on suspend (section 11.5). The working period ends at the last event before the gap.

## 6. Models

### 6.1 Interface

```go
type Model interface {
    Generate(ctx context.Context, req *Request) (*Response, error)
    Stream(ctx context.Context, req *Request, on func(Delta)) (*Response, error)
}
```

`Request` carries the system prompt, the messages, the tool specifications, the effort level, and the output token limit. Messages use a small provider-neutral block set: text, image, document, tool use, tool result, and opaque. The opaque block carries provider data such as thinking blocks without interpretation.

### 6.2 Adapters

| Selector prefix | Library | Default model |
|---|---|---|
| `anthropic/` | `github.com/anthropics/anthropic-sdk-go` | `claude-opus-5-5` |
| `gemini/` | `google.golang.org/genai` | `gemini-3.8-flash` |
| `openai/` | OpenAI-compatible chat completions over HTTP | none |

A model selector is the string `provider/model`, for example `anthropic/claude-opus-5-5`. The default selector is `anthropic/claude-opus-5-5`. The `gemini` adapter exists because ax injects `GEMINI_API_KEY` and the ax `Model` resource defaults to Google. The `openai` adapter exists for OpenAI itself, for proxies, and for local servers such as vLLM and Ollama.

Each provider has a connection block under `providers.<name>` in `fw.yaml`, and every field of it has an `FW_PROVIDERS_<NAME>_*` variable (section 12.1):

| Field | Meaning |
|---|---|
| `baseUrl` | The endpoint of the provider. A local server or a proxy needs no code change. |
| `apiKeyEnv` | The secret that holds the API key, for a key under another name than the default |
| `headers` | Extra headers on every request. A value that names a secret is replaced by the secret. |
| `timeout` | The limit of one call, retries included. |
| `tls` | A CA bundle, a client certificate and key, or `insecure` for development. |
| `options` | Options that one adapter reads. Section 6.6 lists the OpenAI options. |

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

The adapter speaks the chat completions API over `net/http` with no SDK, so one adapter serves OpenAI, Azure OpenAI, OpenRouter, LiteLLM, vLLM, Ollama, and tabbyAPI. Every call streams. The adapter retries a 429, a 5xx, and a network error that happens before the first response byte. It waits as `Retry-After` names, or with an exponential backoff. It never retries once a stream started. A stream with no byte for `idleTimeout` fails, and a response larger than 16 MB fails. A 401 or a 403 names the secret that the key comes from. A 404 that says the model is unknown lists the models that the server offers. The method `Models` lists them for `fw doctor`.

`providers.openai.options` holds these keys:

| Option | Values | Default |
|---|---|---|
| `reasoning` | `auto`, `on`, `off`: send the effort as `reasoning_effort` | `auto`: on for o-series and GPT-5 names |
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
| `memory` | Read and write notes in files under `$FW_HOME/memory`, one directory per agent profile (section 7.8) | idempotent |
| `skill` | List the skills, or read one skill with its files | read-only, idempotent, parallel-safe |
| `clock` | Give the time in the zone of the run and the working time left before the deadline (section 5.8) | read-only, idempotent, parallel-safe |
| `ask_user` | Ask the caller a question and wait for the answer | read-only |
| `spawn_task` | Start a child run with its own task, prompt, tools, and budget | parallel-safe |
| `await_task` | Wait for a child run and return its output | read-only, parallel-safe |
| `send` | Publish a message to a bus subject | none |

The workbench adds the built-in tool `propose_agent` (the workbench architecture, section 6.4).

Four built-in tools set `Action`:

| Tool | Action |
|---|---|
| `bash` | Run command |
| `http` | Send request |
| `spawn_task` | Hand off task |
| `send` | Send message |

The other built-in tools have no label. `files` and `memory` read, write, and delete, so no one label fits them. `ask_user` asks a question and does not act. `clock`, `skill`, and `await_task` change nothing, so a person seldom allows their calls.

The `files` tool never opens a path under the paths that `Config.Reserved` lists: the home directory of `fw`, the directory of the file ledger, and the configuration file. The check uses the real path, after symbolic links. A change there changes what `fw` allows at its next start. For example, a profile file under `$FW_HOME/agents` can grant its agent access and read secrets from the environment through `${VAR}`. Every other path under the root needs no approval, except the paths that the rules of the workspace refuse (section 7.9).

When the provider is Anthropic, `bash`, `files`, and `memory` use the Anthropic-defined tool shapes `bash_20250124`, `text_editor_20250728`, and `memory_20250818`. Other providers get the same tools with a plain JSON schema. Each profile lists the tools that it allows. The `bash` shell never inherits `FW_API_TOKEN` or a model API key, and `FW_BASH_HIDE` names more variables to hide. The shell starts with `FW_HOME` and with the variables of `tools.bash.env`. A script inside the sandbox therefore reads an endpoint from its environment instead of from a literal in a prompt. `tools.bash.timeout` bounds one command, 120 seconds by default. A tool that implements `io.Closer` is closed at shutdown, which ends the shell.

### 7.3 MCP client over HTTP and HTTPS

MCP is the Model Context Protocol. The revision used here is 2026-07-28, in which every request is self-contained and there is no session. `fw` is an MCP client for every server under `tools.mcp` in `fw.yaml` and for every server in the ax Workspace field `spec.mcp.servers`. An entry with `endpoint` uses the Streamable HTTP transport. An entry with `command` and `args` uses the stdio transport. The client library is `github.com/modelcontextprotocol/go-sdk`.

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
| `trusted` | Treat the tool annotations and titles of this server as true. A title becomes the label of the button that allows a call. | `false` |

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

The default policy allows read-only tools, asks before destructive tools, and denies nothing. Rules in `fw.yaml` change this per tool name, per flag, or per profile. A profile policy combines with the global policy, and the stricter verdict wins, so a profile can tighten the rules but not loosen them. A child run also carries the rules of every ancestor. With `policy.external`, a control plane outside the process decides every tool call in place of the rules (section 13.5). The policy of a profile and the rules of the ancestors can still only make its verdict stricter. A security team therefore changes the policy without a rebuild.

A rule is one of four forms:

| Form | Example | Matches |
|---|---|---|
| a flag | `destructive` | every tool with that flag |
| a tool glob | `git.*` | the tool name |
| a tool and a command pattern | `bash: git push*` | the `command` field of the input |
| a tool, a field, and a pattern | `files: path=*.env` | that string field of the input |

In a command or field pattern, `*` matches any text, slashes included, and `?` matches one character. If the input holds that field, the field form applies. Otherwise the text matches the command, so `bash: FOO=bar*` still matches a command. With the field form, a deployment restricts what a tool call can touch: a path, a URL, a subject, a method.

A tool can resolve its input before the policy decides (`fw.Resolver`). The `files` tool gives the real path of the file, relative to its root, after `./`, an absolute path, and symbolic links. A deny or an ask rule matches the path of the model or the resolved path. An allow rule matches only the resolved path, so `docs/../secrets/a` does not pass `allow: files: path=docs/*`. `policy.external` gets the resolved input in the field `resolved`.

### 7.6 Many tools

A large tool list costs tokens, and a changed tool list breaks the prompt cache. When more than 24 tools are loaded, the Anthropic adapter registers the tool search tool and marks the rest as deferred. The model then loads the schemas it needs. A tool source that changes at runtime appends tools instead of replacing the list, so the cached prefix stays valid.

### 7.7 Tool limits

Every tool call goes through the limits middleware, `fw.LimitTools`. The configuration `tools.limits` sets the defaults, and `tools.limits.perTool` overrides them per tool name or glob.

| Limit | Default | Effect |
|---|---|---|
| `maxInputBytes` | 256 KB | A larger input does not run. The model reads an error that names the limit. |
| `maxOutputBytes` | 100 KB | A larger result keeps its head and its tail. A note at the cut says how many bytes are gone. |
| `timeout` | 10m | A call that runs longer gets an error result and its context ends |

The limits apply to the built-in tools, to MCP tools, to A2A peers, and to tools passed as Go options alike. The reason is that the middleware wraps every tool outermost. The kernel records the bounded result in the ledger, so the history that the next model call sends is bounded too. An image or a document in a result counts with its size. If it does not fit, the kernel drops it with a note. The cut keeps two thirds of the room for the head and one third for the tail. The start of a listing and the end of a log therefore both survive.

### 7.8 Memory

The `memory` tool keeps notes between runs in files. The model addresses them as `/memories/...`. With `tools.memory.scope: agent`, the default, each agent profile has its own directory, `$FW_HOME/memory/agents/<agent>`. With `tools.memory.scope: shared`, every profile uses `$FW_HOME/memory/shared`. A child run uses the directory of its own profile.

At the start of a run, the memory tool adds a section to the system prompt. The section lists each file with its path and its first line, newest first, up to 40 files and about 4 KB. A `description:` field in front matter takes the place of the first line. The section also tells the model what to save: a user preference, a decision and its reason, or a fact that took effort to find. It tells the model to start each file with one line that says what it holds, and never to store a secret. The section looks like this:

```
# Memory

The memory tool keeps notes between runs under /memories. Each line below names a file and its first line, newest first. View a file when its line matters for the task.

- /memories/build.md: How to build and test this repository
- /memories/user/preferences.md: The user wants short answers

Save what a later run needs and cannot find in the workspace: a preference of the user, a decision and its reason, a fact that took effort to find. Start each file with one line that says what it holds, because that line appears here at the start of every run. Keep one topic in each file. Update a file instead of adding a second one, and delete a note that turns out to be wrong. Never store a secret, a key, or a password.
```

A general mechanism carries the section. When the run starts, a tool that implements `fw.Briefer` adds a section to the system prompt. The context of the call carries the agent and the run. The `clock` tool uses the same mechanism for the date (section 5.8). The kernel records the text in `fw.run.started`, in the field `brief`. The prompt therefore stays the same for the life of the run, and its cached prefix stays valid. A resume and a replay see the same prompt. A note that the model writes during a run appears in the index of the next run.

### 7.9 Workspace

The workspace is the directory that the `files` tool and `bash` work in. It is `tools.files.root`, else the first binding under ax, else the working directory. The kernel owns the workspace as an `fw.Files`. A file tool gets it from the context of its call, so the kernel checks each name and records each change.

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

A name follows `io/fs`: slash-separated, relative, and clean. `DirFiles` does each operation through `os.Root`, so no name leaves the directory, through a symbolic link either. The `memory` tool uses `DirFiles` on its own directory under `$FW_HOME`, outside the workspace.

The kernel checks the real name of each operation, after symbolic links, against these rules:

| Rule | Key | Default |
|---|---|---|
| Never read or written: the home directory of `fw`, the file ledger, and the configuration file (`Config.Reserved`) | none | always |
| Never read or written | `tools.files.deny` | `.env`, `.env.*`, `*.pem`, `*.key`, `.netrc`, `**/.git/config` |
| Read, never written | `tools.files.readOnly` | `.git` |
| Largest file that is read or written | `tools.files.maxFileBytes` | 8 MiB |

A pattern without a slash matches one part of the path at any depth, as in `.gitignore`. For example, `.git` matches the directory `.git` and every path under it, and `*.pem` matches `certs/web.pem`. A pattern with a slash matches from the root, and `**` matches any number of directories. A list in `fw.yaml` replaces the default list. The variables are `FW_TOOLS_FILES_DENY` and `FW_TOOLS_FILES_READ_ONLY`, with commas between the patterns, and `FW_TOOLS_FILES_MAX_FILE_BYTES`. A listing leaves out each path that the rules refuse.

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

An attachment is a file that a person sends with a message, such as a screenshot, a log, or a spreadsheet. The kernel keeps the bytes in the workspace under `attachments/`, and the ledger holds a reference. A replay, a bus message, or a projection therefore never carries the bytes.

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

A grant is the permission that a person gives `fw` at one authorization server. In OAuth terms, it is a refresh token (RFC 6749, section 6), with its scopes and its client. `fw` keeps one grant per person, per authorization server, and per client. With a grant, a run goes on for hours or days after the access token of the person expired. The grant also survives a restart of the process.

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
| `grants.store` | `FW_GRANTS_STORE` | `file`, `memory`, or a registered driver | `file` |
| `grants.dir` | `FW_GRANTS_DIR` | The directory of the file store | `<home>/grants` |
| `grants.key` | `FW_GRANTS_KEY` | The `Secrets` name of the key that encrypts the file store | `FW_GRANT_KEY` |

An entry with `grant: user` needs an HTTP endpoint. `auth.mode` must include `oidc`, because only a person with an OpenID Connect identity can connect. An entry without a client of its own also needs `auth.oidc.clientId`. With the browser sign-in of the workbench on (the workbench architecture, section 5), a person who opens a connect link signs in first. Without it, that person pastes a token first. The `connections` line of `fw doctor` reports each of these conditions.

#### Two kinds of service

A service can trust the sign-in issuer: its authorization server is the issuer of `auth.oidc`. Such an entry needs no client of its own. It uses the client of the browser sign-in, and the person connects once for all such entries.

A service can also have an authorization server of its own, such as a hosted product. The entry then names its client with `clientId` and `clientSecret`. If `issuer` is empty, `fw` reads the protected resource metadata of the MCP server (RFC 9728) to find the authorization server. Then it reads the metadata of that server (RFC 8414, or OpenID Connect discovery). The person connects each such service on its own.

Each authorization server registers one more redirect address: `/ui/connect/callback` on the public address of `fw` (the workbench architecture, section 2.1).

#### How a person connects

The kernel pauses the call, keeps the grant, and resumes the call. The workbench serves the Connect card, the routes under `/v1/connections`, and the callback `/ui/connect/callback` (the workbench architecture, sections 2.1, 3.4, and 6.2). A program that embeds the kernel without the workbench calls `BeginConnect` and `FinishConnect` itself (section 12.8). The flow with the workbench has these steps:

1. A call needs a grant that the person does not have, or the server refused the grant. The call pauses. The kernel appends `fw.approval.requested` with `connect`: the server, its title, and the reason, `missing` or `expired`. The run moves to `input-required`.
2. Every surface shows the request. The dashboard shows a card with Connect and Skip in the run, and a row under Needs you on the home. An MCP client gets a URL-mode elicitation (the workbench architecture, section 3.4).
3. Connect opens `/ui#connect/<server>/<run>`. If the person is not signed in, the page signs the person in first.
4. The page calls `POST /v1/connections/<server>` and sends the browser to the authorization request in the answer. The request uses the authorization code flow with PKCE, a `state`, the scopes, and the `resource` of the MCP server (RFC 8707).
5. The authorization server sends the browser to `GET /ui/connect/callback`. `fw` exchanges the code and stores the grant. Then it resumes every paused call of the person for every service that is now connected.
6. The browser returns to the page, which says Connected and offers the run.

The answer of the code exchange must hold a refresh token. Without one, the connection fails, and the message asks for offline access. Skip denies the request. The call then fails with a plain message to the model, and the run does not ask again for that service. The kernel refuses a decision that allows a connect request, because a click is not a grant. Only the end of a connection allows it.

#### Tokens for a server

Each call needs an access token of the person for its server. Without `exchange`, `fw` refreshes the grant with the `resource` parameter of the server (RFC 8707). The authorization server then issues a token for that server alone, as the MCP authorization specification requires. With `exchange`, `fw` refreshes the grant once for a token of its own client. Then it exchanges that token for a token with the configured `audience` (RFC 8693). Use `exchange` with an authorization server that ignores `resource`.

Access tokens stay in memory until shortly before they expire. `fw` keeps one MCP session per person and server, because a server can bind a session to the person of its first request. The tool list of a server is shared. Until a person with a grant lists the tools, the server shows one tool, `<server>.connect`. A call of that tool asks the person to connect, and the real tools appear in the next step.

A run without a person gets an error from such a tool, never the identity of the agent. A workload, the static token, a bus intake, and an anonymous caller have no person.

#### Rules that keep a grant alive

Many authorization servers rotate refresh tokens (RFC 9700, section 4.14). Each refresh returns a new refresh token and ends the old one. If an old refresh token comes back, the server cannot tell a thief from the client. It then revokes the grant, and some servers revoke more. `fw` therefore follows four rules with every server:

1. A grant has one holder: the grant store of one process.
2. One refresh of a grant runs at a time, for all the runs of the person.
3. `fw` stores the new refresh token durably before it uses the new access token.
4. If the server refuses a grant, `fw` deletes it, and the next call asks the person to connect again.

A lost grant is safe, because the person connects again. An old copy of a grant is not safe, because its refresh token counts as a reuse.

By default, `fw` asks for the scope `offline_access` (OpenID Connect Core 1.0, section 11). A plain refresh token usually belongs to the browser session of the person. If the person signs out, or if that session reaches its time limit, the refresh token ends. Each refresh from `fw` also keeps that browser session alive. An offline grant has a session of its own, and the person can revoke it at the authorization server. Some servers have no such scope. Google is one: set `offline: false` and give `authParams` the values `access_type: offline` and `prompt: consent`.

#### The grant store

`fw.GrantStore` keeps the grants. Each write of a grant gets a new revision number. A write can name the revision that it replaces, so a shared store can refuse a stale write.

The default store, `file`, keeps one file per grant under `grants.dir`. The file name is a hash, so no subject or issuer appears in a directory listing. AES-256-GCM encrypts each file. The key comes from the secret `FW_GRANT_KEY`. Without that secret, the store creates a random key in the file `grant.key`, with mode 0600. A write goes to a temporary file. `fw` syncs that file, renames it over the old file, and syncs the directory. After the write returns, the grant is therefore whole and durable.

The store `memory` keeps the grants in the process, and a stop of the process forgets them. `fw.RegisterGrantStore` adds a driver, and `fw.WithGrantStore` replaces the store from Go. Tests use `fw.NewMemGrantStore()`.

#### Restarts and ax

A call that waits for a connection survives a restart. The service never saw the call, so the call runs again after the restart, without the question of section 4.5. If the person is still not connected, the call asks again. A skip before the restart still ends the call.

Under ax, the home directory is on the durable volume. A suspend saves that volume and then kills the process (section 11.5). A refresh after the save is lost on resume, so the store holds the old refresh token. A server with rotation then refuses the grant, and the person connects again. A resume from an older snapshot, or a copy of an actor, has the same effect. The person sees one more Connect card. If `fw` has a client of its own, no other grant of the person breaks (see the example below).

#### Example: Keycloak

Keycloak is a common OpenID Connect issuer, and it shows each rule. Give `fw` a client of its own at the issuer, for example `fw`:

- Make it a confidential client with the standard flow. Token exchange needs a confidential client.
- Register `https://fw.example.com/ui/callback` and `https://fw.example.com/ui/connect/callback` as its redirect URIs.
- Keep the client scope `offline_access` on the client. Keycloak assigns it by default.
- For each MCP server with `exchange`, turn on standard token exchange on the `fw` client. Set `exchange.audience` to the client id of the MCP server.

Keycloak keeps a grant with `offline_access` as an offline session. The offline session survives the sign-out of the person. By default, the setting Offline Session Idle ends it after 30 days without a refresh. The person sees it under Applications in the account console and can revoke it there.

At the issuer, the setting Revoke Refresh Token turns rotation on. It is off by default. The setting Refresh Token Max Reuse says how many times an old refresh token still works. With rotation on, a reuse past that number gets `invalid_grant`. Keycloak also detaches the client session from the user session, which ends every refresh token of that client for that person. A dedicated client therefore limits the damage to the grants of `fw`, and the other applications of the person keep their sessions. `fw` assumes rotation with a reuse count of 0, the strictest case, so it works with every setting.

The MCP guide of Keycloak lists Resource Indicators (RFC 8707) as not supported. Keycloak therefore ignores `resource`, and a refreshed token does not name the MCP server in its audience. Use one of two ways:

- Set `exchange.audience` to the MCP server. Keycloak then issues a token for that server alone.
- Add an audience mapper to the `fw` client that names the MCP server. Every token of the client then works at that server. Each server then gets a token that also works at the other servers.

```yaml
auth:
  mode: oidc
  oidc:
    issuer: https://id.example.com/realms/main
    audience: [fw]
    clientId: fw
    clientSecret: FW_OIDC_CLIENT_SECRET
tools:
  mcp:
    - name: tickets
      title: Tickets
      endpoint: https://tickets.example.com/mcp
      auth:
        oauth:
          grant: user
          exchange:
            audience: tickets-mcp
```

#### Security

- No refresh token, access token, code, or PKCE verifier enters a ledger event, a log line, or an error message. The ledger holds the connect request and its decision. The decider is the person, and the note is `connected`.
- The file store encrypts every grant. Keep `FW_GRANT_KEY` in a secret store, apart from the volume of the grants. A key in `grant.key` beside the grants protects only a copy of the grant files that leaves the key behind.
- A cookie binds each connection to the browser that started it. The cookie holds the PKCE verifier, so it is HttpOnly and signed, and it lasts 10 minutes. A link from another person therefore cannot connect their account to your runs.
- For a service that trusts the sign-in issuer, the ID token of the connection must name the person of the run. A browser that is signed in at the issuer as another person cannot connect.
- Disconnect deletes the grant. If the authorization server offers revocation (RFC 7009), `fw` also revokes the grant there.
- In a fleet, a grant lives on the member that handled the callback. A run on another member does not see that grant. A grant store that members share is future work.
- `fw` has no device flow for the command line yet. A person connects in the workbench, in the dashboard or through an MCP client.

## 8. Surfaces

### 8.1 One listener

Every surface mounts on one `http.ServeMux`. The listener speaks HTTP/1.1 and h2c, so gRPC and HTTP share the port. h2c is HTTP/2 without TLS. The routes are:

| Path | Surface | Present |
|---|---|---|
| `/v1/*` | Native API | always |
| `/.well-known/agent-card.json`, `/a2a/*` | A2A server | default on |
| `/metrics` | Prometheus (section 10) | always |
| `/healthz`, `/readyz` | Health | always |
| `/metadata/v1alpha1/ax/*`, `/metadata/v1alpha1/fw/*` | ax metadata | under ax |
| gRPC guest services of `ax ssh` (section 11.12) | ax debug | under ax with `spec.debug: true` |

The workbench mounts more routes on the same listener: `/mcp`, `/ui`, the browser sign-in, and the routes for people (the workbench architecture, section 2).

```go
type Surface interface {
    Mount(mux *http.ServeMux, a *Agent) error
}
```

### 8.2 Native API

The native API is small. Every call returns JSON. Every error is an RFC 9457 problem document.

| Method and path | Purpose |
|---|---|
| `POST /v1/runs` | Create a run. Body fields: `agent`, `input`, `title`, `instructions`, `profile`, `budget`, `metadata`, `wait`, `files` (section 7.10). |
| `GET /v1/runs` | List runs. `?parent={id}` lists the children of a run. |
| `GET /v1/runs/{id}` | Get one run. |
| `GET /v1/runs/{id}/events` | Stream the ledger as server-sent events. `Last-Event-ID` resumes from a sequence number. Under `Accept: application/json`, one page of events with `lines`, the short line of each event, as the MCP tool `run_events` of the workbench gives. |
| `POST /v1/runs/{id}/messages` | Send a follow-up message. Body fields: `content`, `files`. |
| `POST /v1/runs/{id}/approvals/{approval}` | Decide a pending approval. Body fields: `decision`, `note`. |
| `POST /v1/runs/{id}/cancel` | Cancel a run. |
| `GET /v1/runs/{id}/files` | List the files that the run changed (section 7.9). |
| `GET /v1/runs/{id}/files/{path}` | The content of a file that the run changed, or a view of it under `Accept: application/json`. `?download=1` always returns the content (section 7.9). |
| `GET /v1/agents` | List agent profiles with their descriptions, instructions, and skills. |
| `GET /v1/tools` | List tools with their specifications. Each entry also has `text`, a plain phrase that says what the tool does. |
| `GET /v1/skills` | List skills. |
| `GET /v1/skills/{name}` | The `SKILL.md` text, or JSON with the file list under `Accept: application/json`. |

`profile` is an inline profile (section 12.5). Its fields are `system`, `instructions`, `skills`, `tools`, `model`, `effort`, `policy`, and `budget`. `budget`, at the top level and inside the profile, takes the seven limits of section 5.2. Examples of a request that the kernel refuses are an unknown model name and a child past the spawn limits. Such a request gets a 400 or a 409 problem document that names the field.

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

The run states are `queued`, `running`, `input-required`, `completed`, `failed`, `canceled`, and `suspended`. Authentication defaults to a bearer token read from `FW_API_TOKEN`. The setting `auth.mode: none` must be explicit. The modes `oidc` and `spiffe` give every caller an identity, and section 13 says what an identity can do. The native API also serves `GET /v1/whoami` and `GET`, `PUT`, and `DELETE /v1/agents/{name}`.

An extension adds routes to this surface (section 12.8). The workbench adds the page, the browser sign-in, home, results, transcripts, the builder, the fleet, usage, and connections (the workbench architecture, section 2).

#### Run titles

`title` names the work in a few words, so a list reads "Fix checkout retries" and not only "reviewer". The metadata key `fw.title` (`fw.MetaTitle`) sets it. The field `title` of `POST /v1/runs` and the argument `title` of the MCP tool `run` of the workbench put a trimmed title into that key. Such a title wins over a `fw.title` entry in `metadata`. A title of more than 200 characters is an invalid request. The MCP tool `run` sends the title to another fleet member in `metadata`. A member of any version therefore keeps it.

Without the key, the run takes `fw.TitleFrom` of its input. That is the first sentence of the first line with text, without Markdown marks, cut at a word to 64 characters (`fw.MaxTitleRunes`). An input without text gives no title. The `status` projection computes this title from `fw.run.queued`, so a run in an older ledger gets a title too.

`titles`, or `FW_TITLES`, sets how a run gets its title. The value `input`, the default of the kernel, keeps the title of the input and makes no extra call. The workbench sets `model` as its default (the workbench architecture, section 1.1). With `model`, a top-level run that starts gets a title of a few words from a model. The kernel makes this call beside the first turn. The call goes to `compaction.model`, else to the model of the run. `fw.CleanTitle` turns the answer into one line of at most 64 characters. The kernel records it as `fw.run.titled` with the title, the usage, the provider, and the model of the call. A program that embeds fw sets the value with the option `fw.WithTitles`. Under `model`, a test with a scripted model uses `fw.WithTitles(fw.TitlesInput)`, because the title call takes an answer of the script.

The usage of the title call counts in the usage, the cost, and the budget of the run, and in `fw_model_tokens_total`. A title that the request sets wins over a written title. A child run, a replay, and a run without text in its input keep the title of the input. A title call that fails changes nothing, so the run keeps the title of its input. A shutdown cancels a title call and never waits for it. The title of a short run can arrive after `fw.run.finished`. It changes only the title and the usage, and the metrics and the traces never read it as a run that opened again.

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

`GET /healthz` returns 200 once the process is up. `GET /readyz` returns 503 until the workspaces are prepared, then 200. `GET /metrics` serves the Prometheus registry (section 10.1). The route carries no authentication, because a scraper has no token. Keep it inside the monitoring perimeter. Under ax, `/readyz` drives the `WorkspaceReady` condition.

### 8.6 Fleet

A fleet is a set of `fw` processes that share one bus and one fleet subject. The fleet driver is part of the workbench (the workbench architecture, section 4). When a program links a fleet driver, the kernel honors its `fw.Fleet`. `Agent.Fleet` then returns the view, and the native routes under `/v1/runs/{id}` forward a request about a run of another member to its owner. The metrics surface reports the fleet gauges (section 10.1). Without a driver, `fleet.subject` gives one warning in the log, and the fleet stays off.

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

A driver can also keep the last event of each subject. Such a driver implements `Retainer`:

```go
type Retainer interface {
    Retain(ctx context.Context, subject string, e Event) error
    Watch(ctx context.Context, pattern string, h func(Event) error) (stop func(), err error)
}
```

`Retain` replaces the kept event of a subject and delivers the event to every watcher. `Watch` first delivers the kept event of each subject that matches the pattern, and then each new one. A retained event never enters the stream of `Publish`, so a heartbeat does not fill the bus. The fleet driver of the workbench uses `Retainer` (the workbench architecture, section 4.2). The drivers `mem`, `nats`, and `redis` implement it.

### 9.2 Drivers

| Driver | Library | Notes |
|---|---|---|
| `mem` | standard library | Default. In-process only. |
| `nats` | `github.com/nats-io/nats.go` with JetStream | Recommended. One binary gives streams, a key-value store, and an object store. |
| `redis` | `github.com/redis/go-redis/v9` with Streams | Reuses the Redis that ax already deploys. No new infrastructure. |
| `kafka` | `github.com/twmb/franz-go` | Not implemented yet (section 16). |

### 9.3 Intake

The setting `bus.intake` names a queue, for example `fw.jobs.reviewer`. Each message is a run request with the same body as `POST /v1/runs`. When the run reaches a terminal event, the handler acknowledges the message. The message id is the idempotency key, so a redelivered message does not start a second run.

### 9.4 Egress

The setting `bus.egress` names a subject prefix, for example `fw.events`. The `bus` projection publishes each ledger event to `<prefix>.<atespace>.<agent>.<type>`. The setting `bus.egressTypes` limits the published types. Consumers include audit, billing, dashboards, and other agents.

### 9.5 Request and reply

A decision point on the bus uses request and reply (section 13.5). `fw` publishes a `fw.decision.requested` event to the subject of `access.external.bus` or `policy.external.bus`. The event names a reply subject of the process, and a responder publishes `fw.decision.answered` to it. The default timeout is 5 seconds. Without an answer in time, a tool call is denied, and a request from a caller is refused with a 503.

<a id="observability"></a>

## 10. Observability

Operators watch `fw` with Prometheus and Grafana. `fw` has no usage page of its own. The kernel keeps the exact record of usage in the ledger.

### 10.1 Metrics

The `metrics` projection maintains this registry. The route `/metrics` carries no authentication, so keep it inside the monitoring perimeter (section 8.5).

| Metric | Type | Labels |
|---|---|---|
| `fw_runs_total` | counter | `agent`, `outcome` |
| `fw_runs_queued_total` | counter | `agent`, `origin` (`http`, `a2a`, `mcp`, `bus`, `cli`, `tool`, `other`) |
| `fw_runs_active` | gauge | `agent` |
| `fw_run_duration_seconds` | histogram | `agent` |
| `fw_run_turns` | histogram | `agent` |
| `fw_budget_exceeded_total` | counter | `agent`, `limit` (`maxTurns`, `maxTokens`, `maxCostUSD`, `maxToolCalls`, `deadline`, `maxRepeats`, `other`) |
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

The origin of a child run is `tool`. `fw_approvals_oldest_timestamp_seconds` is the Unix time of the request of the oldest approval that waits. An agent without such an approval has no series.

`fw_model_tokens_total` and `fw_model_cost_usd_total` also count the summary calls of a compaction, from `fw.context.compacted`, and the title call of a run, from `fw.run.titled`. The kernel adds these tokens to the usage of the run, so the metrics match the ledger. These calls do not count in `fw_model_requests_total`. `fw_model_cost_usd_total` counts priced tokens only. The tokens of a model without a price go to `fw_model_unpriced_tokens_total`, because their cost is unknown, not zero. A price of zero is a price. Without a price table, every token is unpriced.

The model latency histogram has buckets up to 900 seconds, because one turn on a large model can run for minutes. The Go runtime and process collectors are on. The library is `github.com/prometheus/client_golang`.

#### Labels

No label holds a principal, a run id, a prompt, a path, or a URL. Tests feed a principal, a run id, and secret text into the events and make sure that no label holds them. These labels hold names that the configuration bounds:

- `agent`: the profiles of `fw.yaml` and the profiles that authorized people create. The kernel refuses a run of an agent that does not exist.
- `tool`: the tools that the kernel resolved. A tool name that the model invents becomes `unknown`.
- `server`: the names of the MCP servers under `tools.mcp`.
- `provider`: the registered model drivers.
- `model`: the models of the configuration, and at most 32 others.
- `driver`: the bus and ledger drivers.
- `member`: the names of the fleet members, which the deployment sets. The fleet gauges forget a member that is gone (see below).

The other labels hold values from fixed lists: `outcome`, `origin`, `limit`, `kind`, `decision`, `action`, `direction`, `state`, and `type`. An origin or a limit that this version does not know becomes `other`. The labels of `fw_build_info` have one series.

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

#### Fleet gauges

If the agent has a fleet (section 8.6), the metrics surface also reports the fleet view:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `fw_fleet_member_up` | gauge | `member` | 1 when the member is live, else 0 |
| `fw_fleet_member_last_seen_timestamp_seconds` | gauge | `member` | The Unix time of the last record or heartbeat of the member |
| `fw_fleet_runs` | gauge | `member`, `state` | Runs that are not finished, by state: `queued`, `running`, `input-required`, `suspended` |
| `fw_fleet_approvals_oldest_timestamp_seconds` | gauge | `member` | The Unix time of the request of the oldest approval that waits on the member |

The collector reads `a.Fleet()` at each scrape, so it finds a fleet that starts after the surface mounts. It reads the view without a principal, so it counts every member and every run that the records hold. It sends no request to another member. The first scrape starts the view and can wait up to 2 s for the retained records. If the view fails, the log gets a warning, and the rest of the scrape still works.

Each rollout of a Deployment adds members with new pod names. The gauges therefore forget the members that are gone, so the member label stays bounded. A member that stopped leaves the gauges at once. A member that is not live and silent for longer than `telemetry.FleetForgetAfter`, 24 hours, leaves them too. A member that holds an open run stays, so a sleeping ax Task with open runs stays.

Under ax, scrape only the members that are always on, such as a front with no agents. A scrape through the atenet router wakes a Task that sleeps. A scrape every 30 seconds therefore keeps every Task awake. An always-on member reports a sleeping Task in its fleet gauges, from the last record of that Task. Prometheus therefore sees the whole fleet and never wakes a Task. Two members can report the same member, so a query takes the max or the min for each member.

#### Prometheus is not the usage record

Use Prometheus for trends, errors, and latency. Do not use it to bill people or to audit usage. A scrape misses what a process does between its last scrape and its exit. Prometheus never sees a process that it does not scrape. A counter starts again at zero after a restart, so a total over a long range is an estimate.

The ledger of each process records every model call with its tokens, and `fw events <run> --format jsonl` exports it. Bus egress (section 9.4) sends the same events to a consumer for billing or audit. The kernel enforces each budget from the ledger, not from Prometheus. The dashboard of the workbench therefore shows no usage page. `GET /v1/usage` and the MCP tool `usage` of the workbench still answer, and both are deprecated. The result of a run keeps the usage of that run beside its limits (the workbench architecture, section 6.3).

#### Scrape configuration, alerts, and a dashboard

The directory [examples/observability](../examples/observability/README.md) holds a supported setup. `prometheus.yml` has the scrape jobs, `alerts.yml` has ten alert rules, and `grafana/fw.json` is one dashboard for Grafana 10 or later. The job `fw-pods` keeps only the pods with the label `fw.dev/scrape: "true"`. The examples put that label on the always-on fronts only, never on an ax Task. The README tells how to load each file.

### 10.2 Traces

The `trace` projection emits OpenTelemetry spans. A run is one span. Each turn is a child span, and each model call and tool call is a child of its turn. The exporter is OTLP, configured by the standard `OTEL_EXPORTER_OTLP_ENDPOINT` variable. The `traceparent` value travels in every ledger event, in every bus message, in the `spawn_task` environment, and in outbound A2A and MCP requests.

### 10.3 Logs

Logs are JSON lines on standard error from `log/slog`, so that `fw-workbench mcp` keeps standard output for the protocol. Each line carries the `run`, `turn`, and `traceparent` that apply. `FW_LOG=debug` adds request and response bodies with secrets redacted.

### 10.4 Trajectories

A trajectory is the full record of what an agent did. The ledger is the trajectory. The command `fw events <run> --format jsonl` exports it. The ax roadmap names trajectory collection at the runner layer as a goal, and the ledger is that record.

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
| `GET /metadata/v1alpha1/ax/task` | The Task as YAML (optional) |
| `GET /metadata/v1alpha1/ax/workspaces` | The Workspaces as YAML (optional) |
| `spec.command` | Started as a supervised child with `AX_METADATA_URL` set |
| `SIGTERM` to PID 1 | Stop or suspend. Ten seconds of grace, then `SIGKILL`. |
| `spec.debug: true` | Guest services over gRPC on port 80, multiplexed with h2c |

### 11.2 How fw meets the contract

`fw` implements the contract itself instead of embedding the ax `runner` package. The reason is port 80. The ax runner owns the port 80 mux, and `fw` needs that port for its own surfaces. The `ax` package defines the manifest types from the YAML schema of the ax repository. `fw` therefore parses the same YAML that the stock runner parses, without an import of the ax module. The container image contains a symlink from `/usr/local/bin/ax-task-runner` to `/usr/local/bin/fw`.

Two images are built. `fw:static` is `gcr.io/distroless/static` plus the binary, for agents that only use HTTP, MCP, and A2A. `fw:toolbox` is a Debian slim image with `git`, `curl`, and common build tools, for coding agents that use `bash`. The stock ax runner image installs Python and the Antigravity agent. `fw` needs neither.

### 11.3 Boot sequence under ax

1. Read `AX_TASK_YAML` and `AX_WORKSPACES_YAML`. Set `FW_HOME` to `/workspace/.fw`.
2. Listen on port 80. Answer `/healthz` with 200. If the Task sets `spec.debug: true`, start the guest services of `ax ssh` (section 11.12).
3. For each workspace binding, in order, write `spec.files`, clone `spec.git`, and create `spec.skills.path`. Skip a binding whose marker exists.
4. Write the marker at `$FW_HOME/ax/initialized-<path>`.
5. If the binding has a `goal` and a model credential is present, run the `bootstrap` profile with that goal. The time limit comes from `AX_BOOTSTRAP_TIMEOUT`, default 10 minutes. The marker is written first, so a container killed during the goal does not run the goal again on resume.
6. Answer `/readyz` with 200.
7. Resume unfinished runs (section 4.5).
8. If `spec.command` is empty, serve. If `spec.command` starts with `fw`, run that subcommand in process. Otherwise, start the command as a supervised child with `AX_METADATA_URL` set.

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

On resume, `fw` boots again, finds the markers, skips setup, and resumes runs from the ledger. A run that was waiting for an approval is still waiting. A run that was in the middle of a model call sends the call again. If the lost events include a tool call that ran, the resumed run asks the policy again and can run the call a second time. The working time of a run ends at the last event before the suspend (section 5.8). The deadline therefore does not count the time that the Task sleeps. A grant that `fw` refreshed after the save comes back as its old copy. The authorization server can then refuse it, and the person connects again (section 7.11).

`fw` still follows section 5.6 on `SIGTERM`. If the worker pod itself stops, Substrate sends `SIGTERM`. After a grace period of 30 minutes, it sends `SIGKILL`.

### 11.6 Using the Workspace spec

The ax Workspace spec carries everything `fw` needs for a profile. `spec.files` ships `fw.yaml` and profile files into the workspace. `spec.git` is cloned by `fw`. After the clone, `fw` removes the user information of an http or https URL from the remote. A token in `spec.git` therefore does not stay in `.git/config`. A later fetch or push needs a credential from the egress policy or from a credential helper. `spec.mcp.servers` become MCP clients. `spec.skills.path` is read for `SKILL.md` folders, which become MCP prompts and on-demand context. An operations team ships an agent by applying a Workspace. See `examples/ax/reviewer.yaml`.

### 11.7 Credentials

If the atespace has a Gemini credential, ax injects `GEMINI_API_KEY`. ax does not deliver the `Model` resource to the runner today, and `spec.env` holds plain values. For another provider, pass the key through `spec.env` or through a `Secrets` driver that the sandbox can reach. Do not write a key into a Workspace file, because the file lands on the durable volume.

### 11.8 Status for the control plane

The ax `TaskStatus` has fields `usage` (prompt and completion tokens) and `pendingApproval` (id, action, time). ax has no RPC for a runner to fill them yet. `fw` serves `GET /metadata/v1alpha1/fw/status` on port 80 with a JSON body in the same shape:

```json
{
  "usage": { "promptTokens": 48210, "completionTokens": 3120 },
  "pendingApproval": { "id": "ap_7f3", "action": "bash: git push", "requestedAt": "2026-10-13T09:14:31Z" },
  "runs": { "active": 1, "inputRequired": 1 }
}
```

The ax controller, or any poller, reads it through the router. When ax adds a reporting RPC, a `Reporter` plug pushes the same data.

### 11.9 Child runs and child tasks

An agent creates other agents with the `spawn_task` tool. The tool starts a child run in the same process with a task of its own. If the parent gives them, the child also gets a system prompt, instructions, tool and skill subsets, a model, an effort, and a budget. These form an inline profile on top of the named profile of the child. The tool is parallel-safe. A model that needs three things done therefore starts three children in one response, and the kernel runs them at the same time. With `wait` false the tool returns the run id at once, and `await_task` collects the result later.

The kernel keeps a child inside its parent:

- A child uses only tools that its own profile and every ancestor allow.
- A child carries the policy rules of every ancestor, and the stricter verdict wins.
- The budget of a child is capped by the budget of its parent, and its deadline is the earliest deadline of its ancestors.
- `spawn.maxDepth` (default 3) bounds how deep the tree goes, and `spawn.maxChildren` (default 16) bounds how many children one run starts in its life. Past either bound, `spawn_task` returns an error result.
- A canceled parent cancels its children.
- A child shares the trace of its parent, so one trace shows the whole tree.
- A child takes no slot of `maxRuns`, because its parent holds one while it waits. Otherwise parents that wait for children can fill every slot.

When a child needs an approval or asks a question, `spawn_task` relays it to the caller of the parent. It arrives as a question on the parent run. Every surface shows it, and the answer goes to the child. The parent run therefore stays the one place to watch.

Under ax, a later milestone creates a child ax Task through the `ax.v1alpha1.AX` gRPC service at `FW_AX_SERVER` (default `ax-server.ax-system.svc.cluster.local:8080`). The child copies the image, the environment, and the workspace bindings of the parent, and adds `FW_PARENT_RUN` and `TRACEPARENT`. The parent then talks to the child by A2A through the router. This builds the task tree that the ax concepts document describes.

### 11.10 Local testing without a cluster

`fw serve --task-file task.yaml --workspace-file ws.yaml` reads the manifests from files instead of the environment. These flags match the stock runner flags. The `--port` flag moves the listener off port 80 for a developer machine.

### 11.11 Gaps in ax today

These gaps exist in ax at the commit read on 2026-10-13. Each one maps to an item on the ax roadmap.

- A Task has no goal field. `fw` takes the goal from `FW_GOAL`, from `spec.command`, from a message through the router, or from a queue.
- The `Model` resource is not delivered to the runner. `fw` reads credentials from the environment (section 11.7).
- There is no runner-to-control-plane status RPC. `fw` serves a status endpoint instead (section 11.8).
- A Task has no address of its own. External A2A clients need a header-adding proxy (section 11.4).
- The stock marker directory `/ax` is not durable. `fw` writes its marker under `/workspace` (section 11.3).
- `ax ssh` sends no standard input. A bare `ax ssh <task>` starts `/bin/sh` with empty input, and the shell exits at once (section 11.12).
- Agent Substrate v0.4.0, read on 2026-10-14, suspends a Task with `SIGKILL` after it saves `/workspace`. No `SIGTERM` comes first. `fw` writes no `fw.run.suspended` event, and the working time of a run handles the missing event (section 11.5).

### 11.12 Guest services and `ax ssh`

`ax ssh <task> -- <command>` runs a command in the sandbox of a Task and prints its output. The command goes to the Agent Substrate guest services, which answer gRPC on port 80. As the runner contract asks, `fw` serves them only for a Task that sets `spec.debug: true`. `ax ssh` refuses a Task that does not set it.

The package `ax/guest` puts the upstream services from `github.com/agent-substrate/env` on one gRPC server, as the stock runner does:

- The process service starts, follows, feeds, and signals processes. `ax ssh` uses it.
- The file system service reads and writes files under the first workspace.
- gRPC reflection describes both services, so a tool such as `grpcurl` needs no proto files.

If a request uses HTTP/2 and its content type starts with `application/grpc`, the listener sends it to the services. Every other request goes to the routes of section 8.1. The services start with the listener in step 2 of section 11.3, before the workspaces are ready. You can therefore inspect a boot that hangs on a clone or a goal. Until the first workspace exists, a process with no working directory starts in the closest parent directory that exists.

A process starts in the first workspace by default. It gets `AX_METADATA_URL`, as the command of the Task does, and it inherits the rest of the environment of `fw`. Its output stays in `/var/log/ate-jobs` for about one hour after it exits. That directory is not on the durable volume. If `fw` cannot write to it, the output goes to `ate-jobs` under the temporary directory. A process stops after one hour or after the timeout of its request, and ten processes can run at the same time.

Each call writes the log line `fw: ax guest call` with the method, the process id, and the name of the program. A call that starts or signals a process, or reads or writes a file, logs at the info level. The line never holds the arguments, the input, or the output of a process, because they can hold a secret. The process service collects the exit status of its own processes at once. The zombie reaper of section 5.6 waits five seconds before it collects a child, so the two never take the same exit status.

Do not set `spec.debug` on a Task whose environment or workspace holds data that other callers must not read. The services carry no authentication, as the runner contract expects, and `ax ssh` sends no credential. The authentication, the access roles, and the policy of `fw` do not apply to these calls. Any caller that reaches port 80 of the sandbox, directly or through the atenet router, can run a process. That process can read every `spec.env` entry and every key in the environment of `fw`. If the services are on, `fw` logs a warning at boot.

The `fw:static` image holds no shell, so `ax ssh <task>` with no command cannot start `/bin/sh`. The `fw` binary is on the PATH of both images, and `fw events` finds the ledger under `/workspace/.fw` without a flag. This command prints the ledger of a run in either image:

```
ax ssh reviewer -- fw events <run-id>
```

At ax commit ac23328, `ax ssh` sends no standard input. A bare `ax ssh <task>` therefore starts `/bin/sh` with empty input, and the shell exits at once. The process service already takes input through `WriteProcessInput`. A later `ax` that sends input therefore needs no change in `fw`.

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

A value in `fw.yaml` and in a profile file expands `${NAME}` and `${NAME:-default}` from the environment before the file is read. `$$` is one `$`. A reference to an unset variable with no default is an error that names the variable, the key, and the file. Prompt text read from a file does not expand.

### 12.2 What you can replace

| Interface | Default | Alternatives | Swap with |
|---|---|---|---|
| `Model` | `anthropic/claude-opus-5-5` | `gemini/*`, `openai/*`, your own | `model:` or `fw.WithModel` |
| `Tool`, `ToolSource` | `bash`, `files`, `http`, `memory` | MCP servers, A2A peers, your own | `tools:` or `fw.Tools` |
| `Policy` | allow read-only, ask on destructive | rules, an external MCP or bus decision point, your own | `policy:`, `policy.external:`, or `fw.WithPolicy` |
| `Ledger` | JSONL files | NATS, Redis, SQLite, your own | `ledger.driver:` or `fw.Ledger` |
| `Bus` | `mem` | NATS, Redis, Kafka, your own | `bus.driver:` or `fw.Bus` |
| `Compactor` | a summary by the model of the run | your own | `compaction:` sets when, `fw.WithCompactor` replaces it |
| `Secrets` | environment and files | Vault, Google Secret Manager | `secrets.driver:` or `fw.Secrets` |
| `Surface` | native, A2A, metrics | MCP from the workbench, any subset, your own | `surfaces:` or `fw.Surfaces` |
| `Observer` | messages, status, metrics, bus, a2a, trace | your own | `fw.Observe` |
| `Authn` | bearer token | none, oidc, spiffe, a chain of modes, your own | `auth.mode:` or `fw.WithAuthn` |
| `Authz` | rules from `access` | an external MCP or bus decision point, your own | `access.external:` or `fw.WithAuthz` |
| `ProfileStore` | one YAML file per profile | your own | `fw.WithProfileStore` |
| TLS provider | plain HTTP | `file`, `spiffe`, your own | `tls.provider:` or `fw.WithTLS` |
| `Prices` | empty | a price table | `prices:` or `fw.Prices` |
| `Clock` | the system clock | a manual clock for tests, your own | `fw.WithClock` |
| `ToolLimits` | 256 KB in, 100 KB out, 10 min | your own numbers per tool | `tools.limits:` |
| `GrantStore` | encrypted files under `<home>/grants` | memory, your own | `grants.store:` or `fw.WithGrantStore` |

### 12.3 Middleware

`ModelMiddleware` wraps a model (section 6.4). `ToolMiddleware` wraps a tool with the same shape. Built-in tool middleware covers timeouts, output size limits, and output redaction. Middleware is the main way to add a behavior without touching the kernel.

### 12.4 Hooks

A hook is an observer (section 4.4) or a policy (section 7.5). An observer sees everything and changes nothing. A policy decides before a tool runs. Both run in process by default. An out-of-process observer reads the egress subject. An out-of-process policy is a decision point: an MCP tool or a bus responder that answers each decision request (section 13.5). The same form decides authorization.

### 12.5 Profiles, instructions, and skills

A profile is a named agent configuration. It holds a description, a system prompt, and a list of instruction texts. It also holds a skill allowlist, a tool allowlist with globs, a model selector, an effort level, a time zone, a policy, and a budget. Profiles live under `agents:` in `fw.yaml` or as one file each under `$FW_HOME/agents/`. The system prompt and each instruction entry is either the text or the path of a file that holds it. A request names a profile with `agent`. One binary serves many profiles, and the A2A card lists each one as a skill.

Instructions reach the model in three layers, and every layer is a plain text:

1. The system prompt of the profile. A profile without one gets `fw.DefaultSystemPrompt`. That prompt says what the agent is and how it works, with the rules that keep a run from looping.
2. The instructions of the profile, under a heading, so one profile composes a base prompt with house rules.
3. The instructions of the run, from the field `instructions` of the request, under their own heading.

After the three layers, the system prompt lists the skills that the profile allows (see below). Last come the sections that tools add at the start of the run, such as the memory index (section 7.8).

A request can also carry an inline profile in the field `profile`. It overlays the named profile for that run. A set system prompt, model, effort, or time zone replaces the value. Its instructions follow the instructions of the named profile. A tool or skill list replaces the list. The policy becomes the stricter of the two, and the budget takes the set fields. The kernel stores the inline profile and the instructions in `fw.run.queued`, so a resume and a replay see the same prompt. A tool list in an inline profile can give a run tools that the tool list of its agent leaves out. The kernel names them in `extraTools` of `fw.run.queued` and of the run object, and leaves out a tool that an ancestor does not allow. Every surface tells the person. Under the first message, the conversation says "For this run only, reviewer can also run shell commands". The result of the MCP `run` tool ends with the same fact, and `fw run` prints it on standard error. The command line builds an inline profile from flags such as `--system`, `--tools`, and `--max-turns`.

A skill is a directory with a `SKILL.md` file whose front matter names and describes it. The skills live under `$FW_HOME/skills`, or under the directories in `tools.skills`, and under the ax Workspace skills path. The system prompt lists the skills that the profile allows, one line each. The `skill` tool returns the body of one skill and the list of its other files. The MCP server of the workbench serves the same files as resources under `fw://skills/<name>`. A profile with no `skills` entry sees every skill, and a profile with an empty list sees none.

### 12.6 Driver registry

Each driver registers itself by name at init, for example `fw.RegisterBus("nats", nats.New)`. A third-party Go module adds a driver by being imported. A blank import in `cmd/fw/drivers.go` selects the drivers that the default binary includes. A custom build leaves out a driver by leaving out its import.

A linked package can also change the default configuration. It calls `fw.RegisterDefaults(func(*fw.Config))` in `init`. `DefaultConfig` applies the changes after the kernel defaults, in the order of registration. A file or a variable still overrides the result. The workbench uses it to add the surface `mcp`, the tool `propose_agent`, and `titles: model` (the workbench architecture, section 1.1).

### 12.7 Out-of-process extension

Three protocols extend `fw` without Go code:

- MCP adds tools and policy checks through the MCP client. The MCP server of the workbench also brings work in through its `run` tool (the workbench architecture, section 3).
- A2A adds peers that the agent can ask.
- The bus adds intake, egress, and request and reply hooks.

### 12.8 Public Go API

The package `fw` is the main public package. It holds the interfaces above, the option functions, and four entry points. A program that builds on the kernel, such as the workbench, also uses two more public packages:

- `cli` holds the command line. `cli.Run` runs a verb. A binary sets function variables such as `cli.NewLogger`, `cli.CardJSON`, and `cli.ServeMCPStdio` for the packages that it links. A nil variable turns off what needs it.
- `surface/native` is the extension point of the native API. `native.Extend` registers an `Extension`, and `native.New` mounts each one after the run routes. `native.NewWith(exts...)` mounts only the given ones.

An extension gets a `*native.Kit`. The kit has `Agent`, `Handle` (behind authentication), `HandlePublic`, and `SetSigninPath`. It also has the helpers of the run routes: `Remote`, `OwnerOf`, `Forward`, `Start`, `Fail`, `Decode`, and `DecodeFiles`. The package also exports `WriteProblem`, `WriteJSON`, `Accepts`, `Cause`, `Attachments`, the type `FileBody`, and the limits `DefaultEventsLimit` and `MaxEventsLimit`.

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

// AuthorizeAll asks the Authz about several checks for the principal in ctx.
// An Authz that implements BatchAuthz gets every check in one call.
func (a *Agent) AuthorizeAll(ctx context.Context, checks []AuthzCheck) []error

// RunClock returns the time of a run: the time now and the start in the zone
// of the run, its working time, and the working time left before the deadline.
func (a *Agent) RunClock(ctx context.Context, id string) (RunClock, error)

// RunBudget returns the effective budget of a run: its own budget, merged
// with the budget of its profile and of the configuration, and capped by
// the budget of its parent.
func (a *Agent) RunBudget(ctx context.Context, id string) (Budget, error)

// TitleFrom makes a short title from the input of a run (section 8.2).
func TitleFrom(input string) string

// Run options: AsAgent, WithInstructions, WithRunProfile, WithRunBudget,
// WithParent, WithMetadata, WithIdempotencyKey, WithOrigin.

// Serve mounts every enabled surface on one listener and blocks.
func (a *Agent) Serve(ctx context.Context) error

// Fleet returns the view of the other members of the fleet, or nil when
// fleet.subject is empty (section 8.6).
func (a *Agent) Fleet() Fleet

// Files returns the workspace with the rules of tools.files (section 7.9).
func (a *Agent) Files() Files

// Connections lists the services that act for a person. BeginConnect,
// FinishConnect, and Disconnect manage the grant of the person at one
// service, and Grants returns the store of the grants (section 7.11).
func (a *Agent) Connections(ctx context.Context, p *Principal) ([]Connection, error)
func (a *Agent) BeginConnect(ctx context.Context, p *Principal, server, redirect string) (ConnectStart, error)
func (a *Agent) FinishConnect(ctx context.Context, p *Principal, start ConnectStart, code string) ([]Connection, error)
func (a *Agent) Disconnect(ctx context.Context, p *Principal, server string) error
func (a *Agent) Grants() GrantStore

// RequestConnect pauses the tool call in ctx until the person of the run
// connects a service. A tool source that acts for a person implements Connector.
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
func WithModelMiddleware(m ...ModelMiddleware) Option
func WithToolMiddleware(m ...ToolMiddleware) Option
func WithProfile(p AgentProfile) Option
func WithBudget(b Budget) Option
func WithListen(addr string) Option
func WithAuthn(a Authn) Option
func WithAuthz(z Authz) Option
func WithProfileStore(s ProfileStore) Option
func WithTLS(c *tls.Config) Option
func WithPrices(p Prices) Option
func WithLogger(l *slog.Logger) Option
func WithClock(c Clock) Option
func WithFleet(f Fleet) Option
func WithFiles(f Files) Option
func WithGrantStore(s GrantStore) Option
func WithoutDefaults() Option
```

`WithAuthz` and `WithPolicy` win over `access.external` and `policy.external` (section 13.5).

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
| `fw init [dir]` | Write a starter `fw.yaml`, a default prompt, and the `agents` and `skills` directories |
| `fw doctor` | Report on the configuration, credential, model, tool sources, connections, bus, ledger, skills, and time zone, one line each |
| `fw run "goal"` | Run once in the terminal and print the output |
| `fw serve` | Serve every enabled surface |
| `fw tools` | List tools from every source |
| `fw skills [name]` | List the skills, or print one skill |
| `fw events <run>` | Print or follow the ledger of a run |
| `fw replay <ledger>` | Replay a ledger against a model |
| `fw card` | Print the A2A agent card |
| `fw config` | Print the resolved configuration with secrets masked |
| `fw env` | Print every configuration variable, and the value of each one that is set |
| `fw agents [name]` | List the profiles, print one, or `put` and `delete` one from a file |
| `fw whoami` | Print the local principal that the commands use |

`fw-workbench` takes every verb above. It adds `fw-workbench mcp`, which serves the MCP server over stdio (the workbench architecture, section 3). In `fw`, the verb `mcp` fails with a message that names `fw-workbench mcp`.

`fw run` shapes the run from flags without a configuration change. `-system` and `-i` take a text or `@path` to a file, and `-tools` and `-skills` take globs. `-model`, `-effort`, `-time-zone`, `-max-turns`, `-deadline`, and `-max-tool-calls` set the rest. The flags build an inline profile (section 12.5). When a run stops at a budget, `fw run` names the limit and the flag or key that raises it.

`fw run` and `fw events --format text` print one line for each event. The line of `fw.run.queued` names the work first, for example `fw.run.queued title="Fix checkout retries" agent=reviewer origin=http`. The title is the title of the run (section 8.2). An input without text gives no `title` field. `fw replay` names the source run with its title on standard error. The JSON of `fw run --json` and `fw replay --json` is the `fw.Run` object, so it gains the field `title`.

## 13. Security

### 13.1 Who the caller is

Every surface authenticates every request with the `Authn` plug and puts the principal in the request context. A principal has a subject, a name, a kind, an issuer, groups, and scopes. The kinds are `user`, `workload`, `token`, `local`, and `anonymous`. A `user` is a person, from an OIDC token. A `workload` is a service or another agent, from a SPIFFE identity. `token` is the static bearer token of the process, and `local` is the user of the process, over stdio. `anonymous` has no credential, and it comes from `auth.mode: none`.

`auth.mode` names one mode, or several separated by commas that are tried in order, for example `oidc,spiffe,token`. The first mode that accepts the credential wins, and the first mode writes the challenge.

| Mode | Credential | Subject |
|---|---|---|
| `token` | the static bearer token from the secret `auth.tokenEnv` | `token` |
| `oidc` | a bearer token from the issuer `auth.oidc.issuer`, checked against its keys, its issuer, its expiry, and `auth.oidc.audience` | the `sub` claim. The name comes from `email` or `auth.oidc.nameClaim`, and the groups from `auth.oidc.groupsClaim`. |
| `spiffe` | a JWT-SVID bearer token. With `auth.spiffe.x509` and a listener that verifies clients, also an X.509-SVID client certificate. | the SPIFFE ID, for example `spiffe://example.org/ns/prod/sa/reviewer` |
| `none` | nothing | `anonymous` |

The `oidc` driver is part of the workbench, in the package `authn/oidc`, and the binary `fw` links only the `spiffe` driver. The `oidc` driver reads the discovery document of the issuer and refreshes its keys. Its challenge names the protected-resource document at `/.well-known/oauth-protected-resource`, so an MCP client finds the authorization server and does the OAuth flow itself. The `spiffe` driver reads the trust bundle from the Workload API at `auth.spiffe.socket`. Without an agent, it reads the bundle from the file or URL `auth.spiffe.bundle`. The same driver is the TLS provider `spiffe`, which serves the X.509-SVID of the workload and verifies client certificates against the bundle. An MCP server entry with `auth.spiffe` sends a JWT-SVID of this workload on every request, so one agent calls another with no shared secret.

TLS on the listener comes from `tls.provider`: `file` uses `tls.cert` and `tls.key`, with `tls.clientCA` and `tls.clientAuth` for client certificates, and a driver such as `spiffe` builds the configuration itself. Without a provider the listener speaks plain HTTP with unencrypted HTTP/2, which is the right choice behind the atenet router.

The workbench adds a browser sign-in to the page at `/ui`, and it serves the protected-resource document (the workbench architecture, sections 2 and 5).

### 13.2 What the caller can do

An agent profile carries an `access` block with three lists: `owners`, `operators`, and `viewers`. An owner manages the profile and does everything below. An operator starts runs, sends follow-ups, approves, and cancels. A viewer reads runs and their ledgers. Whoever starts a run can always view and operate that run. A child run belongs to the one who started its parent, so the same people see the whole tree.

Each entry names principals in one of five forms:

| Entry | Matches |
|---|---|
| `alice@example.com` or `spiffe://example.org/ns/prod/*` | the subject or the name, as a glob where `*` matches any text |
| `group:platform-team` | a group of the principal, as a glob |
| `kind:workload` | every principal of that kind |
| `issuer:https://idp.example.com` | every principal from that issuer |
| `*` | every principal |

The process-wide block `access` holds `admins`, who own every agent, `creators`, who can add agents, and `default`. With `default: open`, which is the default, an agent that has no `access` block is open to every authenticated principal as an operator. That is how fw behaved before access existed. With `default: closed`, such an agent is reachable by its admins only. A deployment therefore turns identity on in two steps: set `auth.mode` and `access.admins`, then switch `access.default` to `closed` once every agent names its owners.

The kernel enforces this through the `Authz` plug on every call that carries a principal. A run on an agent needs the operator role, and a read of a run needs the viewer role or the run itself. A decision or a cancel needs the operator role or the run itself, and a profile change needs the owner role. A call that carries no principal comes from the process itself, such as a tool that starts a child run, and is trusted. A target can name another member of the fleet (section 8.6). The default rules refuse such a target, because they do not know the rules of another process. The default `Authz` is the rules above, and `fw.WithAuthz` replaces it with a policy of your own. `access.external` replaces it with a control plane outside the process (section 13.5).

### 13.3 Managing agents

A profile is created, changed, and removed at runtime through `PutProfile` and `DeleteProfile`. The native API, `fw agents`, and the MCP server of the workbench expose them. A principal that creates a profile without an `access` block becomes its first owner. An individual or a group therefore owns the agents that they make. With `access.external`, that owner is data: the access block travels in each later check, and the control plane decides whether it counts. The `ProfileStore` keeps each change as one YAML file under `$FW_HOME/agents`, and the next start reads it. A profile that `fw.yaml` defines is not changed from the API, because the file wins at the next load.

`PUT /v1/agents/{name}?dryRun=true` checks a profile as a save does and changes nothing. `Agent.CheckProfile` runs these checks in the kernel. `Agent.ProfileGrants` gives the verdict of each tool of an agent, and the view of one agent carries it in plain words as `abilities`. The workbench adds the MCP tools, the page, the shared views, and the agent builder (the workbench architecture, section 6).

### 13.4 Rules

- Authentication is on by default. A missing `FW_API_TOKEN` with `auth.mode: token` stops the process at start.
- Secrets never enter the ledger. Before each append, the kernel replaces each known secret value and each text with the shape of a key (section 4.2). Give a secret to an agent through `Secrets` or the egress policy. A secret in the input of a run reaches the model as `[redacted]`.
- Tool results are data, not instructions. The kernel passes them to the model as tool result blocks only.
- Operator instructions use system messages. User and tool content never reach the system prompt.
- Destructive tools ask by default. A deployment that wants no prompts sets the policy to allow, in writing, in `fw.yaml`.
- The `files` tool is jailed to its root path and never opens the home directory of `fw`, the file ledger, or the configuration file. The `http` tool uses an allowlist. Sandboxing of `bash` belongs to the container runtime, which is ax under ax.
- Annotations from an MCP server are hints. The policy trusts only the servers that `fw.yaml` lists as trusted.
- Budgets cap every run. A run with no budget gets the profile budget. A profile with no budget gets the default: 200 turns, 2 hours of working time, 1000 tool calls, 32 calls per turn, and 5 repeats of one call.
- Tool limits bound every call: 256 KB in, 100 KB out, and 10 minutes, per tool and per configuration. A large data source cannot flood the context window.
- A child run never has more than its parent: fewer tools, stricter rules, a smaller budget, and a bounded depth and count.
- An inline profile on a request never carries an `access` block, so a request cannot grant itself a role. Its tool list can give a run more tools than its agent lists, and every surface says so (section 12.5).
- The principal of a run and the decider of an approval are in the ledger, so every action has a name on it.
- A control plane that does not answer never opens access. With `access.external`, a request from a caller gets a 503. With `policy.external`, the tool call is denied.

Do not set `auth.mode: none` on a listener that is reachable from outside the cluster. Anyone who reaches the port can start runs and approve tool calls. Do not set `access.default: closed` without an entry in `access.admins`, or nobody can manage the agents until the configuration changes.

### 13.5 External decisions

A deployment with many agents often keeps its rules in one control plane. With `access.external`, that control plane decides who can do what. With `policy.external`, it decides which tool calls run. `fw` sends the facts of each decision and enforces the answer. It makes no decision of its own for that part, so the control plane is the single source of truth. A security team changes a rule without a change to `fw.yaml` and without a rebuild.

Both blocks have the same fields:

| Field | Meaning | Default |
|---|---|---|
| `mcp` | The name of a `tools.mcp` entry. The endpoint, the TLS, and the authentication of that entry apply. | none |
| `tool` | The tool on that server | `authorize` for `access.external`, `decide` for `policy.external` |
| `bus` | The bus subject that receives the requests | none |
| `timeout` | The limit of one request | `5s` |

A block sets exactly one of `mcp` and `bus`, or the process stops at start. Every field has a variable, for example `FW_ACCESS_EXTERNAL_MCP=control` and `FW_POLICY_EXTERNAL_BUS=fw.policy`. The options `fw.WithAuthz` and `fw.WithPolicy` win over the blocks. This configuration sends both kinds of decision to one MCP server:

```yaml
tools:
  mcp:
    - name: control
      endpoint: https://control.example.com/mcp
      auth: { bearer: CONTROL_TOKEN }
access:
  external: { mcp: control }
policy:
  external: { mcp: control }
```

#### The requests and the answers

Every request is a JSON object with `kind` and `version`. The kind is `authz` or `policy`, so one tool can serve both. The version is 1. An authorization request asks about one principal and one or more checks:

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

The actions are the actions of section 13.2: `agent.view`, `agent.run`, `agent.edit`, `agent.delete`, `agent.create`, `run.view`, `run.operate`, and `admin`. `access` is the access block of the profile, as data. A check about an agent or a run of another fleet member also carries `member`, the name of that member (section 8.6). Then `access` and `lineage` come from the record of that member. `run` is set for an action on a run. Its `principal` is the subject that started the run, and `lineage` lists the ancestors of a child run, the parent first. The answer holds one decision per check, in the same order:

```json
{"decisions": [{"allow": false, "reason": "only the owner of the root run operates it"}, {"allow": true, "ttlMs": 30000}]}
```

An answer can also name teams (section 13.6). `teams` beside `decisions` names the teams of the principal. `teams` in a decision names the teams of the agent of that check. An answer without `teams` leaves the teams that fw knows as they are, and an empty list clears them.

```json
{"teams": ["payments"], "decisions": [{"allow": true, "teams": ["payments", "platform"]}]}
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

With `mcp`, `fw` calls the tool by its plain name with the request as the arguments. It reads the answer from `structuredContent`, or else from the first text block that holds a JSON object. A result with `isError` counts as no answer. A server that is down after repeated failures (section 7.3) also counts as no answer. The MCP tool source leaves out every server that a block names, so the model never sees or calls a decision tool. The MCP driver registers this transport with `fw.RegisterDecisionPoint`.

With `bus`, `fw` publishes a `fw.decision.requested` event to the subject. Its data holds `replyTo`, a reply subject of the process, and `request`, the request above. A responder publishes a `fw.decision.answered` event to `replyTo`. Its data holds `inReplyTo`, the id of the request event, and `answer`. Any process that subscribes to the subject can answer. A Go responder reads a request with `fw.DecisionRequestOf` and builds the answer event with `fw.AnswerDecision`.

`fw doctor` prints one line for each decision point. For an MCP server, it makes sure that the server lists the tool. A bus subject has no probe, because a responder only answers a request.

#### Authorization

With `access.external`, `fw` applies none of its own access rules. `access.default`, `access.admins`, and `access.creators` do not apply. The access block of the profile travels in each check, and the control plane decides whether it counts.

The control plane also decides about a child run by itself, from the lineage in the check. After a refusal, `fw` does not ask again for the ancestors of the run, as the rules of section 13.2 do. A listing of runs or agents sends all of its checks in one request, and so does each view of the dashboard.

`ttlMs` lets `fw` reuse a decision for that many milliseconds. Zero, the default, means no reuse, so a revocation takes effect at the next request. The cache key holds every fact of the check. A change to an access block or to a run therefore asks again.

If the control plane does not answer in time, `fw` refuses the request with `fw.ErrAuthzUnavailable`. An answer that `fw` cannot read, or that holds the wrong number of decisions, counts as no answer. The native API answers 503 with `Retry-After: 5`, and the MCP surface returns an error result that says that the service did not answer. A caller therefore tells a refusal, a 403, from a control plane that is down.

A2A has no error for a service that is down, so the A2A surface sends the server error of the protocol. Its details carry the reason `AUTHZ_UNAVAILABLE` and `retryAfterSeconds: 5`. The metadata of its error info carries `fwReason: AUTHZ_UNAVAILABLE` and `retryAfter: "5"`. The JSON-RPC binding sends this error with the code -32000 and HTTP 200, as JSON-RPC defines. The REST binding sends HTTP 503 with `Retry-After: 5`, and the error document has the code 503 and the status `UNAVAILABLE`. If the first event of a stream on the REST binding is this error, the stream gets the same 503. The surface holds the headers of a stream until its first event to make this possible, and that event comes right after the access check.

A call from the process itself carries no principal and never asks. Runs, child runs, resumes, and the bootstrap goal therefore keep working during an outage. With `access.external`, `fw agents` and `fw-workbench mcp` act as the local principal (section 13.1). The control plane therefore also decides for the local user.

#### Policy

`policy.external` replaces the rules of `policy:`. The control plane decides every tool call. `allow` runs the call, and `deny` refuses it with the reason. `ask` asks the caller for an approval (section 5.3). When `access.external` is set, it authorizes the decision on that approval as the action `run.operate`.

The policy of a profile and the rules of the ancestors of a run still apply, and the stricter verdict wins. They can therefore only make the verdict of the control plane stricter. If the control plane does not answer, or answers with an unknown verdict, the call is denied. The model reads the reason in the tool result.

After an approval, the kernel asks again about the calls of the response that are still open. The verdict therefore holds at the moment that a call runs.

Do not add a second `tools.mcp` entry with the endpoint of a decision point. `fw` hides the entry that a block names from the model, but the model can call the decision tool through the second entry.

### 13.6 Teams

A team is a group of people and agents that work together. The dashboard of the workbench shows the work of a person and of the person's teams first. The runs of other people sit behind one row. A team groups the view and grants no access: the access blocks of section 13.2, or the control plane of section 13.5, still decide who can do what.

The admins keep the teams upstream, in the identity provider or in the control plane:

| Source | The teams of a person | The teams of an agent |
|---|---|---|
| The rules of section 13.2 | The groups of the principal, from the groups claim of OIDC (section 13.1), that match `access.teams` | The groups that the access block of the profile names as `group:<name>` and that match `access.teams` |
| `access.external` | The `teams` of the answer of the control plane | The `teams` of each decision about that agent |

`access.teams` lists glob patterns over group names, such as `team-*`. An empty list, which is the default, makes every group a team. The variable is `FW_ACCESS_TEAMS`, with commas between the patterns. With `access.external`, fw takes no team from the groups or the access blocks. The control plane is then the single source of the teams too. A team name is an opaque string, so the control plane can use an id or a name.

A run keeps the teams of the person who started it, in `fw.run.queued`. A child run keeps the teams of its parent. The teams of a run therefore stay as they were at the start. Every fleet member reads them from the run record, with no request to the control plane. `whoami` returns the teams of the caller, and each agent view returns the teams of the agent.

The runs lists take `by`, which keeps a part of the runs that the caller can see:

| `by` | The runs |
|---|---|
| `me` | The runs that the caller started |
| `team` | The runs that the caller started, and the runs that a person on a team of the caller started |
| `others` | Every other run that the caller can see |

The native API takes `?by=team` on `GET /v1/runs`, and the MCP tool `runs_list` of the workbench takes `by`. The sentence and the counts of the answer cover the same runs.

In the workbench, home (`GET /v1/home`) holds the runs of `by=team`. The row and the status line of a run of a teammate name that person, for example "by alice". In the conversation, a message of another person shows the name of that person above it. In the Agents place, the agents that the caller owns come first, then the agents of the caller's teams, then the rest. If other people have runs that the caller can see, a last row on home reads "Runs by others" with their number. It opens their runs, grouped by person. A person with no team sees only their own runs on home and the rest behind that row.

## 14. Repository layout

Drivers live in public packages, not under `internal/`, because a Go program outside this module selects a driver by importing it.

```
cmd/fw/                 the binary and the drivers that it links
cli/                    the command line, public so that another binary reuses it
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
tools/builtin/          bash, files, http, memory, skill, clock, ask_user, send,
                        spawn_task, await_task
tools/mcp/              MCP client as a ToolSource, a decision point, and a Connector
tools/a2a/              A2A peers as a ToolSource
bus/nats/ bus/redis/
surface/native/         the native API for runs and agents, and its extension point
surface/a2a/ surface/axmeta/ surface/metrics/
surface/view/           the shapes that the native routes return
ax/                     runner contract: manifests, workspace setup, supervision
ax/guest/               the guest services of ax ssh
telemetry/              Prometheus registry, OpenTelemetry setup, slog
examples/               fw.yaml, ax manifests, observability, prompts, skills
docs/ARCHITECTURE.md    this document
Dockerfile              fw:static on distroless
Dockerfile.toolbox      fw:toolbox on Debian with git
```

The workbench architecture, section 1.3, gives the layout of the workbench.

## 15. Dependencies

The default binary carries these modules. A blank import in `cmd/fw/drivers.go` links every driver, and no driver sits behind a build tag. The ax manifest types are defined in the `ax` package from the YAML schema, so the binary does not import the ax module. Unencrypted HTTP/2 on one port comes from the standard library `http.Server.Protocols` field.

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
- The ax contract comes from the ax repository at the commit read on 2026-10-13. ax is pre-release and expects breaking changes.
- The A2A version is 1.0. The MCP revision is 2026-07-28. The Go MCP SDK from v1.7.0 targets that revision. It states that it keeps the deprecated features for at least twelve months, and the dual-era behavior depends on that.
- `fw` implements the ax runner contract itself instead of embedding the ax `runner` package (section 11.2).
- NATS is the recommended bus. Redis is the zero-new-infrastructure choice on an ax cluster.
