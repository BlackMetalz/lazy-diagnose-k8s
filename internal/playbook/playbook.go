package playbook

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lazy-diagnose-k8s/internal/composer"
	"github.com/lazy-diagnose-k8s/internal/diagnosis"
	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/lazy-diagnose-k8s/internal/provider"
)

// ProgressFunc is called to report progress to the user.
type ProgressFunc func(msg string)

// Engine runs diagnosis playbooks.
type Engine struct {
	collector *provider.Collector
	diagnosis *diagnosis.Engine
}

// New creates a new playbook engine.
func New(collector *provider.Collector, diagEngine *diagnosis.Engine) *Engine {
	return &Engine{
		collector: collector,
		diagnosis: diagEngine,
	}
}

// Run executes the diagnosis playbook for the given intent and target.
func (e *Engine) Run(ctx context.Context, req *domain.DiagnosisRequest, progress ProgressFunc) *domain.DiagnosisResult {
	start := time.Now()

	if progress == nil {
		progress = func(string) {}
	}

	// Default time range: last 1 hour
	timeRange := domain.TimeRange{
		From: time.Now().Add(-1 * time.Hour),
		To:   time.Now(),
	}

	// For rollout regression, look at a wider window
	if req.Intent == domain.IntentRolloutRegression {
		timeRange.From = time.Now().Add(-3 * time.Hour)
	}

	// Step 1: Collect evidence (all providers run concurrently)
	progress("Collecting data from K8s, logs, metrics...")
	bundle := e.collector.Collect(ctx, req.Target, timeRange)
	bundle.CollectedAt = time.Now()

	// Report what we got
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

	// Step 2: Diagnose
	progress("Analyzing...")
	result := e.diagnosis.DiagnoseWithContext(ctx, req.Intent, bundle)
	result.RequestID = req.ID

	// Step 3: Compose commands
	result.SuggestedCommands = composer.Compose(req.Intent, result)

	// Step 4: Recommended steps
	result.RecommendedSteps = e.recommendSteps(req.Intent, result)

	result.Duration = time.Since(start)
	return result
}

func (e *Engine) recommendSteps(intent domain.Intent, result *domain.DiagnosisResult) []string {
	var steps []string

	if result.PrimaryHypothesis == nil {
		return []string{"Check manually using commands below", "Inspect logs and events for clues"}
	}

	switch intent {
	case domain.IntentCrashLoop:
		switch result.PrimaryHypothesis.ID {
		case "oom_resource":
			steps = []string{
				"Increase memory limit for the container",
				"Check application for memory leaks",
				"Consider optimizing memory usage",
			}
		case "config_env_missing":
			steps = []string{
				"Verify ConfigMap/Secret is mounted correctly",
				"Check environment variables in deployment spec",
			}
		case "dependency_connectivity":
			steps = []string{
				"Check if dependency service is healthy",
				"Verify network policies allow traffic",
				"Check DNS resolution in the cluster",
			}
		case "probe_issue":
			steps = []string{
				"Review liveness/readiness probe config",
				"Increase initialDelaySeconds if app starts slowly",
				"Verify probe endpoint is responding",
			}
		case "bad_image":
			steps = []string{
				"Verify image tag exists in the registry",
				"Check imagePullSecrets configuration",
				"Verify network connectivity to the registry",
			}
		}

	case domain.IntentPending:
		switch result.PrimaryHypothesis.ID {
		case "insufficient_resources":
			steps = []string{
				"Scale down other workloads or scale up the cluster",
				"Reduce resource requests if reasonable",
				"Check namespace resource quotas",
			}
		case "taint_mismatch":
			steps = []string{
				"Add matching toleration to pod spec",
				"Or remove taint from node if not needed",
			}
		case "pvc_binding":
			steps = []string{
				"Check if PersistentVolume is available",
				"Verify StorageClass exists and provisioner is working",
			}
		}

	case domain.IntentRolloutRegression:
		switch result.PrimaryHypothesis.ID {
		case "release_regression":
			steps = []string{
				"Rollback to previous revision using command below",
				"Diff current vs previous revision",
				"Review application changes in the new release",
			}
		case "dependency_exposed":
			steps = []string{
				"Check if dependency services are healthy",
				"New release may have exposed a pre-existing dependency bug",
			}
		case "resource_pressure":
			steps = []string{
				"Increase resource limits for the new release",
				"Check if new release increased resource consumption",
			}
		}
	}

	if len(steps) == 0 {
		steps = []string{"Check manually using commands below"}
	}

	return steps
}
