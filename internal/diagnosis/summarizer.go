package diagnosis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// SummarizerConfig configures the LLM summarizer backend.
type SummarizerConfig struct {
	// Backend: "ollama", "gemini", "openrouter", "openai", or any OpenAI-compatible endpoint
	Backend string
	// BaseURL for the API. Auto-set based on Backend if empty.
	BaseURL string
	// APIKey for the provider. Not needed for Ollama.
	APIKey string
	// Model name. Auto-set based on Backend if empty.
	Model string
}

// Summarizer uses an LLM to generate natural language diagnosis summaries.
// Supports any OpenAI-compatible API (Ollama, Gemini, OpenRouter, OpenAI, etc.)
type Summarizer struct {
	client  openai.Client
	model   string
	backend string
}

// Backend returns the resolved backend name.
func (s *Summarizer) Backend() string { return s.backend }

// ModelName returns the resolved model name.
func (s *Summarizer) ModelName() string { return s.model }

// NewSummarizer creates a new LLM summarizer from config.
func NewSummarizer(cfg SummarizerConfig) *Summarizer {
	baseURL, model := resolveBackend(cfg)

	var opts []option.RequestOption
	opts = append(opts, option.WithBaseURL(baseURL))
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	} else {
		// Ollama doesn't need auth, but SDK requires non-empty key
		opts = append(opts, option.WithAPIKey("not-needed"))
	}

	client := openai.NewClient(opts...)

	return &Summarizer{
		client:  client,
		model:   model,
		backend: strings.ToLower(strings.TrimSpace(cfg.Backend)),
	}
}

func resolveBackend(cfg SummarizerConfig) (baseURL, model string) {
	baseURL = strings.TrimSpace(cfg.BaseURL)
	model = strings.TrimSpace(cfg.Model)

	switch strings.ToLower(strings.TrimSpace(cfg.Backend)) {
	case "ollama":
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		if model == "" {
			model = "gemma3:4b"
		}
	case "gemini":
		if baseURL == "" {
			baseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
		}
		if model == "" {
			model = "gemini-2.0-flash"
		}
	case "openrouter":
		if baseURL == "" {
			baseURL = "https://openrouter.ai/api/v1"
		}
		if model == "" {
			model = "google/gemini-2.0-flash-exp:free"
		}
	case "openai":
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		if model == "" {
			model = "gpt-4o-mini"
		}
	case "anthropic":
		if baseURL == "" {
			baseURL = "https://api.anthropic.com/v1"
		}
		if model == "" {
			model = "claude-haiku-4-5"
		}
	default:
		// Custom endpoint — user provides everything
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		if model == "" {
			model = "gemma3:4b"
		}
	}

	return baseURL, model
}

// evidenceSummary is a simplified view of evidence for the LLM prompt.
type evidenceSummary struct {
	Target         string                   `json:"target"`
	Intent         string                   `json:"intent"`
	Pods           []podBrief               `json:"pods,omitempty"`
	Events         []eventBrief             `json:"events,omitempty"`
	Rollout        *domain.RolloutStatus    `json:"rollout,omitempty"`
	Resources      *domain.ResourceRequests `json:"resources,omitempty"`
	LogErrors      []domain.LogPattern      `json:"log_errors,omitempty"`
	LogTotal       int                      `json:"log_total_lines"`
	MemoryUsageMi  *float64                 `json:"memory_usage_mi,omitempty"`
	MemoryLimitMi  *float64                 `json:"memory_limit_mi,omitempty"`
	CPUUsage       *float64                 `json:"cpu_usage_cores,omitempty"`
	CPULimit       *float64                 `json:"cpu_limit_cores,omitempty"`
	RestartRate    *float64                 `json:"restart_rate_15m,omitempty"`
	Hypotheses     []hypothesisBrief        `json:"hypotheses"`
	MissingSources []string                 `json:"missing_sources,omitempty"`
}

type podBrief struct {
	Name         string           `json:"name"`
	Phase        string           `json:"phase"`
	Ready        bool             `json:"ready"`
	RestartCount int              `json:"restart_count"`
	Containers   []containerBrief `json:"containers,omitempty"`
}

type containerBrief struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
	ExitCode        int    `json:"exit_code,omitempty"`
	LastTermination string `json:"last_termination,omitempty"`
}

type eventBrief struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int    `json:"count"`
}

type hypothesisBrief struct {
	Name    string   `json:"name"`
	Score   int      `json:"score"`
	Max     int      `json:"max_score"`
	Signals []string `json:"matched_signals"`
}

const systemPrompt = `You are a Kubernetes diagnosis expert. You receive an evidence bundle from an automated monitoring system and need to explain the results to an operator.

Rules:
- Write concise, clear English
- Distinguish between OBSERVATIONS (facts) and INFERENCES (conclusions)
- If data is missing (missing_sources), note it and lower confidence
- Do not invent information beyond the evidence
- Focus on root cause, not listing symptoms
- Keep summary to 3-5 sentences`

// Summarize generates a natural language summary using the configured LLM backend.
func (s *Summarizer) Summarize(ctx context.Context, intent domain.Intent, bundle *domain.EvidenceBundle, result *domain.DiagnosisResult) (string, error) {
	evidence := buildEvidenceSummary(intent, bundle, result)

	evidenceJSON, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal evidence: %w", err)
	}

	userPrompt := fmt.Sprintf(`Evidence bundle from automated diagnosis for %s (intent: %s).

Evidence:
%s

Write a concise summary (3-5 sentences) explaining:
1. What is happening
2. Root cause (based on highest-scored hypothesis)
3. Confidence level
4. If any data is missing, note it`, evidence.Target, intent, string(evidenceJSON))

	params := openai.ChatCompletionNewParams{
		Model: s.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
		MaxTokens:   openai.Int(500),
		Temperature: openai.Float(0.3),
	}

	// Retry with backoff for rate limiting (429)
	var resp *openai.ChatCompletion
	var lastErr error
	for attempt := range 3 {
		resp, lastErr = s.client.Chat.Completions.New(ctx, params)
		if lastErr == nil {
			break
		}
		if !strings.Contains(lastErr.Error(), "429") {
			break // non-retryable error
		}
		wait := time.Duration(2<<attempt) * time.Second // 2s, 4s, 8s
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("LLM API (%s): %w", s.model, lastErr)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}

	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

func buildEvidenceSummary(intent domain.Intent, bundle *domain.EvidenceBundle, result *domain.DiagnosisResult) evidenceSummary {
	es := evidenceSummary{
		Target: bundle.Target.FullName(),
		Intent: string(intent),
	}

	if bundle.K8sFacts != nil {
		for _, pod := range bundle.K8sFacts.PodStatuses {
			pb := podBrief{
				Name:         pod.Name,
				Phase:        pod.Phase,
				Ready:        pod.Ready,
				RestartCount: pod.RestartCount,
			}
			for _, cs := range pod.ContainerStatuses {
				pb.Containers = append(pb.Containers, containerBrief{
					Name:            cs.Name,
					State:           cs.State,
					Reason:          cs.Reason,
					ExitCode:        cs.ExitCode,
					LastTermination: cs.LastTermination,
				})
			}
			es.Pods = append(es.Pods, pb)
		}

		for _, ev := range bundle.K8sFacts.Events {
			if ev.Type == "Warning" && len(es.Events) < 10 {
				es.Events = append(es.Events, eventBrief{
					Type:    ev.Type,
					Reason:  ev.Reason,
					Message: ev.Message,
					Count:   ev.Count,
				})
			}
		}

		es.Rollout = bundle.K8sFacts.RolloutStatus
		es.Resources = bundle.K8sFacts.ResourceRequests
	}

	if bundle.LogsFacts != nil {
		es.LogErrors = bundle.LogsFacts.TopErrors
		es.LogTotal = bundle.LogsFacts.TotalLines
	}

	if bundle.MetricsFacts != nil {
		if bundle.MetricsFacts.MemoryUsage != nil {
			v := *bundle.MetricsFacts.MemoryUsage / (1024 * 1024)
			es.MemoryUsageMi = &v
		}
		if bundle.MetricsFacts.MemoryLimit != nil {
			v := *bundle.MetricsFacts.MemoryLimit / (1024 * 1024)
			es.MemoryLimitMi = &v
		}
		es.CPUUsage = bundle.MetricsFacts.CPUUsage
		es.CPULimit = bundle.MetricsFacts.CPULimit
		es.RestartRate = bundle.MetricsFacts.RestartRate
	}

	if result.PrimaryHypothesis != nil {
		es.Hypotheses = append(es.Hypotheses, hypothesisBrief{
			Name:    result.PrimaryHypothesis.Name,
			Score:   result.PrimaryHypothesis.Score,
			Max:     result.PrimaryHypothesis.MaxScore,
			Signals: result.PrimaryHypothesis.Signals,
		})
	}
	for _, h := range result.AlternativeHypotheses {
		es.Hypotheses = append(es.Hypotheses, hypothesisBrief{
			Name:    h.Name,
			Score:   h.Score,
			Max:     h.MaxScore,
			Signals: h.Signals,
		})
	}

	for _, ps := range bundle.ProviderStatuses {
		if !ps.Available {
			es.MissingSources = append(es.MissingSources, ps.Name)
		}
	}
	if !bundle.HasLogs() {
		es.MissingSources = append(es.MissingSources, "logs")
	}
	if !bundle.HasMetrics() {
		es.MissingSources = append(es.MissingSources, "metrics")
	}

	return es
}
