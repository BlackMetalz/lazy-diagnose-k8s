package resolver

import (
	"testing"
)

func TestResolve_ExactResource(t *testing.T) {
	r := New()

	target, err := r.Resolve("deployment/my-app", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind != "deployment" {
		t.Errorf("expected deployment, got %s", target.Kind)
	}
	if target.ResourceName != "my-app" {
		t.Errorf("expected my-app, got %s", target.ResourceName)
	}
	if target.Namespace != "staging" {
		t.Errorf("expected staging, got %s", target.Namespace)
	}
}

func TestResolve_FullPath(t *testing.T) {
	r := New()

	target, err := r.Resolve("prod/deployment/checkout", "default")
	if err != nil {
		t.Fatal(err)
	}
	if target.Namespace != "prod" {
		t.Errorf("expected prod, got %s", target.Namespace)
	}
	if target.Kind != "deployment" {
		t.Errorf("expected deployment, got %s", target.Kind)
	}
	if target.ResourceName != "checkout" {
		t.Errorf("expected checkout, got %s", target.ResourceName)
	}
}

func TestResolve_PlainName(t *testing.T) {
	r := New()

	target, err := r.Resolve("checkout", "demo-prod")
	if err != nil {
		t.Fatal(err)
	}
	if target.ResourceName != "checkout" {
		t.Errorf("expected checkout, got %s", target.ResourceName)
	}
	if target.Namespace != "demo-prod" {
		t.Errorf("expected demo-prod, got %s", target.Namespace)
	}
	if target.Kind != "deployment" {
		t.Errorf("expected deployment (default), got %s", target.Kind)
	}
	// Should have app=checkout selector for K8s provider
	if target.Selectors["app"] != "checkout" {
		t.Errorf("expected selector app=checkout, got %v", target.Selectors)
	}
}

func TestResolve_Empty(t *testing.T) {
	r := New()

	_, err := r.Resolve("", "default")
	if err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestResolve_PodShorthand(t *testing.T) {
	r := New()

	target, err := r.Resolve("pod/my-pod-abc-123", "default")
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind != "pod" {
		t.Errorf("expected pod, got %s", target.Kind)
	}
}
