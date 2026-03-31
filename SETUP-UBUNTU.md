# Local Development Setup (Ubuntu)

Guide for setting up a dev environment on Ubuntu (22.04+) with Docker Engine.

> **macOS?** See [SETUP.md](SETUP.md) — uses `host.docker.internal` which works out of the box with Docker Desktop.

## Why a separate guide?

`host.docker.internal` — used by monitoring pods to reach VictoriaMetrics/VictoriaLogs on the host — only resolves on Docker Desktop (macOS/Windows). On Ubuntu with Docker Engine, it doesn't exist.

This guide uses **separate manifests** (`deploy/monitoring/ubuntu/`) that read the host IP from a Kubernetes ConfigMap. The Makefile auto-detects the kind Docker bridge gateway IP — no manual patching needed.

---

## Data Flow Overview

Same as macOS, but pods reach the host via the Docker bridge gateway IP (e.g. `172.19.0.1`) instead of `host.docker.internal`.

```
kind cluster (ingress ports: 80/443)
  ├── kube-state-metrics ─── scrape ──→ vmagent ─── remote_write ──→ VictoriaMetrics (host:8428)
  ├── kubelet/cAdvisor   ─── scrape ──┘                               via 172.19.0.1
  ├── nginx-ingress      ─── scrape ──┘  (HTTP request metrics for 5xx detection)
  ├── container logs      ─── tail  ──→ vlagent  ─── native push ──→ VictoriaLogs  (host:9428)
  │                                                                    via 172.19.0.1
  ├── vmalert ─── query VM ──→ evaluate rules ──→ Alertmanager ──→ webhook ──→ Bot :8080
  └── test workloads (checkout, worker, payment, webapp-testing ...)
```

---

## Prerequisites

```bash
# Docker Engine
sudo apt-get update && sudo apt-get install -y docker.io
sudo usermod -aG docker $USER  # then re-login

# kind
go install sigs.k8s.io/kind@latest

# kubectl
sudo snap install kubectl --classic

# go (if not installed)
sudo snap install go --classic
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

# Verify
curl -s 'http://localhost:8428/api/v1/query?query=up' | jq .
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=*' | head
```

**UI:**
- Metrics: http://localhost:8428/vmui/
- Logs: http://localhost:9428/select/vmui/

---

## Step 3: Deploy monitoring stack (one command)

This is where it differs from macOS. Instead of applying the default manifests, use:

```bash
make ubuntu-monitoring
```

This command:
1. Auto-detects the kind Docker bridge gateway IP (e.g. `172.19.0.1`)
2. Creates a `host-endpoints` ConfigMap in the `monitoring` namespace with all host URLs
3. Deploys kube-state-metrics, vmagent, vlagent, vmalert, alertmanager — all using the Ubuntu-specific manifests that read URLs from the ConfigMap

Expected output:
```
Detected host IP: 172.19.0.1
...
Done. Ubuntu monitoring deployed to cluster 1 (host: 172.19.0.1)
```

**Verify pods:**
```bash
kubectl -n monitoring get pods
```

**Verify metrics are flowing:**
```bash
# Wait ~30s for first scrape
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=kube_pod_info' | jq '.data.result | length'
curl -s 'http://localhost:8428/api/v1/query' --data-urlencode 'query=container_memory_usage_bytes' | jq '.data.result | length'
```

**Verify logs are flowing:**
```bash
curl -s 'http://localhost:9428/select/logsql/query' -d 'query=*' -d 'limit=5' | jq
```

---

## Step 4: Deploy test workloads

```bash
make scenarios

# Verify
make scenarios-status
```

**Generate 5xx traffic** (requires ingress from Step 1.5):
```bash
# ~10 req/s hitting /503 via ingress, Ctrl+C to stop
make load-5xx

# For cluster 2 (port 8180)
make load-5xx LOAD_5XX_PORT=8180
```

---

## Step 5: Alerting (verify)

Alertmanager was already deployed by `make ubuntu-monitoring`. Verify alerts fire after workloads are unhealthy:

```bash
# Check Alertmanager
kubectl -n monitoring port-forward svc/alertmanager 9093:9093 &
curl -s http://localhost:9093/api/v2/alerts | jq '.[].labels.alertname'

# Wait ~2 min for alert rules to trigger
curl -s 'http://localhost:8428/api/v1/query' \
  --data-urlencode 'query=ALERTS{alertstate="firing"}' | jq '.data.result[] | {alertname: .metric.alertname, pod: .metric.pod}'
```

---

## Step 6: Run the bot

```bash
export TELEGRAM_BOT_TOKEN="your-token-here"
export TELEGRAM_CHAT_ID="your-chat-id"
make run
```

---

## Multi-Cluster (Cluster 2)

```bash
# Create cluster 2 (ports 8180/8443 to avoid conflict with cluster 1)
make cluster2-create

# Install ingress controller on cluster 2
make ingress-cluster2

# Deploy monitoring (Ubuntu)
make ubuntu-cluster2-monitoring

# Deploy test workloads
make cluster2-scenarios

# Configure bot — uncomment clusters section in configs/config.yaml:
#   clusters:
#     - name: lazy-diag
#       context: kind-lazy-diag
#       default: true
#     - name: lazy-diag-2
#       context: kind-lazy-diag-2

# Test
# /check checkout -c lazy-diag-2
# /scan demo-prod -c lazy-diag-2
```

---

## How it works (under the hood)

The Ubuntu manifests in `deploy/monitoring/ubuntu/` differ from the macOS ones in one way: instead of hardcoding `host.docker.internal`, they read URLs from environment variables sourced from a ConfigMap:

```yaml
# macOS (deploy/monitoring/vmagent.yaml)
args:
  - "--remoteWrite.url=http://host.docker.internal:8428/api/v1/write"

# Ubuntu (deploy/monitoring/ubuntu/vmagent.yaml)
args:
  - "--remoteWrite.url=$(VM_WRITE_URL)"
env:
  - name: VM_WRITE_URL
    valueFrom:
      configMapKeyRef:
        name: host-endpoints
        key: vm_write_url
```

The `host-endpoints` ConfigMap is created by `make ubuntu-monitoring` with the auto-detected IP:

```bash
kubectl -n monitoring get configmap host-endpoints -o yaml
# vm_url: http://172.19.0.1:8428
# vm_write_url: http://172.19.0.1:8428/api/v1/write
# vl_write_url: http://172.19.0.1:9428/insert/native
```

For alertmanager, the config YAML itself needs the IP (can't use env vars in a ConfigMap-embedded YAML), so `gen-alertmanager-config.sh` generates it dynamically.

---

## Estimated Resource Usage

Same as macOS — see [SETUP.md](SETUP.md#estimated-resource-usage).

---

## Cleanup

```bash
# Delete cluster
kind delete cluster --name lazy-diag

# Delete Victoria stack
docker rm -f victoria-metrics victoria-logs
docker volume rm victoria-metrics-data victoria-logs-data
```

---

## Troubleshooting

**`make ubuntu-monitoring` fails with "Cannot detect kind network gateway":**
```bash
# Is the kind cluster running?
kind get clusters
docker ps | grep kind

# Manual check of the gateway IP:
docker network inspect kind | grep Gateway
```

**vmagent still can't push metrics after `make ubuntu-monitoring`:**
```bash
# Check the ConfigMap has the right IP
kubectl -n monitoring get configmap host-endpoints -o yaml

# Check vmagent sees the env var
kubectl -n monitoring exec deployment/vmagent -- env | grep VM_WRITE_URL

# Test connectivity from inside the pod
kubectl -n monitoring exec deployment/vmagent -- wget -qO- http://172.19.0.1:8428/api/v1/query?query=up
```

**VictoriaMetrics not reachable from pods (connection refused):**
```bash
# Is Victoria listening?
curl -s http://localhost:8428/api/v1/query?query=up

# Is the firewall blocking Docker bridge traffic?
sudo iptables -L -n | grep 172.19
# If blocked, allow it:
sudo iptables -I INPUT -s 172.19.0.0/16 -j ACCEPT
```

**kind cluster IP changed after recreating:**

The gateway IP may change if you delete and recreate the kind cluster. Re-run `make ubuntu-monitoring` to update the ConfigMap.
