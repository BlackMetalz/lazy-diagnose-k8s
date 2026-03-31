# Cluster 2 Setup (Multi-Cluster)

Guide for adding a second kind cluster to test multi-cluster diagnosis. Cluster 1 must be set up first — see [SETUP.md](SETUP.md).

## Data Flow Overview

```
kind cluster "lazy-diag-2" (ingress ports: 8180/8443)
  ├── kube-state-metrics ─── scrape ──→ vmagent ─── remote_write ──→ VictoriaMetrics (host:8428)  ← shared with cluster 1
  ├── kubelet/cAdvisor   ─── scrape ──┘                               (label: cluster=lazy-diag-2)
  ├── nginx-ingress      ─── scrape ──┘                               (HTTP request metrics)
  └── container logs      ─── tail  ──→ vlagent  ─── native push ──→ VictoriaLogs  (host:9428)   ← shared with cluster 1
                                                                       (label: cluster=lazy-diag-2)
Bot (host)
  ├── Reads kubeconfig contexts: kind-lazy-diag (cluster 1), kind-lazy-diag-2 (cluster 2)
  ├── Telegram: /check checkout -c lazy-diag-2
  └── Metrics/Logs queries filtered by cluster label
```

Both clusters push to the **same** VictoriaMetrics/VictoriaLogs instances. Data is separated by `cluster` label added via vmagent `external_labels` and vlagent `-remoteWrite.label`.

---

## Prerequisites

- Cluster 1 fully set up per [SETUP.md](SETUP.md) (kind cluster + VictoriaMetrics + VictoriaLogs running on host)
- Docker Desktop running

---

## Step 1: Create kind cluster 2

```bash
make cluster2-create

# Verify
kubectl cluster-info --context kind-lazy-diag-2
kubectl get nodes --context kind-lazy-diag-2

# Both clusters should exist
kind get clusters
# lazy-diag
# lazy-diag-2
```

---

## Step 1.5: Install Nginx Ingress Controller (optional, for HTTP metrics)

Cluster 2 uses ports **8180/8443** to avoid conflict with cluster 1's 80/443. `make ingress-cluster2` installs nginx-ingress-controller and enables Prometheus metrics (`--enable-metrics=true`, port 10254).

```bash
make ingress-cluster2

# Verify controller is running
kubectl --context kind-lazy-diag-2 get pods -n ingress-nginx

# Verify metrics endpoint (after sending at least one request through ingress)
kubectl --context kind-lazy-diag-2 -n ingress-nginx exec deploy/ingress-nginx-controller -- curl -s localhost:10254/metrics | grep nginx_ingress_controller_requests | head -3
```

> **Note:** If cluster 2 was created before the kind-config-cluster2.yaml update (extraPortMappings), recreate it: `make cluster2-clean && make cluster2-create`

---

## Step 2: Deploy monitoring stack into cluster 2

Cluster 2 uses its own vmagent/vlagent that push to the shared Victoria instances on the host. The key difference from cluster 1:

- **vmagent**: `external_labels: { cluster: lazy-diag-2 }` — all scraped metrics carry this label
- **vlagent**: `-remoteWrite.label=cluster=lazy-diag-2` — all collected logs carry this label

```bash
make cluster2-monitoring

# Verify pods are running
kubectl --context kind-lazy-diag-2 -n monitoring get pods
```

Wait ~30s for vmagent to complete first scrape cycle.

**Verify metrics are flowing with cluster label:**
```bash
# Should return results with cluster="lazy-diag-2"
curl -s 'http://localhost:8428/api/v1/query' \
  --data-urlencode 'query=kube_pod_info{cluster="lazy-diag-2"}' | jq '.data.result | length'

# Compare: cluster 1 metrics
curl -s 'http://localhost:8428/api/v1/query' \
  --data-urlencode 'query=kube_pod_info{cluster="lazy-diag"}' | jq '.data.result | length'
```

**Verify logs are flowing with cluster label:**
```bash
curl -s 'http://localhost:9428/select/logsql/query' \
  -d 'query=cluster:lazy-diag-2' -d 'limit=5' | jq
```

---

## Step 3: Update cluster 1 monitoring (one-time)

Cluster 1's vmagent/vlagent need updating to also tag data with `cluster=lazy-diag` label. Without this, cluster 1 data has no cluster label and can't be distinguished from cluster 2.

```bash
kubectl --context kind-lazy-diag apply -f deploy/monitoring/vmagent.yaml
kubectl --context kind-lazy-diag apply -f deploy/monitoring/vlagent.yaml

# Restart to pick up config changes
kubectl --context kind-lazy-diag -n monitoring rollout restart deployment/vmagent
kubectl --context kind-lazy-diag -n monitoring rollout restart daemonset/vlagent
```

**Verify cluster 1 now has the label:**
```bash
# Wait ~30s for new scrape cycle
curl -s 'http://localhost:8428/api/v1/query' \
  --data-urlencode 'query=kube_pod_info{cluster="lazy-diag"}' | jq '.data.result | length'
```

---

## Step 4: Deploy test workloads into cluster 2

```bash
make cluster2-scenarios

# Verify
kubectl --context kind-lazy-diag-2 get pods -A | grep -E 'demo-'
```

**Generate 5xx traffic on cluster 2** (requires ingress from Step 1.5):
```bash
# Cluster 2 ingress is on port 8180, Ctrl+C to stop
make load-5xx LOAD_5XX_PORT=8180
```

---

## Step 5: Configure bot for multi-cluster

Edit `configs/config.yaml` — uncomment the `clusters` section:

```yaml
clusters:
  - name: lazy-diag
    context: kind-lazy-diag
    default: true
  - name: lazy-diag-2
    context: kind-lazy-diag-2
```

---

## Step 6: Run the bot

```bash
export TELEGRAM_BOT_TOKEN="your-token-here"
export TELEGRAM_CHAT_ID="your-chat-id"
make run
```

You should see in logs:
```
cluster initialized  name=lazy-diag    context=kind-lazy-diag
cluster initialized  name=lazy-diag-2  context=kind-lazy-diag-2
bot configured       clusters=2        default=lazy-diag
```

---

## Step 7: Test multi-cluster commands

```
# Default cluster (lazy-diag)
/check checkout
/scan prod

# Cluster 2
/check checkout -c lazy-diag-2
/scan demo-prod -c lazy-diag-2
/deploy payment -c lazy-diag-2
```

Inline action buttons (AI Investigation, Static Analysis, Logs) automatically target the correct cluster.

---

## Alerting for Cluster 2 (Optional)

To receive alerts from cluster 2, deploy vmalert + alertmanager into cluster 2:

```bash
# Alert rules
kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/alert-rules.yaml

# Alertmanager — update webhook URL if bot runs on host
kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/alertmanager.yaml

# vmalert — evaluates rules against VictoriaMetrics on host
kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/vmalert.yaml
```

Alerts from cluster 2 will carry `cluster=lazy-diag-2` label (propagated from vmagent `external_labels` → kube-state-metrics → alert rules). The bot uses this label to route diagnosis to the correct cluster's K8s API.

---

## Resource Usage (Cluster 2 only)

| Component | RAM | Note |
|---|---|---|
| kind cluster (2 nodes) | ~2GB | control-plane + worker |
| kube-state-metrics | ~64MB | inside kind |
| vmagent | ~128MB | inside kind |
| vlagent | ~28MB per node | DaemonSet, 2 nodes |
| nginx-ingress | ~128MB | inside kind (optional) |
| **Total cluster 2** | **~2.4GB** | |
| **Total both clusters** | **~5.3GB** | Cluster 1 (~2.9GB) + Cluster 2 (~2.4GB) |

---

## Cleanup

```bash
# Delete cluster 2 only
make cluster2-clean

# Delete both clusters
make cluster2-clean
kind delete cluster --name lazy-diag
docker rm -f victoria-metrics victoria-logs
docker volume rm victoria-metrics-data victoria-logs-data
```

---

## Troubleshooting

**vmagent in cluster 2 can't push metrics:**
```bash
kubectl --context kind-lazy-diag-2 -n monitoring logs deployment/vmagent

# Test host connectivity from inside cluster 2
# macOS:
kubectl --context kind-lazy-diag-2 -n monitoring exec deployment/vmagent -- \
  wget -qO- http://host.docker.internal:8428/api/v1/query?query=up
# Ubuntu (use your HOST_IP — see SETUP.md "Host IP" section):
kubectl --context kind-lazy-diag-2 -n monitoring exec deployment/vmagent -- \
  wget -qO- http://172.19.0.1:8428/api/v1/query?query=up
```

**Ubuntu: `host.docker.internal` not resolving:**

Run `make host-ip-patch HOST_IP=<your-ip>` before deploying monitoring. See [SETUP.md Host IP section](SETUP.md#host-ip-macos-vs-ubuntu) for details.

**No data with cluster label:**
```bash
# Check vmagent config is applied
kubectl --context kind-lazy-diag-2 -n monitoring get configmap vmagent-config -o yaml | grep external_labels -A 2

# Check vlagent args
kubectl --context kind-lazy-diag-2 -n monitoring get daemonset vlagent -o yaml | grep remoteWrite.label
```

**Bot shows "unknown cluster, using default":**
- Verify `clusters` section is uncommented in `configs/config.yaml`
- Verify the context name matches: `kubectl config get-contexts`
- Cluster name in `-c` flag must match `name` in config (not the context name)
