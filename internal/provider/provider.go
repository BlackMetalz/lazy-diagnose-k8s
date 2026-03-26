package provider

import (
	"context"
	"sync"
	"time"

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
	Timeout time.Duration // per-provider timeout, default 30s
}

// Collect gathers evidence from all providers concurrently.
// Returns partial results if some providers fail (degraded mode).
func (c *Collector) Collect(ctx context.Context, target *domain.Target, timeRange domain.TimeRange) *domain.EvidenceBundle {
	bundle := &domain.EvidenceBundle{
		Target: target,
	}

	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	// K8s provider
	if c.K8s != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			provCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			facts, err := c.K8s.CollectFacts(provCtx, target)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
					Name: "kubernetes", Available: false, Error: err.Error(), Duration: time.Since(start),
				})
			} else {
				bundle.K8sFacts = facts
				bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
					Name: "kubernetes", Available: true, Duration: time.Since(start),
				})
			}
		}()
	}

	// Logs provider
	if c.Logs != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			provCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			facts, err := c.Logs.CollectFacts(provCtx, target, timeRange)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
					Name: "logs", Available: false, Error: err.Error(), Duration: time.Since(start),
				})
			} else {
				bundle.LogsFacts = facts
				bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
					Name: "logs", Available: true, Duration: time.Since(start),
				})
			}
		}()
	}

	// Metrics provider
	if c.Metrics != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			provCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			facts, err := c.Metrics.CollectFacts(provCtx, target, timeRange)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
					Name: "metrics", Available: false, Error: err.Error(), Duration: time.Since(start),
				})
			} else {
				bundle.MetricsFacts = facts
				bundle.ProviderStatuses = append(bundle.ProviderStatuses, domain.ProviderStatus{
					Name: "metrics", Available: true, Duration: time.Since(start),
				})
			}
		}()
	}

	wg.Wait()
	return bundle
}
