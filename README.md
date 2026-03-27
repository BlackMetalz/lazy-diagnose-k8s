# lazy-diagnose-k8s

Telegram ChatOps bot for Kubernetes diagnosis. Receives alerts from Alertmanager or manual commands, auto-diagnoses using deterministic playbooks, and returns structured results with suggested kubectl commands.

## How It Works

```
Alertmanager ──webhook──→ Bot :8080 ──→ Telegram: alert notification
                                         [🤖 AI] [📊 Static] [📜 Logs]
                                              ↓ user clicks
                                         Bot runs investigation → result

User ──────── /check ───→ Bot → static analysis → result + [🤖 AI] [📊 Static] [📜 Logs] [🔍 Scan]
User ──────── /scan ────→ Bot → find unhealthy pods → list
```

- **Alert mode**: Alertmanager fires → bot sends notification → user clicks action button → investigation runs
- **Manual mode**: User sends `/check` or `/scan` → bot runs analysis → returns result with action buttons
- **No auto-diagnosis on alerts** — saves LLM tokens, user decides what to investigate

## Quick Start

```bash
export TELEGRAM_BOT_TOKEN="your-token"
make run
```

## Commands

| Command | Description |
|---|---|
| `/scan [namespace]` | Find all unhealthy pods in a namespace |
| `/check <target> [-n ns]` | Diagnose a specific target |
| `/diag <target> <context>` | Diagnose with description |
| `/pod <pod-name>` | Check a specific pod |
| `/deploy <deployment>` | Check rollout status |
| `/help` | Show usage guide |

**Target** can be a pod/deployment name, or a path like `deployment/checkout` or `prod/deployment/checkout`. The bot auto-discovers the namespace via fuzzy pod search.

**Namespace**: auto-detected from fuzzy search. Override with `-n demo-prod` or set `DEFAULT_NAMESPACE` env var.

**Examples:**
```
/scan                          # scan default namespace
/scan prod                     # scan specific namespace
/check checkout                # diagnose checkout in default ns
/check checkout -n staging     # diagnose in staging
/diag payment just deployed, seeing 5xx
/deploy payment
```

## Playbooks

| Playbook | Detects |
|---|---|
| **CrashLoop** | OOMKilled, missing config/env, dependency timeout, probe failure, bad image |
| **Pending** | Insufficient resources, taint/toleration mismatch, affinity, PVC binding, quota |
| **Rollout regression** | Release regression, image pull failure, exposed dependency bugs, resource pressure |

## Alertmanager Integration

The bot runs an HTTP server (default `:8080`) that receives Alertmanager webhooks. When an alert fires, the bot:

1. Extracts K8s target from alert labels (pod, deployment, namespace)
2. Runs the same diagnosis pipeline as `/check`
3. Sends alert notification + diagnosis result to configured Telegram chats
4. Attaches inline action buttons (Rerun / Logs / Scan NS)

**Configure in `config.yaml`:**
```yaml
telegram:
  alert_chat_ids: [YOUR_CHAT_ID]  # where alerts go

webhook:
  enabled: true
  addr: ":8080"
  bearer_token: ""  # optional auth
```

**Alert rules included** (`deploy/monitoring/alert-rules.yaml`):
- `KubePodCrashLooping` — CrashLoopBackOff > 1 min
- `KubePodNotReady` — Pending/Unknown > 2 min
- `ContainerOOMKilled` — OOM termination
- `ContainerHighRestartCount` — restarts > 5
- `KubePodImagePullError` — ErrImagePull/ImagePullBackOff > 1 min
- `KubeDeploymentReplicasMismatch` — desired != ready > 3 min

**Alerting stack:**
```bash
kubectl apply -f deploy/monitoring/alert-rules.yaml
kubectl apply -f deploy/monitoring/alertmanager.yaml
kubectl apply -f deploy/monitoring/vmalert.yaml
```

## Inline Action Buttons

When an alert arrives, the bot sends a notification with 3 action buttons — **no auto-diagnosis**, you choose what to run:

| Button | What it does | Uses LLM? |
|---|---|---|
| **🤖 AI Investigation** | Collects evidence, sends to LLM for free-form analysis | Yes |
| **📊 Static Analysis** | Runs deterministic playbook scoring (rule-based) | No |
| **📜 Logs** | Queries VictoriaLogs, shows raw container logs (last 30 min) | No |

After any action, a second row of buttons appears for follow-up:

| Button | Action |
|---|---|
| 🤖 **AI** | Run AI Investigation |
| 📊 **Static** | Run Static Analysis |
| 📜 **Logs** | Show logs |
| 🔍 **Scan NS** | Scan entire namespace |

The `/check` command also shows these buttons after returning results.

## LLM Summarizer

Optional LLM-powered summaries. Without it, the bot uses template-based summaries (deterministic, fast). Configure in `config.yaml`:

```yaml
llm:
  enabled: true
  backend: ollama          # ollama | gemini | openrouter | openai | custom
  model: gemma3:4b         # auto-set per backend if empty
  # api_key: your-key      # not needed for ollama
```

| Backend | Cost | Default model |
|---|---|---|
| **Ollama** | Free (local) | `gemma3:4b` |
| **Gemini** | Free tier (15 RPM) | `gemini-2.0-flash` |
| **OpenRouter** | Free models available | `meta-llama/llama-3.3-70b-instruct:free` |
| **OpenAI** | Paid | `gpt-4o-mini` |

See [full LLM setup guide](#llm-backend-details) below.

## Configuration

All config in `configs/config.yaml`. Env vars override config file values.

| Setting | Config key | Env var | Default |
|---|---|---|---|
| Telegram token | `telegram.token` | `TELEGRAM_BOT_TOKEN` | (required) |
| Alert chat IDs | `telegram.alert_chat_ids` | | `[]` |
| Webhook enabled | `webhook.enabled` | | `true` |
| Webhook address | `webhook.addr` | | `:8080` |
| VictoriaMetrics | `providers.victoria_metrics_url` | `VICTORIA_METRICS_URL` | `http://localhost:8428` |
| VictoriaLogs | `providers.victoria_logs_url` | `VICTORIA_LOGS_URL` | `http://localhost:9428` |
| Default namespace | | `DEFAULT_NAMESPACE` | `prod` |
| LLM backend | `llm.backend` | `LLM_BACKEND` | (disabled) |
| LLM model | `llm.model` | `LLM_MODEL` | (per backend) |
| LLM API key | `llm.api_key` | `LLM_API_KEY` | |

## Deploy to K8s

```bash
make docker-load    # build image + load into kind

kubectl apply -f deploy/bot/namespace.yaml
kubectl apply -f deploy/bot/rbac.yaml
kubectl apply -f deploy/bot/configmap.yaml

kubectl create secret generic lazy-diagnose-secrets \
  --namespace=lazy-diagnose \
  --from-literal=TELEGRAM_BOT_TOKEN=your-token

kubectl apply -f deploy/bot/deployment.yaml
```

## Test Scenarios

9 pre-built K8s failure scenarios. See [deploy/test-workloads/SCENARIOS.md](deploy/test-workloads/SCENARIOS.md) for details.

```bash
make scenarios          # deploy all scenarios
make scenarios-status   # check pod status vs expected
make scenarios-clean    # remove all
```

| Scenario | Command | Expected |
|---|---|---|
| OOMKilled | `/check checkout` | CrashLoop — OOM / Resource exhaustion |
| Insufficient resources | `/check worker` | Pending — Insufficient cluster resources |
| Config missing | `/check api-config-missing` | CrashLoop — Config / Env missing |
| Probe failure | `/check api-probe-fail` | CrashLoop — Probe misconfiguration |
| Bad image | `/check api-bad-image` | CrashLoop — Bad image / Startup failure |
| Dependency fail | `/check api-dependency-fail` | CrashLoop — Dependency / Connectivity |
| Node selector | `/check ml-worker-taint` | Pending — Affinity / NodeSelector issue |
| PVC not bound | `/check db-pvc-pending` | Pending — PVC binding issue |
| Rollout regression | `/deploy payment` | Rollout — Release caused regression |

## Local Development

See [SETUP.md](SETUP.md) for full local setup (kind + VictoriaMetrics + VictoriaLogs + Alertmanager).

## Documentation

- [Architecture](docs/architecture.md) — system design, modules, design decisions
- [Diagnosis Flow](docs/flow.md) — step-by-step walkthrough of a diagnosis run
- [Test Scenarios](deploy/test-workloads/SCENARIOS.md) — pre-built K8s failure scenarios
- [Local Setup](SETUP.md) — kind cluster + monitoring stack setup guide

## Project Structure

```
cmd/bot/main.go                     Entry point (Telegram + webhook server)
internal/
  adapter/telegram/                 Telegram bot, message formatting, callbacks
  webhook/                          HTTP server for Alertmanager webhooks
  config/                           Config structs + YAML loader
  composer/                         kubectl command generator
  diagnosis/                        Scoring engine + LLM summarizer + redaction
  domain/                           Domain types + intent classifier
  playbook/                         Playbook orchestration
  provider/                         Data collection (K8s, metrics, logs)
  resolver/                         Target resolver (name -> K8s resource)
configs/                            Sample configs (config.yaml, service_map, playbook_rules)
deploy/
  bot/                              K8s deployment manifests for the bot
  monitoring/                       kube-state-metrics, vmagent, vlagent, alertmanager, vmalert
  test-workloads/                   Test scenarios (OOM, Pending, Rollout, etc.)
docs/                               Architecture and flow documentation
```

## LLM Backend Details

### Ollama (free, local)

```bash
brew install ollama && ollama serve & && ollama pull gemma3:4b
```
```yaml
llm:
  enabled: true
  backend: ollama
  model: gemma3:4b     # or llama3.1:8b, gemma3:12b
```

### Gemini (free tier)

Get API key at https://aistudio.google.com/apikey
```yaml
llm:
  enabled: true
  backend: gemini
  api_key: your-key
  model: gemini-2.0-flash
```

### OpenRouter (free models)

Create account at https://openrouter.ai (no credit card needed)
```yaml
llm:
  enabled: true
  backend: openrouter
  api_key: your-key
  model: meta-llama/llama-3.3-70b-instruct:free
```

Other free models: `google/gemma-3-27b-it:free`, `nvidia/nemotron-3-super-120b-a12b:free`, `openrouter/free` (auto-select). Full list: https://openrouter.ai/collections/free-models

### Custom endpoint

Any OpenAI-compatible API:
```yaml
llm:
  enabled: true
  backend: custom
  base_url: http://your-server:8080/v1
  model: your-model
  api_key: your-key
```
