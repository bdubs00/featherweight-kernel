> Moved on 2026-10-17 from the root of featherweight-agent. The text below is unchanged. Its references live elsewhere now: `docs/ARCHITECTURE.md` is the file of that name in this repository, and `REVIEW.md`, `CURRENT_STATE_REVIEW.md`, and `PRODUCT_DIRECTION.md` are under `docs/` in featherweight-workbench. The order of work after the split is `docs/ROADMAP.md`.

# Kernel first: a plan to split featherweight-agent

Date: 2026-10-16. Nothing in the code changed. This plan reads branch `user-grants` at 854ebc4, `docs/ARCHITECTURE.md`, `REVIEW.md`, and `CURRENT_STATE_REVIEW.md`. It also reads AX at ac23328 from the e2e cache. It compares upstream AX at 8d2ccf3 and Substrate v0.5.0, as of 2026-10-16.

## 1. The answer

`fw` is two products in one process. The first is a kernel that makes one AX Task a durable agent with a budget, a policy, and a ledger. The second is a workbench for people: a dashboard, a builder, sign-in, teams, a fleet view, connections to services, and a store of agents. Both run inside the sandbox today.

The workbench does control-plane work. It holds the refresh tokens of people, decides team access, and stores agent definitions. Upstream AX now states in `SECURITY.md` (commit e5fbde7) that the sandbox and the runner are untrusted. Substrate also snapshots and restores the sandbox, so state in it can go back in time. Section 11.5 of the architecture already records one result: a restore brings back an old refresh token, and the person must connect again.

The split gives each product one place:

1. The kernel stays in this module. It does one job: it makes one AX Task a durable, bounded, observable agent.
2. The workbench moves to a second module. It runs outside the sandboxes as a normal Deployment. It reads Tasks from the AX API and reaches each Task through the atenet router.

About 15,400 of the 49,300 non-test Go lines (31 percent) leave the kernel. The 2,547-line page leaves with them. The appendix lists the files.

## 2. Where fw repeats AX or works against it

| Part of fw | What AX has | Decision |
|---|---|---|
| Workspace setup in `ax/prepare.go`: files, a go-git clone, the skills directory, markers | The stock runner does the same. The AX roadmap moves setup into a separate setup actor. | Keep it as a thin adapter and add nothing. When the setup actor ships, delete it. |
| The `bootstrap` profile for a binding `goal` | Upstream f0f16ff marks `WorkspaceRef.goal` with "Consider removing goal". | Keep it as it is. Put no more work into it. |
| The fleet: `fleet/`, member records on the bus, forwarding between members | `ListTasks`, `WatchTask`, phases, and conditions | Move the view out. The workbench lists Tasks from AX. The kernel only publishes a summary of its runs. |
| Many agents in one process: the profile store, access roles per agent, teams | The Task is the unit of isolation. AX calls it cheap to create, isolate, suspend, and throw away. | Run one agent definition per Task. To create an agent, the workbench applies a Task and a Workspace. |
| `spawn_task` children in the same process | A task tree, where each node is a Task | Keep children in the process for cheap subtasks. Add an AX driver that starts a child Task (section 3). |
| Provider settings in `providers:` | The `Model` resource. AX does not deliver it to the runner yet. | In AX mode, take the model from the environment or the `Model`. Do not add provider features. |
| Outbound JWT-SVIDs in `authn/spiffe` | SPIFFE for Tasks is on the roadmap. Unreleased Substrate adds `ate-idp-server`, which signs actor JWTs. | Keep the plug. When Substrate releases it, add an actor-JWT driver. |
| Acting for a person: `fw/connect.go`, `tools/mcp/person.go`, `tools/mcp/user.go`, grants under `/workspace` | The egress policy of Substrate adds credentials at the gateway. | Move it out. A refresh token belongs in the workbench or at the egress gateway, not on a volume that a restore rolls back. |
| Browser sign-in, the dashboard, the builder, model titles, home, and the result view | AX has no user interface. | Move it to the workbench. This is not a copy of AX, but it is not kernel work. |
| `/metadata/v1alpha1/fw/status` in the shape of `TaskStatus.pendingApproval` and `usage` | Upstream f0f16ff removed both fields with the note "Nothing set or read them". | AX keeps agent meaning out of the Task. That layer belongs to fw. Keep the status endpoint and make it the contract that the workbench reads. |
| AX metadata on port 80 | Upstream 22954f6 moved the metadata to `127.0.0.1:8099`. Port 80 now serves only `/healthz`, `/readyz`, and the guest services. | Follow upstream (section 6, step 1). |
| Guest services in `ax/guest` | The runner contract asks for them. | Keep them. `ax ssh` now sends standard input (upstream 92d297e). |

## 3. What the kernel adds that AX does not have

These items are the value of the kernel to AX, in order of value.

1. A run continues after a suspend. Substrate restores the files of `/workspace` and starts a new process. The ledger turns that restore into the same run: the same history, the same pending approval, and a deadline that counts only working time. No other part of AX gives this. Put it first in the README.

2. The kernel knows the exact idle state of the agent. The AX roadmap plans idle detection from processes, I/O, network traffic, and sessions. The kernel knows the exact state: no run works, and every open run waits for a person. Publish `idle`, `idleSince`, and what each run waits for in the status endpoint. A small controller in the workbench then calls `SuspendTask`, and the router wakes the Task on the next request. A run that waits two days for an approval then uses no worker while it waits. A suspend at an idle point also has no tool call in flight, so the window where events are lost after the snapshot holds nothing.

3. Delegation has limits. AX gives the task tree but no rules for it. The kernel already caps a child by the tools, the policy, the budget, and the deadline of every ancestor. Put `spawn_task` behind a `Spawner` plug. The default stays in the process. An AX driver calls `CreateTask` with the image, the environment, and the bindings of the parent, and passes the caps as `FW_*` variables. The parent talks to the child over A2A through the router. Section 11.9 of the architecture already plans this.

4. Budgets and policy apply where the tokens are spent. The AX roadmap lists token and timeout budgets and approval policies for a Task. AX can declare them, but only the runner sees each model call and each tool call. Every budget and policy key is already settable through `spec.env`. Document that as the AX method. If AX adds Task fields for them, map those fields.

5. The ledger is a trajectory. The AX roadmap wants trajectories from the runner layer. The ledger is already a redacted CloudEvents record of prompts, responses, tool calls, and lifecycle events. Add an observer that exports each event as an OTLP log record, so that any collector gets it with no change to AX.

6. Replay tests a model change. `fw replay` runs a recorded run against another model with the recorded tool results. AX has nothing like it.

7. A run can branch, later. The AX roadmap plans branches of a Task. A fork of a ledger at one position gives the branch a coherent history and a new run id. This is the base of the "Try it before you do it" idea in `PRODUCT_DIRECTION.md`. Do it after the items above.

## 4. Target layout

The kernel keeps the module path and the git history. The workbench becomes a nested module with its own `go.mod`.

```
featherweight-agent/        module github.com/bdubs00/featherweight-agent   (the kernel)
  fw/                       contract and kernel
  fw/fwtest/                fake models and scripted helpers (new)
  api/                      wire types and a Go client for the native API (new)
  model/                    anthropic, openai, gemini
  tools/                    builtin (without propose_agent), mcp (service identity only), a2a
  surface/                  native (runs only), a2a, metrics, axmeta
  ax/  ax/guest/            the runner contract
  config/  telemetry/
  bus/                      nats, redis: optional intake and egress
  authn/spiffe/
  cmd/fw/
  workbench/                module github.com/bdubs00/featherweight-agent/workbench (new)
    dashboard/              the page and the home, result, and builder projections
    mcp/                    the MCP server that people and desktop hosts use
    signin/  teams/  connections/  agents/  fleet/
    cmd/workbench/
```

The workbench MCP server is the "spin up agents in AX" piece of your first intent. Its `agent_put` applies a Task and a Workspace through the AX API. Its `run` tool posts to the Task through the router. The local Claude Desktop setup (`fw mcp` over stdio) becomes `workbench mcp` pointed at a local `fw serve`.

## 5. Rules for the boundary

1. The workbench imports only `api/` from the kernel. If the workbench imports another kernel package, a test in the workbench module fails.
2. `api/` holds `Event`, `Run`, `RunRequest`, the event data types, and an HTTP client. `fw` keeps its exported names as aliases, for example `type Event = api.Event`. No signature in `fw` changes.
3. The workbench never wakes a Task to draw a list. It reads `ListTasks`, `WatchTask`, and the run summaries on the bus. Only a person who opens a run or acts on it makes the workbench wake a Task.
4. The kernel trusts a caller through `Authn`: a token or a SPIFFE identity. The workbench authenticates people and passes the person as the principal of the run. The kernel records that principal in `fw.run.queued`, as it does today.
5. A feature that needs more than one Task, or a person, goes in the workbench. A feature that needs a model call, a tool call, or the ledger goes in the kernel.

## 6. Kernel cleanup, in order

1. Follow upstream AX. Serve the AX metadata on `127.0.0.1:8099` and point `AX_METADATA_URL` there (22954f6). Read the YAML keys `createTime` and `updateTime` and the type `ResourcesSpec` (f0f16ff). Remove the stdin gap from sections 11.11 and 11.12 and from the README (92d297e). AX now pins Substrate `v0.4.1-0.20261009152331-f6a1dc72a35d`, so the e2e patch to v0.4.0 needs a review. Under Substrate v0.5.0, an actor with no egress policy gets no DNS, so the e2e egress policy must name every host that fw calls.
2. Make `titles: input` the kernel default again. Model titles move to the workbench. The workbench asks for a title the first time that it shows a run. This removes three defects of `CURRENT_STATE_REVIEW.md` from the kernel: the late title that leaves ledger files open, the test race of the title default, and the lost usage of an empty title.
3. Fix resume. It classifies a run by its last event type and not by its projected state (`fw/resume.go:56`). This defect does not depend on titles.
4. Remove the multi-user layer from `fw/`: `teams.go`, `fleet.go`, the runtime profile store, `grants.go`, `connect.go`, and the access roles beyond "who can call this Task". `access.external` moves to the workbench. `policy.external` stays, because it governs tool calls, and AX governance plugs in there.
5. Narrow the native API to runs. Keep runs, events, transcript, messages, approvals, cancel, files, tools, skills, and a read-only `GET /v1/agents`. Move home, builder, fleet, the agent writes, whoami, usage, connections, `/ui`, and sign-in. Give the status endpoint a kernel path such as `/v1/status`, because `/metadata/` now names the AX loopback endpoints.
6. Fix the kernel defects that the reviews found. Give each run its own bash shell. Count children against a limit of active work. Add one budget for a whole delegation tree. Before a call that is not idempotent, write the call to the ledger with `fsync`. On resume, mark the outcome of such a call unknown, and do not ask the policy again.
7. Make the binary lean by default. Section 15 names six build tags: `kafka`, `redis`, `gemini`, `guest`, `vault`, and `gsm`. The code has none of them. A build without gemini, nats, redis, oidc, the fleet, and the MCP surface took the stripped binary from 56.6 MB to 41.7 MB. That build still links the dashboard through `surface/native`.
8. Split `docs/ARCHITECTURE.md` (2,047 lines) into a kernel document, an AX document, and a workbench document. Remove "It does not describe code that exists yet" (section 1) and "Nothing in this document is compiled or tested yet" (section 17). Put section 5.6 before section 5.7.
9. Shrink `fw.Config`. It has 30 top-level keys. The keys `access`, `fleet`, `grants`, `titles`, `mcp`, and the client fields of `auth.oidc` leave with the workbench.

## 7. Changes to propose upstream

On 2026-10-14 you chose to change only fw at that stage. These proposals remove copies instead of working around AX. Each one is optional.

1. Add an `http.Handler` field to `runner.Config`, so that a runner adds its own routes to port 80. fw then embeds `runner.Run` and deletes `ax/prepare.go`, `ax/supervise.go`, `ax/stash.go`, and go-git. That is about 900 lines.
2. Stop the process before the snapshot, or kill it while it is paused. Under Substrate v0.4.0, your notes record a pause, a save, an unpause, and then `SIGKILL`. Events written after the save are lost, but their side effects are not.
3. Let a runner report a condition, such as `WaitingForInput`, so that `ax get tasks` shows it. This replaces the two fields that f0f16ff removed, without agent meaning in the schema.
4. Accept an idle signal from the runner for the planned automatic suspend.

## 8. Decisions for you

1. Nested module or separate repository. A nested module keeps one place for the e2e loop and makes the boundary a compile error. If other people adopt the kernel, a separate repository is better. Start with the nested module.
2. The MCP server. Moving it out of the kernel is the cleaner split. An MCP host needs one URL, and a Task has no URL of its own without the router header. The cost is that a local desktop setup runs two processes instead of one.
3. Several profiles in one Task. Keep them as child definitions for `spawn_task` and for local use. Stop presenting them as separate agents for separate people.
4. The branch `user-grants` merged as PR #22 on 2026-10-16. No pull request is open, so the split strands no work.

## 9. Order of work

1. Freeze new product features until step 4 is done.
2. Do step 1 of section 6, because AX changed under the e2e loop.
3. Create `api/` with the aliases and the client. Create the workbench module and move the files of the appendix. At first the workbench talks to one `fw` process, as the page does now, so nothing breaks.
4. Switch the workbench to the AX API for lists and for agent creation.
5. Build the kernel value of section 3, items 2 to 5: the idle signal and the suspend controller, the AX `Spawner`, the trajectory export, and budgets from the Task.
6. Do steps 2 to 9 of section 6 along the way. Each step ends with `go build ./... && go vet ./... && go test ./...` green in both modules.

## Appendix: files that leave the kernel

These counts are non-test lines on 854ebc4. Their tests move with them.

| Files | Lines |
|---|---|
| `surface/dashboard/*.go` and `index.html` | 3,104 and 2,547 |
| `surface/mcp/*.go` | 4,189 |
| `surface/native/`: `builder.go`, `connect.go`, `fleet.go`, `manage.go`, `signin.go` | 1,969 |
| `fleet/*.go` and `telemetry/fleet.go` | 1,382 |
| `authn/oidc/*.go` | 868 |
| `fw/`: `connect.go`, `profiles.go`, `fleet.go`, `grants.go`, `title.go`, `teams.go` | 1,752 |
| `tools/mcp/person.go` and `tools/mcp/user.go` | 1,582 |
| `tools/builtin/propose_agent.go` | 198 |
| `cmd/fw/internal/cli/identity.go` | 342 |

`fw/external.go` and `fw/authz.go` split between the two modules. They are not in the total.
