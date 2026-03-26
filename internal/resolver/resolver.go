package resolver

import (
	"fmt"
	"strings"

	"github.com/lazy-diagnose-k8s/internal/config"
	"github.com/lazy-diagnose-k8s/internal/domain"
)

// Resolver resolves user input into a concrete K8s target.
type Resolver struct {
	serviceMap *config.ServiceMap
}

// New creates a new Resolver with the given service map.
func New(sm *config.ServiceMap) *Resolver {
	return &Resolver{serviceMap: sm}
}

// Resolve attempts to resolve the raw target string into a Target.
// Strategy: exact resource match → service_map lookup → error with hint.
func (r *Resolver) Resolve(raw string, defaultNamespace string) (*domain.Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty target")
	}

	// 1. Check if it looks like an exact resource reference: kind/name or namespace/kind/name
	if t, ok := r.parseExactResource(raw, defaultNamespace); ok {
		return t, nil
	}

	// 2. Lookup in service_map
	if entry := r.serviceMap.Lookup(raw); entry != nil {
		return r.entryToTarget(entry), nil
	}

	// 3. Try case-insensitive match
	if entry := r.serviceMap.Lookup(strings.ToLower(raw)); entry != nil {
		return r.entryToTarget(entry), nil
	}

	return nil, fmt.Errorf(
		"không tìm thấy target '%s'. Thử dùng tên deployment/pod chính xác hoặc thêm vào service_map.yaml",
		raw,
	)
}

// parseExactResource handles formats like:
//   - "deployment/checkout" → kind=deployment, name=checkout
//   - "pod/checkout-abc-123" → kind=pod, name=checkout-abc-123
//   - "prod/deployment/checkout" → ns=prod, kind=deployment, name=checkout
func (r *Resolver) parseExactResource(raw string, defaultNs string) (*domain.Target, bool) {
	parts := strings.SplitN(raw, "/", 3)

	switch len(parts) {
	case 2:
		kind := normalizeKind(parts[0])
		if kind == "" {
			return nil, false
		}
		return &domain.Target{
			Name:         parts[1],
			Namespace:    defaultNs,
			Kind:         kind,
			ResourceName: parts[1],
		}, true
	case 3:
		kind := normalizeKind(parts[1])
		if kind == "" {
			return nil, false
		}
		return &domain.Target{
			Name:         parts[2],
			Namespace:    parts[0],
			Kind:         kind,
			ResourceName: parts[2],
		}, true
	}
	return nil, false
}

func (r *Resolver) entryToTarget(entry *config.ServiceEntry) *domain.Target {
	kind, name := parseResourceRef(entry.PrimaryResource)
	return &domain.Target{
		Name:         entry.Name,
		Namespace:    entry.Namespace,
		Kind:         kind,
		ResourceName: name,
		Selectors:    entry.Selectors,
		MetricsJob:   entry.MetricsJob,
		RolloutTarget: entry.RolloutTarget,
	}
}

func parseResourceRef(ref string) (kind, name string) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 {
		return normalizeKind(parts[0]), parts[1]
	}
	return "deployment", ref
}

func normalizeKind(k string) string {
	switch strings.ToLower(k) {
	case "deploy", "deployment", "deployments":
		return "deployment"
	case "sts", "statefulset", "statefulsets":
		return "statefulset"
	case "ds", "daemonset", "daemonsets":
		return "daemonset"
	case "pod", "pods":
		return "pod"
	case "svc", "service", "services":
		return "service"
	default:
		return ""
	}
}
