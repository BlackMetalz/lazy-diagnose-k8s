package playbook

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lazy-diagnose-k8s/internal/composer"
	"github.com/lazy-diagnose-k8s/internal/diagnosis"
	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/lazy-diagnose-k8s/internal/holmes"
	"github.com/lazy-diagnose-k8s/internal/provider"
)

// ProgressFunc is called to report progress to the user.
type ProgressFunc func(msg string)

// Engine runs diagnosis using evidence-first analysis.
type Engine struct {
	collector  *provider.Collector
	summarizer *diagnosis.Summarizer
	holmes     *holmes.Client
	logger     *slog.Logger
}

// New creates a new playbook engine.
func New(collector *provider.Collector, summarizer *diagnosis.Summarizer, logger *slog.Logger) *Engine {
	return &Engine{
		collector:  collector,
		summarizer: summarizer,
		logger:     logger,
	}
}

// SetHolmes sets the HolmesGPT client for deep investigation.
func (e *Engine) SetHolmes(h *holmes.Client) { e.holmes = h }

// HasHolmes returns true if HolmesGPT is configured.
func (e *Engine) HasHolmes() bool { return e.holmes != nil }

// DeepResult holds the parsed result of a Holmes investigation.
type DeepResult struct {
	Status    string // one-line health status
	Problem   string // what's wrong
	RootCause string // why
	Fix       string // actionable steps
	Raw       string // original output (fallback)
}

// Parsed returns true if structured sections were extracted.
func (d *DeepResult) Parsed() bool {
	return d.Problem != "" && d.RootCause != ""
}

// DeepInvestigate runs a HolmesGPT investigation for the given target.
func (e *Engine) DeepInvestigate(ctx context.Context, target *domain.Target) (*DeepResult, error) {
	if e.holmes == nil {
		return nil, fmt.Errorf("HolmesGPT not configured")
	}
	question := fmt.Sprintf(
		"Investigate ONLY the %s %s in namespace %s. "+
			"Focus on this specific resource: check its pods, logs, events, and status. "+
			"Do NOT investigate other resources in the namespace. "+
			"When done, output your final answer using EXACTLY this format:\n\n"+
			"STATUS: <one line: healthy/unhealthy + replica count + restart count>\n"+
			"PROBLEM: <one line describing the actual problem>\n"+
			"ROOT_CAUSE: <1-3 lines explaining why>\n"+
			"FIX: <numbered list of actionable fixes, max 4 items>\n\n"+
			"Use only plain text. No markdown. Be concise.",
		target.Kind, target.ResourceName, target.Namespace)

	raw, err := e.holmes.Investigate(ctx, question)
	if err != nil {
		return nil, err
	}
	return parseDeepResult(raw), nil
}

// parseDeepResult extracts STATUS/PROBLEM/ROOT_CAUSE/FIX sections from Holmes output.
// Uses position-based search so markers can appear anywhere (not just line starts).
func parseDeepResult(raw string) *DeepResult {
	// Ordered markers — each section runs until the next marker
	type marker struct {
		key    string
		prefix string
	}
	markers := []marker{
		{"STATUS", "STATUS:"},
		{"PROBLEM", "PROBLEM:"},
		{"ROOT_CAUSE", "ROOT_CAUSE:"},
		{"FIX", "FIX:"},
	}

	// Find position of each marker in the text
	type found struct {
		key string
		pos int // position of content (after "KEY:")
	}
	var hits []found
	for _, m := range markers {
		idx := strings.Index(raw, m.prefix)
		if idx >= 0 {
			hits = append(hits, found{key: m.key, pos: idx + len(m.prefix)})
		}
	}

	sections := make(map[string]string, len(markers))
	for i, h := range hits {
		var end int
		if i+1 < len(hits) {
			// Content runs until next marker's prefix start
			end = hits[i+1].pos - len(markers[i+1].prefix)
		} else {
			end = len(raw)
		}
		// Find the start of the next marker prefix, not its content pos
		content := strings.TrimSpace(raw[h.pos:end])
		sections[h.key] = content
	}

	rootCause := sections["ROOT_CAUSE"]
	fix := sections["FIX"]

	// If FIX is empty but ROOT_CAUSE contains a numbered list, split it out
	if fix == "" && rootCause != "" {
		rootCause, fix = splitNumberedList(rootCause)
	}

	return &DeepResult{
		Status:    sections["STATUS"],
		Problem:   sections["PROBLEM"],
		RootCause: rootCause,
		Fix:       fix,
		Raw:       raw,
	}
}

// splitNumberedList extracts a trailing numbered list from text.
// Returns (prose, numberedList). If no numbered list found, returns (text, "").
func splitNumberedList(text string) (string, string) {
	lines := strings.Split(text, "\n")

	// Scan backwards to find where the numbered list starts
	listStart := -1
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if len(trimmed) > 1 && trimmed[0] >= '1' && trimmed[0] <= '9' && (trimmed[1] == '.' || trimmed[1] == ' ' || trimmed[1] == ')') {
			listStart = i
		} else if listStart >= 0 && trimmed != "" {
			// Hit a non-numbered, non-empty line — stop
			break
		}
	}

	if listStart < 0 {
		return text, ""
	}

	prose := strings.TrimSpace(strings.Join(lines[:listStart], "\n"))
	list := strings.TrimSpace(strings.Join(lines[listStart:], "\n"))
	return prose, list
}

// GetCollector returns the provider collector for direct use.
func (e *Engine) GetCollector() *provider.Collector { return e.collector }

// HasSummarizer returns true if LLM summarizer is configured.
func (e *Engine) HasSummarizer() bool { return e.summarizer != nil }

// HasLogsProvider returns true if logs provider is available.
func (e *Engine) HasLogsProvider() bool {
	return e.collector != nil && e.collector.Logs != nil
}

// CollectLogs queries the logs provider directly.
func (e *Engine) CollectLogs(ctx context.Context, target *domain.Target, timeRange domain.TimeRange) (*domain.LogsFacts, error) {
	if e.collector == nil || e.collector.Logs == nil {
		return nil, fmt.Errorf("logs provider not available")
	}
	return e.collector.Logs.CollectFacts(ctx, target, timeRange)
}

// SummarizeWithLLM sends evidence to LLM for free-form analysis (AI Investigation).
func (e *Engine) SummarizeWithLLM(ctx context.Context, intent domain.Intent, bundle *domain.EvidenceBundle) (string, error) {
	if e.summarizer == nil {
		return "", fmt.Errorf("LLM summarizer not configured")
	}
	// Run analyzer first to get hypothesis for context
	result := diagnosis.AnalyzeEvidence(bundle)
	return e.summarizer.Summarize(ctx, intent, bundle, result)
}

// RunWithoutLLM runs evidence-first diagnosis (Static Analysis). No LLM call.
func (e *Engine) RunWithoutLLM(ctx context.Context, req *domain.DiagnosisRequest, progress ProgressFunc) *domain.DiagnosisResult {
	start := time.Now()
	if progress == nil {
		progress = func(string) {}
	}

	bundle := e.collectBundle(ctx, req, progress)

	// Debug: log what we collected
	podCount := 0
	eventCount := 0
	if bundle.K8sFacts != nil {
		podCount = len(bundle.K8sFacts.PodStatuses)
		eventCount = len(bundle.K8sFacts.Events)
	}
	e.logger.Info("evidence collected",
		"target", req.Target.FullName(),
		"pods", podCount,
		"events", eventCount,
		"k8s", bundle.HasK8s(),
		"logs", bundle.HasLogs(),
		"metrics", bundle.HasMetrics(),
	)

	progress("Analyzing evidence...")
	result := diagnosis.AnalyzeEvidence(bundle)
	result.RequestID = req.ID
	result.SuggestedCommands = composer.Compose(req.Intent, result)
	result.RecommendedSteps = recommendSteps(req.Intent, result)
	result.Duration = time.Since(start)
	return result
}

// Run executes diagnosis (same as RunWithoutLLM — kept for compatibility).
func (e *Engine) Run(ctx context.Context, req *domain.DiagnosisRequest, progress ProgressFunc) *domain.DiagnosisResult {
	return e.RunWithoutLLM(ctx, req, progress)
}

func (e *Engine) collectBundle(ctx context.Context, req *domain.DiagnosisRequest, progress ProgressFunc) *domain.EvidenceBundle {
	timeRange := domain.TimeRange{
		From: time.Now().Add(-1 * time.Hour),
		To:   time.Now(),
	}
	if req.Intent == domain.IntentRolloutRegression {
		timeRange.From = time.Now().Add(-3 * time.Hour)
	}

	progress("Collecting data from K8s, logs, metrics...")
	bundle := e.collector.Collect(ctx, req.Target, timeRange)
	bundle.CollectedAt = time.Now()

	var statusParts []string
	for _, ps := range bundle.ProviderStatuses {
		if ps.Available {
			statusParts = append(statusParts, fmt.Sprintf("✓ %s (%s)", ps.Name, ps.Duration.Round(time.Millisecond)))
		} else {
			statusParts = append(statusParts, fmt.Sprintf("✗ %s: %s", ps.Name, ps.Error))
		}
	}
	if len(statusParts) > 0 {
		progress(strings.Join(statusParts, "\n"))
	}

	return bundle
}

func recommendSteps(intent domain.Intent, result *domain.DiagnosisResult) []string {
	if result.PrimaryHypothesis == nil {
		return []string{"Check manually using commands below", "Inspect logs and events for clues"}
	}

	switch result.PrimaryHypothesis.ID {
	case "oom_resource":
		return []string{
			"Increase memory limit for the container",
			"Check application for memory leaks",
			"Consider optimizing memory usage",
		}
	case "config_env_missing":
		return []string{
			"Verify ConfigMap/Secret is mounted correctly",
			"Check environment variables in deployment spec",
		}
	case "dependency_connectivity":
		return []string{
			"Check if dependency service is healthy",
			"Verify network policies allow traffic",
			"Check DNS resolution in the cluster",
		}
	case "probe_issue":
		return []string{
			"Review liveness probe config",
			"Increase initialDelaySeconds if app starts slowly",
			"Verify probe endpoint is responding",
		}
	case "readiness_probe_fail":
		return []string{
			"Check why readiness endpoint returns non-200",
			"Verify app dependencies are reachable (cache, DB, etc.)",
			"Review readinessProbe config and endpoint",
		}
	case "bad_image", "bad_image_tag":
		return []string{
			"Verify image tag exists in the registry",
			"Check imagePullSecrets configuration",
		}
	case "bad_image_auth":
		return []string{
			"Check imagePullSecrets and registry credentials",
			"Verify service account has pull permissions",
		}
	case "bad_image_network":
		return []string{
			"Check registry hostname and DNS",
			"Verify network connectivity to registry",
		}
	case "insufficient_resources":
		return []string{
			"Scale down other workloads or scale up the cluster",
			"Reduce resource requests if reasonable",
		}
	case "taint_mismatch":
		return []string{
			"Add matching toleration to pod spec",
			"Or remove taint from node if not needed",
		}
	case "affinity_issue":
		return []string{
			"Check nodeSelector/affinity matches available nodes",
			"Remove or adjust nodeSelector constraint",
		}
	case "pvc_binding":
		return []string{
			"Check if PersistentVolume is available",
			"Verify StorageClass exists and provisioner is working",
		}
	case "init_container_fail":
		return []string{
			"Check init container logs for errors",
			"Verify init container image and command",
		}
	case "app_crash":
		return []string{
			"Check container logs for stack trace or panic",
			"Review recent code changes that may cause runtime errors",
		}
	case "permission_error":
		return []string{
			"Check ServiceAccount permissions (RBAC)",
			"Verify file/directory permissions in container",
		}
	case "http_error_spike":
		return []string{
			"Check application logs for error stack traces",
			"Review recent deployments or config changes",
			"Check downstream service health",
			"Consider rollback if caused by recent deploy",
		}
	default:
		return []string{"Check logs and events using commands below"}
	}
}
