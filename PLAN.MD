# lazy-diagnose-k8s — MVP Execution Plan

> **Telegram ChatOps bot + deterministic diagnosis playbooks + MCP-backed data collection (K8s / VictoriaMetrics / VictoriaLogs) + structured summary + suggested kubectl commands.**

Mục tiêu cốt lõi: **giảm thời gian investigation** khi có incident trên Kubernetes. Thay vì phải tự tay kubectl get/describe/logs rồi đọc metrics, bot gom hết và trả kết luận + command copy-paste.

---

## 1. Scope MVP

### In scope

3 bài toán chính (chiếm phần lớn thời gian investigation thực tế):

1. **CrashLoop / restart bất thường**
2. **Pending / scheduling issue**
3. **Rollout regression sau deploy**

### Out of scope (làm sau)

- Bot execute command lên cluster (write action)
- Multi-cluster
- RBAC / team scoping
- Conversation follow-up (mỗi message = 1 diagnosis run độc lập)
- Fuzzy target matching / candidate selection UI
- Inline button actions (show raw evidence, rerun...)
- Full service catalog / dependency graph

---

## 2. Kiến trúc (simplified cho MVP)

```
Telegram
  → Telegram Adapter (nhận message, parse command, gửi progress, trả kết quả)
  → Intent Classifier (rule-based: crashloop / pending / rollout / unknown)
  → Target Resolver (exact match → service_map lookup → fail with hint)
  → Playbook Engine (chạy diagnosis flow theo intent)
      ├── KubernetesProvider (via K8s MCP server)
      ├── LogsProvider (via VictoriaLogs MCP server)
      └── MetricsProvider (via VictoriaMetrics MCP server)
  → Diagnosis Engine (normalize evidence + score hypotheses + summarize)
  → Command Composer (sinh kubectl commands gợi ý)
  → Telegram Adapter (render structured response)
```

### Điểm thiết kế quan trọng

- **Playbook quyết flow điều tra. LLM chỉ summarize/explain.** Không để LLM tự quyết investigation path.
- **MCP-first**: dùng MCP server có sẵn cho K8s, VictoriaMetrics, VictoriaLogs. Không tự build datasource layer.
- **Read-only**: tất cả provider chỉ đọc, không write vào cluster.

---

## 3. Modules (5 module gọn thay vì 10+)

### 3.1 Telegram Adapter

- Nhận message/command
- Parse intent + target từ message
- Gửi progress update (đang check K8s... đang lấy logs...)
- Render structured response

### 3.2 Target Resolver

MVP chỉ cần 2 bước:

1. **Exact match** — resource name thật (deployment, pod, service)
2. **service_map.yaml lookup** — map tên logic → resource thật

Nếu không match → trả message rõ ràng: "Không tìm thấy target X. Thử dùng tên deployment/pod chính xác hoặc thêm vào service_map.yaml."

### 3.3 Playbook Engine

Chạy diagnosis flow theo intent. Mỗi playbook define:
- Cần collect gì (K8s facts, logs, metrics)
- Các hypothesis cần score
- Scoring rules

### 3.4 Diagnosis Engine

Gộp 3 việc vào 1 module:

- **Normalize** raw data từ providers thành evidence bundle chuẩn
- **Score** hypotheses theo rules (rule-based, weighted scoring)
- **Summarize** thành output dễ đọc

### 3.5 Command Composer

Sinh kubectl commands gợi ý: rollback, restart, inspect.
Chỉ recommend rollback khi có dấu hiệu regression. Restart chỉ là secondary.

---

## 4. Ba Playbook MVP

### A. CrashLoop / Restart bất thường

**Collect:**
- Pod status, container state, termination reason, restart count
- Events
- Current logs + previous logs
- Metrics: restart rate, memory, CPU

**Hypotheses + scoring ví dụ:**

| Hypothesis | Signal | Weight |
|---|---|---|
| OOM / resource | terminationReason=OOMKilled | +40 |
| | memory usage near limit | +30 |
| | restart count > 5 | +20 |
| Config / env missing | logs chứa "env not found", "config error" | +50 |
| | container exit code 1 trong < 10s | +20 |
| Dependency / connectivity | logs chứa "connection refused", "timeout" | +40 |
| | multiple containers affected | +20 |
| Probe issue | events chứa "Liveness probe failed" | +50 |
| | container running nhưng bị restart | +20 |
| Bad image / startup failure | events chứa "ErrImagePull", "ImagePullBackOff" | +60 |
| | container never started | +30 |

**Confidence:** total >= 70 → High, 40-69 → Medium, < 40 → Low

### B. Pending / Scheduling issue

**Collect:**
- Pod conditions
- Scheduler events
- Requests / limits
- Node selector, affinity, taints/tolerations
- PVC status

**Hypotheses:** insufficient resources, taint mismatch, affinity issue, PVC binding, quota issue

### C. Rollout regression

**Collect:**
- Rollout status, revision, ready vs desired replicas
- Pod restarts / readiness failures sau deploy
- Logs comparison before/after
- Metrics comparison before/after (error rate, latency)

**Hypotheses:** release regression, dependency issue lộ sau release, unrelated traffic spike, resource pressure

---

## 5. Provider Layer

Cả 3 provider đều gọi qua MCP server tương ứng:

| Provider | MCP Server | Dữ liệu |
|---|---|---|
| KubernetesProvider | Kubernetes MCP | pod status, events, deployments, conditions |
| LogsProvider | VictoriaLogs MCP | application logs, error patterns |
| MetricsProvider | VictoriaMetrics MCP | CPU, memory, restart rate, error rate, latency |

**Error handling cho mỗi provider:**
- Timeout: 30s mặc định
- Nếu MCP server down/timeout → đánh dấu provider unavailable, tiếp tục với data còn lại
- Max retry: 1 lần

---

## 6. Degraded Mode

| Thiếu gì | Vẫn trả được | Confidence |
|---|---|---|
| Metrics unavailable | Diagnosis từ K8s + logs | Hạ 1 bậc |
| Logs unavailable | Diagnosis từ K8s + metrics | Hạ 1 bậc |
| Chỉ có K8s | Kết luận sơ bộ từ events + status | Low |
| MCP server down | Báo rõ provider nào lỗi | N/A |

Notes luôn ghi rõ nguồn nào thiếu.

---

## 7. Output Contract

Mỗi diagnosis run trả về:

```
summary:              string    — kết luận ngắn gọn
confidence:           High|Medium|Low
primary_hypothesis:   string    — nguyên nhân chính
supporting_evidence:  []string  — bằng chứng
recommended_steps:    []string  — bước tiếp theo
suggested_commands:   []string  — kubectl commands copy-paste
notes:                []string  — cảnh báo, nguồn thiếu, caveats
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

Scoring weights + trigger patterns cho từng hypothesis (xem section 4).

### redaction_rules.yaml

Mask: token, password, api_key, secret-like fields trước khi đưa vào summarizer.

---

## 9. Tech Stack (cần chốt)

**Đề xuất:**

- **Ngôn ngữ:** Go — phù hợp ecosystem K8s, performance tốt, deploy đơn giản (single binary)
- **Telegram SDK:** go-telegram-bot-api hoặc telebot
- **MCP client:** mcp-go hoặc tương đương
- **Config:** YAML (viper hoặc raw yaml.v3)
- **Deployment:** chạy như 1 Pod trong cluster (cần ServiceAccount read-only)
- **Logging:** structured logging (slog)

---

## 10. Build Order

### Phase 1 — Foundation (ngày 1-3)

- [ ] Chốt tech stack, init project structure
- [ ] Domain models: Intent, Target, EvidenceBundle, DiagnosisResult, HypothesisScore
- [ ] Telegram Adapter: nhận message, parse command, gửi text response
- [ ] Config loader: service_map.yaml, playbook_rules.yaml

### Phase 2 — Target + Providers (ngày 4-6)

- [ ] Target Resolver (exact match + service_map lookup)
- [ ] Setup + test 3 MCP servers (K8s, VictoriaMetrics, VictoriaLogs)
- [ ] Provider interfaces + kết nối MCP
- [ ] Mock providers cho testing

### Phase 3 — CrashLoop E2E (ngày 7-10)

- [ ] CrashLoop playbook: collect → normalize → score → summarize
- [ ] Diagnosis Engine: evidence normalization + hypothesis scoring
- [ ] Command Composer
- [ ] Telegram rendering structured response
- [ ] Test với real cluster: OOMKilled, missing env, dependency timeout

### Phase 4 — Playbook mở rộng (ngày 11-14)

- [ ] Pending playbook
- [ ] Rollout regression playbook
- [ ] Test scenarios cho cả 2

### Phase 5 — Hardening (ngày 15-18)

- [ ] Degraded mode: handle provider timeout/down
- [ ] Redaction rules
- [ ] Timeout + retry logic
- [ ] Progress update trên Telegram (đang check K8s... đang lấy logs...)
- [ ] Structured logging

### Phase 6 — Polish + Test (ngày 19-21)

- [ ] Test full demo scenarios (9 test cases)
- [ ] Polish output format
- [ ] Fix edge cases
- [ ] Document deployment steps

---

## 11. Test Scenarios

### CrashLoop
1. Missing env → container exit nhanh, logs báo env error
2. OOMKilled → terminationReason=OOMKilled, memory near limit
3. Dependency timeout → logs báo connection refused/timeout

### Pending
4. Insufficient CPU → scheduler event báo FailedScheduling
5. Taint mismatch → node có taint, pod không có toleration
6. PVC pending → PVC ở Pending state

### Rollout regression
7. Deploy gây 5xx tăng → error rate spike sau revision mới
8. Deploy gây readiness fail → pods not ready sau rollout
9. False alarm: issue do downstream → metrics cho thấy root cause ở service khác

---

## 12. Acceptance Criteria

MVP done khi:

1. ✅ Telegram bot nhận message và trả kết quả end-to-end
2. ✅ Resolve được target qua exact name hoặc service_map
3. ✅ Chạy được 3 playbook (CrashLoop, Pending, Rollout regression)
4. ✅ Trả structured response đúng format (summary, confidence, evidence, commands)
5. ✅ Có suggested kubectl commands (rollback/restart/inspect)
6. ✅ Degraded mode khi thiếu metrics hoặc logs
7. ✅ Không có write action lên cluster
8. ✅ Giảm được thời gian investigation so với manual kubectl workflow

---

## Tóm tắt thay đổi so với plan gốc

| Cắt bỏ khỏi MVP | Lý do |
|---|---|
| Conversation Manager | Thêm complexity, MVP stateless đủ dùng |
| Fuzzy target matching | Over-engineering, exact + service_map đủ cho MVP |
| Inline button actions | Nice-to-have, không critical |
| Candidate selection UI | Cần fuzzy match, đã cắt |

| Gộp lại | Trước | Sau |
|---|---|---|
| Diagnosis Engine | Evidence Normalizer + Hypothesis Scorer + Summarizer (3 module) | 1 module |

| Thêm mới | Lý do |
|---|---|
| Tech stack section | Plan gốc thiếu |
| MCP server error handling | Failure mode phổ biến nhất |
| Scoring ví dụ cụ thể | Plan gốc mơ hồ về cách scoring |
| Timeline thực tế hơn (3 tuần) | 2 tuần quá lạc quan |
