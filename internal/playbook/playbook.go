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

// DeepInvestigate runs a HolmesGPT investigation for the given target.
func (e *Engine) DeepInvestigate(ctx context.Context, target *domain.Target) (string, error) {
	if e.holmes == nil {
		return "", fmt.Errorf("HolmesGPT not configured")
	}
	question := fmt.Sprintf("what is wrong with the %s %s in namespace %s?",
		target.Kind, target.ResourceName, target.Namespace)
	return e.holmes.Investigate(ctx, question)
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
	default:
		return []string{"Check logs and events using commands below"}
	}
}
