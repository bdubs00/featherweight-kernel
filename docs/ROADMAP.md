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
2. **Per-run execution isolation:** Enforce one active run at a time in an AX worker Task (`maxRuns: 1` under AX, or isolate `bash` shell sessions per run id) so concurrent runs never share a mutable shell session or collide in `/workspace`.
3. **Durable intent before non-idempotent tools:** Call `fsync` in `FileLedger.Append` on `fw.tool.requested` whenever the tool is not idempotent, before the tool executes. A snapshot or crash during tool execution will then always trigger the `"rerun after resume"` approval gate on restore.

### Stage 2: Zero-cost waiting (idle signal and auto-suspend)

1. **Kernel idle state:** Report `idle` and `idleSince` in `GET /v1/status` and on the egress stream when no run works and every open run waits in `input-required` (for an approval, a question, or an OAuth connection) or has finished.
2. **Front suspend controller:** Add a controller in `fw-workbench` that watches the fleet state and calls `SuspendTask` on the AX API when a worker Task (including a finished or waiting child Task from `spawn.driver: ax`) stays idle past a grace period. The `atenet-router` wakes the Task on the next approval, message, or run request.

### Stage 3: End-to-end fleet acceptance and record retirement

1. **Cluster acceptance run (`featherweight-lab`):** Run a multi-task batch job end to end on the k3s/AX cluster as a signed-in person from Claude Code: `enqueue` through the front, execution across AX workers, OAuth token brokering to an upstream MCP server, OTLP export to the collector, auto-suspend on approval, and wake-on-approve through `atenet-router`.
2. **Retire retained member records:** Once all workers run the bare `fw` binary and report through the AX API and `bus.egress`, remove the periodic member heartbeat record and `fw.Retainer`.

### Stage 4: Ledger and workspace branching (`fw fork`)

1. **Kernel fork primitive:** Add `Agent.Fork` and `POST /v1/runs/{id}/fork` to branch a run at sequence number `seq`. Copy the ledger events up to `seq` into a new run id so the branch keeps the conversation history and the provider prompt cache prefix.
2. **Speculative child Tasks:** Combine `Fork` with `spawn.driver: ax` so a person or an agent can try alternative prompts, models, or tool choices in parallel AX Tasks and compare the resulting `fw.file.changed` diffs and check outputs.

### Stage 5: Final boundary and identity cleanup

1. **Control-plane authorization at the front:** Move `access.external` and multi-user role checks beyond single-Task caller verification out of the worker kernel and into the front.
2. **Workload identity for workers:** Replace the shared static fleet token between workers and the front with actor JWTs once Substrate ships `ate-idp-server`.
3. **Lean worker builds:** Add build tags so the worker image links only the drivers it uses inside AX.

## 5. Rules for every stage

- Add a UI view only when the kernel cannot export the same fact to Prometheus, Grafana, an OpenTelemetry collector, or the AX API.
- Every stage ends with `go build ./... && go vet ./... && go test ./...` green in both `featherweight-kernel` and `featherweight-workbench`.
- Any stage that changes the AX runner contract, the spawner, or the fleet runs the `featherweight-lab` suite against the cluster before the next stage begins.
