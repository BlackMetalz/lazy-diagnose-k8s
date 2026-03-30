package diagnosis

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
			model = "meta-llama/llama-3.3-70b-instruct:free"
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
	case "fpt":
		if baseURL == "" {
			baseURL = "https://mkp-api.fptcloud.com"
		}
		if model == "" {
			model = "Qwen2.5-Coder-32B-Instruct"
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

const systemPrompt = `You are a senior SRE writing an incident note. You receive structured K8s evidence and must tell the on-call engineer what broke and what to do.

Rules:
- 2-4 sentences max. Every word must earn its place.
- Lead with the root cause, not symptoms. Bad: "The pod is crash-looping." Good: "OOMKilled — container uses 510Mi against a 512Mi limit."
- Include specific numbers from the evidence (exit codes, memory values, restart counts, event messages). Never say "high" or "significant" without a number.
- End with one concrete next action. Not "consider increasing" — say "increase memory limit to 1Gi" or "check for memory leaks in the heap profile."
- Plain text only. No markdown, no bullet points, no headers.
- Never repeat the target path or hypothesis name verbatim from the input — the operator already sees those in the UI.
- If data is missing (missing_sources is non-empty), say which source is missing and how it affects diagnosis. Do not lower confidence — just state the gap.
- Do not invent facts. Only reference data present in the evidence.`

// Summarize generates a natural language summary using the configured LLM backend.
func (s *Summarizer) Summarize(ctx context.Context, intent domain.Intent, bundle *domain.EvidenceBundle, result *domain.DiagnosisResult) (string, error) {
	evidence := buildEvidenceSummary(intent, bundle, result)

	evidenceJSON, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal evidence: %w", err)
	}

	// Use short target name (resource name only, not full path)
	targetName := evidence.Target
	if bundle.Target != nil && bundle.Target.ResourceName != "" {
		targetName = bundle.Target.ResourceName
	}

	userPrompt := fmt.Sprintf(`Target: %s (intent: %s)

%s

Write the incident note.`, targetName, intent, string(evidenceJSON))

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
	start := time.Now()
	slog.Info("LLM request", "backend", s.backend, "model", s.model, "target", evidence.Target)

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
		slog.Warn("LLM rate limited, retrying", "attempt", attempt+1, "backend", s.backend)
		wait := time.Duration(2<<attempt) * time.Second // 2s, 4s, 8s
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
	if lastErr != nil {
		slog.Error("LLM request failed", "backend", s.backend, "model", s.model, "error", lastErr, "duration", time.Since(start))
		return "", fmt.Errorf("LLM API (%s): %w", s.model, lastErr)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}

	tokens := int(resp.Usage.TotalTokens)
	slog.Info("LLM response", "backend", s.backend, "model", s.model, "tokens", tokens, "duration", time.Since(start).Round(time.Millisecond))

	text := strings.TrimSpace(resp.Choices[0].Message.Content)
	return stripMarkdown(text), nil
}

// stripMarkdown removes common markdown formatting that LLMs tend to include.
// We render as plain text inside our own HTML template, so markdown syntax shows raw.
func stripMarkdown(s string) string {
	// **bold** or __bold__ → bold
	for _, marker := range []string{"**", "__"} {
		for strings.Contains(s, marker) {
			start := strings.Index(s, marker)
			end := strings.Index(s[start+len(marker):], marker)
			if end == -1 {
				break
			}
			inner := s[start+len(marker) : start+len(marker)+end]
			s = s[:start] + inner + s[start+len(marker)+end+len(marker):]
		}
	}
	// *italic* → italic (single asterisk, not inside words)
	// `code` → code
	s = strings.ReplaceAll(s, "`", "")
	// # headers → remove #
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(line, "# ")
	}
	return strings.Join(lines, "\n")
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
