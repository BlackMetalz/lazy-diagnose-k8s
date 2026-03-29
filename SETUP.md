# Local Development Setup

Guide for setting up a dev environment. Tested on macOS (M1/M2) and Ubuntu 22.04+.

## Data Flow Overview

```
kind cluster
  ├── kube-state-metrics ─── scrape ──→ vmagent ─── remote_write ──→ VictoriaMetrics (host:8428)
  ├── kubelet/cAdvisor   ─── scrape ──┘
  ├── container logs      ─── tail  ──→ vlagent  ─── native push ──→ VictoriaLogs  (host:9428)
  ├── vmalert ─── query VM ──→ evaluate rules ──→ Alertmanager ──→ webhook ──→ Bot :8080
  └── test workloads (checkout, worker, payment, ...)

Bot (host)
  ├── :8080 ← Alertmanager webhooks (auto-diagnosis)
  ├── Telegram polling (manual /check, /scan)
  └── → K8s API / VictoriaMetrics / VictoriaLogs
```

Without this pipeline, providers query empty data sources.

---

## Prerequisites

**macOS:**
```bash
brew install kind kubectl go
# Docker Desktop must be running, allocate >= 4GB RAM
```

**Ubuntu:**
```bash
# Docker Engine (not Docker Desktop)
sudo apt-get install -y docker.io
# kind
go install sigs.k8s.io/kind@latest
# kubectl
sudo snap install kubectl --classic
# go
sudo snap install go --classic
```

---

## Host IP: macOS vs Ubuntu

Pods inside kind need to reach VictoriaMetrics/VictoriaLogs running on the host. The monitoring manifests use `host.docker.internal` by default — this works on **macOS** (Docker Desktop) but **not on Ubuntu** (Docker Engine).

**macOS (Docker Desktop):** `host.docker.internal` resolves automatically. No extra steps needed.

**Ubuntu (Docker Engine):** Use the Docker bridge gateway IP instead. Run this once to get it:

```bash
HOST_IP=$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}}{{end}}{{end}}' | grep -oE '([0-9]+\.){3}[0-9]+')
echo "Host IP: $HOST_IP"
# Example output: 172.19.0.1
```

Then patch all monitoring manifests to replace `host.docker.internal` with the actual IP:

```bash
# Patch all monitoring YAMLs at once
sed -i "s|host.docker.internal|${HOST_IP}|g" \
  deploy/monitoring/vmagent.yaml \
  deploy/monitoring/vmagent-cluster2.yaml \
  deploy/monitoring/vlagent.yaml \
  deploy/monitoring/vlagent-cluster2.yaml \
  deploy/monitoring/vmalert.yaml \
  deploy/monitoring/alertmanager.yaml \
  deploy/bot/deployment.yaml
```

> **Important:** Don't commit these changes — they're local to your machine. To undo: `git checkout deploy/`

Alternatively, set `HOST_IP` and use the Makefile shortcut:

```bash
make host-ip-patch HOST_IP=172.19.0.1
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

## Step 2: VictoriaMetrics + VictoriaLogs on host

Run on Docker host. Pods inside kind reach these via `host.docker.internal` (macOS) or the Docker bridge gateway IP (Ubuntu — see [Host IP section](#host-ip-macos-vs-ubuntu) above).

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
kubectl -n monitoring logs deployment/vmagent --tail=20
```

**Verify metrics are flowing:**
```bash
# Wait ~30s for vmagent to complete first scrape cycle
sleep 30
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
kubectl -n monitoring logs daemonset/vlagent --tail=20
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
# Alert rules (CrashLoop, Pending, OOMKilled, ImagePull, Replicas mismatch)
kubectl apply -f deploy/monitoring/alert-rules.yaml

# Alertmanager — receives alerts from vmalert, sends webhooks to bot
kubectl apply -f deploy/monitoring/alertmanager.yaml

# vmalert — evaluates rules against VictoriaMetrics, sends to Alertmanager
kubectl apply -f deploy/monitoring/vmalert.yaml

# Verify
kubectl -n monitoring get pods
kubectl -n monitoring logs deployment/vmalert --tail=10
kubectl -n monitoring logs deployment/alertmanager --tail=10
```

**Verify alerts are firing** (after deploying test workloads):
```bash
# Check vmalert alerts
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=ALERTS{alertstate="firing"}' | jq '.data.result[] | {alertname: .metric.alertname, pod: .metric.pod, severity: .metric.severity}'

# Check Alertmanager
kubectl -n monitoring port-forward svc/alertmanager 9093:9093 &
curl -s http://localhost:9093/api/v2/alerts | jq '.[].labels.alertname'
```

**Note:** The bot must be running with `webhook.enabled: true` (default port `:8080`) for Alertmanager to deliver alerts. Alertmanager inside kind reaches the bot via `host.docker.internal:8080` (macOS) or `<HOST_IP>:8080` (Ubuntu — see [Host IP section](#host-ip-macos-vs-ubuntu)).

---

## Step 6: Deploy test workloads

```bash
kubectl create namespace prod
kubectl apply -f deploy/test-workloads/workloads.yaml
```

Base workloads create 3 test scenarios:

| Workload | Expected State | Diagnosis Case |
|---|---|---|
| `checkout` | CrashLoopBackOff (OOMKilled) | CrashLoop playbook |
| `worker` | Pending (requests 100 CPU, 256Gi RAM) | Pending playbook |
| `payment` | Running (healthy) | Baseline / no issue |

Additional scenarios (deployed via `make scenarios`):

| Workload | Expected State | Diagnosis Case |
|---|---|---|
| `api-config-missing` | CrashLoopBackOff | Missing env var |
| `api-dependency-fail` | CrashLoopBackOff | Connection refused |
| `api-runtime-crash` | CrashLoopBackOff | App runs 30s then crashes |
| `api-init-fail` | Init:CrashLoopBackOff | Init container migration fail |
| `api-bad-image` | ErrImagePull | Image not found |
| `api-probe-fail` | Running + restarts | Liveness probe fail |
| `api-not-ready` | Running (0/1 Ready) | Readiness probe fail — silent failure |
| `ml-worker-taint` | Pending | Node selector mismatch |
| `db-pvc-pending` | Pending | PVC not bound |

Full details — see [deploy/test-workloads/SCENARIOS.md](deploy/test-workloads/SCENARIOS.md).

**Verify:**
```bash
kubectl get pods -n prod
```

**Verify end-to-end data flow:**
```bash
# Metrics: restart count
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=kube_pod_container_status_restarts_total{namespace="prod"}' | jq '.data.result[] | {pod: .metric.pod, restarts: .value[1]}'

# Logs: checkout container
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=kubernetes.pod_namespace:prod AND kubernetes.container_name:checkout' -d 'limit=10' | jq
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
| vmalert | ~32MB | inside kind |
| Alertmanager | ~32MB | inside kind |
| VictoriaMetrics | ~200MB | Docker host |
| VictoriaLogs | ~200MB | Docker host |
| Bot | ~50MB | Go binary |
| **Total** | **~2.8GB** | M1 Pro 16GB: plenty of headroom |

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

**vmagent can't push metrics (`lookup host.docker.internal: no such host`):**

This is the most common issue on **Ubuntu**. `host.docker.internal` only works with Docker Desktop (macOS/Windows).

```bash
kubectl -n monitoring logs deployment/vmagent
```

Fix: patch manifests with the Docker bridge gateway IP (see [Host IP section](#host-ip-macos-vs-ubuntu)):
```bash
HOST_IP=$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}}{{end}}{{end}}' | grep -oE '([0-9]+\.){3}[0-9]+')
make host-ip-patch HOST_IP=$HOST_IP

# Re-apply and restart
kubectl apply -f deploy/monitoring/vmagent.yaml
kubectl -n monitoring rollout restart deployment/vmagent
```

Verify connectivity from inside the pod:
```bash
# macOS
kubectl -n monitoring exec deployment/vmagent -- wget -qO- http://host.docker.internal:8428/api/v1/query?query=up

# Ubuntu (use your HOST_IP)
kubectl -n monitoring exec deployment/vmagent -- wget -qO- http://172.19.0.1:8428/api/v1/query?query=up
```

**vlagent can't push logs (same `no such host` error):**

Same root cause as above. After patching manifests:
```bash
kubectl apply -f deploy/monitoring/vlagent.yaml
kubectl -n monitoring rollout restart daemonset/vlagent

# Verify
kubectl -n monitoring exec daemonset/vlagent -- wget -qO- http://172.19.0.1:9428/health
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
docker ps  # Docker Desktop (macOS) or Docker Engine (Ubuntu) must be running
kind get clusters
```
