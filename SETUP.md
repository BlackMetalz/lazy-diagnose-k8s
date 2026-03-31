# Local Development Setup (macOS)

Guide for setting up a dev environment on macOS with Docker Desktop.

> **Ubuntu?** See [SETUP-UBUNTU.md](SETUP-UBUNTU.md) — uses separate manifests that auto-detect the host IP. No patching needed.

## Data Flow Overview

```
kind cluster (ingress ports: 80/443)
  ├── kube-state-metrics ─── scrape ──→ vmagent ─── remote_write ──→ VictoriaMetrics (host:8428)
  ├── kubelet/cAdvisor   ─── scrape ──┘
  ├── nginx-ingress      ─── scrape ──┘  (HTTP request metrics for 5xx detection)
  ├── container logs      ─── tail  ──→ vlagent  ─── native push ──→ VictoriaLogs  (host:9428)
  ├── vmalert ─── query VM ──→ evaluate rules ──→ Alertmanager ──→ webhook ──→ Bot :8080
  └── test workloads (checkout, worker, payment, webapp-testing ...)

Bot (host)
  ├── :8080 ← Alertmanager webhooks (auto-diagnosis)
  ├── Telegram polling (manual /check, /scan)
  └── → K8s API / VictoriaMetrics / VictoriaLogs
```

Without this pipeline, providers query empty data sources.

---

## Prerequisites

```bash
brew install kind kubectl go
# Docker Desktop must be running, allocate >= 4GB RAM
```

---

## Step 1: Create kind cluster

```bash
kind create cluster --config deploy/kind-config.yaml

# Verify
kubectl cluster-info --context kind-lazy-diag
kubectl get nodes
```

---

## Step 1.5: Install Nginx Ingress Controller (optional, for HTTP metrics)

Required for HTTP 5xx error rate detection. `make ingress` installs nginx-ingress-controller and enables Prometheus metrics (`--enable-metrics=true`, port 10254).

```bash
make ingress

# Verify controller is running
kubectl get pods -n ingress-nginx

# Verify metrics endpoint (after sending at least one request through ingress)
kubectl -n ingress-nginx exec deploy/ingress-nginx-controller -- curl -s localhost:10254/metrics | grep nginx_ingress_controller_requests | head -3
```

> **Note:** The kind config has `extraPortMappings` for ports 80/443. If you created the cluster before this change, recreate it: `kind delete cluster --name lazy-diag && kind create cluster --config deploy/kind-config.yaml`

---

## Step 2: VictoriaMetrics + VictoriaLogs on host

Run on Docker host, receive data from inside the kind cluster via `host.docker.internal`.

```bash
# VictoriaMetrics — receives metrics from vmagent
docker run -d \
  --name victoria-metrics \
  --restart unless-stopped \
  -p 8428:8428 \
  -v victoria-metrics-data:/victoria-metrics-data \
  victoriametrics/victoria-metrics:v1.138.0 \
  -retentionPeriod=7d

# VictoriaLogs — receives logs from vlagent
docker run -d \
  --name victoria-logs \
  --restart unless-stopped \
  -p 9428:9428 \
  -v victoria-logs-data:/victoria-logs-data \
  victoriametrics/victoria-logs:v1.48.0

# Check it
docker ps
```

**Verify:**
```bash
curl -s 'http://localhost:8428/api/v1/query?query=up' | jq .
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=*' | head
```

**UI:**
- Metrics: http://localhost:8428/vmui/
- Logs: http://localhost:9428/select/vmui/

---

## Step 3: Metrics pipeline (kube-state-metrics + vmagent)

Deploy into the kind cluster. vmagent scrapes metrics and remote_writes to VictoriaMetrics on the host.

```
kube-state-metrics (pod/deployment/restart metrics)
        ↓ scrape
kubelet/cAdvisor (CPU, memory, container metrics)
        ↓ scrape
     vmagent
        ↓ remote_write
VictoriaMetrics (host:8428)
```

```bash
kubectl create namespace monitoring

# kube-state-metrics — exposes K8s object metrics (pod status, restart count, ...)
kubectl apply -f deploy/monitoring/kube-state-metrics.yaml

# vmagent — scrapes kube-state-metrics + kubelet/cAdvisor, pushes to VictoriaMetrics
kubectl apply -f deploy/monitoring/vmagent.yaml

# Sleep to wait
sleep 30

# Verify
kubectl -n monitoring get pods
```

**Verify metrics are flowing:**
```bash
# Wait ~30s for vmagent to complete first scrape cycle
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=kube_pod_info' | jq '.data.result | length'
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=container_memory_usage_bytes' | jq '.data.result | length'
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=kube_pod_container_status_restarts_total' | jq '.data.result[:2]'
```

Results > 0 means the metrics pipeline is working.

---

## Step 4: Logs pipeline (vlagent)

Deploy vlagent DaemonSet into kind. Native log collector from VictoriaMetrics, purpose-built for VictoriaLogs.

Why vlagent over fluent-bit/vector:
- **4.6x throughput** vs fluent-bit (~143k vs ~31k logs/s)
- **4.2x less CPU** than fluent-bit, **10.5x less CPU** than vector
- **28 MiB RAM** — lowest among all tested collectors
- Fluent-bit and vector **lose logs during file rotation** — vlagent does not
- Native protocol to VictoriaLogs, minimal config

> Source: [VictoriaMetrics Log Collectors Benchmark 2026](https://victoriametrics.com/blog/log-collectors-benchmark-2026/)

```
/var/log/containers/*.log
        ↓ tail + K8s metadata enrichment (pod labels, annotations, node info)
    vlagent (DaemonSet, one per node)
        ↓ native protocol
VictoriaLogs (host:9428)
```

```bash
kubectl apply -f deploy/monitoring/vlagent.yaml

# Verify
kubectl -n monitoring get pods -l app=vlagent
```

**Verify logs are flowing:**
```bash
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=*' -d 'limit=5' | jq
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=kubernetes.pod_namespace:prod' -d 'limit=5' | jq
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=_msg:error OR _msg:Error' -d 'limit=5' | jq
```

---

## Step 5: Alerting pipeline (vmalert + Alertmanager)

Evaluates alerting rules against VictoriaMetrics. When an alert fires, Alertmanager sends a webhook to the bot, which auto-diagnoses and sends results to Telegram.

```
VictoriaMetrics (metrics data)
        ↓ query
     vmalert (evaluates alerting rules every 15s)
        ↓ fires alert
  Alertmanager (routes + groups alerts)
        ↓ webhook POST
  Bot :8080/webhook/alertmanager
        ↓ auto-diagnose
  Telegram (sends diagnosis + action buttons)
```

```bash
# Alert rules (CrashLoop, Pending, OOMKilled, ImagePull, HTTP 5xx, Replicas mismatch)
kubectl apply -f deploy/monitoring/alert-rules.yaml

# Alertmanager — receives alerts from vmalert, sends webhooks to bot
kubectl apply -f deploy/monitoring/alertmanager.yaml

# vmalert — evaluates rules against VictoriaMetrics, sends to Alertmanager
kubectl apply -f deploy/monitoring/vmalert.yaml

# Verify
kubectl -n monitoring get pods
```

**Verify alerts are firing** (after deploying test workloads):
```bash
# Check vmalert alerts
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=ALERTS{alertstate="firing"}' | jq '.data.result[] | {alertname: .metric.alertname, pod: .metric.pod, severity: .metric.severity}'

# Check Alertmanager
kubectl -n monitoring port-forward svc/alertmanager 9093:9093 &
curl -s http://localhost:9093/api/v2/alerts | jq '.[].labels.alertname'
```

**vmalert UI** (see all rules + firing status):
```bash
kubectl -n monitoring port-forward deployment/vmalert 8880:8880
# Open http://localhost:8880/vmalert/groups
```

**Note:** The bot must be running with `webhook.enabled: true` (default port `:8080`) for Alertmanager to deliver alerts. Alertmanager inside kind reaches the bot via `host.docker.internal:8080`.

---

## Step 6: Deploy test workloads

Deploy all test scenarios at once:

```bash
make scenarios
```

This creates namespaces (`demo-prod`, `demo-staging`, `demo-infra`) and deploys all test workloads.

Or step by step:

```bash
# Create namespaces first (required!)
kubectl apply -f deploy/test-workloads/namespaces.yaml

# Base workloads
kubectl apply -f deploy/test-workloads/workloads.yaml

# All scenario workloads
kubectl apply -f deploy/test-workloads/
```

**Verify:**
```bash
make scenarios-status
```

Test scenarios:

| Namespace | Workload | Expected State | Diagnosis Case |
|---|---|---|---|
| demo-prod | `checkout` | CrashLoopBackOff (OOMKilled) | CrashLoop playbook |
| demo-prod | `worker` | Pending (requests 100 CPU) | Pending playbook |
| demo-prod | `payment` | Running (healthy) | Baseline / no issue |
| demo-staging | `api-config-missing` | CrashLoopBackOff | Missing env var |
| demo-staging | `api-dependency-fail` | CrashLoopBackOff | Connection refused |
| demo-staging | `api-runtime-crash` | CrashLoopBackOff | App runs 30s then crashes |
| demo-staging | `api-init-fail` | Init:CrashLoopBackOff | Init container migration fail |
| demo-staging | `webapp-testing` | Running (5xx via ingress) | HTTP error rate spike |
| demo-infra | `api-bad-image` | ErrImagePull | Image not found |
| demo-infra | `api-probe-fail` | Running + restarts | Liveness probe fail |
| demo-infra | `api-not-ready` | Running (0/1 Ready) | Readiness probe fail |
| demo-infra | `ml-worker-taint` | Pending | Node selector mismatch |
| demo-infra | `db-pvc-pending` | Pending | PVC not bound |

Full details — see [deploy/test-workloads/SCENARIOS.md](deploy/test-workloads/SCENARIOS.md).

**Generate 5xx traffic** (requires ingress from Step 1.5):
```bash
# ~10 req/s hitting /503 via ingress, Ctrl+C to stop
make load-5xx
```

Wait ~2 min for `HighHTTP5xxErrorRate` alert to fire. Verify with `/check webapp-testing -n demo-staging` in Telegram.

**Verify end-to-end data flow:**
```bash
# Metrics: restart count
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=kube_pod_container_status_restarts_total{namespace="demo-prod"}' | jq '.data.result[] | {pod: .metric.pod, restarts: .value[1]}'

# Metrics: 5xx error rate (after running load-5xx)
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=rate(nginx_ingress_controller_requests{status=~"5.."}[5m])' | jq '.data.result[] | {service: .metric.service, status: .metric.status, rate: .value[1]}'

# Logs: checkout container
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=kubernetes.pod_namespace:demo-prod AND kubernetes.container_name:checkout' -d 'limit=10' | jq
```

---

## Step 7: Run the bot

```bash
# Create a Telegram bot:
# 1. Chat @BotFather on Telegram
# 2. /newbot → copy the token

export TELEGRAM_BOT_TOKEN="your-token-here"
export TELEGRAM_CHAT_ID="your-chat-id"  # Get from @userinfobot on Telegram
make run
```

---

## Estimated Resource Usage

| Component | RAM | Note |
|---|---|---|
| kind cluster (2 nodes) | ~2GB | control-plane + worker |
| kube-state-metrics | ~64MB | inside kind |
| vmagent | ~128MB | inside kind |
| vlagent | ~28MB per node | DaemonSet, 2 nodes |
| nginx-ingress | ~128MB | inside kind (optional) |
| vmalert | ~32MB | inside kind |
| Alertmanager | ~32MB | inside kind |
| VictoriaMetrics | ~200MB | Docker host |
| VictoriaLogs | ~200MB | Docker host |
| Bot | ~50MB | Go binary |
| **Total** | **~2.9GB** | M1 Pro 16GB: plenty of headroom |

---

## Cleanup

```bash
# Delete cluster (removes kube-state-metrics, vmagent, vlagent, workloads)
kind delete cluster --name lazy-diag

# Delete Victoria stack
docker rm -f victoria-metrics victoria-logs
docker volume rm victoria-metrics-data victoria-logs-data
```

---

## Troubleshooting

**vmagent can't push metrics:**
```bash
kubectl -n monitoring logs deployment/vmagent

# Common issue: host.docker.internal not resolving
# Fix: make sure Docker Desktop is running (not colima/rancher)
# If on Ubuntu, use the Ubuntu-specific setup instead: see SETUP-UBUNTU.md
kubectl -n monitoring exec deployment/vmagent -- wget -qO- http://host.docker.internal:8428/api/v1/query?query=up
```

**vlagent can't push logs:**
```bash
kubectl -n monitoring logs daemonset/vlagent

kubectl -n monitoring exec daemonset/vlagent -- wget -qO- http://host.docker.internal:9428/health
```

**VictoriaMetrics/Logs container won't start:**
```bash
docker logs victoria-metrics
docker logs victoria-logs

# Port conflict?
lsof -i :8428
lsof -i :9428
```

**kind cluster won't start:**
```bash
docker ps  # Docker Desktop must be running
kind get clusters
```
