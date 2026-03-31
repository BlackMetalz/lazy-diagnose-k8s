package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// Provider collects metrics from VictoriaMetrics via PromQL HTTP API.
type Provider struct {
	baseURL    string
	cluster    string // cluster label filter for multi-cluster setups
	httpClient *http.Client
}

// New creates a new metrics provider.
func New(baseURL string) *Provider {
	return &Provider{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewWithCluster creates a metrics provider that filters by cluster label.
func NewWithCluster(baseURL, cluster string) *Provider {
	return &Provider{
		baseURL: baseURL,
		cluster: cluster,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// CollectFacts queries VictoriaMetrics for relevant metrics.
func (p *Provider) CollectFacts(ctx context.Context, target *domain.Target, timeRange domain.TimeRange) (*domain.MetricsFacts, error) {
	facts := &domain.MetricsFacts{
		TimeRange: timeRange,
	}

	namespace := target.Namespace
	// Use pod name prefix or container name for filtering
	podFilter := target.ResourceName + ".*"

	// Build cluster filter for PromQL
	clusterFilter := ""
	if p.cluster != "" {
		clusterFilter = fmt.Sprintf(`cluster="%s",`, p.cluster)
	}

	// Restart rate (restarts in last 15 min)
	restartRate, err := p.queryScalar(ctx, fmt.Sprintf(
		`sum(increase(kube_pod_container_status_restarts_total{%snamespace="%s",pod=~"%s"}[15m]))`,
		clusterFilter, namespace, podFilter,
	))
	if err == nil && restartRate != nil {
		facts.RestartRate = restartRate
	}

	// Memory usage (current, bytes)
	memUsage, err := p.queryScalar(ctx, fmt.Sprintf(
		`sum(container_memory_working_set_bytes{%snamespace="%s",pod=~"%s",container!=""})`,
		clusterFilter, namespace, podFilter,
	))
	if err == nil && memUsage != nil {
		facts.MemoryUsage = memUsage
	}

	// Memory limit (bytes)
	memLimit, err := p.queryScalar(ctx, fmt.Sprintf(
		`sum(kube_pod_container_resource_limits{%snamespace="%s",pod=~"%s",resource="memory"})`,
		clusterFilter, namespace, podFilter,
	))
	if err == nil && memLimit != nil {
		facts.MemoryLimit = memLimit
	}

	// CPU usage (cores)
	cpuUsage, err := p.queryScalar(ctx, fmt.Sprintf(
		`sum(rate(container_cpu_usage_seconds_total{%snamespace="%s",pod=~"%s",container!=""}[5m]))`,
		clusterFilter, namespace, podFilter,
	))
	if err == nil && cpuUsage != nil {
		facts.CPUUsage = cpuUsage
	}

	// CPU limit (cores)
	cpuLimit, err := p.queryScalar(ctx, fmt.Sprintf(
		`sum(kube_pod_container_resource_limits{%snamespace="%s",pod=~"%s",resource="cpu"})`,
		clusterFilter, namespace, podFilter,
	))
	if err == nil && cpuLimit != nil {
		facts.CPULimit = cpuLimit
	}

	// HTTP 5xx error rate (current, per second over 5m)
	// Try nginx-ingress metrics first, fallback to generic http_requests_total
	errorRate, err := p.queryScalar(ctx, fmt.Sprintf(
		`sum(rate(nginx_ingress_controller_requests{%snamespace="%s",service=~"%s.*",status=~"5.."}[5m]))`,
		clusterFilter, namespace, podFilter,
	))
	if err == nil && errorRate != nil {
		facts.ErrorRate = errorRate
	} else {
		// Fallback: generic app-exported metrics
		errorRate, err = p.queryScalar(ctx, fmt.Sprintf(
			`sum(rate(http_requests_total{%snamespace="%s",pod=~"%s",code=~"5.."}[5m]))`,
			clusterFilter, namespace, podFilter,
		))
		if err == nil && errorRate != nil {
			facts.ErrorRate = errorRate
		}
	}

	// HTTP 5xx error rate (1h ago baseline for spike detection)
	errorRateBefore, err := p.queryScalar(ctx, fmt.Sprintf(
		`sum(rate(nginx_ingress_controller_requests{%snamespace="%s",service=~"%s.*",status=~"5.."}[5m] offset 1h))`,
		clusterFilter, namespace, podFilter,
	))
	if err == nil && errorRateBefore != nil {
		facts.ErrorRateBefore = errorRateBefore
	} else {
		errorRateBefore, err = p.queryScalar(ctx, fmt.Sprintf(
			`sum(rate(http_requests_total{%snamespace="%s",pod=~"%s",code=~"5.."}[5m] offset 1h))`,
			clusterFilter, namespace, podFilter,
		))
		if err == nil && errorRateBefore != nil {
			facts.ErrorRateBefore = errorRateBefore
		}
	}

	return facts, nil
}

// promResponse represents the VictoriaMetrics/Prometheus API response.
type promResponse struct {
	Status string   `json:"status"`
	Data   promData `json:"data"`
}

type promData struct {
	ResultType string       `json:"resultType"`
	Result     []promResult `json:"result"`
}

type promResult struct {
	Metric map[string]string `json:"metric"`
	Value  [2]interface{}    `json:"value"` // [timestamp, "value"]
}

// queryScalar executes a PromQL query and returns a single scalar value.
func (p *Provider) queryScalar(ctx context.Context, query string) (*float64, error) {
	u, err := url.Parse(p.baseURL + "/api/v1/query")
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("query", query)
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query VictoriaMetrics: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("VictoriaMetrics returned %d: %s", resp.StatusCode, string(body))
	}

	var promResp promResponse
	if err := json.Unmarshal(body, &promResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if promResp.Status != "success" || len(promResp.Data.Result) == 0 {
		return nil, nil // no data
	}

	// Extract scalar value from first result
	valStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return nil, nil
	}

	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return nil, nil
	}

	return &val, nil
}
