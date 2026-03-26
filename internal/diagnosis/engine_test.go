package diagnosis

import (
	"testing"

	"github.com/lazy-diagnose-k8s/internal/config"
	"github.com/lazy-diagnose-k8s/internal/domain"
	"github.com/lazy-diagnose-k8s/internal/provider"
)

func loadTestRules() *config.PlaybookRules {
	rules, err := config.LoadPlaybookRules("../../configs/playbook_rules.yaml")
	if err != nil {
		panic("failed to load test rules: " + err.Error())
	}
	return rules
}

func TestDiagnose_OOMKilled(t *testing.T) {
	engine := New(loadTestRules())
	bundle := provider.NewOOMKilledFixture()

	result := engine.Diagnose(domain.IntentCrashLoop, bundle)

	if result.PrimaryHypothesis == nil {
		t.Fatal("expected primary hypothesis, got nil")
	}

	if result.PrimaryHypothesis.ID != "oom_resource" {
		t.Errorf("expected oom_resource, got %s", result.PrimaryHypothesis.ID)
	}

	if result.Confidence != domain.ConfidenceHigh {
		t.Errorf("expected High confidence, got %s", result.Confidence)
	}

	if result.PrimaryHypothesis.Score < 70 {
		t.Errorf("expected score >= 70, got %d", result.PrimaryHypothesis.Score)
	}

	t.Logf("Summary: %s", result.Summary)
	t.Logf("Score: %d/%d", result.PrimaryHypothesis.Score, result.PrimaryHypothesis.MaxScore)
	t.Logf("Signals: %v", result.PrimaryHypothesis.Signals)
	t.Logf("Evidence: %v", result.SupportingEvidence)
}

func TestDiagnose_Pending(t *testing.T) {
	engine := New(loadTestRules())
	bundle := provider.NewPendingFixture()

	result := engine.Diagnose(domain.IntentPending, bundle)

	if result.PrimaryHypothesis == nil {
		t.Fatal("expected primary hypothesis, got nil")
	}

	if result.PrimaryHypothesis.ID != "insufficient_resources" {
		t.Errorf("expected insufficient_resources, got %s", result.PrimaryHypothesis.ID)
	}

	// No metrics/logs, confidence should be degraded
	if result.Confidence == domain.ConfidenceHigh {
		t.Errorf("expected degraded confidence (no metrics/logs), got High")
	}

	t.Logf("Summary: %s", result.Summary)
	t.Logf("Confidence: %s", result.Confidence)
	t.Logf("Notes: %v", result.Notes)
}

func TestDiagnose_NoEvidence(t *testing.T) {
	engine := New(loadTestRules())
	bundle := &domain.EvidenceBundle{
		Target: &domain.Target{
			Name:         "test",
			Namespace:    "default",
			Kind:         "deployment",
			ResourceName: "test",
		},
	}

	result := engine.Diagnose(domain.IntentCrashLoop, bundle)

	if result.Confidence != domain.ConfidenceLow {
		t.Errorf("expected Low confidence with no evidence, got %s", result.Confidence)
	}

	t.Logf("Summary: %s", result.Summary)
}
