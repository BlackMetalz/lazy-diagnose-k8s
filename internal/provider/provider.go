package provider

import (
	"context"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// KubernetesProvider collects K8s facts for a target.
type KubernetesProvider interface {
	CollectFacts(ctx context.Context, target *domain.Target) (*domain.K8sFacts, error)
}

// LogsProvider collects log facts for a target.
type LogsProvider interface {
	CollectFacts(ctx context.Context, target *domain.Target, timeRange domain.TimeRange) (*domain.LogsFacts, error)
}

// MetricsProvider collects metrics facts for a target.
type MetricsProvider interface {
	CollectFacts(ctx context.Context, target *domain.Target, timeRange domain.TimeRange) (*domain.MetricsFacts, error)
}

// Collector orchestrates data collection from all providers with timeout/degraded handling.
type Collector struct {
	K8s     KubernetesProvider
	Logs    LogsProvider
	Metrics MetricsProvider
}

// Collect gathers evidence from all available providers.
// Returns partial results if some providers fail (degraded mode).
func (c *Collector) Collect(ctx context.Context, target *domain.Target, timeRange domain.TimeRange) *domain.EvidenceBundle {
	bundle := &domain.EvidenceBundle{
		Target: target,
	}

	// K8s provider
	if c.K8s != nil {
		k8sFacts, err := c.K8s.CollectFacts(ctx, target)
		if err != nil {
			bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
				Name:      "kubernetes",
				Available: false,
				Error:     err.Error(),
			})
		} else {
			bundle.K8sFacts = k8sFacts
			bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
				Name:      "kubernetes",
				Available: true,
			})
		}
	}

	// Logs provider
	if c.Logs != nil {
		logsFacts, err := c.Logs.CollectFacts(ctx, target, timeRange)
		if err != nil {
			bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
				Name:      "logs",
				Available: false,
				Error:     err.Error(),
			})
		} else {
			bundle.LogsFacts = logsFacts
			bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
				Name:      "logs",
				Available: true,
			})
		}
	}

	// Metrics provider
	if c.Metrics != nil {
		metricsFacts, err := c.Metrics.CollectFacts(ctx, target, timeRange)
		if err != nil {
			bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
				Name:      "metrics",
				Available: false,
				Error:     err.Error(),
			})
		} else {
			bundle.MetricsFacts = metricsFacts
			bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
				Name:      "metrics",
				Available: true,
			})
		}
	}

	return bundle
}
