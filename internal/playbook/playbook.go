package playbook

import (
	"context"
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

	// Step 1: Collect evidence
	progress("Đang thu thập dữ liệu K8s...")
	bundle := e.collector.Collect(ctx, req.Target, timeRange)
	bundle.CollectedAt = time.Now()

	if bundle.HasK8s() {
		progress("✓ Đã lấy K8s status/events")
	}
	if bundle.HasLogs() {
		progress("✓ Đã lấy logs")
	}
	if bundle.HasMetrics() {
		progress("✓ Đã lấy metrics")
	}

	// Step 2: Diagnose
	progress("Đang phân tích...")
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
		return []string{"Kiểm tra thủ công bằng các command bên dưới", "Xem logs và events để tìm thêm manh mối"}
	}

	switch intent {
	case domain.IntentCrashLoop:
		switch result.PrimaryHypothesis.ID {
		case "oom_resource":
			steps = []string{
				"Tăng memory limit cho container",
				"Kiểm tra memory leak trong application",
				"Xem xét optimize memory usage",
			}
		case "config_env_missing":
			steps = []string{
				"Kiểm tra ConfigMap/Secret đã được mount đúng chưa",
				"Verify environment variables trong deployment spec",
			}
		case "dependency_connectivity":
			steps = []string{
				"Kiểm tra service dependency có đang healthy không",
				"Verify network policy cho phép traffic",
				"Check DNS resolution trong cluster",
			}
		case "probe_issue":
			steps = []string{
				"Review liveness/readiness probe config",
				"Tăng initialDelaySeconds nếu app startup chậm",
				"Kiểm tra probe endpoint có respond đúng không",
			}
		case "bad_image":
			steps = []string{
				"Verify image tag tồn tại trong registry",
				"Kiểm tra imagePullSecrets",
				"Kiểm tra network connectivity tới registry",
			}
		}

	case domain.IntentPending:
		switch result.PrimaryHypothesis.ID {
		case "insufficient_resources":
			steps = []string{
				"Scale down workload khác hoặc scale up cluster",
				"Giảm resource requests nếu hợp lý",
				"Kiểm tra resource quota của namespace",
			}
		case "taint_mismatch":
			steps = []string{
				"Thêm toleration phù hợp vào pod spec",
				"Hoặc remove taint khỏi node nếu không cần thiết",
			}
		case "pvc_binding":
			steps = []string{
				"Kiểm tra PersistentVolume có available không",
				"Verify StorageClass tồn tại và provisioner hoạt động",
			}
		}

	case domain.IntentRolloutRegression:
		switch result.PrimaryHypothesis.ID {
		case "release_regression":
			steps = []string{
				"Rollback về revision trước bằng command bên dưới",
				"Kiểm tra diff giữa revision hiện tại và trước đó",
				"Review application changes trong release mới",
			}
		case "dependency_exposed":
			steps = []string{
				"Kiểm tra dependency services có healthy không",
				"Release mới có thể đã expose một bug dependency có sẵn",
			}
		case "resource_pressure":
			steps = []string{
				"Tăng resource limits cho release mới",
				"Kiểm tra xem release mới có tăng resource consumption không",
			}
		}
	}

	if len(steps) == 0 {
		steps = []string{"Kiểm tra thủ công bằng các command bên dưới"}
	}

	return steps
}
