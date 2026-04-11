.PHONY: build run test lint clean docker-build docker-run docker-load deploy scenarios scenarios-clean scenarios-status demo-alerts load-5xx load-5xx-mix ingress ingress-cluster2 cluster2-create cluster2-monitoring cluster2-scenarios cluster2-clean ubuntu-monitoring ubuntu-cluster2-monitoring ext-up ext-down ext-status

BINARY=lazy-diagnose-k8s
IMAGE=lazy-diagnose-k8s:latest

# ──────────────────────────────────────────────
# Build & Run
# ──────────────────────────────────────────────

build:
	go build -o bin/$(BINARY) ./cmd/bot

# Run locally (no Docker). Holmes CLI must be installed on host.
# Works on both macOS and Ubuntu — services accessed via localhost.
run:
	go run ./cmd/bot

test:
	go test ./... -v

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/

# ──────────────────────────────────────────────
# Docker
# ──────────────────────────────────────────────

docker-build:
	docker build -t $(IMAGE) .

# Shared env vars for docker-run targets
DOCKER_ENV = \
	-e TELEGRAM_BOT_TOKEN -e TELEGRAM_CHAT_ID \
	-e LLM_BASE_URL -e LLM_API_KEY -e LLM_MODEL -e HOLMES_MODEL \
	-e CONFIG_PATH=/etc/lazy-diagnose-k8s/config.yaml

# macOS (Docker Desktop): port mapping + host.docker.internal for host access
docker-run: docker-run-macos

docker-run-macos: docker-build
	@# Rewrite kubeconfig for Docker Desktop: 127.0.0.1 → host.docker.internal
	@# Also skip TLS verify since kind cert is issued for 127.0.0.1, not host.docker.internal
	@mkdir -p /tmp/lazy-diagnose-k8s
	@sed -e 's|https://127.0.0.1:|https://host.docker.internal:|g' \
		-e 's|certificate-authority-data:.*|insecure-skip-tls-verify: true|g' \
		$(HOME)/.kube/config > /tmp/lazy-diagnose-k8s/kubeconfig
	docker run --rm -p 8080:8080 \
		-v /tmp/lazy-diagnose-k8s/kubeconfig:/root/.kube/config:ro \
		$(DOCKER_ENV) \
		-e VICTORIA_METRICS_URL=$${VICTORIA_METRICS_URL:-http://host.docker.internal:8428} \
		-e VICTORIA_LOGS_URL=$${VICTORIA_LOGS_URL:-http://host.docker.internal:9428} \
		$(IMAGE)

# Ubuntu (Docker Engine): --net=host works natively, localhost = host
docker-run-ubuntu: docker-build
	docker run --rm --net=host \
		-v $(HOME)/.kube/config:/root/.kube/config:ro \
		$(DOCKER_ENV) \
		$(IMAGE)

# Load image into kind cluster
docker-load: docker-build
	kind load docker-image $(IMAGE) --name lazy-diag

# ──────────────────────────────────────────────
# Deploy to K8s
# ──────────────────────────────────────────────

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

# ──────────────────────────────────────────────
# Test Scenarios
# ──────────────────────────────────────────────

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
	@echo "  demo-staging: api-config-missing=CrashLoop  api-dependency-fail=CrashLoop  api-runtime-crash=CrashLoop  api-init-fail=Init:CrashLoop  webapp-testing=Running(5xx)"
	@echo "  demo-infra:   api-bad-image=ImagePull  api-probe-fail=CrashLoop  ml-worker-taint=Pending  db-pvc-pending=Pending  api-not-ready=Running(0/1)"

scenarios-clean:
	@kubectl delete -f deploy/test-workloads/ --ignore-not-found
	@kubectl delete pvc data-pvc-test -n demo-infra --ignore-not-found
	@kubectl delete ns demo-prod demo-staging demo-infra --ignore-not-found
	@echo "All scenarios removed."

# ──────────────────────────────────────────────
# Demo Alerts
# ──────────────────────────────────────────────

demo-alerts:
	@./deploy/test-workloads/demo-webhooks.sh $(NUM)

# ──────────────────────────────────────────────
# Traffic Generation (5xx)
# ──────────────────────────────────────────────

LOAD_5XX_PORT ?= 80
LOAD_5XX_RPS ?= 10

load-5xx:
	@echo "Sending 100%% 5xx to localhost:$(LOAD_5XX_PORT)/503 (~$(LOAD_5XX_RPS) req/s). Ctrl+C to stop."
	@while true; do curl -s -o /dev/null -w "%{http_code}\n" -H "Host: webapp-testing.local" http://localhost:$(LOAD_5XX_PORT)/503; sleep $$(echo "scale=3; 1/$(LOAD_5XX_RPS)" | bc); done

load-5xx-mix:
	@echo "Sending ~50%% 5xx to localhost:$(LOAD_5XX_PORT)/version (~$(LOAD_5XX_RPS) req/s). Ctrl+C to stop."
	@while true; do curl -s -o /dev/null -w "%{http_code}\n" -H "Host: webapp-testing.local" http://localhost:$(LOAD_5XX_PORT)/version; sleep $$(echo "scale=3; 1/$(LOAD_5XX_RPS)" | bc); done

# ──────────────────────────────────────────────
# Ingress
# ──────────────────────────────────────────────

ingress:
	kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.12.0/deploy/static/provider/kind/deploy.yaml
	@echo "Waiting for ingress-nginx controller to be ready..."
	kubectl wait --namespace ingress-nginx \
		--for=condition=ready pod \
		--selector=app.kubernetes.io/component=controller \
		--timeout=120s
	@echo "Enabling metrics on ingress-nginx..."
	kubectl -n ingress-nginx patch deployment ingress-nginx-controller --type=json \
		-p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--enable-metrics=true"},{"op":"add","path":"/spec/template/spec/containers/0/ports/-","value":{"containerPort":10254,"name":"metrics","protocol":"TCP"}}]'
	kubectl -n ingress-nginx rollout status deployment/ingress-nginx-controller --timeout=120s
	@echo "Ingress-nginx is ready (metrics on :10254)."

ingress-cluster2:
	kubectl --context kind-lazy-diag-2 apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.12.0/deploy/static/provider/kind/deploy.yaml
	@echo "Waiting for ingress-nginx controller to be ready on cluster 2..."
	kubectl --context kind-lazy-diag-2 wait --namespace ingress-nginx \
		--for=condition=ready pod \
		--selector=app.kubernetes.io/component=controller \
		--timeout=120s
	@echo "Enabling metrics on ingress-nginx (cluster 2)..."
	kubectl --context kind-lazy-diag-2 -n ingress-nginx patch deployment ingress-nginx-controller --type=json \
		-p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--enable-metrics=true"},{"op":"add","path":"/spec/template/spec/containers/0/ports/-","value":{"containerPort":10254,"name":"metrics","protocol":"TCP"}}]'
	kubectl --context kind-lazy-diag-2 -n ingress-nginx rollout status deployment/ingress-nginx-controller --timeout=120s
	@echo "Ingress-nginx is ready on cluster 2 (ports 8180/8443, metrics on :10254)."

# ──────────────────────────────────────────────
# Multi-cluster
# ──────────────────────────────────────────────

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

# ──────────────────────────────────────────────
# Ubuntu Monitoring (Docker Engine — uses kind bridge gateway IP)
# ──────────────────────────────────────────────

KIND_HOST_IP = $(shell docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' 2>/dev/null | grep -oE '([0-9]+\.){3}[0-9]+' | head -1)

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

# ──────────────────────────────────────────────
# External Resources (VictoriaMetrics + VictoriaLogs)
# ──────────────────────────────────────────────

VM_CONTAINER ?= victoria-metrics
VL_CONTAINER ?= victoria-logs
VM_IMAGE ?= victoriametrics/victoria-metrics:v1.138.0
VL_IMAGE ?= victoriametrics/victoria-logs:v1.48.0
VM_DATA ?= victoria-metrics-data
VL_DATA ?= victoria-logs-data

ext-up:
	@echo "Starting VictoriaMetrics..."
	@docker run -d --name $(VM_CONTAINER) \
		-p 8428:8428 \
		-v $(VM_DATA):/victoria-metrics-data \
		$(VM_IMAGE) 2>/dev/null || docker start $(VM_CONTAINER)
	@echo "Starting VictoriaLogs..."
	@docker run -d --name $(VL_CONTAINER) \
		-p 9428:9428 \
		-v $(VL_DATA):/victoria-logs-data \
		$(VL_IMAGE) 2>/dev/null || docker start $(VL_CONTAINER)
	@echo "External resources are up."
	@echo "  VictoriaMetrics: http://localhost:8428"
	@echo "  VictoriaLogs:    http://localhost:9428"

ext-down:
	@echo "Stopping VictoriaMetrics..."
	@docker stop $(VM_CONTAINER) 2>/dev/null || true
	@echo "Stopping VictoriaLogs..."
	@docker stop $(VL_CONTAINER) 2>/dev/null || true
	@echo "External resources stopped."

ext-status:
	@echo "=== External Resources ==="
	@docker ps -a --filter name=$(VM_CONTAINER) --filter name=$(VL_CONTAINER) --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
