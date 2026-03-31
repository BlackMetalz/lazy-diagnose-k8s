package provider

import (
	"context"
	"time"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// MockK8sProvider returns fake K8s data for testing.
type MockK8sProvider struct {
	Facts *domain.K8sFacts
	Err   error
}

func (m *MockK8sProvider) CollectFacts(_ context.Context, _ *domain.Target) (*domain.K8sFacts, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Facts, nil
}

// MockLogsProvider returns fake log data for testing.
type MockLogsProvider struct {
	Facts *domain.LogsFacts
	Err   error
}

func (m *MockLogsProvider) CollectFacts(_ context.Context, _ *domain.Target, _ domain.TimeRange) (*domain.LogsFacts, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Facts, nil
}

// MockMetricsProvider returns fake metrics data for testing.
type MockMetricsProvider struct {
	Facts *domain.MetricsFacts
	Err   error
}

func (m *MockMetricsProvider) CollectFacts(_ context.Context, _ *domain.Target, _ domain.TimeRange) (*domain.MetricsFacts, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Facts, nil
}

// NewOOMKilledFixture creates a mock evidence bundle for OOMKilled scenario.
func NewOOMKilledFixture() *domain.EvidenceBundle {
	memUsage := 510.0 * 1024 * 1024 // 510Mi
	memLimit := 512.0 * 1024 * 1024 // 512Mi
	restartRate := 3.0

	return &domain.EvidenceBundle{
		Target: &domain.Target{
			Name:         "checkout",
			Namespace:    "prod",
			Kind:         "deployment",
			ResourceName: "checkout",
		},
		K8sFacts: &domain.K8sFacts{
			PodStatuses: []domain.PodStatus{
				{
					Name:         "checkout-7f8b9c-x4k2p",
					Phase:        "Running",
					Ready:        false,
					RestartCount: 8,
					ContainerStatuses: []domain.ContainerStatus{
						{
							Name:            "checkout",
							Ready:           false,
							RestartCount:    8,
							State:           "waiting",
							Reason:          "CrashLoopBackOff",
							ExitCode:        137,
							LastTermination: "OOMKilled",
						},
					},
				},
			},
			Events: []domain.K8sEvent{
				{
					Type:    "Warning",
					Reason:  "OOMKilling",
					Message: "Memory cgroup out of memory: Killed process 1234 (checkout)",
					Count:   8,
				},
				{
					Type:    "Warning",
					Reason:  "BackOff",
					Message: "Back-off restarting failed container checkout in pod checkout-7f8b9c-x4k2p",
					Count:   8,
				},
			},
		},
		LogsFacts: &domain.LogsFacts{
			TotalLines: 1500,
			ErrorCount: 45,
			TopErrors: []domain.LogPattern{
				{Pattern: "java.lang.OutOfMemoryError", Count: 12, Sample: "java.lang.OutOfMemoryError: Java heap space"},
				{Pattern: "Allocation failed", Count: 8, Sample: "Allocation failed - JavaScript heap out of memory"},
			},
			TimeRange: domain.TimeRange{
				From: time.Now().Add(-1 * time.Hour),
				To:   time.Now(),
			},
		},
		MetricsFacts: &domain.MetricsFacts{
			MemoryUsage: &memUsage,
			MemoryLimit: &memLimit,
			RestartRate: &restartRate,
		},
	}
}

// NewHTTPErrorSpikeFixture creates a mock evidence bundle for HTTP 5xx error rate spike.
// Pods are healthy and running, but metrics show elevated 5xx error rate.
func NewHTTPErrorSpikeFixture() *domain.EvidenceBundle {
	errorRate := 2.5
	errorRateBefore := 0.1

	return &domain.EvidenceBundle{
		Target: &domain.Target{
			Name:         "webapp-testing",
			Namespace:    "demo-staging",
			Kind:         "deployment",
			ResourceName: "webapp-testing",
		},
		K8sFacts: &domain.K8sFacts{
			PodStatuses: []domain.PodStatus{
				{
					Name:         "webapp-testing-7f8b9c-x4k2p",
					Phase:        "Running",
					Ready:        true,
					RestartCount: 0,
					ContainerStatuses: []domain.ContainerStatus{
						{
							Name:  "webapp",
							Ready: true,
							State: "running",
						},
					},
				},
			},
			RolloutStatus: &domain.RolloutStatus{
				CurrentRevision:     "3",
				DesiredReplicas:     2,
				ReadyReplicas:       2,
				UpdatedReplicas:     2,
				UnavailableReplicas: 0,
			},
		},
		MetricsFacts: &domain.MetricsFacts{
			ErrorRate:       &errorRate,
			ErrorRateBefore: &errorRateBefore,
		},
		CollectedAt: time.Now(),
	}
}

// NewPendingFixture creates a mock evidence bundle for Pending scenario.
func NewPendingFixture() *domain.EvidenceBundle {
	return &domain.EvidenceBundle{
		Target: &domain.Target{
			Name:         "worker",
			Namespace:    "prod",
			Kind:         "deployment",
			ResourceName: "worker",
		},
		K8sFacts: &domain.K8sFacts{
			PodStatuses: []domain.PodStatus{
				{
					Name:         "worker-5d4f8b-abc12",
					Phase:        "Pending",
					Ready:        false,
					RestartCount: 0,
				},
			},
			Events: []domain.K8sEvent{
				{
					Type:    "Warning",
					Reason:  "FailedScheduling",
					Message: "0/3 nodes are available: 3 Insufficient cpu. preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod.",
					Count:   15,
				},
			},
			ResourceRequests: &domain.ResourceRequests{
				CPURequest:    "4",
				CPULimit:      "4",
				MemoryRequest: "8Gi",
				MemoryLimit:   "8Gi",
			},
		},
	}
}
