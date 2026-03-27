# Static Analysis Reference

This document describes the static (rule-based) analyzer in `internal/diagnosis/analyzer.go`. The static analyzer runs deterministically on structured Kubernetes facts without calling any LLM. It is the primary diagnosis path and produces results in milliseconds.

---

## Table of Contents

1. [Analysis Flow](#analysis-flow)
2. [Data Sources Collected](#data-sources-collected)
3. [Hypothesis Catalog](#hypothesis-catalog)
4. [Correlation](#correlation)
5. [Confidence Calculation](#confidence-calculation)
6. [Output Format](#output-format)
7. [Known Limitations vs LLM-Based Investigation](#known-limitations-vs-llm-based-investigation)

---

## Analysis Flow

The analyzer executes five steps sequentially, then correlates the results. Each step reads one data source and returns at most one `finding` (hypothesis ID, name, score, signals, detail).

### Step 1: Container State (`analyzeContainerState`)

Reads `K8sFacts.PodStatuses[].ContainerStatuses` and `InitContainerStatuses`. This is the highest-priority source because container state is the most direct indicator of failure.

**Evaluation order** (first match wins):

1. `OOMKilled` reason or last termination reason -- produces `oom_resource`
2. `ErrImagePull` / `ImagePullBackOff` -- delegates to `classifyImagePullError` for sub-classification
3. `CreateContainerConfigError` -- produces `config_env_missing`
4. `CrashLoopBackOff` with exit code 137 -- produces `oom_resource`
5. `CrashLoopBackOff` with other exit codes -- produces `app_crash`
6. `terminated` with non-zero exit code -- produces `app_crash`
7. Pod phase `Pending` with no container statuses -- produces `pending`
8. Init container in `waiting` or `terminated` with non-zero exit -- produces `init_container_fail`

**Example**: A pod with container reason `CrashLoopBackOff` and exit code 137 produces:
```
finding{ID: "oom_resource", Score: 50, Signals: ["crashloop", "exit_137"]}
```

### Step 2: Events (`analyzeEvents`)

Reads `K8sFacts.Events`, filtering only `Warning` type events. Checks in order:

1. `FailedScheduling` reason -- sub-classified by message content into `insufficient_resources`, `taint_mismatch`, `affinity_issue`, or `pvc_binding`
2. Message contains "liveness probe failed" -- produces `probe_issue`
3. Message contains "readiness probe failed" -- produces `probe_issue`
4. Message contains "manifest unknown" or "not found" -- produces `bad_image_tag`
5. Message contains "unauthorized" or "denied" -- produces `bad_image_auth`

**Example**: A `FailedScheduling` event with message containing "untolerated taint" produces:
```
finding{ID: "taint_mismatch", Score: 60, Signals: ["event_FailedScheduling"]}
```

### Step 3: Container Logs (`analyzeContainerLogs`)

Reads `K8sFacts.PodStatuses[].ContainerStatuses[].PreviousLogs` first, then falls back to `CurrentLogs`. Previous logs are preferred because they come from the crashed container instance.

If no K8s container logs are available, falls back to **Step 3b: VictoriaLogs** (`analyzeVictoriaLogs`), which reads `LogsFacts.TopErrors` and `LogsFacts.RecentLines`.

Log lines are classified by `classifyLogLine` using keyword matching:

| Pattern Group | Keywords | Hypothesis ID |
|---|---|---|
| Config/env missing | `missing required`, `not set`, `config validation failed`, `env not found`, `undefined variable`, `no such file`, `cannot find`, `configuration error`, `invalid config` | `config_env_missing` |
| Dependency/connectivity | `connection refused`, `connection timed out`, `dial tcp`, `econnrefused`, `etimedout`, `no such host`, `service unavailable`, `503`, `connect: connection refused` | `dependency_connectivity` |
| OOM (application-level) | `outofmemoryerror`, `out of memory`, `cannot allocate memory`, `allocation failed`, `heap out of memory` | `oom_resource` |
| Permission errors | `permission denied`, `access denied`, `unauthorized`, `forbidden` | `permission_error` |

**Example**: A previous log line containing "connection refused" produces:
```
finding{ID: "dependency_connectivity", Score: 50, Signals: ["log_connectivity_error"]}
```

### Step 4: Metrics (`analyzeMetrics`)

Reads `MetricsFacts`. Two checks:

1. **Memory near limit**: If `MemoryUsage / MemoryLimit > 0.9` (90%), produces `oom_resource` with score 40 and signal `memory_near_limit`.
2. **High restart rate**: If `RestartRate > 2` restarts per 15 minutes, produces `high_restarts` with score 30 and signal `high_restart_rate`.

**Example**: Memory at 95% of limit produces:
```
finding{ID: "oom_resource", Score: 40, Signals: ["memory_near_limit"], Detail: "memory 95% of limit"}
```

### Step 5: Correlate (`correlate`)

Takes all four findings and selects the best root cause. See [Correlation](#correlation) below.

---

## Data Sources Collected

The Kubernetes provider (`internal/provider/kubernetes/k8s.go`) collects structured facts via `CollectFacts`:

| Data | Source | When Collected | Used By |
|---|---|---|---|
| **Pod statuses** | `pods.Get` / `pods.List` by label selector | Always | Step 1 (container state) |
| **Container statuses** | Pod status (state, reason, message, exit code, last termination) | Always | Step 1 |
| **Container image** | Pod spec + pod status | Always | Image pull classification, summary |
| **Env ref errors** | Pod spec -- checks for non-optional ConfigMap/Secret key refs | Always | Step 1 (config_env_missing) |
| **Container logs (current)** | `pods.GetLogs` -- last 50 lines, 256KB cap | When container has restarts > 0, or state is waiting/terminated | Step 3 |
| **Container logs (previous)** | `pods.GetLogs(Previous: true)` -- last 50 lines | When container has restarts > 0 | Step 3 |
| **Init container logs** | `pods.GetLogs` -- last 30 lines | When init container is waiting or terminated | Step 3 |
| **Events** | `events.List` filtered by resource name prefix, sorted by last seen, limited to 20 | Always | Step 2 |
| **Rollout status** | `deployments.Get` -- revision, desired/ready/updated/unavailable replicas | Only for Deployments | Evidence collection, summary |
| **Resource requests/limits** | Pod spec first container | When pods exist | Step 1 context, summary |
| **Owner chain** | Pod owner refs, then ReplicaSet owner refs | Always | Evidence display |
| **Node info** | Pod spec (nodeSelector, tolerations, affinity) | When any pod is Pending | Evidence for scheduling issues |
| **Node resources** | `nodes.List` -- sum of allocatable CPU/memory | When any pod is Pending | Evidence for scheduling issues |
| **VictoriaLogs** | External log provider (top errors, recent lines) | Separate provider | Step 3b fallback |
| **Metrics** | External metrics provider (memory usage/limit, restart rate, CPU, error rate, latency) | Separate provider | Step 4 |

### Pod Discovery

Pods are found by:
1. Direct get (if target kind is `pod`)
2. Label selector from target selectors, or fallback `app=<resource-name>`
3. If no pods found, fallback to name prefix match across all pods in the namespace

---

## Hypothesis Catalog

Every hypothesis ID the static analyzer can produce, with its triggers and base scores.

### `oom_resource` -- OOM / Resource exhaustion

| Trigger Source | Base Score | Signals |
|---|---|---|
| Container reason `OOMKilled` or last termination `OOMKilled` | 60 | `oomkilled` |
| `CrashLoopBackOff` with exit code 137 (SIGKILL) | 50 | `crashloop`, `exit_137` |
| Log line matches OOM keywords | 50 | `log_oom` |
| Memory usage > 90% of limit (metrics) | 40 | `memory_near_limit` |

**Summary**: "Container terminated by OOM Killer (exit code 137). Memory: request=X, limit=Y. Usage: XMi / YMi (Z%). Restarted N times. The container is using more memory than its limit allows. Either increase the memory limit or investigate the application for memory leaks."

### `config_env_missing` -- Config / Env missing

| Trigger Source | Base Score | Signals |
|---|---|---|
| Container reason `CreateContainerConfigError` | 70 | `config_error` |
| Container reason `CreateContainerConfigError` with env ref errors | 70 | `env_ref_error` |
| Log line matches config keywords | 50 | `log_config_error` |

**Summary**: "Container crashes on startup -- missing config or environment variable. Log: <first error line>. Restarted N times."

### `dependency_connectivity` -- Dependency / Connectivity issue

| Trigger Source | Base Score | Signals |
|---|---|---|
| Log line matches connectivity keywords | 50 | `log_connectivity_error` |

**Summary**: "Container can't reach dependency. Log: <first error line>. Restarted N times."

### `bad_image_tag` -- Image tag does not exist

| Trigger Source | Base Score | Signals |
|---|---|---|
| Container message contains "not found" or "manifest unknown" | 70 | `image_not_found` |
| Event message contains "manifest unknown" or "not found" | 70 | `event_manifest_unknown` |

**Summary**: "Image tag does not exist: <image>. Verify the tag is published in the registry."

### `bad_image_auth` -- Image pull authentication failed

| Trigger Source | Base Score | Signals |
|---|---|---|
| Container message contains "unauthorized", "denied", or "forbidden" | 70 | `image_auth_failed` |
| Event message contains "unauthorized" or "denied" | 70 | `event_unauthorized` |

**Summary**: "Authentication failed pulling <image>. Check imagePullSecrets and registry credentials."

### `bad_image_network` -- Registry unreachable

| Trigger Source | Base Score | Signals |
|---|---|---|
| Container message contains "no such host", "dial tcp", or "i/o timeout" | 70 | `registry_unreachable` |

**Summary**: "Can't reach registry for <image>. Check registry hostname and network/DNS connectivity."

### `bad_image` -- Image pull failure (generic)

| Trigger Source | Base Score | Signals |
|---|---|---|
| `ErrImagePull` / `ImagePullBackOff` that doesn't match any specific sub-pattern | 60 | `image_pull_error` |

**Summary**: Falls through to default: "Issue detected: Image pull failure."

### `app_crash` -- Application crash

| Trigger Source | Base Score | Signals |
|---|---|---|
| `CrashLoopBackOff` with non-137 exit code | 30 | `crashloop`, `exit_<code>` |
| Container terminated with non-zero exit code | 30 | `terminated`, `exit_<code>` |

**Summary**: "Container crashed. Log: <first error line>. Restarted N times. Check logs for root cause."

### `pending` -- Pod stuck in Pending

| Trigger Source | Base Score | Signals |
|---|---|---|
| Pod phase `Pending` with no container statuses | 40 | `pod_phase_pending` |

**Summary**: Falls through to default or scheduling-specific summaries if events provide more detail.

### `init_container_fail` -- Init container failure

| Trigger Source | Base Score | Signals |
|---|---|---|
| Init container state `waiting` or `terminated` with non-zero exit | 50 | `init_container_waiting` or `init_container_terminated` |

**Summary**: "Init container failed -- main container hasn't started. Log: <first error line>."

### `insufficient_resources` -- Insufficient cluster resources

| Trigger Source | Base Score | Signals |
|---|---|---|
| `FailedScheduling` event (default, no taint/affinity/PVC keywords) | 60 | `event_FailedScheduling` |

**Summary**: "Pod can't be scheduled -- insufficient cluster resources. Scheduler: <event message>. Scale up cluster or reduce resource requests."

### `taint_mismatch` -- Taint / Toleration mismatch

| Trigger Source | Base Score | Signals |
|---|---|---|
| `FailedScheduling` event with "untolerated taint" | 60 | `event_FailedScheduling` |

**Summary**: "Pod can't be scheduled -- node taint not tolerated. Scheduler: <event message>."

### `affinity_issue` -- Affinity / NodeSelector issue

| Trigger Source | Base Score | Signals |
|---|---|---|
| `FailedScheduling` event with "node affinity" or "node selector" | 60 | `event_FailedScheduling` |

**Summary**: "Pod can't be scheduled -- no node matches affinity/nodeSelector. Scheduler: <event message>."

### `pvc_binding` -- PVC binding issue

| Trigger Source | Base Score | Signals |
|---|---|---|
| `FailedScheduling` event with "persistentvolumeclaim" or "unbound" | 60 | `event_FailedScheduling` |

**Summary**: "Pod stuck Pending -- PVC can't bind. Scheduler: <event message>."

### `probe_issue` -- Probe failure

| Trigger Source | Base Score | Signals |
|---|---|---|
| Event message contains "liveness probe failed" | 50 | `event_liveness_probe_failed` |
| Event message contains "readiness probe failed" | 40 | `event_readiness_probe_failed` |

**Summary**: "Container killed by probe failure. Event: <event message>. Check probe config -- initialDelaySeconds may be too short."

### `permission_error` -- Permission / Auth error

| Trigger Source | Base Score | Signals |
|---|---|---|
| Log line contains "permission denied", "access denied", "unauthorized", or "forbidden" | 40 | `log_permission_error` |

**Summary**: "Container hit a permission/auth error. Log: <first error line>."

### `high_restarts` -- High restart rate

| Trigger Source | Base Score | Signals |
|---|---|---|
| Restart rate > 2 per 15 minutes (metrics) | 30 | `high_restart_rate` |

**Summary**: "Container has high restart rate. Log: <first error line>. Restarted N times."

---

## Correlation

The `correlate` function takes the four findings (container state, events, logs, metrics) and selects a single primary hypothesis.

### Algorithm

1. **First pass**: Select the finding with the highest individual score across all four sources.
2. **Second pass**: For every other finding that shares the same hypothesis ID as the winner, add a **+15 correlation boost** and merge its signals into the winner's signal list.
3. **Cap**: The final score is capped at `MaxScore` (always 100).

### How Correlation Boosts Work

When multiple independent data sources agree on the same root cause, the score increases. This reflects higher certainty.

**Example -- OOM detected from three sources**:

| Source | Finding ID | Base Score |
|---|---|---|
| Container state | `oom_resource` (OOMKilled reason) | 60 |
| Logs | `oom_resource` (log contains "out of memory") | 50 |
| Metrics | `oom_resource` (memory at 95% of limit) | 40 |

The container state finding wins (score 60). Logs and metrics both match the same ID, so each adds +15. Final score: `min(60 + 15 + 15, 100) = 90`. Signals: `["oomkilled", "log_oom", "memory_near_limit"]`.

**Example -- CrashLoop with connectivity issue in logs**:

| Source | Finding ID | Base Score |
|---|---|---|
| Container state | `app_crash` (CrashLoopBackOff, exit 1) | 30 |
| Events | (none) | 0 |
| Logs | `dependency_connectivity` (connection refused) | 50 |
| Metrics | (none) | 0 |

The log finding wins (score 50). No other source has the same ID, so no boost. Final score: 50. The primary hypothesis is `dependency_connectivity`, not `app_crash`, because logs had the higher score despite container state being analyzed first.

---

## Confidence Calculation

Confidence is derived from the primary hypothesis score and data availability.

### Score Thresholds

| Hypothesis Score | Base Confidence |
|---|---|
| >= 60 | High |
| >= 35 | Medium |
| < 35 | Low |

### Degradation Rules

- If Kubernetes data (`K8sFacts`) is missing, `High` is downgraded to `Medium`.
- No hypothesis at all results in `Low`.

### What Affects the Score

The score is determined entirely by which trigger fired (base score from the hypothesis catalog above) plus any correlation boost (+15 per additional source that agrees). There is no weighting by recency, event count, or restart count -- those details are included in the summary text but do not affect the numerical score.

---

## Output Format

The `DiagnosisResult` struct contains:

| Field | Description |
|---|---|
| `Summary` | Human-readable paragraph combining hypothesis name, specific details (image name, memory numbers, event message, log line), restart count, and actionable guidance. |
| `Confidence` | `High`, `Medium`, or `Low`. |
| `PrimaryHypothesis` | The winning `HypothesisScore` -- ID, name, score, max score, and list of signals. |
| `SupportingEvidence` | Array of redacted evidence strings showing all collected data: owner chain, pod phase/restarts, conditions (non-True only), container state/reason/exit code, last termination, env ref issues, log lines (up to 5 error lines or last 3 lines), init container state, warning events with count, rollout status, resource requests/limits, node count, memory usage from metrics, restart rate. |
| `Notes` | Warnings about missing data sources (unavailable providers, no logs available). |

### Evidence Redaction

All supporting evidence strings pass through `RedactSlice` before output, which strips sensitive values from the text.

### Summary Generation

Each hypothesis ID has a dedicated summary template in `generateSummary`. Summaries are built by concatenating:

1. A fixed opening sentence describing the root cause
2. Contextual details (image name, memory request/limit, memory usage percentage)
3. First error log line (from previous container logs, current logs, or VictoriaLogs top errors)
4. Restart count
5. Actionable recommendation

When no hypothesis is identified: "No clear anomaly detected. Check manually using the commands below."

---

## Known Limitations vs LLM-Based Investigation

The static analyzer is fast, deterministic, and requires no external API calls. However, it has significant blind spots compared to an LLM-based system like HolmesGPT.

### What Static Analysis Cannot Do

1. **No cross-service reasoning**. The analyzer looks at a single target workload. It cannot determine that service A is crashing because service B (its dependency) is down, or trace a failure through a chain of microservices.

2. **No semantic log understanding**. Log classification uses fixed keyword lists. A log line like "failed to initialize payment processor: stripe API key expired" will match `config_env_missing` (due to "failed" + "initialize"), but the analyzer cannot understand that the real issue is an expired API key in an external service.

3. **No root cause vs symptom distinction in ambiguous cases**. If a pod is OOMKilled AND has connectivity errors in logs, the analyzer picks whichever scores higher. An LLM can reason about whether the OOM is the cause (memory leak) or the effect (retry storms from connection failures filling buffers).

4. **No historical/temporal reasoning**. The analyzer sees a snapshot. It cannot determine "this started happening after the last deployment" or "memory has been growing linearly over 6 hours". It has no concept of trends.

5. **No configuration analysis**. The analyzer does not read ConfigMaps, Secrets, Ingress rules, NetworkPolicies, RBAC, or HPA configurations. It can detect that a ConfigMap ref is missing, but cannot tell you that a ConfigMap value is wrong.

6. **No natural language explanations**. Summaries are template-based. An LLM can explain WHY a particular exit code matters, suggest specific Helm values to change, or explain the interaction between memory requests and the OOM killer in a way that is tailored to the user's context.

7. **Single hypothesis output**. The analyzer picks one primary hypothesis. It does not present ranked alternatives with explanations of why one is more likely than another.

8. **Fixed keyword lists**. New failure patterns require code changes. An LLM can recognize novel error messages it has never been explicitly programmed to handle.

9. **No metric trend analysis**. The analyzer checks if memory is above 90% right now. It cannot detect a slow memory leak (e.g., memory growing 5% per hour) or correlate a latency spike with a deployment timestamp.

10. **Limited image pull classification**. Image pull errors are classified by message substrings. Edge cases (e.g., rate limiting from Docker Hub, manifest list architecture mismatch) may fall through to the generic `bad_image` hypothesis.

### What Static Analysis Does Well

1. **Speed**: Results in milliseconds, no LLM API latency.
2. **Determinism**: Same input always produces the same output. No hallucinations.
3. **Cost**: Zero API cost per diagnosis.
4. **Common cases**: The top 10 Kubernetes failure modes (OOM, CrashLoop, ImagePull, Pending, probe failures, missing config) are well covered and account for the majority of real incidents.
5. **Structured signals**: Every hypothesis comes with machine-readable signals that can be used for alerting, dashboards, or downstream automation.
6. **Privacy**: No cluster data leaves the system. All analysis is local.
