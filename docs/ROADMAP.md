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
4. The fleet: `fw/fleet.go`, `surface/native/fleet.go`, `surface/view/fleet.go`, `telemetry/fleet.go`, and the key `fleet` of the configuration.
5. Grants and connections: `connect.go`, `grants.go`, `tools/mcp/person.go`, and `tools/mcp/user.go`. The kernel keeps one primitive: a run can pause until a credential arrives from outside. The OAuth flows and the grant store go to the workbench.

After each move, `fw.Config` loses the matching key. The target is a configuration without `access`, `fleet`, `grants`, `titles`, and `mcp`.

## Stage 4: export instead of view

1. Push metrics over OTLP, next to the Prometheus pull at `/metrics`. The standard `OTEL_EXPORTER_OTLP_*` variables configure it. A Task that sleeps stops reporting, and no scrape wakes it.
2. Export each ledger event as an OTLP log record through an `fw.Observer`. The ledger is the trajectory that the ax roadmap asks for at the runner layer.
3. Add a collector example next to `examples/observability/prometheus.yml`.
4. Remove the fleet gauges after the OTLP push exists.

## Stage 5: the value of the kernel to ax

Do these in order. KERNEL_SPLIT.md, section 3, explains each item.

1. The idle state in the status body.
2. A controller in the workbench that reads the idle state and calls `SuspendTask`.
3. A `Spawner` plug with an ax driver that creates a child Task.
4. Budgets from the Task environment.
5. Build tags for a smaller binary.

## Until stage 3 is done

Add no product feature, change no page, and add no new use of the fleet.

Each stage ends with `go build ./... && go vet ./... && go test ./...` green in both repositories.
