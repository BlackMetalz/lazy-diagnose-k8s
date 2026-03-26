# Architecture

## Overview

lazy-diagnose-k8s is a Telegram bot that diagnoses Kubernetes issues using a **deterministic playbook-driven** approach. The system collects data from multiple sources, scores hypotheses using weighted rules, and returns structured results with actionable commands.

Key design principle:

> **Playbooks decide the investigation flow. LLM only summarizes/explains.**

The bot does not let an LLM decide what to query or how to investigate. Instead, predefined playbooks define exactly what data to collect and how to score it. This makes the system predictable, testable, and debuggable.

## System Diagram

```
┌──────────────────────────────────────────────────────────────────┐
│                         Telegram                                  │
│                     (user sends message)                          │
└──────────────────────┬───────────────────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│                    Telegram Adapter                                │
│                                                                    │
│  • Parse message → extract command + target                        │
│  • Send progress updates                                           │
│  • Format + send final result (HTML)                               │
│                                                                    │
│  Files: internal/adapter/telegram/adapter.go, bot.go               │
└──────────────────────┬───────────────────────────────────────────┘
                       │
              ┌────────┴────────┐
              ▼                 ▼
┌─────────────────┐  ┌──────────────────┐
│ Intent Classifier│  │  Target Resolver  │
│                  │  │                   │
│ Rule-based:      │  │ 1. Exact match    │
│ • crashloop      │  │ 2. service_map    │
│ • pending        │  │    lookup         │
│ • rollout        │  │ 3. Error + hint   │
│ • unknown        │  │                   │
│                  │  │ File:             │
│ File:            │  │ internal/resolver/│
│ internal/domain/ │  │ resolver.go       │
│ intent.go        │  │                   │
└────────┬─────────┘  └────────┬──────────┘
         │                     │
         └──────────┬──────────┘
                    ▼
┌──────────────────────────────────────────────────────────────────┐
│                      Playbook Engine                              │
│                                                                    │
│  Orchestrates the diagnosis run:                                   │
│  1. Determine time range based on intent                           │
│  2. Collect evidence (parallel)                                    │
│  3. Run diagnosis engine                                           │
│  4. Compose kubectl commands                                       │
│  5. Generate recommended steps                                     │
│                                                                    │
│  File: internal/playbook/playbook.go                               │
└──────────────────────┬───────────────────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│                    Provider Collector                              │
│                                                                    │
│  Runs all 3 providers concurrently with per-provider timeout.      │
│  Returns partial results if any provider fails (degraded mode).    │
│                                                                    │
│  File: internal/provider/provider.go                               │
│                                                                    │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐            │
│  │   K8s        │  │   Metrics    │  │   Logs       │            │
│  │   Provider   │  │   Provider   │  │   Provider   │            │
│  │              │  │              │  │              │            │
│  │ client-go    │  │ PromQL HTTP  │  │ LogsQL HTTP  │            │
│  │              │  │              │  │              │            │
│  │ Pod status   │  │ Restart rate │  │ Container    │            │
│  │ Events       │  │ CPU/Memory   │  │ logs         │            │
│  │ Conditions   │  │ Error rate   │  │ Error        │            │
│  │ Rollout      │  │              │  │ patterns     │            │
│  │ Resources    │  │              │  │              │            │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘            │
│         │                 │                 │                     │
│         ▼                 ▼                 ▼                     │
│       K8s API      VictoriaMetrics    VictoriaLogs                │
│                    (localhost:8428)   (localhost:9428)             │
└──────────────────────────────────────────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│                    Evidence Bundle                                 │
│                                                                    │
│  Normalized struct containing all collected data:                  │
│  • K8sFacts (pod status, events, rollout, resources)               │
│  • LogsFacts (lines, error count, top error patterns)              │
│  • MetricsFacts (CPU, memory, restart rate)                        │
│  • ProviderStatuses (which sources succeeded/failed)               │
│                                                                    │
│  File: internal/domain/types.go                                    │
└──────────────────────┬───────────────────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│                    Diagnosis Engine                                │
│                                                                    │
│  1. Load hypothesis rules for the intent                           │
│  2. Score each hypothesis by matching signals against evidence     │
│  3. Rank hypotheses by score                                       │
│  4. Calculate confidence (based on score + data completeness)      │
│  5. Collect supporting evidence                                    │
│  6. Generate summary (LLM or template fallback)                    │
│  7. Redact sensitive data                                          │
│                                                                    │
│  Files: internal/diagnosis/engine.go, summarizer.go, redact.go     │
└──────────────────────┬───────────────────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│                   Command Composer                                 │
│                                                                    │
│  Generates kubectl commands based on:                              │
│  • Intent (crashloop → logs/describe, rollout → rollback)          │
│  • Primary hypothesis (OOM → check resources, probe → check probe) │
│  • Target (namespace, resource kind, name)                         │
│                                                                    │
│  File: internal/composer/composer.go                               │
└──────────────────────┬───────────────────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│                   Diagnosis Result                                 │
│                                                                    │
│  {                                                                 │
│    summary:               "Container OOMKilled — hitting 128Mi"    │
│    confidence:            "High"                                   │
│    primary_hypothesis:    { name, score, signals }                 │
│    alternative_hypotheses: [...]                                   │
│    supporting_evidence:   ["Pod checkout: restarts=14", ...]       │
│    recommended_steps:     ["Increase memory limit", ...]           │
│    suggested_commands:    ["kubectl logs ...", ...]                 │
│    notes:                 ["Missing metrics data..."]              │
│  }                                                                 │
│                                                                    │
│  File: internal/domain/types.go (DiagnosisResult)                  │
└──────────────────────┬───────────────────────────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│                  Telegram Adapter (output)                         │
│                                                                    │
│  Formats DiagnosisResult into HTML message:                        │
│  🔴 prod/deployment/checkout                                       │
│  ─────────────────────                                             │
│  Container OOMKilled — hitting memory limit (128Mi)...             │
│                                                                    │
│  Root cause: OOM / Resource exhaustion (67%)                       │
│  Evidence: ...                                                     │
│  Next steps: ...                                                   │
│  Commands: <pre>kubectl logs ...</pre>                             │
└──────────────────────────────────────────────────────────────────┘
```

## Module Responsibilities

| Module | Path | Responsibility |
|---|---|---|
| **Telegram Adapter** | `internal/adapter/telegram/` | Parse messages, format output, manage bot lifecycle |
| **Intent Classifier** | `internal/domain/intent.go` | Classify user message into crashloop/pending/rollout/unknown |
| **Target Resolver** | `internal/resolver/` | Map user input to concrete K8s resource |
| **Playbook Engine** | `internal/playbook/` | Orchestrate the full diagnosis run |
| **Provider Collector** | `internal/provider/` | Concurrent data collection with timeout + degraded mode |
| **K8s Provider** | `internal/provider/kubernetes/` | Pod status, events, rollout, resources via client-go |
| **Metrics Provider** | `internal/provider/metrics/` | PromQL queries to VictoriaMetrics |
| **Logs Provider** | `internal/provider/logs/` | LogsQL queries to VictoriaLogs |
| **Diagnosis Engine** | `internal/diagnosis/` | Hypothesis scoring, confidence, summarization, redaction |
| **Command Composer** | `internal/composer/` | Generate kubectl commands based on diagnosis |
| **Config** | `internal/config/` | YAML config loading (service_map, playbook_rules, etc.) |
| **Domain** | `internal/domain/` | Shared types (Target, EvidenceBundle, DiagnosisResult) |

## Key Design Decisions

### Playbook-driven, not LLM-driven

The investigation flow is deterministic. Each playbook defines:
- What data to collect
- Which hypotheses to evaluate
- Scoring weights for each signal

This means the same input always produces the same diagnosis. The LLM is only used for summarization (optional).

### Concurrent providers with degraded mode

All three providers (K8s, metrics, logs) run in parallel with individual timeouts. If one fails, the others still contribute. Confidence is automatically degraded when data is missing.

### OpenAI-compatible LLM interface

The summarizer uses the OpenAI chat completions API format, which works with Ollama (local), Gemini, OpenRouter, OpenAI, and any compatible endpoint. No vendor lock-in.

### Direct API calls over MCP

For MVP, the bot uses direct API calls (client-go, HTTP) instead of MCP servers. This is simpler, more reliable, and easier to test. MCP can be added as an alternative provider layer later.
