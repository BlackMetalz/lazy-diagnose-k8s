package diagnosis

import (
	"testing"

	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/lazy-diagnose-k8s/internal/provider"
)

func TestAnalyze_OOMKilled(t *testing.T) {
	bundle := provider.NewOOMKilledFixture()

	result := AnalyzeEvidence(bundle)

	if result.PrimaryHypothesis == nil {
		t.Fatal("expected primary hypothesis, got nil")
	}

	if result.PrimaryHypothesis.ID != "oom_resource" {
		t.Errorf("expected oom_resource, got %s", result.PrimaryHypothesis.ID)
	}

	if result.Confidence == domain.ConfidenceLow {
		t.Errorf("expected Medium or High confidence, got Low")
	}

	t.Logf("Summary: %s", result.Summary)
	t.Logf("Score: %d/%d", result.PrimaryHypothesis.Score, result.PrimaryHypothesis.MaxScore)
	t.Logf("Signals: %v", result.PrimaryHypothesis.Signals)
	t.Logf("Evidence: %v", result.SupportingEvidence)
}

func TestAnalyze_Pending(t *testing.T) {
	bundle := provider.NewPendingFixture()

	result := AnalyzeEvidence(bundle)

	if result.PrimaryHypothesis == nil {
		t.Fatal("expected primary hypothesis, got nil")
	}

	// Should detect pending or scheduling issue
	id := result.PrimaryHypothesis.ID
	if id != "pending" && id != "insufficient_resources" && id != "taint_mismatch" && id != "affinity_issue" {
		t.Errorf("expected pending-related hypothesis, got %s", id)
	}

	t.Logf("Summary: %s", result.Summary)
	t.Logf("Hypothesis: %s", result.PrimaryHypothesis.Name)
}

func TestAnalyze_NoEvidence(t *testing.T) {
	bundle := &domain.EvidenceBundle{
		Target: &domain.Target{
			Name:         "test",
			Namespace:    "default",
			Kind:         "deployment",
			ResourceName: "test",
		},
	}

	result := AnalyzeEvidence(bundle)

	if result.Confidence != domain.ConfidenceLow {
		t.Errorf("expected Low confidence with no evidence, got %s", result.Confidence)
	}
	t.Logf("Summary: %s", result.Summary)
}

func TestAnalyze_ConfigMissing(t *testing.T) {
	bundle := &domain.EvidenceBundle{
		Target: &domain.Target{
			Name: "api", Namespace: "demo", Kind: "deployment", ResourceName: "api",
		},
		K8sFacts: &domain.K8sFacts{
			PodStatuses: []domain.PodStatus{
				{
					Name: "api-xxx", Phase: "Running", RestartCount: 5,
					ContainerStatuses: []domain.ContainerStatus{
						{
							Name: "api", State: "waiting", Reason: "CrashLoopBackOff",
							ExitCode: 0, LastTermination: "Error", LastExitCode: 1,
							RestartCount: 5,
							PreviousLogs: []string{
								"Starting server...",
								"FATAL: missing required env DATABASE_URL",
								"Error: config validation failed - DATABASE_URL is not set",
							},
						},
					},
				},
			},
		},
	}

	result := AnalyzeEvidence(bundle)

	if result.PrimaryHypothesis == nil {
		t.Fatal("expected primary hypothesis, got nil")
	}

	if result.PrimaryHypothesis.ID != "config_env_missing" {
		t.Errorf("expected config_env_missing, got %s (name: %s)", result.PrimaryHypothesis.ID, result.PrimaryHypothesis.Name)
	}

	t.Logf("Summary: %s", result.Summary)
	t.Logf("Signals: %v", result.PrimaryHypothesis.Signals)
}

func TestAnalyze_ImagePullBackOff(t *testing.T) {
	bundle := &domain.EvidenceBundle{
		Target: &domain.Target{
			Name: "api", Namespace: "demo", Kind: "deployment", ResourceName: "api",
		},
		K8sFacts: &domain.K8sFacts{
			PodStatuses: []domain.PodStatus{
				{
					Name: "api-xxx", Phase: "Pending", RestartCount: 0,
					ContainerStatuses: []domain.ContainerStatus{
						{
							Name: "api", Image: "nginx:v99-does-not-exist",
							State: "waiting", Reason: "ImagePullBackOff",
							Message: `Back-off pulling image "nginx:v99-does-not-exist": rpc error: not found`,
						},
					},
				},
			},
		},
	}

	result := AnalyzeEvidence(bundle)

	if result.PrimaryHypothesis == nil {
		t.Fatal("expected primary hypothesis, got nil")
	}

	if result.PrimaryHypothesis.ID != "bad_image_tag" {
		t.Errorf("expected bad_image_tag, got %s", result.PrimaryHypothesis.ID)
	}

	t.Logf("Summary: %s", result.Summary)
}

func TestAnalyze_DependencyFail(t *testing.T) {
	bundle := &domain.EvidenceBundle{
		Target: &domain.Target{
			Name: "api", Namespace: "demo", Kind: "deployment", ResourceName: "api",
		},
		K8sFacts: &domain.K8sFacts{
			PodStatuses: []domain.PodStatus{
				{
					Name: "api-xxx", Phase: "Running", RestartCount: 3,
					ContainerStatuses: []domain.ContainerStatus{
						{
							Name: "api", State: "waiting", Reason: "CrashLoopBackOff",
							LastTermination: "Error", LastExitCode: 1, RestartCount: 3,
							PreviousLogs: []string{
								"Connecting to postgres:5432...",
								"Error: dial tcp 10.96.0.1:5432: connect: connection refused",
								"FATAL: could not connect to database",
							},
						},
					},
				},
			},
		},
	}

	result := AnalyzeEvidence(bundle)

	if result.PrimaryHypothesis == nil {
		t.Fatal("expected primary hypothesis, got nil")
	}

	if result.PrimaryHypothesis.ID != "dependency_connectivity" {
		t.Errorf("expected dependency_connectivity, got %s", result.PrimaryHypothesis.ID)
	}

	t.Logf("Summary: %s", result.Summary)
}

func TestAnalyze_HTTPErrorSpike(t *testing.T) {
	bundle := provider.NewHTTPErrorSpikeFixture()

	result := AnalyzeEvidence(bundle)

	if result.PrimaryHypothesis == nil {
		t.Fatal("expected primary hypothesis, got nil")
	}

	if result.PrimaryHypothesis.ID != "http_error_spike" {
		t.Errorf("expected http_error_spike, got %s", result.PrimaryHypothesis.ID)
	}

	if result.PrimaryHypothesis.Score < 40 {
		t.Errorf("expected score >= 40, got %d", result.PrimaryHypothesis.Score)
	}

	t.Logf("Summary: %s", result.Summary)
	t.Logf("Score: %d/%d", result.PrimaryHypothesis.Score, result.PrimaryHypothesis.MaxScore)
	t.Logf("Signals: %v", result.PrimaryHypothesis.Signals)
	t.Logf("Evidence: %v", result.SupportingEvidence)
}
