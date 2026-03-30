.PHONY: build run test lint clean docker-build docker-load deploy scenarios scenarios-clean scenarios-status demo-alerts cluster2-create cluster2-monitoring cluster2-scenarios cluster2-clean host-ip-patch ubuntu-monitoring ubuntu-cluster2-monitoring

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

# Run Docker container locally
docker-run: docker-build
	docker run --rm --net=host -v $(HOME)/.kube/config:/root/.kube/config:ro -e TELEGRAM_BOT_TOKEN -e TELEGRAM_CHAT_ID -e LLM_BACKEND -e LLM_API_KEY -e LLM_MODEL -e HOLMES_MODEL -e HOLMES_BASE_URL -e HOLMES_API_KEY -e CONFIG_PATH=/etc/lazy-diagnose-k8s/config.yaml $(IMAGE)

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

# ──── Ubuntu (Docker Engine) ────
# Uses deploy/monitoring/ubuntu/ YAMLs that read host IP from a ConfigMap.
# No file patching needed — auto-detects the kind Docker bridge gateway IP.

# Detect host IP from kind Docker network (IPv4 gateway)
KIND_HOST_IP = $(shell docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' 2>/dev/null | grep -oE '([0-9]+\.){3}[0-9]+' | head -1)

# Deploy monitoring to cluster 1 (Ubuntu)
ubuntu-monitoring:
	@if [ -z "$(KIND_HOST_IP)" ]; then echo "ERROR: Cannot detect kind network gateway. Is the kind cluster running?"; exit 1; fi
	@echo "Detected host IP: $(KIND_HOST_IP)"
	kubectl create namespace monitoring --dry-run=client -o yaml | kubectl apply -f -
	kubectl -n monitoring create configmap host-endpoints \
		--from-literal=vm_url=http://$(KIND_HOST_IP):8428 \
		--from-literal=vm_write_url=http://$(KIND_HOST_IP):8428/api/v1/write \
		--from-literal=vl_write_url=http://$(KIND_HOST_IP):9428/insert/native \
		--dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f deploy/monitoring/kube-state-metrics.yaml
	kubectl apply -f deploy/monitoring/ubuntu/vmagent.yaml
	kubectl apply -f deploy/monitoring/ubuntu/vlagent.yaml
	kubectl apply -f deploy/monitoring/alert-rules.yaml
	./deploy/monitoring/ubuntu/gen-alertmanager-config.sh $(KIND_HOST_IP)
	kubectl apply -f deploy/monitoring/ubuntu/alertmanager.yaml
	kubectl apply -f deploy/monitoring/ubuntu/vmalert.yaml
	@echo ""
	@echo "Done. Ubuntu monitoring deployed to cluster 1 (host: $(KIND_HOST_IP))"

# Deploy monitoring to cluster 2 (Ubuntu)
ubuntu-cluster2-monitoring:
	@if [ -z "$(KIND_HOST_IP)" ]; then echo "ERROR: Cannot detect kind network gateway. Is the kind cluster running?"; exit 1; fi
	@echo "Detected host IP: $(KIND_HOST_IP)"
	kubectl --context kind-lazy-diag-2 create namespace monitoring --dry-run=client -o yaml | kubectl --context kind-lazy-diag-2 apply -f -
	kubectl --context kind-lazy-diag-2 -n monitoring create configmap host-endpoints \
		--from-literal=vm_url=http://$(KIND_HOST_IP):8428 \
		--from-literal=vm_write_url=http://$(KIND_HOST_IP):8428/api/v1/write \
		--from-literal=vl_write_url=http://$(KIND_HOST_IP):9428/insert/native \
		--dry-run=client -o yaml | kubectl --context kind-lazy-diag-2 apply -f -
	kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/kube-state-metrics.yaml
	kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/ubuntu/vmagent-cluster2.yaml
	kubectl --context kind-lazy-diag-2 apply -f deploy/monitoring/ubuntu/vlagent-cluster2.yaml
	@echo ""
	@echo "Done. Ubuntu monitoring deployed to cluster 2 (host: $(KIND_HOST_IP))"
