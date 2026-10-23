# featherweight-kernel

One static Go binary, `fw`, that makes one google/ax Task a durable agent. It runs the agent loop with a budget, a policy, and an append-only ledger. A run continues after a suspend, a restart, or a resume in a fresh sandbox, because the kernel rebuilds it from the ledger. `fw` also runs in a plain container or as a Go library.

`fw` serves the agent through a native HTTP API, an A2A server, and a Prometheus endpoint on one port. It accepts work from HTTP, A2A, or a message queue. Every component is an interface with one default, so you replace a component without a fork.

The workbench for people lives in its own repository, featherweight-workbench. It adds the page, the browser sign-in, the MCP server, the fleet view, and the agent builder on top of this kernel.

The design is in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). The [kernel architecture map](docs/KERNEL_MAP.md) diagrams the implemented components, run execution, and ax deployment, with links to the source. The configuration schema, with every key and its default, is [examples/fw.yaml](examples/fw.yaml). An example ax deployment is [examples/ax/reviewer.yaml](examples/ax/reviewer.yaml), and an example skill is [examples/skills/release-notes](examples/skills/release-notes).

## Build

Install Go 1.27 or newer and `make`. The Makefile calls `go` from your PATH. To use a different toolchain, set `GO`, for example `make GO=/opt/go/bin/go build`.

```
make build    # writes bin/fw
make test     # runs every test with the race detector
```

Container images: `make image` builds `fw:static` on distroless, and `make image-toolbox` builds `fw:toolbox` on Debian with git. Both contain the `ax-task-runner` symlink that ax expects.

Without a build tag the binary carries every driver. `TAGS=lean` builds the core alone, and each driver comes back with its own tag: `nats`, `redis`, `anthropic`, `openai`, `gemini`, `spiffe`, `oidc`, `metrics`, and `a2a`. A worker image for one fleet therefore builds with the drivers it uses, for example `make image-toolbox TAGS=lean,nats,openai,oidc`, at about half the size (docs/ARCHITECTURE.md section 15). The tag `memory_sqlite` adds the FTS5 index of the memory to either build.

## Start here

```
./bin/fw init      # writes fw.yaml, prompts/default.md, agents/, and skills/
export FW_MODEL=anthropic/claude-opus-5-5   # or set model in fw.yaml; there is no default model
export ANTHROPIC_API_KEY=...                # the credential of that provider
./bin/fw doctor    # checks the credential, the model, the drivers, and the skills
./bin/fw run "List the files in this directory and say which one is the largest."
```

A model must be set. The `fw.yaml` of `fw init` shows one commented `model` line for each provider, and `fw init` says so. Without a model, `fw` refuses every run, and `fw doctor` warns about it.

`fw run` prints one line per event to standard error and the final answer to standard output. When a tool needs an approval, it asks on the terminal. The flags shape the run without a configuration change. `-system` and `-i` take a text or `@path` to a file. `-tools` and `-skills` take globs. `-model`, `-effort`, `-time-zone`, `-max-turns`, `-deadline`, and `-max-tool-calls` set the rest. `-agent` selects a profile from `fw.yaml`, and `-json` prints the run object.

## Safeguards

Every run has a budget. It limits turns, tokens, cost, working time, tool calls in total, tool calls per response, and repeats of one tool call. A model that loops makes the same call with the same input again and again, or the same cycle of two or three calls. The kernel warns the model once and then ends the run. Every tool call has limits: 256 KB of input, 100 KB of output, and 10 minutes. A large file or a large response cannot flood the context window. A result that is too large keeps its head and its tail. A policy rule restricts what a tool call touches, for example `deny: ["files: path=*.env", "bash: git push*"]`. The defaults work. The keys `budget`, `tools.limits`, and `policy` in `fw.yaml` change them.

## Agents that create agents

The `spawn_task` tool starts a child run with its own task, prompt, tools, skills, model, and budget, and returns its answer. A model starts several children in one response, and they run at the same time. A child never has more than its parent: fewer tools, stricter rules, a smaller budget, and a bounded depth and count. When a child needs an approval, the question appears on the parent run, so the parent stays the one place to watch. With `spawn.driver: ax`, each child gets an ax Task of its own, acts as the same person, and the parent follows it through the atenet router; section 11.9 of the architecture has the details.

## Instructions and skills

A profile holds a system prompt, instruction texts, a tool allowlist, a skill allowlist, a model, a policy, and a budget. A request adds instructions for one run, or an inline profile that overlays the named one. A skill is a directory with a `SKILL.md` file under `skills/`. The system prompt lists the skills. If a task matches a skill, the model reads it with the `skill` tool. `fw skills` lists them.

## Long runs and memory

A long run does not outgrow the context window. Before each model call, the kernel estimates the size of the context from the usage that the provider reports. At 262,144 tokens, the model writes a summary of the run, and the summary replaces the history. If a model call fails because the context is full, the run gets a summary and one more try. A local model with a small window therefore needs no setting. `compaction.at` changes the size, and `compaction.model` sends the summary call to another model. A profile can set its own `compaction` block.

The `memory` tool keeps notes between runs as Markdown files, by default in one directory per agent profile under `memory/agents/`. At the start of a run, the first user message lists each note with its first line, inside a `<context>` block that the system prompt marks as data and not as instructions. The model therefore knows what it learned without a tool call, and a planted note never gains the authority of the system prompt. `tools.memory.scope: principal` gives each person their own notes in each profile, which a profile that more than one person runs wants, and it is the default inside an ax Task, where the runs of many people follow one another. The notes of one person stay out of the runs of another, but a run records what it read: anyone who can view the run reads the notes that the run read and wrote, in its ledger. `shared` gives every profile the same notes, and through the API only an admin writes them. A note holds at most 128 KB, a scope at most 2,000 notes, and a note name at most 8 parts, with no control character. Each change that a run makes appears in its ledger as `fw.memory.changed`, without the text, and each write through the API goes to the log with its caller.

The `recall` tool searches the notes, the earlier runs of the agent that the same person started, the files of the workspace, and the skills that the profile allows, and reads one record by its ref, such as `mem://agent/default/build.md` or `run://<id>`. A run without a principal sees only the runs without one. The routes `/v1/memory` of the native API search the memory and read and write notes, so a person, a script, or another harness can seed and curate the memory of an agent without a run. The build tag `memory_sqlite` adds a SQLite FTS5 index of the notes, for about 4 MB; without it, a search scans the notes. Section 7.8 of the architecture has the details.

An interrupted `insert` or `str_replace` needs approval before it runs again, because it can already have changed the file. Approval authorizes a rerun, which can repeat the edit. An interrupted `view`, `create`, `delete`, or `rename` runs again at once, so an unattended job does not wait for a person after a suspend.

## Time

The system prompt names the date and the time zone of the run. The `clock` tool gives the model the exact time, the working time of the run, and the time left before its deadline. If more than 10 minutes pass between two model calls, the kernel tells the model the new time and the gap. `time.zone`, or `FW_TIME_ZONE`, sets the zone, for example `America/Chicago`, and a profile sets its own with `timeZone`. A sandbox under ax has no zone of its own, so it uses UTC until you set one. The deadline counts working time, so the time that a run waits for an approval or a follow-up does not count.

## Identity and ownership

`auth.mode` names one authentication mode or a chain, for example `spiffe,token`. With `spiffe`, a JWT-SVID or an X.509-SVID from the Workload API or a bundle file makes a workload a principal. An MCP server entry with `auth.spiffe` calls another agent with a JWT-SVID instead of a shared secret. The `oidc` mode for people comes with the workbench. A profile carries an `access` block with owners, operators, and viewers, as subjects, globs, `group:<name>`, or `kind:<kind>`. The process-wide `access` block names the admins and the creators, and `access.default: closed` locks every agent that names no owner. The kernel enforces the roles on every surface, and a run belongs to whoever started it. `policy.external` hands every tool-call decision to a control plane behind an MCP tool or a bus subject; if the control plane does not answer, `fw` denies the tool call. The authorization of people by a control plane is a plug of the workbench (`workbench.access.external`), because a worker decides with the rules of `access` alone. `fw agents`, `fw whoami`, and `fw doctor` show the state from the command line.

## Acting for a person

By default, an MCP server acts as the agent itself, through one service account. Set `auth.oauth.grant: user` on a `tools.mcp` entry, and the entry acts for the person who started the run. The service then applies the rights of that person. The first call that needs the service pauses the run until the person connects. The kernel keeps no grant and runs no OAuth flow: a linked package gives it the tokens of a person through `fw.PersonTokens` and resumes the paused calls with `Agent.Connected`. The workbench is that package. It keeps the grants, serves the Connect card, the connection routes, and the callback, so this feature needs the workbench. In a fleet, the front keeps every grant, and a Task with `tokens.url` set gets the tokens of a person from the front, so no refresh token enters a sandbox. Section 7.11 of the architecture has the primitive, and section 11 of the workbench architecture has the flows and a Keycloak example.

## Serve

```
export ANTHROPIC_API_KEY=... FW_API_TOKEN=secret
./bin/fw serve
```

The one listener, `:8080` by default, serves:

| Path | What |
|---|---|
| `POST /v1/runs` and the rest of `/v1/*` | the native API, bearer token |
| `/.well-known/agent-card.json`, `/a2a/jsonrpc`, `/a2a/` | A2A 1.0 |
| `/metrics` | Prometheus, without authentication, so keep it inside the monitoring perimeter |
| `/healthz`, `/readyz` | health |

Start a run and wait for it:

```
curl -s -H "Authorization: Bearer secret" -H "Content-Type: application/json" \
     -d '{"input": "Say hello.", "instructions": "Answer in one sentence.", "wait": true}' http://localhost:8080/v1/runs
```

Follow the ledger of a run as server-sent events with `GET /v1/runs/{id}/events`, or on the command line with `fw events <id> --follow`. Each run has a title, such as "Fix checkout retries". It comes from the request, or else from the first sentence of the input. The workbench lets a model write one and records it on the run.

Each file ledger has one writer process. Another writer gets an ownership error; `fw events` can read while the server runs. A server that boots while another process holds the ledger, such as `fw run` through `ax ssh`, waits up to 30 seconds for it and then exits with a message that names the other holder; `fw run` and `fw replay` beside a server fail at once. Use separate homes and ledgers for workers. The file driver syncs accepted runs, external follow-ups, budget changes, approvals, and unsafe tool intents before acknowledging them. See [the durability contract](docs/ARCHITECTURE.md#43-storage) for the complete boundaries and recovery rules.

`maxQueuedRuns` bounds the backlog behind `maxRuns`; zero selects 64 extra admitted root drivers. A full queue refuses a new run: the native API returns HTTP 429 with `Retry-After`, and the A2A server returns the JSON-RPC server error -32000 with the reason `SATURATED` and `retryAfterSeconds` in its data, or HTTP 429 with `Retry-After` on its REST binding. A follow-up or an approval of an accepted run is never refused. A storage failure makes `/readyz` return 503 and blocks execution until storage is repaired and the process restarts; A2A answers it with the reason `STORAGE_UNAVAILABLE`. Open runs expose `blocked` alongside their last recorded state, and `GET /v1/status` reports `blocked: true` and the process idle since the failure, so a controller can suspend it.

## Observability

`/metrics` serves Prometheus metrics for runs, models, tools, approvals, and budgets. No label holds a person, a run id, a prompt, or a path. [examples/observability](examples/observability/README.md) holds a scrape configuration, alert rules, and a Grafana dashboard. The ledger stays the exact record of usage.

The same registry exports over OTLP. Task-quality metrics distinguish execution completion, model grading, and trusted verification, and track operator actions, recovery latency, confirmed duplicate effects, and priced cost per successful completion episode. Integrations report verification and confirmed effects through `Agent.ObserveQuality`; retries alone are not evidence of duplicate effects.

## Other commands

`fw tools` lists every tool, including the tools of the MCP servers and the A2A peers in the configuration. `fw card` prints the A2A agent card. `fw config` prints the resolved configuration with secrets masked. `fw env` prints every configuration variable and the value of each one that is set. `fw replay <run> --model gemini/gemini-3.8-flash` runs a recorded run again against another model with the recorded tool results, which is a regression test for a model change.

## Inside google/ax

Point a Task at the `fw:toolbox` image and leave `spec.command` empty. `fw` prepares the workspaces, serves on port 80, and waits for work. The metadata endpoints of the runner contract answer on `127.0.0.1:8099` inside the sandbox, at `AX_METADATA_URL`, as the stock runner does. `GET /v1/status` on port 80 gives the state of the agent at a glance. Any Task in the cluster is reachable through the atenet router:

```
curl -H 'ate-target-actor: default/reviewer' -H 'Content-Type: application/json' \
     -d '{"input": "Review pull request 42", "wait": true}' \
     http://atenet-router.ate-system.svc.cluster.local/v1/runs
```

Under ax, `fw` runs as root and starts `bash` and the stdio MCP servers as the tool user `nonroot`, uid 65532, which both images hold (`tools.user`). Some sandboxes take `CAP_SETUID` or another capability of section 7.12 from root, as gVisor under Agent Substrate does. There, the tools run as the user of `fw`, and the log says so. That user owns the workspace, but it cannot read or change `/workspace/.fw`, which holds the ledger and `fw.yaml`, and it cannot read the memory or the environment of `fw`. A shell gets no variable that holds a secret, and with `tools.files.perRun`, on by default under ax, each run starts in an empty directory of its own. A credential that the egress gateway adds to requests still reaches every process of the sandbox, the shell included, so a Task that reads untrusted input gets no shell. Section 7.12 of the architecture has the rules and that pattern.

To inspect a sandbox with `ax ssh`, set `spec.debug: true` on the Task. `fw` then serves the guest services of `ax ssh` on port 80, during the boot and after it:

```
ax ssh reviewer -- ls -la /workspace
ax ssh reviewer -- fw events <run-id>
```

Do not set `spec.debug` on a Task whose environment or workspace holds keys that other callers must not read. The guest services carry no authentication, and any caller that reaches port 80 can run a process in the sandbox.

One Task from one profile file, without a front, takes one verb each. The verbs dial `--server` and `--router`, so outside the cluster, forward the ports of `svc/ax-server` and `svc/atenet-router` first:

```
fw ax render -f agents/reviewer.yaml --name reviewer --image <image@sha256:...>   # print the Workspace and the Task, with placeholders for secrets
fw ax up -f agents/reviewer.yaml --name reviewer --image <image@sha256:...>       # apply, resume, and wait for /readyz through the router
fw ax status --name reviewer                                                       # the phase, the conditions, and GET /v1/status of the kernel
fw ax events --name reviewer <run-id>                                              # the ledger of a run through the router
fw ax down --name reviewer                                                         # delete the Task and its Workspace
```

`render` and `up` derive one egress host for each endpoint of the configuration. Inside a Task, `fw doctor` adds the lines `ax reach`, `ax trust bundle`, `ax tools user`, and `ax durable` (section 21 of the architecture).

See section 11 of the architecture for the boot sequence, suspend and resume, `ax ssh`, and the gaps in ax today.

## Configuration

`fw` reads `fw.yaml` from `$FW_CONFIG`, `./fw.yaml`, or `$FW_HOME/fw.yaml`, and then `FW_*` variables. A key path maps to a variable name by upper snake case: `bus.url` is `FW_BUS_URL`. Every key is reachable from the environment, maps and named lists included: `FW_TOOLS_MCP_GIT_ENDPOINT` sets the endpoint of the MCP server named `git`, and `FW_PROVIDERS_OPENAI_BASE_URL` points the OpenAI adapter at a local server. A value in the file expands `${NAME}` and `${NAME:-default}`. Profiles live under `agents:` or as files under `$FW_HOME/agents/`.

## Models

The selector `provider/model` picks the adapter: `anthropic/claude-opus-5-5`, `gemini/gemini-3.8-flash`, or `openai/<model>`. There is no default, and a paid model is never a built-in choice: `fw init` and `examples/fw.yaml` show the `model` line as a comment. A process without a model boots and serves, and refuses every run until `model`, `FW_MODEL`, or the profile of the run names one. The OpenAI adapter speaks the chat completions API to OpenAI, to Azure OpenAI, and to a local server such as vLLM or Ollama. The key `providers.openai` in `fw.yaml` holds the base URL, the headers, the TLS configuration, and the options.

## Layout

Every driver is a public package. A program that imports a driver registers it. The binary `fw` links these drivers:

- the models `model/anthropic`, `model/gemini`, and `model/openai`
- the tools `tools/builtin`, `tools/mcp`, and `tools/a2a`
- the buses `bus/nats` and `bus/redis`
- the authentication `authn/spiffe`
- the surfaces `surface/native`, `surface/a2a`, and `surface/metrics`

The packages `ax`, `surface/axmeta`, `config`, `telemetry`, and `surface/view` support them. The kernel and the contract are in `fw`, and the verbs of the command line are in `cli`.

## Building on the kernel

A program that adds surfaces or tools links the kernel packages and calls `cli.Run`, as `cmd/fw` does. Three hooks let such a program extend the kernel without a change to it:

1. `fw.RegisterDefaults` changes the default configuration in `init`, for example to turn on a surface.
2. `native.Extend` adds routes to the native surface, behind the same authentication as the run routes.
3. The function variables of package `cli`, such as `cli.ServeMCPStdio`, add what a verb needs.

The workbench is built this way.

## License

MIT. See [LICENSE](LICENSE).
