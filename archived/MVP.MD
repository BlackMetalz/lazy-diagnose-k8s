# lazy-diagnose-k8s — MVP Execution Plan

> **Telegram ChatOps bot + deterministic diagnosis playbooks + MCP-backed data collection (K8s / VictoriaMetrics / VictoriaLogs) + structured summary + suggested kubectl commands.**

Core objective: **reduce investigation time** when incidents occur on Kubernetes. Instead of manually running kubectl get/describe/logs and reading metrics, the bot collects everything and returns a conclusion + copy-paste commands.

---

## 1. Scope MVP

### In scope

3 main problems (which account for the majority of actual investigation time):

1. **CrashLoop / abnormal restarts**
2. **Pending / scheduling issues**
3. **Rollout regression after deploy**

### Out of scope (deferred)

- Bot executing commands on the cluster (write actions)
- Multi-cluster
- RBAC / team scoping
- Conversation follow-up (each message = 1 independent diagnosis run)
- Fuzzy target matching / candidate selection UI
- Inline button actions (show raw evidence, rerun...)
- Full service catalog / dependency graph

---

## 2. Architecture (simplified for MVP)

```
Telegram
  → Telegram Adapter (receive message, parse command, send progress, return results)
  → Intent Classifier (rule-based: crashloop / pending / rollout / unknown)
  → Target Resolver (exact match → service_map lookup → fail with hint)
  → Playbook Engine (run diagnosis flow based on intent)
      ├── KubernetesProvider (via K8s MCP server)
      ├── LogsProvider (via VictoriaLogs MCP server)
      └── MetricsProvider (via VictoriaMetrics MCP server)
  → Diagnosis Engine (normalize evidence + score hypotheses + summarize)
  → Command Composer (generate suggested kubectl commands)
  → Telegram Adapter (render structured response)
```

### Key Design Decisions

- **Playbooks drive the investigation flow. LLM only summarizes/explains.** The LLM does not decide the investigation path.
- **MCP-first**: use existing MCP servers for K8s, VictoriaMetrics, VictoriaLogs. No custom-built datasource layer.
- **Read-only**: all providers only read, never write to the cluster.

---

## 3. Modules (5 compact modules instead of 10+)

### 3.1 Telegram Adapter

- Receive messages/commands
- Parse intent + target from message
- Send progress updates (checking K8s... fetching logs...)
- Render structured response

### 3.2 Target Resolver

MVP only needs 2 steps:

1. **Exact match** — actual resource name (deployment, pod, service)
2. **service_map.yaml lookup** — map logical name → actual resource

If no match → return a clear message: "Target X not found. Try using the exact deployment/pod name or add it to service_map.yaml."

### 3.3 Playbook Engine

Runs the diagnosis flow based on intent. Each playbook defines:
- What to collect (K8s facts, logs, metrics)
- Which hypotheses to score
- Scoring rules

### 3.4 Diagnosis Engine

Combines 3 responsibilities into 1 module:

- **Normalize** raw data from providers into a standardized evidence bundle
- **Score** hypotheses according to rules (rule-based, weighted scoring)
- **Summarize** into a readable output

### 3.5 Command Composer

Generates suggested kubectl commands: rollback, restart, inspect.
Only recommends rollback when there are signs of regression. Restart is only secondary.

---

## 4. Three MVP Playbooks

### A. CrashLoop / Abnormal Restarts

**Collect:**
- Pod status, container state, termination reason, restart count
- Events
- Current logs + previous logs
- Metrics: restart rate, memory, CPU

**Hypotheses + scoring example:**

| Hypothesis | Signal | Weight |
|---|---|---|
| OOM / resource | terminationReason=OOMKilled | +40 |
| | memory usage near limit | +30 |
| | restart count > 5 | +20 |
| Config / env missing | logs contain "env not found", "config error" | +50 |
| | container exit code 1 within < 10s | +20 |
| Dependency / connectivity | logs contain "connection refused", "timeout" | +40 |
| | multiple containers affected | +20 |
| Probe issue | events contain "Liveness probe failed" | +50 |
| | container running but being restarted | +20 |
| Bad image / startup failure | events contain "ErrImagePull", "ImagePullBackOff" | +60 |
| | container never started | +30 |

**Confidence:** total >= 70 → High, 40-69 → Medium, < 40 → Low

### B. Pending / Scheduling Issue

**Collect:**
- Pod conditions
- Scheduler events
- Requests / limits
- Node selector, affinity, taints/tolerations
- PVC status

**Hypotheses:** insufficient resources, taint mismatch, affinity issue, PVC binding, quota issue

### C. Rollout Regression

**Collect:**
- Rollout status, revision, ready vs desired replicas
- Pod restarts / readiness failures after deploy
- Logs comparison before/after
- Metrics comparison before/after (error rate, latency)

**Hypotheses:** release regression, dependency issue exposed after release, unrelated traffic spike, resource pressure

---

## 5. Provider Layer

All 3 providers communicate via their corresponding MCP server:

| Provider | MCP Server | Data |
|---|---|---|
| KubernetesProvider | Kubernetes MCP | pod status, events, deployments, conditions |
| LogsProvider | VictoriaLogs MCP | application logs, error patterns |
| MetricsProvider | VictoriaMetrics MCP | CPU, memory, restart rate, error rate, latency |

**Error handling for each provider:**
- Timeout: 30s default
- If MCP server is down/timed out → mark provider as unavailable, continue with remaining data
- Max retry: 1 attempt

---

## 6. Degraded Mode

| What is missing | Still provides | Confidence |
|---|---|---|
| Metrics unavailable | Diagnosis from K8s + logs | Downgraded by 1 level |
| Logs unavailable | Diagnosis from K8s + metrics | Downgraded by 1 level |
| Only K8s available | Preliminary conclusion from events + status | Low |
| MCP server down | Clearly reports which provider failed | N/A |

Notes always clearly state which data sources are missing.

---

## 7. Output Contract

Each diagnosis run returns:

```
summary:              string    — concise conclusion
confidence:           High|Medium|Low
primary_hypothesis:   string    — primary root cause
supporting_evidence:  []string  — evidence
recommended_steps:    []string  — next steps
suggested_commands:   []string  — kubectl commands for copy-paste
notes:                []string  — warnings, missing sources, caveats
```

---

## 8. Config

### service_map.yaml

```yaml
services:
  - name: checkout
    aliases: [checkout-service, checkout-api]
    namespace: prod
    primary_resource: deployment/checkout
    selectors:
      app: checkout
    metrics_job: checkout-api
```

### playbook_rules.yaml

Scoring weights + trigger patterns for each hypothesis (see section 4).

### redaction_rules.yaml

Mask: token, password, api_key, secret-like fields before passing to the summarizer.

---

## 9. Tech Stack (to be finalized)

**Proposed:**

- **Language:** Go — fits the K8s ecosystem, good performance, simple deployment (single binary)
- **Telegram SDK:** go-telegram-bot-api or telebot
- **MCP client:** mcp-go or equivalent
- **Config:** YAML (viper or raw yaml.v3)
- **Deployment:** runs as a single Pod in the cluster (requires a read-only ServiceAccount)
- **Logging:** structured logging (slog)

---

## 10. Build Order

### Phase 1 — Foundation (days 1-3)

- [ ] Finalize tech stack, init project structure
- [ ] Domain models: Intent, Target, EvidenceBundle, DiagnosisResult, HypothesisScore
- [ ] Telegram Adapter: receive message, parse command, send text response
- [ ] Config loader: service_map.yaml, playbook_rules.yaml

### Phase 2 — Target + Providers (days 4-6)

- [ ] Target Resolver (exact match + service_map lookup)
- [ ] Setup + test 3 MCP servers (K8s, VictoriaMetrics, VictoriaLogs)
- [ ] Provider interfaces + MCP connection
- [ ] Mock providers for testing

### Phase 3 — CrashLoop E2E (days 7-10)

- [ ] CrashLoop playbook: collect → normalize → score → summarize
- [ ] Diagnosis Engine: evidence normalization + hypothesis scoring
- [ ] Command Composer
- [ ] Telegram rendering structured response
- [ ] Test with real cluster: OOMKilled, missing env, dependency timeout

### Phase 4 — Expanded Playbooks (days 11-14)

- [ ] Pending playbook
- [ ] Rollout regression playbook
- [ ] Test scenarios for both

### Phase 5 — Hardening (days 15-18)

- [ ] Degraded mode: handle provider timeout/down
- [ ] Redaction rules
- [ ] Timeout + retry logic
- [ ] Progress updates on Telegram (checking K8s... fetching logs...)
- [ ] Structured logging

### Phase 6 — Polish + Test (days 19-21)

- [ ] Test full demo scenarios (9 test cases)
- [ ] Polish output format
- [ ] Fix edge cases
- [ ] Document deployment steps

---

## 11. Test Scenarios

### CrashLoop
1. Missing env → container exits quickly, logs report env error
2. OOMKilled → terminationReason=OOMKilled, memory near limit
3. Dependency timeout → logs report connection refused/timeout

### Pending
4. Insufficient CPU → scheduler event reports FailedScheduling
5. Taint mismatch → node has taint, pod lacks toleration
6. PVC pending → PVC in Pending state

### Rollout Regression
7. Deploy causes 5xx increase → error rate spike after new revision
8. Deploy causes readiness failure → pods not ready after rollout
9. False alarm: issue caused by downstream → metrics show root cause in another service

---

## 12. Acceptance Criteria

MVP is done when:

1. Telegram bot receives messages and returns results end-to-end
2. Can resolve targets via exact name or service_map
3. Can run all 3 playbooks (CrashLoop, Pending, Rollout Regression)
4. Returns structured response in correct format (summary, confidence, evidence, commands)
5. Provides suggested kubectl commands (rollback/restart/inspect)
6. Degraded mode works when metrics or logs are unavailable
7. No write actions on the cluster
8. Reduces investigation time compared to manual kubectl workflow

---

## Summary of Changes from Original Plan

| Removed from MVP | Reason |
|---|---|
| Conversation Manager | Adds complexity, stateless MVP is sufficient |
| Fuzzy target matching | Over-engineering, exact + service_map is sufficient for MVP |
| Inline button actions | Nice-to-have, not critical |
| Candidate selection UI | Requires fuzzy match, which was cut |

| Consolidated | Before | After |
|---|---|---|
| Diagnosis Engine | Evidence Normalizer + Hypothesis Scorer + Summarizer (3 modules) | 1 module |

| Newly Added | Reason |
|---|---|
| Tech stack section | Missing from original plan |
| MCP server error handling | Most common failure mode |
| Concrete scoring examples | Original plan was vague about scoring |
| More realistic timeline (3 weeks) | 2 weeks was too optimistic |
