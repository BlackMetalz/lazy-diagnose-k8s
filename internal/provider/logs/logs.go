package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// Provider collects logs from VictoriaLogs via LogsQL HTTP API.
type Provider struct {
	baseURL    string
	cluster    string // cluster label filter for multi-cluster setups
	httpClient *http.Client
}

// New creates a new logs provider.
func New(baseURL string) *Provider {
	return &Provider{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewWithCluster creates a logs provider that filters by cluster label.
func NewWithCluster(baseURL, cluster string) *Provider {
	return &Provider{
		baseURL: baseURL,
		cluster: cluster,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// CollectFacts queries VictoriaLogs for container logs.
func (p *Provider) CollectFacts(ctx context.Context, target *domain.Target, timeRange domain.TimeRange) (*domain.LogsFacts, error) {
	facts := &domain.LogsFacts{
		TimeRange: timeRange,
	}

	// Build LogsQL query for the target
	// Search by pod name prefix (deployment name) — more reliable than container name
	// since container name often differs from deployment name (e.g. deployment=api-config-missing, container=api)
	query := fmt.Sprintf(
		`kubernetes.pod_namespace:%s AND kubernetes.pod_name:%s*`,
		target.Namespace, target.ResourceName,
	)
	// Filter by cluster label in multi-cluster setups
	if p.cluster != "" {
		query = fmt.Sprintf(`cluster:%s AND %s`, p.cluster, query)
	}

	// Get recent logs
	recentLines, err := p.queryLogs(ctx, query, timeRange, 200)
	if err != nil {
		return nil, fmt.Errorf("query logs: %w", err)
	}

	facts.TotalLines = len(recentLines)

	// Extract recent lines (last 50 for display)
	if len(recentLines) > 50 {
		facts.RecentLines = recentLines[len(recentLines)-50:]
	} else {
		facts.RecentLines = recentLines
	}

	// Count errors and extract patterns
	errorPatterns := make(map[string]int)
	errorSamples := make(map[string]string)
	for _, line := range recentLines {
		lower := strings.ToLower(line)
		if containsError(lower) {
			facts.ErrorCount++
			pattern := extractErrorPattern(line)
			errorPatterns[pattern]++
			if _, exists := errorSamples[pattern]; !exists {
				errorSamples[pattern] = line
			}
		}
	}

	// Convert to sorted top errors
	facts.TopErrors = topErrorPatterns(errorPatterns, errorSamples, 5)

	return facts, nil
}

// logEntry represents a single log line from VictoriaLogs.
type logEntry struct {
	Msg  string `json:"_msg"`
	Time string `json:"_time"`
}

// queryLogs executes a LogsQL query and returns log messages.
func (p *Provider) queryLogs(ctx context.Context, query string, timeRange domain.TimeRange, limit int) ([]string, error) {
	u, err := url.Parse(p.baseURL + "/select/logsql/query")
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("query", query)
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("start", timeRange.From.Format(time.RFC3339))
	params.Set("end", timeRange.To.Format(time.RFC3339))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query VictoriaLogs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("VictoriaLogs returned %d: %s", resp.StatusCode, string(body))
	}

	// VictoriaLogs returns JSON lines (one JSON object per line)
	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry logEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Msg != "" {
			lines = append(lines, entry.Msg)
		}
	}

	return lines, scanner.Err()
}

func containsError(lower string) bool {
	errorKeywords := []string{
		"error", "exception", "fatal", "panic",
		"fail", "critical", "oom", "killed",
		"timeout", "refused", "unavailable",
	}
	for _, kw := range errorKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// extractErrorPattern simplifies a log line into a pattern for grouping.
func extractErrorPattern(line string) string {
	// Truncate to first meaningful part
	if len(line) > 120 {
		line = line[:120]
	}

	// Common Java/Go/Node error patterns
	patterns := []string{
		"OutOfMemoryError", "StackOverflowError", "NullPointerException",
		"panic:", "fatal error:", "runtime error:",
		"connection refused", "connection timeout", "dial tcp",
		"ECONNREFUSED", "ETIMEDOUT", "ENOTFOUND",
		"OOMKilled", "Killed",
		"permission denied", "access denied",
		"no such file", "file not found",
		"env not found", "config error", "missing required",
	}

	lower := strings.ToLower(line)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return p
		}
	}

	// Fallback: first 80 chars
	if len(line) > 80 {
		return line[:80]
	}
	return line
}

func topErrorPatterns(patterns map[string]int, samples map[string]string, limit int) []domain.LogPattern {
	type kv struct {
		Pattern string
		Count   int
	}
	var sorted []kv
	for k, v := range patterns {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Count > sorted[j].Count
	})

	if len(sorted) > limit {
		sorted = sorted[:limit]
	}

	var result []domain.LogPattern
	for _, s := range sorted {
		result = append(result, domain.LogPattern{
			Pattern: s.Pattern,
			Count:   s.Count,
			Sample:  samples[s.Pattern],
		})
	}
	return result
}
