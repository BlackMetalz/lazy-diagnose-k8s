# Data Collection — Low-Level Design

This document describes exactly what data is collected from each source, how it maps to the evidence bundle, and how each piece of data is used during analysis.

Inspired by the HolmesGPT "skillset/toolset" approach: define a fixed set of data-gathering skills, run them all, then analyze the combined evidence.

---

## Data Sources

```
┌─────────────────────────────────────────────────────────────┐
│                    K8s API (client-go)                      │
│                                                             │
│  CoreV1:                       AppsV1:                      │
│  • Pods.Get / Pods.List        • Deployments.Get            │
│  • Pods.GetLogs (stream)       • ReplicaSets.Get            │
│  • Events.List                                              │
│  • Namespaces.List                                          │
│  • Nodes.List                                               │
├─────────────────────────────────────────────────────────────┤
│                    VictoriaMetrics (PromQL)                 │
│  • restart rate                                             │
│  • CPU usage / limit                                        │
│  • memory usage / limit                                     │
│  • error rate (5xx/s)                                       │
│  • latency (p99)                                            │
├─────────────────────────────────────────────────────────────┤
│                    VictoriaLogs (LogsQL)                    │
│  • container logs (fallback when K8s API logs are empty)    │
│  • error count                                              │
│  • top error patterns                                       │
└─────────────────────────────────────────────────────────────┘
```

---

## K8s Provider — Collected Data

File: `internal/provider/kubernetes/k8s.go`

The K8s provider is the primary data source. It runs 7 collection steps sequentially within a single `CollectFacts()` call.

### 1. Pod Status

**API call:** `CoreV1.Pods.List` (by label selector) or `CoreV1.Pods.Get` (for pod targets)

**Data collected per pod:**

| Field | Source | Used in analysis |
|-------|--------|-----------------|
| `Name` | `pod.Name` | Evidence display, log fetching |
| `Phase` | `pod.Status.Phase` | Step 1: Pending detection |
| `Ready` | Pod condition `Ready=True` | Evidence display |
| `RestartCount` | Sum of all container restart counts | Summary generation |

**Pod conditions collected:**

| Condition | Source | Used in analysis |
|-----------|--------|-----------------|
| `Ready` | `pod.Status.Conditions` | Evidence: shows False + reason |
| `Initialized` | `pod.Status.Conditions` | Evidence: init container issues |
| `PodScheduled` | `pod.Status.Conditions` | Scanner: Unschedulable detection |
| `ContainersReady` | `pod.Status.Conditions` | Evidence display |

Non-True conditions with messages are included in supporting evidence.

### 2. Container Status

**Data collected per container:**

| Field | Source | Used in analysis |
|-------|--------|-----------------|
| `Name` | `containerStatus.Name` | Log fetching, evidence display |
| `Image` | `pod.Spec.Containers[i].Image` | Evidence: show exact image ref |
| `State` | Running/Waiting/Terminated from `containerStatus.State` | Step 1: determine container state |
| `Reason` | `state.Waiting.Reason` or `state.Terminated.Reason` | Step 1: OOMKilled, CrashLoopBackOff, ErrImagePull, CreateContainerConfigError |
| `Message` | `state.Waiting.Message` or `state.Terminated.Message` | Step 1: detailed error (e.g. "configmap 'app-config' not found") |
| `ExitCode` | `state.Terminated.ExitCode` | Step 1: exit 137=OOM, exit 1=app error |
| `LastTermination` | `lastTerminationState.Terminated.Reason` | Step 1: OOMKilled from previous run |
| `LastExitCode` | `lastTerminationState.Terminated.ExitCode` | Step 1: previous exit code |
| `RestartCount` | `containerStatus.RestartCount` | Triggers log fetching, summary |
| `EnvErrors` | Scanned from `pod.Spec.Containers[i].Env` | Step 1: config_env_missing detection |
| `CurrentLogs` | K8s API GetLogs (current, 50 lines) | Step 3: log analysis |
| `PreviousLogs` | K8s API GetLogs (previous, 50 lines) | Step 3: log analysis (preferred for crashed containers) |

**Env ref error detection** (`checkEnvRefs`):

Scans each container spec for environment variable references that could fail:
- `env[].valueFrom.configMapKeyRef` — non-optional ConfigMap references
- `env[].valueFrom.secretKeyRef` — non-optional Secret references
- `envFrom[].configMapRef` — bulk ConfigMap mounts
- `envFrom[].secretRef` — bulk Secret mounts

Output format: `"env DB_HOST refs ConfigMap/app-config key database_host"`

### 3. Init Container Status

Same fields as container status. Collected from `pod.Status.InitContainerStatuses`.

**Trigger:** Always collected if init containers exist.

**Used in analysis:**
- Step 1: `init_container_fail` finding if state=waiting or terminated with non-zero exit code
- Evidence: shows init container state/reason/exit_code
- Log fetching: current logs fetched for failed init containers (30 lines)

### 4. Container Logs (K8s API)

**API call:** `CoreV1.Pods.GetLogs` (stream, with `TailLines` and `Previous` options)

**Trigger:** Logs are fetched for containers with:
- `RestartCount > 0` (current + previous logs)
- `State == "waiting"` (current logs only)
- `State == "terminated"` (current logs only)

**Parameters:**
| Parameter | Value | Reason |
|-----------|-------|--------|
| `TailLines` | 50 (main), 30 (init) | Last N lines to avoid large payloads |
| `Previous` | true/false | Previous container's logs (for crash analysis) |
| `LimitBytes` | 256KB (reader limit) | Safety cap on response size |

**Used in analysis:**
- Step 3: `analyzeContainerLogs()` — scans previous logs first (more relevant for crashes), then current
- Log pattern classification: config errors, connectivity issues, OOM, permission errors
- Evidence: top error lines from logs shown in supporting evidence
- Summary: top log error included in natural language summary

### 5. Events Timeline

**API call:** `CoreV1.Events.List` (namespace-scoped, no field selector)

**Post-processing:**
- Filter: only events where `InvolvedObject.Name` starts with or equals target resource name
- Sort: by `LastTimestamp` descending (most recent first)
- Limit: 20 most recent events

**Data collected per event:**

| Field | Source | Used in analysis |
|-------|--------|-----------------|
| `Type` | `event.Type` | Filter: only Warning events analyzed |
| `Reason` | `event.Reason` | Step 2: FailedScheduling, probe failures |
| `Message` | `event.Message` | Step 2: sub-cause detection (taints, affinity, PVC, image errors) |
| `Count` | `event.Count` | Evidence display |
| `FirstSeen` | `event.FirstTimestamp` | Timeline context |
| `LastSeen` | `event.LastTimestamp` | Sort order |

### 6. Rollout Status

**API call:** `AppsV1.Deployments.Get`

**Trigger:** Only for `kind=deployment` targets.

**Data collected:**

| Field | Source | Used in analysis |
|-------|--------|-----------------|
| `CurrentRevision` | `deploy.Annotations["deployment.kubernetes.io/revision"]` | Evidence display |
| `DesiredReplicas` | `deploy.Spec.Replicas` | Evidence display |
| `ReadyReplicas` | `deploy.Status.ReadyReplicas` | Evidence display |
| `UpdatedReplicas` | `deploy.Status.UpdatedReplicas` | Evidence display |
| `AvailableReplicas` | `deploy.Status.AvailableReplicas` | Evidence display |
| `UnavailableReplicas` | `deploy.Status.UnavailableReplicas` | Evidence display |

### 7. Resource Requests/Limits

**API call:** `CoreV1.Pods.List` (Limit=1, same label selector)

**Data collected from first container of first pod:**

| Field | Source | Used in analysis |
|-------|--------|-----------------|
| `CPURequest` | `container.Resources.Requests[cpu]` | Evidence display |
| `CPULimit` | `container.Resources.Limits[cpu]` | Evidence display |
| `MemoryRequest` | `container.Resources.Requests[memory]` | Evidence display |
| `MemoryLimit` | `container.Resources.Limits[memory]` | Summary: "Memory limit: 128Mi" |

### 8. Owner Chain

**API calls:** `CoreV1.Pods.List` (Limit=1) → `AppsV1.ReplicaSets.Get` (if RS owner)

**Logic:** Start from pod, follow `OwnerReferences` up:
- Pod → ReplicaSet → Deployment (typical)
- Pod → StatefulSet (if StatefulSet)
- Pod → DaemonSet (if DaemonSet)

**Output:** `[{Kind: "Pod", Name: "checkout-7b9f8d-x4k2p"}, {Kind: "ReplicaSet", Name: "checkout-7b9f8d"}, {Kind: "Deployment", Name: "checkout"}]`

**Used in analysis:** Evidence display — "Owner: Pod/x → ReplicaSet/y → Deployment/z"

### 9. Node Resources

**API call:** `CoreV1.Nodes.List`

**Trigger:** Only for Pending pods (scheduling failure diagnosis).

**Data collected:**

| Field | Source | Used in analysis |
|-------|--------|-----------------|
| `NodeCount` | `len(nodes.Items)` | Evidence display |
| `TotalCPU` | Sum of `node.Status.Capacity[cpu]` | Evidence context |
| `TotalMemory` | Sum of `node.Status.Capacity[memory]` | Evidence context |
| `AllocatableCPU` | Sum of `node.Status.Allocatable[cpu]` | Evidence: available resources |
| `AllocatableMemory` | Sum of `node.Status.Allocatable[memory]` | Evidence: available resources |

### 10. Node Info (for Pending)

**Trigger:** Only for Pending pods.

**Data collected:**
- `NodeSelector` — from pod spec
- `Tolerations` — from pod spec
- `Affinity` — from pod spec

Used to diagnose scheduling constraints (taint mismatch, affinity issues).

---

## Metrics Provider — Collected Data

File: `internal/provider/metrics/metrics.go`

**Source:** VictoriaMetrics (PromQL HTTP API)

| Metric | PromQL (conceptual) | Used in analysis |
|--------|---------------------|-----------------|
| `RestartRate` | `increase(kube_pod_container_status_restarts_total[15m])` | Step 4: high_restarts finding if > 2 |
| `CPUUsage` | `rate(container_cpu_usage_seconds_total[5m])` | Evidence display |
| `CPULimit` | `kube_pod_container_resource_limits{resource="cpu"}` | Evidence display |
| `MemoryUsage` | `container_memory_working_set_bytes` | Step 4: memory_near_limit if > 90% of limit |
| `MemoryLimit` | `kube_pod_container_resource_limits{resource="memory"}` | Step 4: denominator for memory ratio |
| `ErrorRate` | `rate(http_requests_total{code=~"5.."}[5m])` | Evidence display |
| `Latency` | `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))` | Evidence display |

---

## Logs Provider — Collected Data

File: `internal/provider/logs/logs.go`

**Source:** VictoriaLogs (LogsQL HTTP API)

**Role:** Fallback only. K8s API logs (current + previous) are the primary log source.

| Field | Description | Used in analysis |
|-------|-------------|-----------------|
| `TotalLines` | Total log lines in time range | Evidence context |
| `ErrorCount` | Lines matching error patterns | Evidence context |
| `TopErrors` | Most frequent error patterns (pattern, count, sample) | Step 3 fallback: analyzeVictoriaLogs() |
| `RecentLines` | Last N log lines | Step 3 fallback, Logs button display |

---

## Analysis Steps — How Data Maps to Findings

File: `internal/diagnosis/analyzer.go`

### Step 1: Container State (`analyzeContainerState`)

Reads `K8sFacts.PodStatuses[].ContainerStatuses[]` and `InitContainerStatuses[]`.

| Condition checked | Finding ID | Score | Signals |
|-------------------|-----------|-------|---------|
| Pod phase = Pending | `pending` | 40 | `pod_phase_pending` |
| Reason = OOMKilled (current or last) | `oom_resource` | 60 | `oomkilled` |
| Reason = ErrImagePull / ImagePullBackOff | `bad_image` | 60 | `image_pull_error` |
| Reason = CreateContainerConfigError | `config_env_missing` | 70 | `config_error` |
| EnvErrors present + CreateContainerConfigError | `config_env_missing` | 70 | `env_ref_error` |
| CrashLoopBackOff + exit 137 | `oom_resource` | 50 | `crashloop, exit_137` |
| CrashLoopBackOff + other exit | `app_crash` | 30 | `crashloop, exit_N` |
| Terminated + non-zero exit | `app_crash` | 30 | `terminated, exit_N` |
| Init container waiting/failed | `init_container_fail` | 50 | `init_container_STATE` |

### Step 2: Events (`analyzeEvents`)

Reads `K8sFacts.Events[]` (Warning type only).

| Condition checked | Finding ID | Score | Signals |
|-------------------|-----------|-------|---------|
| FailedScheduling (default) | `insufficient_resources` | 60 | `event_FailedScheduling` |
| FailedScheduling + "untolerated taint" | `taint_mismatch` | 60 | `event_FailedScheduling` |
| FailedScheduling + "node affinity/selector" | `affinity_issue` | 60 | `event_FailedScheduling` |
| FailedScheduling + "persistentvolumeclaim" | `pvc_binding` | 60 | `event_FailedScheduling` |
| "liveness probe failed" | `probe_issue` | 50 | `event_liveness_probe_failed` |
| "readiness probe failed" | `probe_issue` | 40 | `event_readiness_probe_failed` |
| "manifest unknown" / "not found" | `bad_image_tag` | 70 | `event_manifest_unknown` |
| "unauthorized" / "denied" | `bad_image_auth` | 70 | `event_unauthorized` |

### Step 3: Logs (`analyzeContainerLogs` + `analyzeVictoriaLogs`)

**Primary:** Reads `K8sFacts.PodStatuses[].ContainerStatuses[].PreviousLogs` (preferred) then `CurrentLogs`.

**Fallback:** If K8s logs empty, reads `LogsFacts.TopErrors[]` then `LogsFacts.RecentLines`.

Each log line is classified by `classifyLogLine()`:

| Pattern keywords | Finding ID | Score | Signals |
|-----------------|-----------|-------|---------|
| missing required, not set, config validation failed, env not found, undefined variable, no such file, cannot find, configuration error, invalid config | `config_env_missing` | 50 | `log_config_error` |
| connection refused, connection timed out, dial tcp, econnrefused, etimedout, no such host, service unavailable, 503 | `dependency_connectivity` | 50 | `log_connectivity_error` |
| outofmemoryerror, out of memory, cannot allocate memory, allocation failed, heap out of memory | `oom_resource` | 50 | `log_oom` |
| permission denied, access denied, unauthorized, forbidden | `permission_error` | 40 | `log_permission_error` |

### Step 4: Metrics (`analyzeMetrics`)

Reads `MetricsFacts`.

| Condition | Finding ID | Score | Signals |
|-----------|-----------|-------|---------|
| Memory usage > 90% of limit | `oom_resource` | 40 | `memory_near_limit` |
| Restart rate > 2/15min | `high_restarts` | 30 | `high_restart_rate` |

### Step 5: Correlate (`correlate`)

1. Find the finding with the highest individual score across all 4 steps.
2. Check if any other step produced a finding with the same ID.
3. For each additional source that agrees: boost score by +15.
4. Cap at `MaxScore`.

**Example:** Container state says `oom_resource` (score 50), logs say `oom_resource` (score 50). Best is container state (50). Logs agree → boost +15 → final score 65.

This correlation is the key insight: when container state, events, logs, and metrics all point to the same root cause, confidence should be higher than any single source alone.

---

## Namespace Scanner — Unhealthy Pod Detection

File: `internal/provider/kubernetes/scanner.go`

The scanner is a separate code path used by `/scan`. It checks all pods in a namespace and flags unhealthy ones.

**Unhealthy conditions checked (in order):**

| Condition | Reason string |
|-----------|--------------|
| Phase = Pending | "Pending" or "Unschedulable" (if PodScheduled=False) |
| Phase = Failed | "Failed" |
| Waiting: CrashLoopBackOff | "CrashLoop (OOMKilled)" or "CrashLoop (Error)" |
| Waiting: ImagePullBackOff / ErrImagePull | "ImagePullBackOff" etc. |
| Waiting: CreateContainerConfigError | "ConfigError" |
| Terminated (not Completed) | Termination reason |
| RestartCount > 3 (but currently running) | "Restarting (OOMKilled, 14x)" |
| Not ready + running + has restarts | "NotReady" |
| Init container waiting or terminated with error | "InitContainer: reason" |

**Fuzzy pod search** (`FuzzyFindPod`):

Searches across pods with scoring:
- Exact name match → 100
- Name prefix → 80
- Name contains → 60
- Owner (ReplicaSet/Deployment) name matches → 50
- App label matches → 40

Used as fallback when `/check` target doesn't match an exact resource name.
