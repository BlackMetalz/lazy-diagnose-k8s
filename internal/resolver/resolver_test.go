package resolver

import (
	"testing"

	"github.com/lazy-diagnose-k8s/internal/config"
)

func testServiceMap() *config.ServiceMap {
	return &config.ServiceMap{
		Services: []config.ServiceEntry{
			{
				Name:            "checkout",
				Aliases:         []string{"checkout-service", "checkout-api"},
				Namespace:       "prod",
				PrimaryResource: "deployment/checkout",
				Selectors:       map[string]string{"app": "checkout"},
				MetricsJob:      "checkout-api",
			},
			{
				Name:            "payment",
				Aliases:         []string{"payment-service"},
				Namespace:       "prod",
				PrimaryResource: "deployment/payment",
			},
		},
	}
}

func TestResolve_ServiceMapName(t *testing.T) {
	r := New(testServiceMap())

	target, err := r.Resolve("checkout", "default")
	if err != nil {
		t.Fatal(err)
	}

	if target.ResourceName != "checkout" {
		t.Errorf("expected checkout, got %s", target.ResourceName)
	}
	if target.Namespace != "prod" {
		t.Errorf("expected prod, got %s", target.Namespace)
	}
	if target.Kind != "deployment" {
		t.Errorf("expected deployment, got %s", target.Kind)
	}
}

func TestResolve_Alias(t *testing.T) {
	r := New(testServiceMap())

	target, err := r.Resolve("checkout-api", "default")
	if err != nil {
		t.Fatal(err)
	}

	if target.Name != "checkout" {
		t.Errorf("expected checkout, got %s", target.Name)
	}
}

func TestResolve_ExactResource(t *testing.T) {
	r := New(testServiceMap())

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
	r := New(testServiceMap())

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
}

func TestResolve_NotFound(t *testing.T) {
	r := New(testServiceMap())

	_, err := r.Resolve("unknown-service", "default")
	if err == nil {
		t.Fatal("expected error for unknown service")
	}

	t.Logf("Error: %s", err)
}

func TestResolve_Empty(t *testing.T) {
	r := New(testServiceMap())

	_, err := r.Resolve("", "default")
	if err == nil {
		t.Fatal("expected error for empty target")
	}
}
