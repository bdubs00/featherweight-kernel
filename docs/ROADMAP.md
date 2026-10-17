# Roadmap

Date: 2026-10-17. This document orders the work after the split of featherweight-agent into this repository and featherweight-workbench. The plan behind the split is [KERNEL_SPLIT.md](KERNEL_SPLIT.md). The reviews that found the defects named below are under `docs/` in the workbench repository.

## The rule

The kernel holds what one agent in one ax Task needs. A feature for people, for many agents, or for many Tasks goes to the workbench. When a choice exists between a view inside `fw` and an export to a system that exists, `fw` exports. Prometheus, Grafana, an OpenTelemetry collector, and the ax API are such systems.

## The fact that orders the work

The workbench binary is still a runner. It serves inside the sandbox, and it imports 18 packages of the kernel. KERNEL_SPLIT.md, section 1, puts the workbench outside the sandboxes as a Deployment. That Deployment reads Tasks from the ax API and reaches each Task through the atenet router. Until the workbench runs that way, the split is a move of code. The stages below make the split real.

## Stage 1: bank the work

Done on 2026-10-17. Both repositories are on GitHub. The planning documents moved into `docs/` of each repository. The e2e loop moved into its own repository, featherweight-lab, next to the other two. It tests both binaries over the wire and holds the setup of one machine, so it belongs to neither.

## Stage 2: follow upstream ax

The kernel part is done on 2026-10-17. The lab part, the new pins of the installer, waits for a run against the homelab cluster.

Upstream google/ax moved twelve commits past ac23328, the commit that the e2e loop pinned. Three commits change the runner contract.

1. Commit 22954f6 serves the metadata endpoints on `127.0.0.1:8099`. Port 80 serves only `/healthz`, `/readyz`, and the guest services. Move `surface/axmeta` to a loopback listener and point `AX_METADATA_URL` there. Give the fw status body the path `/v1/status` on the main listener, so the workbench reads it through the router.
2. Commit f0f16ff renames `creationTimestamp` to `createTime`, `lastTransitionTime` to `updateTime`, and `ResourceReqs` to `ResourcesSpec`. It removes `pendingApproval` and `usage` from `TaskStatus`. Update the YAML types in `ax/` and section 11.8 of `docs/ARCHITECTURE.md`.
3. Commit 92d297e forwards standard input in `ax ssh`. Remove the gap from sections 11.11 and 11.12 of `docs/ARCHITECTURE.md` and from `README.md`.

The lab installer now pins ax 8d2ccf3 and the Substrate commit that it names, f6a1dc7, and applies a patch only when one exists for the chosen Substrate version. The next `make e2e-k3s-install` rebuilds Substrate and ax on the cluster, and the next `make e2e-k3s` proves the kernel against them. Substrate v0.5.0 denies DNS to an actor without an egress policy, so a later move to v0.5.0 must name every host that `fw` calls in the egress policy.

## Stage 3: move the hooks of the workbench out of fw

The package `fw` still holds about 2,800 lines for the workbench: `fleet.go`, `grants.go`, `connect.go`, `teams.go`, `profiles.go`, `title.go`, and parts of `external.go` and `authz.go`. Move them in this order, the smallest and least connected first.

1. Teams: `teams.go`, `Agent.TeamsOf`, and the teams in `fw.run.queued`. Done on 2026-10-17. The move added two hooks to the kernel for every later step: a claimed configuration block (`fw.RegisterExtension`) and the run annotator (`fw.RegisterRunAnnotator`). `GET /v1/whoami` moved to the workbench with it.
2. Model titles: `title.go` and `fw.TitlesModel`. Fix the resume classification in `fw/resume.go` at the same time. It reads the last event type instead of the projected state. Done on 2026-10-17. The kernel gained `Agent.Annotate` and the event `fw.run.annotated` in their place, the rebuild reads the state of a run, and the file ledger closes the file of a finished run again after an annotation.
3. Profile writes: `PutProfile`, `DeleteProfile`, and the routes `PUT` and `DELETE /v1/agents/{name}`. Done on 2026-10-17. The kernel keeps `Agent.SetProfiles` for a package that writes profile files, and the workbench package `agents` owns the writes, the ownership rule, the routes, the MCP tools, and the verbs `put` and `delete`.
4. The fleet: `fw/fleet.go`, `surface/native/fleet.go`, `surface/view/fleet.go`, `telemetry/fleet.go`, and the key `fleet` of the configuration. Done on 2026-10-17. The kernel gained services, the relay of the native surface, collectors of the metrics surface, and doctor checks in their place, and the workbench fleet reads `workbench.fleet` and `FW_WORKBENCH_FLEET_*`.
5. Grants and connections: `connect.go`, `tools/mcp/person.go`, and `tools/mcp/user.go`. The kernel keeps one primitive: a run can pause until a credential arrives from outside. The OAuth flows and the grant store go to the workbench. Done on 2026-10-17. The kernel gained `fw.PersonTokens`, `Agent.SetPersonTokens`, and `Agent.Connected`, and the workbench package `connections` reads `workbench.grants` and `FW_WORKBENCH_GRANTS_*`. The file `fw/grants.go` stays, because it projects the tool grants of a profile and has nothing to do with OAuth.

After each move, `fw.Config` loses the matching key. The keys `fleet`, `grants`, and `titles` are gone. The keys `access` and `mcp` remain. `access.external` and the roles beyond "who can call this Task" are the next move, and the server surface `mcp` of the workbench still reads its block from the kernel configuration.

## Stage 4: the shape of a fleet

KERNEL_SPLIT.md, section 7, defines a worker and a front. This stage makes a fleet take that shape, in the order below. One use case orders it. A person in Claude Code splits a large job into many pieces and sends them to the fleet, every run acts as that person at the services behind agentgateway, and nothing runs on the laptop.

1. The token broker. Done on 2026-10-17: the kernel package `tokens` with the key `tokens`, the routes under `/v1/tokens` on the front, and the notice `fw.connection.connected` on the bus. The front keeps every grant and runs every refresh, and a worker gets the access token of a person for a run of that person. The refresh token never enters a Task.
2. The `Spawner` plug with the ax driver. Done on 2026-10-17: `spawn.driver: ax` gives each child of `spawn_task` a Task of its own, the ledger of the parent records the child in `fw.run.child`, and a trusted caller names the person of a run on the native API.
3. The fleet from the platforms. The front reads the Task list of the atespaces that `workbench.fleet.atespaces` names from the ax API, and the state of every run from the egress stream that `workbench.fleet.egress` names. It reads the running Tasks once at start and lives on the stream after that. The fleet record and its heartbeat leave the workers, and a worker is the bare `fw` binary. The members that still publish records stay visible until the last one is gone, and then the record and the `Retainer` go. The sources are done on 2026-10-17: `workbench.fleet.atespaces`, `workbench.fleet.egress`, the field `node` of every event, and the one read of a running Task. The record still ships, and the lab run decides when it goes.
4. The enqueue path and the batch id. The MCP server of the front gets a tool that puts run requests on the intake queue of a profile, and the run annotator gives every run of one job a batch id, so the runs of one job across many Tasks have one name in every list and every metric.
5. The export of a worker. Metrics go to an OpenTelemetry collector over OTLP, next to the Prometheus pull, with the standard `OTEL_EXPORTER_OTLP_*` variables, and each ledger event goes out as an OTLP log record through an `fw.Observer`. A Task that sleeps stops reporting, and no scrape wakes it. A collector example sits next to `examples/observability/prometheus.yml`. The Operator view of the page gives way to a link to Grafana, and the fleet gauges go.
6. The acceptance test. A small batch of the driving job runs end to end on the lab cluster, as one person, through Claude Code, the front, the queue, the workers, and the broker. This is the first time the whole chain carries real work, and it decides what else gets cut.
7. The idle state in the status body, and a controller in the front that reads it and calls `SuspendTask`, so idle Tasks, including the child Tasks of the spawner, stop costing money.

## Stage 5: the rest of the value to ax

KERNEL_SPLIT.md, section 3, explains these items.

1. Budgets from the Task environment, documented as the ax method.
2. Build tags for a smaller worker binary.
3. The move of `access.external` and the roles beyond "who can call this Task" to the front, with the control plane deciding for the whole fleet.
4. A workload identity for the workers, so the front recognizes a Task without the shared static token, once Substrate ships it.

## Rules for every stage

Add a page or a view only when the kernel cannot export the same fact to Prometheus, Grafana, an OpenTelemetry collector, or the ax API.

Each stage ends with `go build ./... && go vet ./... && go test ./...` green in both repositories. A stage that changes the runner contract or the fleet also runs the lab against the homelab cluster before the next one begins.
