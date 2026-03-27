# Diagnosis Flow

Step-by-step walkthrough of what happens when the bot receives input.

There are two entry points:
1. **Alert flow**: Alertmanager webhook → notification + buttons → user clicks → investigation
2. **Manual flow**: User sends `/check` or `/scan`

---

## Alert Flow (Proactive)

```
vmalert (every 15s) → evaluates rules against VictoriaMetrics
    ↓ alert fires (e.g. KubePodCrashLooping)
Alertmanager → groups alerts, waits group_wait (15s)
    ↓ POST /webhook/alertmanager
Bot webhook server (:8080) → parses payload
    ↓ extracts target from labels: namespace, pod, deployment
Telegram → sends alert NOTIFICATION only + 3 action buttons
    [🤖 AI Investigation] [📊 Static Analysis] [📜 Logs]

User clicks button → callback handler runs investigation:
    🤖 → collect evidence → send to LLM → reply to alert with AI analysis
    📊 → collect evidence → run analyzer.go → reply to alert with structured result
    📜 → query VictoriaLogs → reply to alert with raw container logs
```

**No auto-diagnosis.** The bot does NOT automatically run diagnosis when an alert fires. It only sends the alert notification with action buttons. This saves LLM tokens and gives the operator control.

**Callback results reply to alert message.** When the user clicks a button, the investigation result is sent as a native Telegram reply to the original alert notification. This keeps the conversation threaded.

**Target extraction from alert labels:**
The webhook parser tries these label keys in order: `pod` → `deployment` → `statefulset` → `daemonset` → `container`. If `deployment` label exists alongside `pod`, it uses the deployment (higher level owner). Namespace comes from the `namespace` label.

**Intent classification from alert name:**
- `KubePodCrashLooping`, `ContainerOOMKilled` → `crashloop`
- `KubePodNotReady` (Pending) → `pending`
- `KubeDeploymentReplicasMismatch` → `rollout_regression`

---

## Manual Flow: `/check checkout`

### Step 1: Message Received

```
User → Telegram API → Bot polling loop
```

`bot.go:handleMessage()` receives the update. Checks if chat ID is allowed (if configured).

### Step 2: Parse Message

```go
ParseMessage("/check checkout")
→ ParsedMessage{ Command: "check", Target: "checkout", RawText: "/check checkout" }
```

`adapter.go:ParseMessage()` handles:
- Slash commands: `/check`, `/diag`, `/pod`, `/deploy`, `/help`
- Free text: `"checkout is crashing"` → extracts first non-noise word as target

### Step 3: Resolve Target

```go
resolver.Resolve("checkout", "prod")
→ Target{ Name: "checkout", Namespace: "prod", Kind: "deployment", ResourceName: "checkout", Selectors: {app: checkout} }
```

`resolver.go:Resolve()` tries in order:
1. **Exact resource** — `deployment/checkout` or `prod/deployment/checkout`
2. **service_map.yaml lookup** — matches name or aliases
3. **Fuzzy pod search** (fallback) — searches pods across namespace by name, owner, label
4. **Error** — returns hint to use exact name or add to service_map

**Fuzzy search scoring:**
| Score | Match type |
|-------|------------|
| 100 | Exact pod name |
| 80 | Pod name prefix (auto-used) |
| 60 | Pod name contains query |
| 50 | Owner (ReplicaSet/Deployment) name matches |
| 40 | App label matches |

If score >= 80, the match is used directly. Lower scores are presented as suggestions:
```
🔍 No exact match for "check". Did you mean:
  • /check checkout-7b9f8d-x4k2p -n prod  (Running)
  • /check checkout-worker-5c8a1 -n prod  (CrashLoopBackOff)
```

### Step 4: Classify Intent

```go
ClassifyIntent("/check checkout")
→ IntentCrashLoop  (default for generic /check)
```

`intent.go:ClassifyIntent()` scans for keywords:
- `crash`, `restart`, `oom`, `killed` → `crashloop`
- `pending`, `stuck`, `scheduling` → `pending`
- `deploy`, `rollout`, `release`, `5xx` → `rollout_regression`
- `check`, `diag` → `crashloop` (default to most common case)

Command overrides:
- `/deploy` → always `rollout_regression`
- `/pod` with no keywords → `crashloop`

### Step 5: Send Progress Message

```
Bot → Telegram: "🔍 Diagnosing prod/deployment/checkout..."
```

This message is edited in-place as progress updates arrive.

### Step 6: Collect Evidence (parallel)

```
┌───────────────────────┐  ┌──────────────┐  ┌──────────────┐
│ K8s Provider (PRIMARY) │  │Metrics Prov. │  │ Logs Provider │
│                        │  │              │  │ (fallback)   │
│ GET pods (label:       │  │ PromQL:      │  │ LogsQL:      │
│   app=checkout)        │  │ restart_rate │  │ container    │
│ → pod status           │  │ memory_usage │  │ logs (only   │
│ → conditions           │  │ memory_limit │  │ if K8s logs  │
│ → container statuses   │  │ cpu_usage    │  │ empty)       │
│ → init containers      │  │ cpu_limit    │  │              │
│ → images               │  │              │  │              │
│ → env ref errors       │  │              │  │              │
│                        │  │              │  │              │
│ GET logs (K8s API)     │  │              │  │              │
│ → current (50 lines)   │  │              │  │              │
│ → previous (50 lines)  │  │              │  │              │
│   (for crashed ctrs)   │  │              │  │              │
│                        │  │              │  │              │
│ LIST events            │  │              │  │              │
│ → warning timeline     │  │              │  │              │
│   (top 20 by recency)  │  │              │  │              │
│                        │  │              │  │              │
│ GET deploy (rollout)   │  │              │  │              │
│                        │  │              │  │              │
│ GET resources req/lim  │  │              │  │              │
│                        │  │              │  │              │
│ Owner chain:           │  │              │  │              │
│ Pod → RS → Deployment  │  │              │  │              │
│                        │  │              │  │              │
│ Node resources         │  │              │  │              │
│ (Pending pods only)    │  │              │  │              │
└───────────┬────────────┘  └──────┬───────┘  └──────┬───────┘
            │                      │                  │
            └──────────────────────┴──────────────────┘
                                   │
                                   ▼
                            Evidence Bundle
```

All providers run concurrently (`sync.WaitGroup`). Each has a 30s timeout. If one fails, the bundle is still returned with partial data + `ProviderStatus.Available = false`.

**Result for checkout (OOMKilled):**

```json
{
  "k8s_facts": {
    "owner_chain": [
      { "kind": "Pod", "name": "checkout-7b9f8d-x4k2p" },
      { "kind": "ReplicaSet", "name": "checkout-7b9f8d" },
      { "kind": "Deployment", "name": "checkout" }
    ],
    "pod_statuses": [{
      "name": "checkout-7b9f8d-x4k2p",
      "phase": "Running",
      "restart_count": 14,
      "conditions": [
        { "type": "Ready", "status": "False", "reason": "ContainersNotReady" },
        { "type": "Initialized", "status": "True" },
        { "type": "PodScheduled", "status": "True" }
      ],
      "container_statuses": [{
        "name": "checkout",
        "image": "registry.example.com/checkout:v2.3.1",
        "state": "waiting",
        "reason": "CrashLoopBackOff",
        "exit_code": 0,
        "last_termination": "OOMKilled",
        "last_exit_code": 137,
        "restart_count": 14,
        "current_logs": ["...last 50 lines..."],
        "previous_logs": ["...last 50 lines from crashed container..."]
      }]
    }],
    "events": [
      { "type": "Warning", "reason": "OOMKilling", "message": "Memory cgroup out of memory...", "count": 14 }
    ],
    "resource_requests": {
      "cpu_request": "100m", "cpu_limit": "500m",
      "memory_request": "64Mi", "memory_limit": "128Mi"
    }
  },
  "metrics_facts": {
    "restart_rate": 3.0,
    "memory_limit": 134217728
  }
}
```

### Step 7: Evidence-First Analysis (5 steps)

The analyzer (`analyzer.go`) reads the evidence bundle directly:

```
Step 1: Container State
  → CrashLoopBackOff with last_termination=OOMKilled, exit_code=137
  → Finding: oom_resource, score=50 (crashloop + exit_137)

Step 2: Events
  → Warning event "OOMKilling"
  → (no FailedScheduling, no probe failures)
  → Finding: (no independent finding — OOM event not matched as standalone)

Step 3: Logs (K8s API — previous container)
  → Scans previous_logs for error patterns
  → "java.lang.OutOfMemoryError: Java heap space"
  → Finding: oom_resource, score=50 (log_oom)

Step 4: Metrics
  → memory_usage / memory_limit > 90%? → check if available
  → restart_rate 3.0 > 2 threshold
  → Finding: high_restarts, score=30

Step 5: Correlate
  → Best finding: oom_resource (score=50 from container state)
  → Log finding also says oom_resource → boost +15 → score=65
  → Cap at max_score=100
  → Final: oom_resource, score=65, signals=[crashloop, exit_137, log_oom]
```

### Step 8: Calculate Confidence

```
Score: 65
65 >= 60 → High
Has K8s data? Yes.
→ Final confidence: High
```

Confidence rules:
- Score >= 60 → High
- Score >= 35 → Medium
- Score < 35 → Low
- Missing K8s data → degrade by one level

### Step 9: Generate Summary

The analyzer generates a structured summary based on the hypothesis ID:

```
Container OOMKilled. Memory limit: 128Mi. Restarted 14 times.
Log: java.lang.OutOfMemoryError: Java heap space
Increase memory limit or fix memory leak.
```

Each hypothesis ID has a dedicated summary template that pulls specific data from the evidence bundle (restart count, memory limit, top log error, event detail, scheduler message, image reference).

**With LLM** (AI Investigation mode):
The engine sends the evidence bundle as JSON to the configured LLM endpoint. The LLM generates a free-form natural language explanation. If LLM fails, user is prompted to use Static Analysis instead.

### Step 10: Compose Commands

Based on intent `crashloop` + hypothesis `oom_resource`:

```bash
kubectl logs deployment/checkout -n prod --tail=100
kubectl logs deployment/checkout -n prod --previous --tail=100
kubectl describe deployment/checkout -n prod | grep -A5 'Resources:'
kubectl top pods -n prod --containers | grep checkout
kubectl rollout restart deployment/checkout -n prod
```

### Step 11: Redact Sensitive Data

All evidence strings are scanned for:
- Bearer tokens
- Passwords
- API keys
- Connection strings

Matched patterns are replaced with `[REDACTED]`.

### Step 12: Format + Send Result

The `DiagnosisResult` is formatted into Telegram HTML. For `/check`, the progress message is edited in-place with the final result:

```
🔴 prod/deployment/checkout
─────────────────────

Container OOMKilled. Memory limit: 128Mi.
Restarted 14 times. Increase memory limit or fix memory leak.

Root cause: OOM / Resource exhaustion (65%)

Evidence:
  Owner: Pod/checkout-7b9f8d-x4k2p → ReplicaSet/checkout-7b9f8d → Deployment/checkout
  Pod checkout-7b9f8d-x4k2p: phase=Running, restarts=14
    Condition Ready=False: ContainersNotReady
    Container checkout [registry.example.com/checkout:v2.3.1]: state=waiting, reason=CrashLoopBackOff
    Last termination: OOMKilled (exit_code=137)
    Log [previous]: java.lang.OutOfMemoryError: Java heap space
  Event [OOMKilling]: Memory cgroup out of memory... (x14)
  Rollout: revision=3, desired=1, ready=0, updated=1, unavailable=1
  Resources: cpu=100m/500m, memory=64Mi/128Mi

Commands:
kubectl logs deployment/checkout -n prod --tail=100
kubectl logs deployment/checkout -n prod --previous --tail=100

42ms · High
```

Post-diagnosis buttons: `[🤖 AI Investigation] [📜 Logs]`

---

## Scan Flow: `/scan`

```
User sends /scan (no namespace) or /scan -n prod
    ↓
/scan (no arg) → ScanAllNamespaces()
    Lists all namespaces, skips system ones (kube-system, kube-public,
    kube-node-lease, local-path-storage, monitoring)
    For each: list pods, check unhealthy
/scan -n prod → ScanNamespace("prod")
    Lists pods in namespace, check unhealthy
    ↓
Unhealthy detection: Pending? Failed? CrashLoopBackOff?
    ImagePullBackOff? ConfigError? High restarts (>3)?
    Init container stuck? NotReady?
    ↓
Deduplicate by owner (Deployment/StatefulSet name)
    ↓
Format table: name, namespace, reason, restarts
    ↓
Send to Telegram
```

---

## Alert Callback Flow

When a user clicks an action button on an alert notification:

```
User clicks [📊 Static Analysis]
    ↓
CallbackQuery received
    data = "static:prod:checkout"
    ↓
Parse: action=static, ns=prod, name=checkout
    ↓
Acknowledge callback (remove spinner)
    ↓
resolveOrFallback(ns, name) → Target
    ↓
Send progress reply (native reply to alert message):
    "📊 Analyzing prod/deployment/checkout..."
    ↓
collectEvidence() → EvidenceBundle
    ↓
engine.RunWithoutLLM() → DiagnosisResult
    ↓
Edit progress reply with final result + buttons
    [🤖 AI Investigation] [📜 Logs]
```

The 3 callback actions:
| Action | What it does |
|--------|-------------|
| `ai:ns:name` | Collect evidence → send to LLM → free-form AI analysis |
| `static:ns:name` | Collect evidence → run analyzer.go → structured diagnosis |
| `logs:ns:name` | Query VictoriaLogs (last 30 min) → raw log lines |

---

## Degraded Mode

When a provider fails or is unavailable:

| Scenario | What happens |
|---|---|
| Metrics provider down | K8s data (including logs) still collected. Confidence not degraded if K8s is sufficient. Note added. |
| VictoriaLogs down | K8s API logs still available (current + previous). Only affects historical/aggregated logs. |
| K8s provider down | Only metrics + VictoriaLogs. Very limited diagnosis. Confidence degraded. |
| All providers down | Returns error message to user. |

The bot never blocks on a single failed provider. Each runs with its own timeout and the diagnosis proceeds with whatever data is available.

---

## State Machine

```
RECEIVED
  → PARSE_MESSAGE
  → RESOLVE_TARGET (exact → service_map → fuzzy search → error)
  → CLASSIFY_INTENT
  → COLLECT_EVIDENCE (parallel, with timeouts)
  → ANALYZE_EVIDENCE (5-step: container → events → logs → metrics → correlate)
  → CALCULATE_CONFIDENCE
  → GENERATE_SUMMARY (template per hypothesis ID, or LLM for AI mode)
  → COMPOSE_COMMANDS
  → REDACT_SENSITIVE
  → FORMAT_RESULT
  → SEND_TO_TELEGRAM (edit progress message or reply to alert)
```

No persistent state between requests. Each message is an independent diagnosis run.
