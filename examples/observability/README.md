# Observability for fw

This directory holds a supported way to watch fw with Prometheus and Grafana. fw has no usage page of its own. Operational reports come from these files:

- `prometheus.yml`: the scrape jobs for fw.
- `otel-collector.yaml`: an OpenTelemetry collector that receives what the Tasks push and serves their metrics to Prometheus.
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
2. Give every Task the variable `OTEL_EXPORTER_OTLP_ENDPOINT` with the address of an OpenTelemetry collector. The Task then pushes its metrics, its ledger as log records, and its traces, and a Task that sleeps pushes nothing. The egress policy of the Task must allow the collector.
3. Run the collector with `otel-collector.yaml` from this directory. It receives OTLP and serves the metrics of every Task to Prometheus on one port, so Prometheus scrapes the collector, never a Task.
4. Scrape the processes that are always on, such as the front, as before.

The series that a Task pushes carry `service.instance.id`, the node of the Task, so one dashboard shows a fleet by Task. The log records carry the run id, the agent, and the trace id of the run, so a log store and a trace store meet on the same ids. The label `batch` on `fw_runs_total` and `fw_runs_queued_total` names the job of a run, so a panel sums a job across every Task.

## Load the alert rules

The file `alerts.yml` holds these rules:

| Alert | Fires when |
|---|---|
| `FwTargetDown` | Prometheus cannot scrape an fw process for 5 minutes. |
| `FwApprovalWaiting` | An approval waits for more than 1 hour on a scraped process. |
| `FwRunFailuresRising` | More than 25% of the runs of an agent fail in 30 minutes, with 3 failures or more. |
| `FwBudgetStops` | A budget limit stops a run. The loop guard has its own alert. |
| `FwLoops` | The loop guard ends a run because the model repeats a tool call. |
| `FwMCPServerDown` | fw marks an MCP server down for 5 minutes. |
| `FwModelErrorRate` | More than 10% of the calls to a model fail in 10 minutes, with 3 failures or more. |
| `FwModelLatencyHigh` | The p95 latency of a model is over 2 minutes for 15 minutes. |

The thresholds are starting points. Change them to fit your agents.

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
- `batch`: the job of a run, from the metadata `fw.batch`, which `enqueue` of the workbench sets; empty for a run of no job.

The other labels, such as `outcome`, `origin`, `limit`, `kind`, and `state`, hold values from fixed lists.

## Five task-quality measures

The quality panels cover all agents and do not use the dashboard Agent filter.
Their labels come from fixed lists only: no agent, batch, run, tool input,
receipt, effect key, principal, or verifier name enters these series. All use
the existing Prometheus registry and its OTLP bridge, not a second meter.

| Measure | Series and denominator |
|---|---|
| Verified completion | `fw_task_completions_total{execution,grader_judgment}` counts every **own terminal episode**, including failures and cancellations. `fw_task_verifications_total{result}` counts one final application receipt per completed episode. Only `result="satisfied"` is external verification. `accepted` means application-reported human acceptance; `rejected` and `unknown` are explicit final observations. No receipt means unverified. |
| Operator interventions | `fw_operator_interventions_total{kind}` counts concrete actions. `fw_operator_interventions_per_run{coverage}` is a histogram at each own finish, including zero-action, failed and canceled episodes. A follow-up begins another completion episode of the same run. |
| Recovery | `fw_recovery_duration_seconds{result}` measures a resume to first model response, tool response, or tool progress, or to a finish/suspend before progress. Results are `progress`, `completed`, `failed`, `canceled`, `interrupted`. Suspended downtime before resume is excluded. |
| Duplicate effects | `fw_confirmed_duplicate_effects_total` counts **additional committed external effects** reported by trusted applications. `fw_ambiguous_tool_retries_total` separately counts another request for an outstanding call id. It never asserts another external effect. |
| Cost per successful task | `fw_successful_task_cost_usd{basis}` is a histogram of own episode model cost for final `accepted` or externally `satisfied` receipts, only with complete prices and usage. Its `_count` is the price-complete successful-task denominator. `fw_successful_task_cost_excluded_total{reason}` records `unpriced_tokens` or `missing_usage` exclusions. |

Execution `completed` does not assert satisfaction. `grader_judgment="satisfied"`
is the outcome of an LLM grader, **not external verification**. The other grader
states are `needs_revision`, `max_iterations`, `unparsed`, `skipped`, `unknown`,
and `unverified` (no grader pass). A revision follow-up clears the previous
judgment. Copied fork history contributes none of these own task completions,
interventions, recovery attempts, costs, receipts, or duplicate effects.

### Queries

External verified completion throughput / all terminal episodes:

```promql
(sum(rate(fw_task_verifications_total{result="satisfied"}[6h])) or vector(0))
  / sum(rate(fw_task_completions_total[6h]))
```

Use `result=~"accepted|satisfied"` instead for accepted-or-verified success.
Display execution and receipt denominators beside the ratio:

```promql
sum by (execution, grader_judgment) (increase(fw_task_completions_total[6h]))
sum by (result) (increase(fw_task_verifications_total[6h]))
```

Completed executions minus final receipts gives unverified episodes **only for
a common complete history/cohort**, not for an arbitrary rate window. Explicit
`result="unknown"` remains separate. Receipts can arrive after the window of
their completion or after restart, so a short-window throughput ratio can
exceed one. Do not clamp it and call it a cohort success rate. Use ledger
completion sequences and receipts for an exact cohort SLO. Missing metric
series are not evidence of verification or known zero failures.

Mean and p95 interventions per episode (complete observer history):

```promql
sum(rate(fw_operator_interventions_per_run_sum{coverage="complete"}[1h]))
  / sum(rate(fw_operator_interventions_per_run_count{coverage="complete"}[1h]))
histogram_quantile(0.95, sum by (le) (rate(fw_operator_interventions_per_run_bucket{coverage="complete"}[1h])))
sum by (kind) (increase(fw_operator_interventions_total[1h]))
```

`kind="decision"` requires a nonempty decider other than `kernel` or the
parent run; decisions on kernel-origin grader runs are excluded.
`kind="followup"` requires a user-role message, no kernel note, and origin
`http`, `cli`, or `mcp`. `kind="budget_change"` requires a nonempty `by` and
one of those interactive origins. Bus, tool, A2A, and kernel followups and
unattributed budget changes do not count. These are ledger-backed operator
actions, not proof of a human: an authorized client can automate a surface or
set a decider. A followup with a budget change counts two concrete actions.
An attributed budget change after finish counts immediately; it enters the
next episode distribution if the run reopens. It never reopens execution.

Recovery p95, with unsuccessful attempts kept separate:

```promql
histogram_quantile(0.95, sum by (le, result) (rate(fw_recovery_duration_seconds_bucket[1h])))
sum by (result) (increase(fw_recovery_duration_seconds_count[1h]))
```

An annotation, followup, model request, or turn start alone is not progress.
A new resume or suspend interrupts an unfinished recovery attempt. Once first
progress ends an attempt, a later execution failure does not relabel it.
When another resume follows a crash without a suspended event, the old
`interrupted` duration ends at its last ledger activity, not the new resume.
That incomplete sample is a lower bound; it excludes the unknown crash gap.
`Metrics.Attach` reconstructs the open episode, interventions, outstanding
requests, and pending resume clock from the ledger without replaying old
counters. Histograms can include pre-attach episode facts. Direct observer
registration without Attach or a full history gives `coverage="partial"`
intervention samples. No completed-run cache is retained. A process lost
before a recovery end exports no finished duration; absence is not success.

Confirmed duplicate effects and ambiguous attempts:

```promql
sum(increase(fw_confirmed_duplicate_effects_total[1h]))
sum(increase(fw_ambiguous_tool_retries_total[1h]))
```

Repeated intents, identical arguments, loop warnings, and tool error results
do not establish external effects. The ambiguous count detects a repeated
outstanding call id only; it does not detect all application retries. Zero
confirmed duplicates without integration reporting means unobserved, not
exactly-once delivery. Prevented duplicates do not count as committed effects.

Mean own episode cost per price-complete accepted/verified task, and exclusions:

```promql
sum by (basis) (rate(fw_successful_task_cost_usd_sum[6h]))
  / sum by (basis) (rate(fw_successful_task_cost_usd_count[6h]))
sum by (reason) (increase(fw_successful_task_cost_excluded_total[6h]))
```

Scope is **one run's own completion episode**, not its root tree. Cost includes
model responses, compactions, and model-written annotations after the previous
own finish through this finish, plus annotations after this finish until the
next work event or receipt. It excludes copied history, child reports, child
and grader usage, and non-model costs. Child runs can report their own success
but are never charged a second time to their parent. Cumulative finished totals
are not added to episode cost. This is the conditional cost of successful
episodes, not total fleet spend divided by successes or amortized failed-work
cost. Unknown token prices exclude the entire sample. An explicit zero price
is valid. A receipt freezes its price/usage snapshot; settle annotations first.
Annotations after the receipt cannot revise a histogram. Prices are those of
the reporting process at receipt time; fleet pricing changes can change the
snapshot.

### Trusted integration receipt API

Run the complete local example (no external services or model provider):

```sh
go run ./examples/observability/quality
```

The example tool simulates a faulty delivery integration. The application
checks authoritative committed delivery rows, verifies delivery, and reports
one confirmed additional effect through the public API. Calling the same
receipt twice does not duplicate any metric. In a real integration, query the
authoritative external system using a stable operation key; do not use model
text or the number of tool requests as evidence.

```go
err := agent.ObserveQuality(ctx, run.ID, fw.QualityObservation{
    ID: "stable-reconciliation-receipt", // opaque; never a secret
    Completion: finishedEvent.Seq,       // own completed finish, not a later annotation
    Verification: "satisfied",          // external check; accepted/rejected/unknown also valid
    DuplicateEffects: confirmedAdditionalCommittedEffects,
})
```

`ObserveQuality` requires run operator authorization when ctx has a principal.
It is a trusted application API, not a model tool or a new unauthenticated
HTTP endpoint. The integration supplies truth and reporting coverage. For
effects-only observations omit verification and completion and supply a
positive duplicate count. Report each additional effect once under a stable
receipt id. Different ids must not report overlapping effect sets.

Receipts use the existing durable `fw.run.annotated` event with a `quality`
object and kernel-computed `qualityCostUSD`, `qualityUnpricedTokens`, and
`qualityCostComplete`. They change no metadata, usage, or execution state.
IDs are bounded to 128 bytes. Retries with identical id and contents write
nothing, including after restart; conflicting ids and a second final
verification for a completion are rejected. `unknown` is final too. A new
followup can have another receipt for its next own completed finish. A copied
fork finish cannot be verified as own work.

Logs keep run, sequence and trace ids plus quality facts. Traces label model
judgments with `fw.outcome.basis="grader_judgment"` and application observations
with `fw.quality.basis="trusted_application"`. A late receipt gets a short
`fw.quality.observation` span in the run's trace, not a reopened execution span.
Recovery ends add `fw.recovery` span events with result and duration. Trace/log
ids can be sensitive operational data; they are never metric labels.

The added alert group covers low verification throughput, intervention load,
slow recovery, confirmed duplicates, unknown successful cost, and high own
successful cost. Tune its thresholds. Enable the verification policy only
where the integration contract promises receipts. Prometheus remains a trend
store: restarts reset counters and exports can miss work before process loss.
