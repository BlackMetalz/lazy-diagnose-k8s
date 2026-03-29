.PHONY: build run test lint clean docker-build docker-load deploy scenarios scenarios-clean scenarios-status demo-alerts cluster2-create cluster2-monitoring cluster2-scenarios cluster2-clean host-ip-patch

BINARY=lazy-diagnose-k8s
IMAGE=lazy-diagnose-k8s:latest

build:
	go build -o bin/$(BINARY) ./cmd/bot

run:
	go run ./cmd/bot

test:
	go test ./... -v

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/

# Docker
docker-build:
	docker build -t $(IMAGE) .

# Load image into kind cluster
docker-load: docker-build
	kind load docker-image $(IMAGE) --name lazy-diag

# Deploy to kind cluster
deploy: docker-load
	kubectl apply -f deploy/bot/namespace.yaml
	kubectl apply -f deploy/bot/rbac.yaml
	kubectl apply -f deploy/bot/configmap.yaml
	@echo "---"
	@echo "Now create the secret:"
	@echo "  kubectl create secret generic lazy-diagnose-secrets \\"
	@echo "    --namespace=lazy-diagnose \\"
	@echo "    --from-literal=TELEGRAM_BOT_TOKEN=your-token-here"
	@echo "---"
	@echo "Then deploy:"
	@echo "  kubectl apply -f deploy/bot/deployment.yaml"

# Test scenarios
scenarios:
	@kubectl apply -f deploy/test-workloads/namespaces.yaml
	@kubectl apply -f deploy/test-workloads/
	@echo ""
	@echo "All scenarios deployed. Wait ~30s then check:"
	@echo "  make scenarios-status"

scenarios-status:
	@echo "=== demo-prod ==="
	@kubectl get pods -n demo-prod 2>/dev/null || true
	@echo ""
	@echo "=== demo-staging ==="
	@kubectl get pods -n demo-staging 2>/dev/null || true
	@echo ""
	@echo "=== demo-infra ==="
	@kubectl get pods -n demo-infra 2>/dev/null || true
	@echo ""
	@echo "=== Expected ==="
	@echo "  demo-prod:    checkout=CrashLoop  worker=Pending  payment=Running"
	@echo "  demo-staging: api-config-missing=CrashLoop  api-dependency-fail=CrashLoop  api-runtime-crash=CrashLoop  api-init-fail=Init:CrashLoop"
	@echo "  demo-infra:   api-bad-image=ImagePull  api-probe-fail=CrashLoop  ml-worker-taint=Pending  db-pvc-pending=Pending  api-not-ready=Running(0/1)"

demo-alerts:
	@./deploy/test-workloads/demo-webhooks.sh $(NUM)

scenarios-clean:
	@kubectl delete -f deploy/test-workloads/ --ignore-not-found
	@kubectl delete pvc data-pvc-test -n demo-infra --ignore-not-found
	@kubectl delete ns demo-prod demo-staging demo-infra --ignore-not-found
	@echo "All scenarios removed."

# Cluster 2 (multi-cluster setup)
cluster2-create:
	kind create cluster --config deploy/kind-config-cluster2.yaml

cluster2-monitoring:
	kubectl --context kind-lazy-diag-2 create namespace monitoring --dry-run=client -o yaml | kubectl --context kind-lazy-diag-2 apply -f -
	kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/kube-state-metrics.yaml
	kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/vmagent-cluster2.yaml
	kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/vlagent-cluster2.yaml

cluster2-scenarios:
	@kubectl --context kind-lazy-diag-2 apply -f deploy/test-workloads/namespaces.yaml
	@kubectl --context kind-lazy-diag-2 apply -f deploy/test-workloads/
	@echo "Scenarios deployed to cluster2. Check: kubectl --context kind-lazy-diag-2 get pods -A"

cluster2-clean:
	kind delete cluster --name lazy-diag-2

# Ubuntu: patch host.docker.internal → real host IP (Docker bridge gateway)
# Usage: make host-ip-patch HOST_IP=172.19.0.1
# To undo: git checkout deploy/
host-ip-patch:
ifndef HOST_IP
	$(error HOST_IP is required. Run: HOST_IP=$$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}}{{end}}{{end}}' | grep -oE '([0-9]+\.){3}[0-9]+') make host-ip-patch)
endif
	@echo "Patching host.docker.internal → $(HOST_IP)"
	@sed -i 's|host\.docker\.internal|$(HOST_IP)|g' \
		deploy/monitoring/vmagent.yaml \
		deploy/monitoring/vmagent-cluster2.yaml \
		deploy/monitoring/vlagent.yaml \
		deploy/monitoring/vlagent-cluster2.yaml \
		deploy/monitoring/vmalert.yaml \
		deploy/monitoring/alertmanager.yaml \
		deploy/bot/deployment.yaml
	@echo "Done. To undo: git checkout deploy/"
