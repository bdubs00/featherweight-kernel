# Roadmap

Date: 2026-10-17. This document orders the work for `featherweight-kernel` (`fw`) and `featherweight-workbench` (`fw-workbench`). The product thesis is in `docs/PRODUCT_DIRECTION.md` of the workbench repository.

## 1. The architecture rule

The kernel holds what one agent in one AX Task needs: the turn loop, the append-only CloudEvents ledger, the model and tool drivers, the budget and policy guards, and the AX runner contract.

A feature for people, for many agents, or for many Tasks belongs in the workbench (`fw-workbench`), which runs outside the sandboxes as a Deployment. When a choice exists between a view inside `fw` and an export to an external system, `fw` exports. Prometheus, Grafana, an OpenTelemetry collector, NATS, and the AX API are those systems.

## 2. The shape of a fleet

A fleet has two kinds of processes:

1. **A worker is a bare kernel (`fw`).** It runs as PID 1 in one AX Task with no workbench code. It pulls run requests from `bus.intake`, publishes lifecycle events to `bus.egress` (narrowed by `bus.egressTypes`), requests short-lived access tokens of people from the front at `tokens.url`, and pushes metrics, ledger logs, and traces over OTLP (`OTEL_EXPORTER_OTLP_ENDPOINT`). It publishes no member record and serves no human UI.
2. **A front is a window (`fw-workbench`).** It runs as a Deployment outside the sandboxes with no agents of its own. It reads the Task list from the AX API (`workbench.fleet.atespaces`) and the state of every run from the bus egress stream (`workbench.fleet.egress`), and it builds the fleet view in memory with the kernel projection. It serves people over MCP (`/mcp`) and the page (`/ui`), brokers OAuth tokens (`POST /v1/tokens`), places batch work on the intake queues (`enqueue`), forwards interactive actions to the owning Task through `atenet-router`, and suspends idle Tasks. The front never polls a sleeping Task through the router; it reads a running Task once at start and lives on the egress stream after that.

## 3. Completed foundation

Stages 1 through 4.5 completed on 2026-10-17:

- **Repository split:** Separated `featherweight-kernel`, `featherweight-workbench`, and `featherweight-lab`.
- **Upstream AX alignment:** Moved AX metadata endpoints to loopback `127.0.0.1:8099`, added `GET /v1/status` on port 80, updated manifest types to AX commit `8d2ccf3`, and enabled stdin over `ax ssh`.
- **Kernel hollowing:** Moved teams, model-generated titles, profile writes, the fleet view, and the OAuth grant store out of `fw` into `featherweight-workbench` behind clean kernel hooks (`fw.RegisterExtension`, `fw.RegisterRunAnnotator`, `fw.RegisterService`, `Agent.Annotate`, `Agent.SetProfiles`, `Agent.SetPersonTokens`, `Agent.Connected`, and `native.Extend`).
- **Token broker (`tokens/`):** Kept all user OAuth refresh tokens on the front; workers fetch short-lived access tokens for a run via `POST /v1/tokens` and resume paused calls on `fw.connection.connected`.
- **AX child spawner (`ax/spawn.go`):** Put `spawn_task` behind `fw.Spawner`; with `spawn.driver: ax`, each child run starts in its own isolated AX Task with attenuated tools, policy, budget, and working-time deadline.
- **Platform-sourced fleet and batches:** Added `workbench.fleet.atespaces`, `workbench.fleet.egress`, `workbench.fleet.intake`, the MCP tools `enqueue` and `batch`, the `fw.batch` metadata key, and OTLP push export (`telemetry/otlp.go`).

## 4. Next stages, in priority order

### Stage 1: Harden the worker inside AX

1. **Resilient bus startup under AX:** Connect to NATS and Redis with background backoff instead of exiting PID 1 when the bus is unreachable at boot. AX boots a Task before its Substrate egress policy takes effect, and golden template boots have no egress policy; background retry stops the startup crash loop.
2. **Per-run execution isolation:** Done on 2026-10-18. `maxRuns` defaults to 1 under AX, so one run at a time holds `/workspace`, and the `bash` tool keeps one shell per run and closes it when the run ends (`fw.RunCloser`), so runs never share a shell even where `maxRuns` is raised.
3. **Durable intent before non-idempotent tools:** Call `fsync` in `FileLedger.Append` on `fw.tool.requested` whenever the tool is not idempotent, before the tool executes. A snapshot or crash during tool execution will then always trigger the `"rerun after resume"` approval gate on restore.

### Stage 2: Zero-cost waiting (idle signal and auto-suspend)

Done on 2026-10-18.

1. **Kernel idle state:** `GET /v1/status` reports `idle` and `idleSince` when no run is queued or running: every open run waits for input, is suspended, or has finished. The egress stream already carries the transitions, the terminal and the `fw.approval.requested` events of every run, so the front derives the same state from the stream and reads the body once before it acts.
2. **Front suspend controller:** The fleet of `fw-workbench` watches the Tasks of `workbench.fleet.atespaces` and calls `SuspendTask` on the AX API when a Task, a child Task of `spawn.driver: ax` included, stays idle past `workbench.fleet.suspendAfter` (default ten minutes). The `atenet-router` wakes the Task on the next approval, message, or run request, and `enqueue` wakes the suspended Tasks that serve the profile of a job, because a sleeping Task reads no queue.

### Stage 3: End-to-end fleet acceptance and record retirement

1. **Cluster acceptance run (`featherweight-lab`):** Done on 2026-10-18 for the token path: `e2e/fleet/batch.py` sends a job through agentgateway with `enqueue`, follows it with `batch`, and checks that every run finished on an AX worker, acts as the sender, and carries the batch; `e2e/fleet/ax/verify.sh` covers the auto-suspend of an idle Task, the wake-on-approve through `atenet-router`, and the phase that follows a wake; `e2e/fleet/check-export.sh` reads the OTLP export back from Prometheus and Jaeger. The signed-in path is verified too: `e2e/fleet/verify-oidc.py` runs, approves, and lists as two people through the OIDC front, and the Task accepts their tokens. The broker path is verified too, on 2026-10-18: `e2e/fleet/connect.py` runs an agent on a Task as a signed-in person, the run pauses with a connect request for an OAuth-protected MCP server, the person connects on the front, the front keeps the grant, the Task gets the token of the person from the broker, and the server answers with the name of the person; another person gets a connect request of their own. The lab authorization server and the server are one program, `e2e/fleet/oauthlab`.
2. **Retire retained member records:** Done on 2026-10-18. The member record, the heartbeat, and `fw.Retainer` are gone, with the retained events of every bus driver. The front reads the ax API and the egress stream only, a worker publishes nothing about itself, and a member outside ax is visible through its events but has no address.

### Stage 4: Ledger and workspace branching (`fw fork`)

1. **Kernel fork primitive:** Done on 2026-10-18. `Agent.Fork` and `POST /v1/runs/{id}/fork` branch a run at the sequence number `seq`: the branch keeps a copy of the events to that point, with the conversation and the prompt cache prefix of the provider, carries `fw.forkOf` and `fw.forkSeq`, goes on as a resumed run does, and takes a new message or another model (the kernel architecture, section 4.7).
2. **Speculative child Tasks:** Done on 2026-10-18. `Fork` with `spawn` runs the branch in a child Task through the `ax` driver, which is a `Forker`: it creates the Task as `spawn_task` does, and the caller gets the branch at once, queued on the new Task, because a Task takes longer to boot than a gateway lets a request wait. The kept events follow in the background through `POST /v1/runs/fork`. The source records the branch as a child, so a person follows it from the front, and each branch has the workspace of its own Task. A branch that its Task never takes, or whose source restarted before the Task took it, is recorded in the source Task as failed with the reason, and the empty child Task is deleted. The MCP tool `fork` of the workbench gives it to people. Verified on the homelab on 2026-10-18 by `e2e/fleet/fork.py` of the lab: a branch in the same Task and a branch in a child Task, with the source unchanged, the child Task in the fleet, and the branch under its source in the view. Two facts of the platforms shaped the lab: ax creates every Task suspended until a caller resumes it, and Substrate gives a new actor no egress policy while ax had no field for one. The lab therefore runs ax with a patch, proposed upstream for google/ax issue 449 from the branch `feat/task-egress-policy` of `github.com/bdubs00/ax`, that adds `spec.egress` to the Task and creates the policy of the actor before the first resume; the kernel passes the egress of a parent Task to its child, and the modules follow the patched ax through a replace directive until the change lands. Verified on the homelab on 2026-10-18: ax created the policy of each member and of a child Task from `spec.egress` before their first run, and the whole lab suite passed on it.

### Stage 5: Final boundary and identity cleanup

1. **Control-plane authorization at the front:** Done on 2026-10-18. `access.external` left the kernel. The front hands the authorization of people to a control plane with `workbench.access.external` (the workbench architecture, section 12), through `fw.WithAuthzFrom`, and a worker decides with the rules of `access` alone: the static token of the fleet, the person who started a run, `access.admins`, and the access block of a profile. `policy.external`, the decision protocol, and the transports stay in the kernel.
2. **Workload identity for workers:** Blocked on Substrate as of 2026-10-18. Substrate v0.4.0 defines `MintActorJWT` on its API and an egress rule effect `actorJwt` that would have the egress gateway put a Substrate-issued JWT (`sub: actor/<atespace>/<name>`, verified by OpenID Connect Discovery against its issuer) on a request of a worker, but the gateway does not mint one yet (`cmd/atenet/internal/router/egress/credentials.go` carries the TODO) and the API server serves no discovery document for the issuer. When it does: the front and the workers accept the issuer with the `oidc` driver, given a second issuer with the kind `workload`, the egress rules for the front and the router carry the `actorJwt` effect, and the static fleet token goes.
3. **Lean worker builds:** Done on 2026-10-18. `cmd/fw/driver_*.go` puts each driver behind a build tag: `-tags lean` builds the core alone, and `nats`, `redis`, `anthropic`, `openai`, `gemini`, `spiffe`, `oidc`, `metrics`, and `a2a` add one driver each (the kernel architecture, section 15). The `oidc` driver moved into the kernel for it, because a worker verifies the tokens of the people who reach it through the front. The lab builds its worker image with `lean,nats,openai,oidc` and runs the Tasks on the kernel binary, not on `fw-workbench`; every lab check passed on those workers on 2026-10-18, after the front learned to build the result and the transcript of a run from the events of its owner, because a kernel worker serves no view for people.

## 5. Rules for every stage

- Add a UI view only when the kernel cannot export the same fact to Prometheus, Grafana, an OpenTelemetry collector, or the AX API.
- Every stage ends with `go build ./... && go vet ./... && go test ./...` green in both `featherweight-kernel` and `featherweight-workbench`.
- Any stage that changes the AX runner contract, the spawner, or the fleet runs the `featherweight-lab` suite against the cluster before the next stage begins.
