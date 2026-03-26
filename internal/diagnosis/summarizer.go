package diagnosis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/lazy-diagnose-k8s/internal/domain"
)

// Summarizer uses Claude API to generate natural language diagnosis summaries.
type Summarizer struct {
	client anthropic.Client
	model  anthropic.Model
}

// NewSummarizer creates a new LLM summarizer.
// If apiKey is empty, it reads from ANTHROPIC_API_KEY env var.
func NewSummarizer(apiKey string, model anthropic.Model) *Summarizer {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	client := anthropic.NewClient(opts...) // returns value, not pointer

	if model == "" {
		model = anthropic.ModelClaudeHaiku4_5
	}

	return &Summarizer{
		client: client,
		model:  model,
	}
}

// evidenceSummary is a simplified view of evidence for the LLM prompt.
type evidenceSummary struct {
	Target     string              `json:"target"`
	Intent     string              `json:"intent"`
	Pods       []podBrief          `json:"pods,omitempty"`
	Events     []eventBrief        `json:"events,omitempty"`
	Rollout    *domain.RolloutStatus `json:"rollout,omitempty"`
	Resources  *domain.ResourceRequests `json:"resources,omitempty"`
	LogErrors  []domain.LogPattern `json:"log_errors,omitempty"`
	LogTotal   int                 `json:"log_total_lines"`
	MemoryUsageMi  *float64       `json:"memory_usage_mi,omitempty"`
	MemoryLimitMi  *float64       `json:"memory_limit_mi,omitempty"`
	CPUUsage       *float64       `json:"cpu_usage_cores,omitempty"`
	CPULimit       *float64       `json:"cpu_limit_cores,omitempty"`
	RestartRate    *float64       `json:"restart_rate_15m,omitempty"`
	Hypotheses []hypothesisBrief   `json:"hypotheses"`
	MissingSources []string        `json:"missing_sources,omitempty"`
}

type podBrief struct {
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restart_count"`
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

const systemPrompt = `Bạn là chuyên gia Kubernetes diagnosis. Bạn nhận evidence bundle từ một hệ thống monitoring tự động và cần giải thích kết quả cho người vận hành.

Quy tắc:
- Viết tiếng Việt, ngắn gọn, dễ hiểu
- Nói rõ: đây là QUAN SÁT (fact) hay SUY LUẬN (inference)
- Nếu dữ liệu thiếu (missing_sources), nói rõ và hạ mức tin cậy
- Không bịa thêm thông tin ngoài evidence
- Tập trung vào nguyên nhân gốc (root cause), không liệt kê triệu chứng dài dòng
- Giữ summary trong 3-5 câu`

// Summarize generates a natural language summary from evidence and scored hypotheses.
func (s *Summarizer) Summarize(ctx context.Context, intent domain.Intent, bundle *domain.EvidenceBundle, result *domain.DiagnosisResult) (string, error) {
	// Build simplified evidence for prompt
	evidence := s.buildEvidenceSummary(intent, bundle, result)

	evidenceJSON, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal evidence: %w", err)
	}

	userPrompt := fmt.Sprintf(`Đây là evidence bundle từ diagnosis tự động cho %s (intent: %s).

Evidence:
%s

Hãy viết summary ngắn gọn (3-5 câu) giải thích:
1. Chuyện gì đang xảy ra
2. Nguyên nhân chính (dựa trên hypothesis score cao nhất)
3. Mức độ tin cậy của kết luận
4. Nếu có dữ liệu thiếu, nói rõ`, evidence.Target, intent, string(evidenceJSON))

	resp, err := s.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     s.model,
		MaxTokens: 500,
		Temperature: anthropic.Float(0.3),
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt},
		},
		Messages: []anthropic.MessageParam{
			{
				Role: anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{
					{OfText: &anthropic.TextBlockParam{Text: userPrompt}},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("claude API: %w", err)
	}

	// Extract text response
	var text string
	for _, block := range resp.Content {
		if block.Type == "text" {
			text += block.AsText().Text
		}
	}

	return strings.TrimSpace(text), nil
}

func (s *Summarizer) buildEvidenceSummary(intent domain.Intent, bundle *domain.EvidenceBundle, result *domain.DiagnosisResult) evidenceSummary {
	es := evidenceSummary{
		Target: bundle.Target.FullName(),
		Intent: string(intent),
	}

	// Pods
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

		// Events (top 10 warning events)
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

	// Logs
	if bundle.LogsFacts != nil {
		es.LogErrors = bundle.LogsFacts.TopErrors
		es.LogTotal = bundle.LogsFacts.TotalLines
	}

	// Metrics
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

	// Hypotheses
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

	// Missing sources
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
