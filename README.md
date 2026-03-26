# lazy-diagnose-k8s

Telegram ChatOps bot for Kubernetes diagnosis. Automatically collects data from K8s, VictoriaMetrics, and VictoriaLogs, then returns a structured diagnosis with suggested kubectl commands.

## Quick Start

```bash
export TELEGRAM_BOT_TOKEN="your-token"
make run
```

## Commands

| Command | Description |
|---|---|
| `/check <target>` | General health check |
| `/diag <target> <description>` | Diagnosis with context |
| `/pod <pod-name>` | Check a specific pod |
| `/deploy <deployment>` | Check rollout status |
| `/help` | Show usage guide |

Target can be a service name from `service_map.yaml`, an exact deployment/pod name, or a path like `deployment/checkout` or `prod/deployment/checkout`.

## Playbooks

| Playbook | Detects |
|---|---|
| **CrashLoop** | OOMKilled, missing config/env, dependency timeout, probe failure, bad image |
| **Pending** | Insufficient resources, taint/toleration mismatch, affinity, PVC binding, quota |
| **Rollout regression** | Release regression, image pull failure, exposed dependency bugs, resource pressure |

## LLM Summarizer

The bot can optionally use an LLM to generate natural language summaries instead of static templates. **Completely optional** — without it, the bot falls back to template-based summaries.

Configure via environment variables:

| Env var | Description |
|---|---|
| `LLM_BACKEND` | Backend: `ollama`, `gemini`, `openrouter`, `openai`, or custom |
| `LLM_BASE_URL` | API endpoint URL (auto-set per backend if empty) |
| `LLM_API_KEY` | API key (not needed for Ollama) |
| `LLM_MODEL` | Model name (auto-set per backend if empty) |

### Ollama (free, local)

Runs entirely on your machine. No cost, no data leaves your network.

```bash
brew install ollama
ollama serve &
ollama pull gemma3:4b

export LLM_BACKEND=ollama
# Defaults: http://localhost:11434/v1, model gemma3:4b
# Override: export LLM_MODEL=llama3.1:8b
```

Requires ~3GB RAM for 4B model, ~5GB for 8B. M1 Pro 16GB handles it easily.

### Gemini (generous free tier)

Google provides 15 RPM + 1M tokens/day free for Gemini Flash.

```bash
# Get API key at https://aistudio.google.com/apikey
export LLM_BACKEND=gemini
export LLM_API_KEY="your-gemini-api-key"
# Default model: gemini-2.0-flash
```

### OpenRouter (free models available)

Aggregator with multiple models, some free. Rate limit: 20 req/min, 200 req/day.

```bash
# Create account at https://openrouter.ai (no credit card needed)
export LLM_BACKEND=openrouter
export LLM_API_KEY="your-openrouter-key"
# Default model: meta-llama/llama-3.3-70b-instruct:free
```

Other free models you can use via `LLM_MODEL`:
- `google/gemma-3-27b-it:free` — lightweight, fast
- `nvidia/nemotron-3-super-120b-a12b:free` — large context (262K tokens)
- `mistralai/mistral-small-3.1-24b-instruct:free`
- `qwen/qwen3-next-80b-a3b-instruct:free`
- `openrouter/free` — auto-selects best free model

Full list: https://openrouter.ai/collections/free-models

### OpenAI

```bash
export LLM_BACKEND=openai
export LLM_API_KEY="your-openai-key"
# Default model: gpt-4o-mini
```

### Custom Endpoint

Any server exposing an OpenAI-compatible API (`/v1/chat/completions`):

```bash
export LLM_BACKEND=custom
export LLM_BASE_URL="http://your-server:8080/v1"
export LLM_MODEL="your-model"
export LLM_API_KEY="your-key"  # if required
```

## Environment Variables

| Env var | Required | Default | Description |
|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | Yes | | Token from @BotFather |
| `VICTORIA_METRICS_URL` | | `http://localhost:8428` | VictoriaMetrics endpoint |
| `VICTORIA_LOGS_URL` | | `http://localhost:9428` | VictoriaLogs endpoint |
| `CONFIG_PATH` | | `configs/config.yaml` | Config file path |
| `SERVICE_MAP_PATH` | | `configs/service_map.yaml` | Service map path |
| `PLAYBOOK_RULES_PATH` | | `configs/playbook_rules.yaml` | Playbook rules path |
| `LLM_BACKEND` | | | LLM backend (see above) |
| `LLM_BASE_URL` | | | LLM API URL |
| `LLM_API_KEY` | | | LLM API key |
| `LLM_MODEL` | | | LLM model name |

## Deploy to K8s

```bash
# Build + load image into kind
make docker-load

# Apply manifests
kubectl apply -f deploy/bot/namespace.yaml
kubectl apply -f deploy/bot/rbac.yaml
kubectl apply -f deploy/bot/configmap.yaml

# Create secret
kubectl create secret generic lazy-diagnose-secrets \
  --namespace=lazy-diagnose \
  --from-literal=TELEGRAM_BOT_TOKEN=your-token

# Deploy
kubectl apply -f deploy/bot/deployment.yaml
```

## Test Scenarios

9 pre-built K8s failure scenarios covering CrashLoop, Pending, and Rollout regression. See [deploy/test-workloads/SCENARIOS.md](deploy/test-workloads/SCENARIOS.md) for full details on each scenario.

```bash
make scenarios          # Deploy all scenarios to namespace prod
make scenarios-status   # Check pod status vs expected state
make scenarios-clean    # Remove all scenarios
```

Or deploy individually:
```bash
kubectl apply -f deploy/test-workloads/scenario-config-missing.yaml
# then: /check api-config-missing
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
| Rollout regression | `/deploy payment` (after applying rollout-regression.yaml) | Rollout — Release caused regression |

## Local Development

See [SETUP.md](SETUP.md) for setting up a kind cluster with VictoriaMetrics + VictoriaLogs locally.

## Project Structure

```
cmd/bot/main.go                     Entry point
internal/
  adapter/telegram/                 Telegram bot + message formatting
  config/                           Config structs + YAML loader
  composer/                         kubectl command generator
  diagnosis/                        Scoring engine + LLM summarizer + redaction
  domain/                           Domain types + intent classifier
  playbook/                         Playbook orchestration
  provider/                         Data collection (K8s, metrics, logs)
  resolver/                         Target resolver (name -> K8s resource)
configs/                            Sample configs
deploy/
  bot/                              K8s deployment manifests
  monitoring/                       kube-state-metrics, vmagent, vlagent
  test-workloads/                   Test scenarios (OOM, Pending, Rollout, etc.)
```
