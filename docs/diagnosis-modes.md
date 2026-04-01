# Diagnosis Modes

lazy-diagnose-k8s has 4 diagnosis modes, each with different trade-offs between speed, depth, and cost.

## Quick Comparison

| Mode | Trigger | Data Sources | LLM | Speed | Cost |
|---|---|---|---|---|---|
| **📊 Static** | Button / `/check` | K8s + Metrics + Logs | No | ~0.5s | Free |
| **🤖 AI** | Button | K8s + Metrics + Logs | Yes (summarizer) | ~5-10s | LLM tokens |
| **🔬 Deep** | Button | Holmes CLI (kubectl) | Yes (agentic) | ~30-60s | LLM tokens × tool calls |
| **📜 Logs** | Button | VictoriaLogs | No | ~0.2s | Free |

## 📊 Static Analysis

Deterministic, rule-based scoring. No LLM. Fastest mode.

### What it does

```
1. Collect evidence (parallel)
   ├─ K8s API:         pods, events, rollout status, resource config, owner chain
   ├─ VictoriaMetrics: restart rate, CPU/memory usage, HTTP 5xx error rate
   └─ VictoriaLogs:    container logs (last 200 lines), error patterns

2. Score hypotheses
   ├─ Match signals against playbook rules (weighted scoring)
   ├─ Select primary hypothesis (highest score)
   └─ Calculate confidence: High (>70pts) / Medium (>40pts) / Low

3. Output
   ├─ Summary (template-based, no LLM)
   ├─ Root cause + confidence %
   ├─ Evidence (pod status, events, metrics)
   ├─ Next steps
   └─ Copy-paste kubectl commands
```

### Analysis rules

Hardcoded in `internal/diagnosis/analyzer.go`. The analyzer reads evidence from each source (container state, events, logs, metrics) and correlates findings:

- **Container state** — OOMKilled, CrashLoopBackOff, ImagePullBackOff, Pending, init container failure
- **Events** — FailedScheduling, probe failure, image pull errors, node affinity issues
- **Logs** — missing config/env vars, connection refused, permission denied, runtime panics
- **Metrics** — memory near limit, high restart rate, HTTP 5xx error rate spike

Each source produces a `finding` with a score. The `correlate()` function picks the best root cause across all sources.

### Example output

```
🟡 HTTP 5xx error rate is elevated. Current: 3.68 req/s.

Target:
  Namespace:  demo-staging
  Deployment: webapp-testing
  Container:  webapp
  Image:      kienlt992/golang-webapp-testing:v5.0.0

Root cause: HTTP 5xx error rate spike (55%)

Evidence:
  Pod webapp-testing-abc123: phase=Running, restarts=0
  Rollout: revision=1, desired=2, ready=2
  HTTP 5xx rate: 3.68 req/s (was 3.85 req/s 1h ago)

Next steps:
  1. Check application logs for error stack traces
  2. Review recent deployments or config changes
  3. Check downstream service health

Commands:
  kubectl logs deployment/webapp-testing -n demo-staging --tail=100
  kubectl rollout restart deployment/webapp-testing -n demo-staging

Confidence: Medium · Analyzed in 1.761s
```

---

## 🤖 AI Investigation

Same evidence as Static, but sends it to an LLM for natural language analysis.

### What it does

```
1. Collect evidence
   └─ Same as Static Analysis (K8s + Metrics + Logs)

2. Run static analysis
   └─ Get hypothesis scores for LLM context

3. Build LLM prompt
   ├─ System: "You are a senior SRE writing an incident note..."
   └─ User:   Target + JSON evidence + hypothesis scores

4. Call LLM
   ├─ Model:       configured via LLM_MODEL (e.g. GLM-4.7)
   ├─ Endpoint:    LLM_BASE_URL (OpenAI-compatible)
   ├─ Temperature: 0.3 (factual, deterministic)
   ├─ Max tokens:  4096
   └─ Retries:     3x with backoff for 429 rate limits

5. Output
   └─ 2-4 sentence incident note (plain text, no markdown)
```

### LLM prompt rules

- Lead with root cause, not symptoms
- Include specific numbers (exit codes, memory values, restart counts)
- End with one concrete action (not "consider" but "increase to 1Gi")
- Never repeat target path or hypothesis name from input
- If data missing, state which source is missing

### Example output

```
🤖 AI Investigation
─────────────────────

OOMKilled — container allocates 512M via --vm-bytes but only has 128Mi memory
limit, causing repeated OOM kills every ~30s. Increase memory limit to at least
512Mi or reduce stress tool allocation to fit within 128Mi.
```

### Cost

One LLM call per investigation. Typical: ~700 input tokens + ~200 output tokens.

---

## 🔬 Deep Investigation

Agentic investigation using HolmesGPT. The AI autonomously runs kubectl commands to investigate.

### What it does

```
1. Construct question
   └─ "Investigate ONLY the deployment X in namespace Y.
       Focus on this specific resource.
       Do NOT investigate other resources.
       Output: STATUS / PROBLEM / ROOT_CAUSE / FIX"

2. Invoke Holmes CLI
   └─ holmes ask "..." --no-interactive --max-steps 15 --fast-mode --model openai/GLM-4.7

3. Holmes agentic loop (up to 15 steps)
   ├─ AI decides which tool to call
   ├─ Tool executes (kubectl get, describe, logs, events...)
   ├─ Result fed back to AI
   ├─ AI decides next tool or concludes
   └─ Repeat until done or max-steps reached

4. Parse output
   ├─ Extract last AI conclusion block
   ├─ Filter noise (litellm warnings, tool status, task tables)
   └─ Parse STATUS / PROBLEM / ROOT_CAUSE / FIX sections

5. Format Telegram message
   ├─ Target info
   ├─ 🔴/🟡/🟢 Status with health icon
   ├─ ⚠️ Problem
   ├─ 🔍 Root cause
   └─ 🛠 Fix (numbered steps)
```

### Holmes toolsets (enabled)

| Toolset | What it does |
|---|---|
| `kubernetes/core` | `kubectl get/describe` with jq queries, paginated |
| `kubernetes/logs` | `kubectl logs` for pods |
| `kubernetes/kube-prometheus-stack` | Query Prometheus targets |
| `kubectl-run` | Run kubectl commands |
| `bash` | Run arbitrary shell commands (kubectl mostly) |
| `docker/core` | Docker inspect/logs |
| `core_investigation` | Task planning (TodoWrite) |
| `internet` | Web search for docs |
| `connectivity_check` | Network connectivity testing |

### Typical tool calls (from real logs)

```json
{
  "tool_calls": 11,
  "tools": [
    "bash: kubectl get deployment webapp-testing -n demo-staging",
    "bash: kubectl describe deployment webapp-testing -n demo-staging",
    "bash: kubectl get pods -n demo-staging -l app=webapp-testing",
    "bash: kubectl describe pod webapp-testing-7799679bdb-gm4bb -n demo-staging",
    "bash: kubectl describe pod webapp-testing-7799679bdb-rxppq -n demo-staging",
    "bash: kubectl logs webapp-testing-7799679bdb-gm4bb -n demo-staging --tail=100",
    "bash: kubectl logs webapp-testing-7799679bdb-rxppq -n demo-staging --tail=100",
    "bash: kubectl get events -n demo-staging --field-selector involvedObject.name=webapp-testing",
    "bash: kubectl get events -n demo-staging --field-selector involvedObject.kind=Pod",
    "bash: kubectl get events -n demo-staging --field-selector reason=BackOff",
    "bash: kubectl get svc -n demo-staging -l app=webapp-testing"
  ],
  "duration": "33.2s"
}
```

### Example output

```
🔬 Deep Investigation
─────────────────────

Target: demo-staging/webapp-testing (deployment)

🔴 Status: unhealthy — 2/2 replicas ready, 0 restarts

⚠️ Problem:
Application consistently returns HTTP 504 Gateway Timeout errors

🔍 Root cause:
The golang-webapp-testing application (v5.0.0) is experiencing timeout issues,
likely waiting for unresponsive downstream services

🛠 Fix:
1. Check downstream service endpoints and verify availability
2. Review application logs for specific timeout details
3. Verify network connectivity between pods and dependencies
4. Consider rolling back to a previous stable version
```

### Cost

Multiple LLM calls per investigation (one per tool-calling step). Typical: 11 tool calls, ~30s, ~5K-15K tokens total.

---

## 📜 Logs

Raw log extraction from VictoriaLogs. No analysis, no LLM.

### What it does

```
1. Query VictoriaLogs
   ├─ Time range: last 30 minutes
   ├─ Filter: pod name / deployment label
   └─ Limit: 200 lines

2. Format output
   ├─ Show last 30 lines (truncated at 200 chars/line)
   ├─ Show time range
   └─ If no logs: suggest kubectl command
```

### Example output

```
📜 Logs (last 30 min)
─────────────────────

Returning status code: 504
Returning status code: 504
Returning status code: 200
Returning status code: 504
... showing 30/156 lines

14:23 — 14:53
```

---

## /check and /scan Commands

### /check — single resource diagnosis

```
/check checkout              → fuzzy match pod, run Static Analysis
/check checkout -n prod      → explicit namespace
/check checkout -c cluster2  → explicit cluster
```

Runs Static Analysis by default, then shows buttons for AI/Logs/Deep.

### /scan — namespace-wide scan

```
/scan prod                   → scan all pods in namespace
/scan                        → scan default namespace
```

Lists all unhealthy pods (CrashLoop, Pending, ImagePull, Not Ready) with `/check` commands for each.

---

## Configuration

### Minimum (Static only — no LLM needed)

```bash
export TELEGRAM_BOT_TOKEN=your-token
# That's it. Static analysis works with just K8s access.
```

### With AI Investigation

```bash
export LLM_BASE_URL=https://mkp-api.fptcloud.com/v1
export LLM_API_KEY=your-key
export LLM_MODEL=GLM-4.7
```

### With Deep Investigation (adds Holmes)

```bash
# Same LLM config as above — Holmes inherits it
export HOLMES_MODEL=GLM-4.7   # optional, defaults to LLM_MODEL
# Holmes CLI must be installed: pip install holmesgpt
```

### With metrics and logs

```bash
export VICTORIA_METRICS_URL=http://localhost:8428
export VICTORIA_LOGS_URL=http://localhost:9428
```

Without VictoriaMetrics/Logs, Static Analysis still works but with degraded confidence (missing data sources noted in output).
