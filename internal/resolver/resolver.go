package resolver

import (
	"fmt"
	"strings"

	"github.com/lazy-diagnose-k8s/internal/domain"
)

// Resolver resolves user input into a concrete K8s target.
type Resolver struct{}

// New creates a new Resolver.
func New() *Resolver {
	return &Resolver{}
}

// Resolve attempts to resolve the raw target string into a Target.
// Handles exact resource paths: deployment/checkout, prod/deployment/checkout.
// For plain names (e.g. "checkout"), returns a best-guess target — caller should
// use fuzzy pod search as fallback.
func (r *Resolver) Resolve(raw string, defaultNamespace string) (*domain.Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty target")
	}

	// 1. Exact resource path: kind/name or namespace/kind/name
	if t, ok := parseExactResource(raw, defaultNamespace); ok {
		return t, nil
	}

	// 2. Plain name — treat as deployment name (fuzzy search will refine)
	return &domain.Target{
		Name:         raw,
		Namespace:    defaultNamespace,
		Kind:         "deployment",
		ResourceName: raw,
		Selectors:    map[string]string{"app": raw},
	}, nil
}

// parseExactResource handles formats like:
//   - "deployment/checkout" → kind=deployment, name=checkout
//   - "pod/checkout-abc-123" → kind=pod, name=checkout-abc-123
//   - "prod/deployment/checkout" → ns=prod, kind=deployment, name=checkout
func parseExactResource(raw string, defaultNs string) (*domain.Target, bool) {
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
