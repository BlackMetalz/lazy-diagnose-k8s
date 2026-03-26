# Diagnosis Flow

Step-by-step walkthrough of what happens when a user sends a message to the bot.

## Example: `/check checkout`

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
3. **Error** — returns hint to use exact name or add to service_map

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
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│ K8s Provider │  │Metrics Prov. │  │ Logs Provider │
│              │  │              │  │              │
│ GET pods     │  │ PromQL:      │  │ LogsQL:      │
│ (label:      │  │ restart_rate │  │ container    │
│  app=checkout│  │ memory_usage │  │ logs for     │
│ )            │  │ memory_limit │  │ checkout in  │
│              │  │ cpu_usage    │  │ last 1h      │
│ LIST events  │  │ cpu_limit    │  │              │
│ (prefix:     │  │              │  │              │
│  checkout)   │  │              │  │              │
│              │  │              │  │              │
│ GET deploy   │  │              │  │              │
│ (rollout     │  │              │  │              │
│  status)     │  │              │  │              │
└──────┬───────┘  └──────┬───────┘  └──────┬───────┘
       │                 │                 │
       └─────────────────┴─────────────────┘
                         │
                         ▼
                  Evidence Bundle
```

All three providers run concurrently (`sync.WaitGroup`). Each has a 30s timeout. If one fails, the bundle is still returned with partial data + `ProviderStatus.Available = false`.

**Result for checkout (OOMKilled):**

```json
{
  "k8s_facts": {
    "pod_statuses": [{
      "name": "checkout-xxx",
      "phase": "Running",
      "restart_count": 14,
      "container_statuses": [{
        "state": "terminated",
        "reason": "OOMKilled",
        "exit_code": 137,
        "last_termination": "OOMKilled"
      }]
    }],
    "events": [
      { "type": "Warning", "reason": "OOMKilling", "message": "Memory cgroup out of memory..." }
    ]
  },
  "metrics_facts": {
    "restart_rate": 3.0,
    "memory_limit": 134217728
  },
  "logs_facts": {
    "total_lines": 5,
    "recent_lines": ["stress: info: [1] dispatching hogs: 0 cpu, 0 io, 1 vm, 0 hdd"]
  }
}
```

### Step 7: Score Hypotheses

The diagnosis engine loads rules for the `crashloop` intent from `playbook_rules.yaml`:

```
Hypothesis: oom_resource (OOM / Resource exhaustion)
  Signal: termination_oom
    Match "OOMKilled" against container reason "OOMKilled" → MATCH ✓ (+40)
  Signal: memory_near_limit
    Match memory_usage > 90% limit → no current usage (pod terminated) → MISS ✗
  Signal: high_restart_count
    Match restart_count > 5 → 14 > 5 → MATCH ✓ (+20)
  Score: 60/90

Hypothesis: config_env_missing
  Signal: env_error_logs → no matching log patterns → MISS ✗
  Signal: fast_exit → no matching pattern → MISS ✗
  Score: 0/70

Hypothesis: probe_issue
  Signal: probe_failed_event → event "OOMKilling" contains... no → MISS ✗
  Signal: running_but_restarting → state=terminated, restart>0 → might match depending on pattern
  Score: 0-20/70

... (all hypotheses scored)
```

Hypotheses ranked by score: `oom_resource (60)` > others.

### Step 8: Calculate Confidence

```
Top score: 60
60 >= 40 → base = Medium
Has logs? Yes. Has metrics? Yes (partial — limit but no usage).
→ Final confidence: Medium
```

Confidence rules:
- Score >= 70 → High
- Score >= 40 → Medium
- Score < 40 → Low
- Missing logs or metrics → degrade by one level

### Step 9: Generate Summary

**Without LLM** (template):
```
Container OOMKilled — hitting memory limit (128Mi). Restarted 14 times.
Increase memory limit or fix memory leak.
```

**With LLM** (if configured):
The engine sends the evidence bundle as JSON to the configured LLM endpoint. The LLM generates a 3-5 sentence natural language explanation.

If LLM fails → automatic fallback to template.

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

The `DiagnosisResult` is formatted into Telegram HTML:

```
🔴 prod/deployment/checkout
─────────────────────

Container OOMKilled — hitting memory limit (128Mi).
Restarted 14 times. Increase memory limit or fix memory leak.

Root cause: OOM / Resource exhaustion (67%)

Evidence:
  Pod checkout-xxx: phase=Running, restarts=14
  Container checkout: state=terminated, reason=OOMKilled, exit_code=137
  Event [OOMKilling]: Memory cgroup out of memory... (x14)
  Rollout: revision=1, desired=1, ready=0, updated=1, unavailable=1
  Resources: cpu=/, memory=64Mi/128Mi
  Restart rate: 3.0/min

Next steps:
  1. Increase memory limit for the container
  2. Check application for memory leaks
  3. Consider optimizing memory usage

Commands:
kubectl logs deployment/checkout -n prod --tail=100
kubectl logs deployment/checkout -n prod --previous --tail=100
kubectl describe deployment/checkout -n prod | grep -A5 'Resources:'
kubectl top pods -n prod --containers | grep checkout
kubectl rollout restart deployment/checkout -n prod

56ms · Medium
```

The progress message from Step 5 is edited in-place with this final result.

---

## Degraded Mode

When a provider fails or is unavailable:

| Scenario | What happens |
|---|---|
| Metrics provider down | K8s + Logs still collected. Confidence degraded. Note added. |
| Logs provider down | K8s + Metrics still collected. Confidence degraded. Note added. |
| K8s provider down | Only metrics + logs. Very limited diagnosis. |
| All providers down | Returns error message to user. |

The bot never blocks on a single failed provider. Each runs with its own timeout and the diagnosis proceeds with whatever data is available.

---

## State Machine

```
RECEIVED
  → PARSE_MESSAGE
  → RESOLVE_TARGET (may fail → error to user)
  → CLASSIFY_INTENT
  → COLLECT_EVIDENCE (parallel, with timeouts)
  → SCORE_HYPOTHESES
  → CALCULATE_CONFIDENCE
  → GENERATE_SUMMARY (LLM or template)
  → COMPOSE_COMMANDS
  → REDACT_SENSITIVE
  → FORMAT_RESULT
  → SEND_TO_TELEGRAM
```

No persistent state between requests. Each message is an independent diagnosis run.
