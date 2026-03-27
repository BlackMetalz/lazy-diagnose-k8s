# Test Scenarios

Pre-built K8s failure scenarios for testing the diagnosis bot. Distributed across 3 namespaces.

## Namespace Layout

| Namespace | Purpose | Scenarios |
|---|---|---|
| `demo-prod` | Core app simulation | checkout (OOM), worker (Pending), payment (healthy) |
| `demo-staging` | Config / dependency / runtime | api-config-missing, api-dependency-fail, api-runtime-crash, api-init-fail |
| `demo-infra` | Image / probe / scheduling | api-bad-image, api-probe-fail, api-not-ready, ml-worker-taint, db-pvc-pending |

## Quick Reference

| Scenario | Namespace | Expected State | Hypothesis | Bot command |
|---|---|---|---|---|
| OOMKilled | demo-prod | CrashLoopBackOff | oom_resource | `/check checkout` |
| Insufficient resources | demo-prod | Pending | insufficient_resources | `/check worker` |
| Healthy baseline | demo-prod | Running | - | `/check payment` |
| Config/env missing | demo-staging | CrashLoopBackOff | config_env_missing | `/check api-config-missing` |
| Dependency unreachable | demo-staging | CrashLoopBackOff | dependency_connectivity | `/check api-dependency-fail` |
| Runtime crash | demo-staging | CrashLoopBackOff | app_crash | `/check api-runtime-crash` |
| Init container fail | demo-staging | Init:CrashLoopBackOff | init_container_fail | `/check api-init-fail` |
| Bad image tag | demo-infra | ErrImagePull | bad_image_tag | `/check api-bad-image` |
| Liveness probe fail | demo-infra | Running + restarts | probe_issue | `/check api-probe-fail` |
| Readiness probe fail | demo-infra | Running (0/1 Ready) | readiness_probe_fail | `/check api-not-ready` |
| Node selector mismatch | demo-infra | Pending | affinity_issue | `/check ml-worker-taint` |
| PVC not bound | demo-infra | Pending | pvc_binding | `/check db-pvc-pending` |
| Rollout regression | demo-prod | ImagePullBackOff | release_regression | `/deploy payment` |

## Usage

```bash
# Deploy all scenarios (creates namespaces automatically)
make scenarios

# Check status
make scenarios-status

# Or deploy individually
kubectl apply -f deploy/test-workloads/scenario-config-missing.yaml
# then: /check api-config-missing

# Clean up
kubectl delete -f deploy/test-workloads/scenario-config-missing.yaml
```

## Deploy All Scenarios

```bash
kubectl apply -f deploy/test-workloads/
```

## Clean Up All

```bash
kubectl delete -f deploy/test-workloads/
```

## Scenario Details

### OOMKilled (workloads.yaml — checkout)

`stress` tool allocates 512MB but container limit is 128Mi. Kernel OOM-kills the process immediately.

**What the bot should detect:**
- `terminationReason: OOMKilled`
- `exitCode: 137`
- High restart count
- Memory usage near limit (from metrics)

### Config/Env Missing (scenario-config-missing.yaml)

Container checks for `DATABASE_URL` env var on startup. Not set → logs error → exits.

**What the bot should detect:**
- Fast exit (container runs < 3s)
- Logs: `"missing required env DATABASE_URL"`
- `exitCode: 1`

### Liveness Probe Fail (scenario-probe-fail.yaml)

Container runs `sleep` but liveness probe checks HTTP port 8080 which has nothing listening. Kubelet kills after 3 failed probes.

**What the bot should detect:**
- Events: `"Liveness probe failed"`
- Container state: running but restart count increasing
- No OOM, no crash — probe-induced restarts

### Bad Image (scenario-bad-image.yaml)

Image tag `nginx:v99.99.99-does-not-exist` doesn't exist in registry.

**What the bot should detect:**
- Events: `"ErrImagePull"`, `"ImagePullBackOff"`
- Container never started

### Dependency Unreachable (scenario-dependency-fail.yaml)

Container tries to connect to `postgres-primary:5432` which doesn't exist in the cluster.

**What the bot should detect:**
- Logs: `"connection refused"`, `"dial tcp"`
- `exitCode: 1`
- CrashLoopBackOff

### Node Selector Mismatch (scenario-taint-mismatch.yaml)

Pod requires `nodeSelector: gpu=true` but no nodes in kind have this label.

**What the bot should detect:**
- Events: `"didn't match Pod's node affinity/selector"`
- Pod stuck in Pending

### PVC Not Bound (scenario-pvc-pending.yaml)

Pod references a PVC with `storageClassName: non-existent-storage-class`. No provisioner exists.

**What the bot should detect:**
- Events: `"unbound PersistentVolumeClaims"`
- Pod stuck in Pending

### Runtime Crash (scenario-runtime-crash.yaml)

App starts normally, processes requests for ~30s, then crashes with a runtime panic (simulating memory leak or unhandled error).

**What the bot should detect:**
- Container ran for 30s+ before crashing (not instant exit)
- Logs show normal startup, then `[FATAL] panic: runtime error`
- `exitCode: 1`
- CrashLoopBackOff with increasing restarts

### Init Container Fail (scenario-init-fail.yaml)

Init container runs a database migration that fails. Main container never starts.

**What the bot should detect:**
- Init container in CrashLoopBackOff
- Main container never started
- Init container logs: `"migration failed"`, `"dirty database state"`
- `exitCode: 1`

### Readiness Probe Fail (scenario-readiness-fail.yaml)

App starts and stays running, but readiness probe fails. Pod is Running with 0 restarts but NOT Ready — Service won't route traffic. A "silent failure" where everything looks OK but users can't reach the app.

**What the bot should detect:**
- Pod Running but `Ready=False`
- No restarts, no crash
- Events: `"Readiness probe failed"`
- Logs: `"Cache warmup failed — /ready will return 503"`

### Rollout Regression (rollout-regression.yaml)

Updates payment deployment to a non-existent image tag. Old pods keep running, new pod fails with ImagePullBackOff.

**What the bot should detect:**
- `updatedReplicas < desiredReplicas`
- `unavailableReplicas > 0`
- Events: `"ImagePullBackOff"` on new pod
