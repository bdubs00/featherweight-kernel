# Observability for fw

This directory holds a supported way to watch fw with Prometheus and Grafana. fw has no usage page of its own. Operational reports come from these files:

- `prometheus.yml`: the scrape jobs for fw.
- `alerts.yml`: the alert rules.
- `grafana/fw.json`: one Grafana dashboard.

Each fw process serves its metrics at `/metrics` on the same port as its API. The surface is on by default. The key `telemetry.metrics`, or the variable `FW_TELEMETRY_METRICS`, moves the path.

## Scrape fw

The file `prometheus.yml` has two jobs:

- The job `fw` scrapes fixed addresses. Replace `fw.example.internal:8080` with the host and port of each fw process that is always on.
- The job `fw-pods` finds Kubernetes pods. It keeps only the pods that carry the label `fw.dev/scrape: "true"`.

To add a pod to the job `fw-pods`, do these steps in the pod template of its Deployment:

1. Add the label `fw.dev/scrape: "true"`.
2. Name the container port of fw `http`.
3. If the pod serves the metrics at a path other than `/metrics`, add the annotation `fw.dev/metrics-path` with that path.

Prometheus must have permission to list and watch pods. The Prometheus Operator and the Helm chart of Prometheus give that permission.

## Keep the metrics inside the monitoring perimeter

The route `/metrics` has no authentication, because a scraper has no token. Anyone who can reach the port can read the metrics. The metrics hold no prompt, no output, no run id, and no user identity. They hold the names of agents, tools, MCP servers, models, and fleet members, and counts of runs and tokens.

Do these things to keep the route private:

1. Do not route `/metrics` through a public gateway. The agentgateway example in `examples/agentgateway` routes `/mcp`, `/ui`, and `/v1` only.
2. Allow traffic to the port of fw from Prometheus and from the clients of fw only. A Kubernetes NetworkPolicy can do this.
3. If the port is public, move the route with `telemetry.metrics` and block that path at the edge.

## Prometheus is not the billing record

Use Prometheus to watch trends, errors, and latency. Do not use it to bill people or to audit usage, for these reasons:

- A scrape happens every 30 seconds. Prometheus misses what a process does between its last scrape and its exit. Prometheus also never sees a process that it does not scrape, such as an ax Task that sleeps.
- When a process restarts, its counters start again at zero. The functions `rate` and `increase` correct for most resets, but a total over a long range is an estimate.
- Each fw process writes every model call, with its tokens, to its ledger. The ledger is the exact record. The command `fw events <run> --format jsonl` exports it.

The budget limits of a run do not depend on Prometheus. The kernel enforces them from the ledger.

## Cost that is unknown

The counter `fw_model_cost_usd_total` counts only the tokens of models that have a price. You set prices under `prices:` in the fw configuration, per million tokens:

```yaml
prices:
  provider/model: { input: 3, output: 15 } # replace with your model and its price
  local/qwen: { input: 0, output: 0 }      # a local model that costs nothing
```

The counter `fw_model_unpriced_tokens_total` counts the tokens of every model that has no price. Their cost is unknown, not zero. The dashboard shows both numbers side by side. A price of zero is a price, so set one for a model that costs nothing, such as a local model.

## Run under ax

ax suspends a Task that is idle. When a request arrives, the atenet router wakes the Task. A scrape is a request, so a scrape through the router wakes the Task. Scrapes every 30 seconds keep every Task awake and remove the benefit of suspend.

Do these things under ax:

1. Do not scrape an ax Task. Do not add an ax Task to the job `fw`, and do not point a scrape at the atenet router.
2. Run one or more fw processes that are always on, for example the `fw-front` Deployment of `examples/agentgateway`. Give them the same bus and the same `fleet.subject` as the Tasks.
3. Scrape only the processes that are always on.

A process with a fleet reports the whole fleet from the records that the bus keeps. It sends no request to the other members. These gauges come from the fleet:

| Metric | Labels | Meaning |
|---|---|---|
| `fw_fleet_member_up` | `member` | 1 when the member is live, else 0. A Task that sleeps is not live. |
| `fw_fleet_member_last_seen_timestamp_seconds` | `member` | The time of the last record or heartbeat of the member. |
| `fw_fleet_runs` | `member`, `state` | Runs that are not finished, by state: `queued`, `running`, `input-required`, `suspended`. |
| `fw_fleet_approvals_oldest_timestamp_seconds` | `member` | The request time of the oldest approval that waits on the member. |

A member that sleeps keeps the counts of its last record. The first scrape of a process starts its fleet view, and it can take up to two seconds.

Two processes can report the same member. The dashboard and the rules take the max or the min for each member, so a member counts once.

## Load the alert rules

The file `alerts.yml` holds these rules:

| Alert | Fires when |
|---|---|
| `FwTargetDown` | Prometheus cannot scrape an fw process for 5 minutes. |
| `FwApprovalWaiting` | An approval waits for more than 1 hour on a scraped process. |
| `FwFleetApprovalWaiting` | An approval waits for more than 1 hour on a fleet member. |
| `FwRunFailuresRising` | More than 25% of the runs of an agent fail in 30 minutes, with 3 failures or more. |
| `FwBudgetStops` | A budget limit stops a run. The loop guard has its own alert. |
| `FwLoops` | The loop guard ends a run because the model repeats a tool call. |
| `FwMCPServerDown` | fw marks an MCP server down for 5 minutes. |
| `FwModelErrorRate` | More than 10% of the calls to a model fail in 10 minutes, with 3 failures or more. |
| `FwModelLatencyHigh` | The p95 latency of a model is over 2 minutes for 15 minutes. |
| `FwFleetMemberDown` | A fleet member is not live for 15 minutes, and its last record shows a running run. |

The thresholds are starting points. Change them to fit your agents. If you use only the fleet gauges, remove `FwApprovalWaiting`, because `FwFleetApprovalWaiting` covers the same approvals.

To load the rules, put `alerts.yml` next to `prometheus.yml`. The line `rule_files: [alerts.yml]` in `prometheus.yml` reads it. With the Prometheus Operator, copy the groups into a `PrometheusRule` resource instead. If `promtool` is installed, run `promtool check config prometheus.yml` to test both files.

## Load the dashboard

The dashboard needs Grafana 10 or later and a Prometheus data source.

1. In Grafana, open Dashboards, then New, then Import.
2. Upload `grafana/fw.json`.
3. Pick the Prometheus data source in the variable `Data source` at the top of the dashboard.

The dashboard has no fixed data source. It reads the variable, so the same file works in every Grafana. To provision it from a file, add `grafana/fw.json` to a dashboard provider. The variable `Agent` filters the panels that have an agent label.

## Labels

No label holds a run id, a principal, a prompt, a path, or a URL. These labels hold names from the configuration:

- `agent`: the agent profiles of fw, from `fw.yaml` and from the profiles that people create in fw.
- `tool` and `server`: the tools and the MCP servers that fw resolved. A tool name that the model invents becomes `unknown`.
- `provider` and `model`: the models of the configuration keep their names. A run can name another model in an inline profile. Up to 32 such models keep their names, and the others share the name `other`.
- `member`: the names of the fleet members, which the deployment sets.

The other labels, such as `outcome`, `origin`, `limit`, `kind`, and `state`, hold values from fixed lists.
