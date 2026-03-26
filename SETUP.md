# Local Development Setup

Hướng dẫn setup môi trường dev trên Mac M1 (16GB RAM).

## Data flow tổng thể

```
kind cluster
  ├── kube-state-metrics ─── scrape ──→ vmagent ─── remote_write ──→ VictoriaMetrics (host:8428)
  ├── kubelet/cAdvisor   ─── scrape ──┘
  ├── container logs      ─── tail  ──→ vlagent  ─── native push ──→ VictoriaLogs  (host:9428)
  └── test workloads (checkout, worker, payment)

Bot ──→ MCP servers ──→ K8s API / VictoriaMetrics / VictoriaLogs
```

Không có pipeline này thì MCP server query vào data source trống.

---

## Prerequisites

```bash
brew install kind kubectl go
# Docker Desktop cần đang chạy, allocate >= 4GB RAM
```

---

## Step 1: Tạo kind cluster

```bash
kind create cluster --config deploy/kind-config.yaml

# Verify
kubectl cluster-info --context kind-lazy-diag
kubectl get nodes
```

---

## Step 2: VictoriaMetrics + VictoriaLogs trên host

Chạy trên Docker host, nhận data từ trong kind cluster qua `host.docker.internal`.

```bash
# VictoriaMetrics — nhận metrics từ vmagent
docker run -d \
  --name victoria-metrics \
  --restart unless-stopped \
  -p 8428:8428 \
  -v victoria-metrics-data:/victoria-metrics-data \
  victoriametrics/victoria-metrics:v1.138.0 \
  -retentionPeriod=7d \
  -selfScrapeInterval=15s

# VictoriaLogs — nhận logs từ vlagent
docker run -d \
  --name victoria-logs \
  --restart unless-stopped \
  -p 9428:9428 \
  -v victoria-logs-data:/victoria-logs-data \
  victoriametrics/victoria-logs:v1.48.0
```

**Verify:**
```bash
curl -s 'http://localhost:8428/api/v1/query?query=up' | jq .
curl -s http://localhost:9428/select/logsql/query -d 'query=*' | head
```

**UI:**
- Metrics: http://localhost:8428/vmui/
- Logs: http://localhost:9428/select/vmui/

---

## Step 3: Metrics pipeline (kube-state-metrics + vmagent)

Deploy vào kind cluster. vmagent scrape metrics rồi remote_write tới VictoriaMetrics trên host.

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
# Tạo namespace
kubectl create namespace monitoring

# Deploy kube-state-metrics — expose K8s object metrics (pod status, restart count, ...)
kubectl apply -f deploy/monitoring/kube-state-metrics.yaml

# Deploy vmagent — scrape kube-state-metrics + kubelet/cAdvisor, push to VictoriaMetrics
kubectl apply -f deploy/monitoring/vmagent.yaml

# Verify
kubectl -n monitoring get pods
kubectl -n monitoring logs deployment/vmagent --tail=20
```

**Verify metrics đang chảy:**
```bash
# Đợi ~30s cho vmagent scrape xong cycle đầu tiên

# Check kube-state-metrics data
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=kube_pod_info' | jq '.data.result | length'

# Check container metrics
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=container_memory_usage_bytes' | jq '.data.result | length'

# Restart count
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=kube_pod_container_status_restarts_total' | jq '.data.result[:2]'
```

Nếu kết quả > 0 → metrics pipeline hoạt động.

---

## Step 4: Logs pipeline (vlagent)

Deploy vlagent DaemonSet vào kind. Native log collector của VictoriaMetrics, build riêng cho VictoriaLogs.

Tại sao vlagent thay vì fluent-bit/vector:
- **4.6x throughput** so với fluent-bit (~143k vs ~31k logs/s)
- **4.2x ít CPU** hơn fluent-bit, **10.5x ít CPU** hơn vector
- **28 MiB RAM** — nhẹ nhất trong tất cả collectors
- Fluent-bit và vector bị **mất log khi file rotation** — vlagent không có issue này
- Native protocol tới VictoriaLogs, không cần config phức tạp

> Source: [VictoriaMetrics Log Collectors Benchmark 2026](https://victoriametrics.com/blog/log-collectors-benchmark-2026/)

```
/var/log/containers/*.log
        ↓ tail + K8s metadata enrichment (pod labels, annotations, node info)
    vlagent (DaemonSet, mỗi node)
        ↓ native protocol
VictoriaLogs (host:9428)
```

```bash
kubectl apply -f deploy/monitoring/vlagent.yaml

# Verify
kubectl -n monitoring get pods -l app=vlagent
kubectl -n monitoring logs daemonset/vlagent --tail=20
```

**Verify logs đang chảy:**
```bash
# Query tất cả logs
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=*' -d 'limit=5' | jq

# Query logs từ namespace prod
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=kubernetes.pod_namespace:prod' -d 'limit=5' | jq

# Query error logs
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=_msg:error OR _msg:Error' -d 'limit=5' | jq
```

Nếu có kết quả → logs pipeline hoạt động.

---

## Step 5: Deploy test workloads

```bash
kubectl create namespace prod
kubectl apply -f deploy/test-workloads/workloads.yaml
```

3 workloads tạo ra 3 test scenarios:

| Workload | Expected State | Diagnosis Case |
|---|---|---|
| `checkout` | CrashLoopBackOff (OOMKilled) | CrashLoop playbook |
| `worker` | Pending (request 100 CPU, 256Gi RAM) | Pending playbook |
| `payment` | Running (healthy) | Baseline / no issue |

**Verify:**
```bash
kubectl get pods -n prod

# Expect:
# checkout-xxx   0/1   CrashLoopBackOff   ...
# worker-xxx     0/1   Pending            ...
# payment-xxx    2/2   Running            ...
```

**Verify data flow end-to-end:**
```bash
# Metrics: restart count cho checkout pod
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=kube_pod_container_status_restarts_total{namespace="prod"}' | jq '.data.result[] | {pod: .metric.pod, restarts: .value[1]}'

# Metrics: memory usage
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=container_memory_usage_bytes{namespace="prod",container!=""}' | jq '.data.result[] | {pod: .metric.pod, container: .metric.container, bytes: .value[1]}'

# Logs: checkout container logs
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=kubernetes.pod_namespace:prod AND kubernetes.container_name:checkout' -d 'limit=10' | jq

# Logs: payment container logs
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=kubernetes.pod_namespace:prod AND kubernetes.container_name:payment' -d 'limit=10' | jq
```

---

## Step 6: Chạy bot

```bash
# Tạo Telegram bot:
# 1. Chat @BotFather trên Telegram
# 2. /newbot → lấy token

export TELEGRAM_BOT_TOKEN="your-token-here"
make run
```

---

## Resource usage ước tính

| Component | RAM | Note |
|---|---|---|
| kind cluster (2 nodes) | ~2GB | control-plane + worker |
| kube-state-metrics | ~64MB | trong kind |
| vmagent | ~128MB | trong kind |
| vlagent | ~28MB per node | DaemonSet, 2 nodes |
| VictoriaMetrics | ~200MB | Docker host |
| VictoriaLogs | ~200MB | Docker host |
| Bot | ~50MB | Go binary |
| **Total** | **~2.7GB** | M1 Pro 16GB: thoải mái |

---

## Cleanup

```bash
# Xóa cluster (xóa hết kube-state-metrics, vmagent, vlagent, workloads)
kind delete cluster --name lazy-diag

# Xóa Victoria stack
docker rm -f victoria-metrics victoria-logs
docker volume rm victoria-metrics-data victoria-logs-data
```

---

## Troubleshooting

**vmagent không push được metrics:**
```bash
# Check logs
kubectl -n monitoring logs deployment/vmagent

# Common issue: host.docker.internal not resolving
# Fix: đảm bảo Docker Desktop đang chạy (không phải colima/rancher)
kubectl -n monitoring exec deployment/vmagent -- wget -qO- http://host.docker.internal:8428/api/v1/query?query=up
```

**vlagent không push được logs:**
```bash
kubectl -n monitoring logs daemonset/vlagent

# Test connectivity
kubectl -n monitoring exec daemonset/vlagent -- wget -qO- http://host.docker.internal:9428/health
```

**VictoriaMetrics/Logs container không start:**
```bash
docker logs victoria-metrics
docker logs victoria-logs

# Port conflict?
lsof -i :8428
lsof -i :9428
```

**kind cluster không start:**
```bash
docker ps  # Docker Desktop phải đang chạy
kind get clusters
```
