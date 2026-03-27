.PHONY: build run test lint clean docker-build docker-load deploy scenarios scenarios-clean scenarios-status demo-alerts

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
	@echo "  demo-staging: api-config-missing=CrashLoop  api-dependency-fail=CrashLoop"
	@echo "  demo-infra:   api-bad-image=ImagePull  api-probe-fail=CrashLoop  ml-worker-taint=Pending  db-pvc-pending=Pending"

demo-alerts:
	@./deploy/test-workloads/demo-webhooks.sh $(NUM)

scenarios-clean:
	@kubectl delete -f deploy/test-workloads/ --ignore-not-found
	@kubectl delete pvc data-pvc-test -n demo-infra --ignore-not-found
	@kubectl delete ns demo-prod demo-staging demo-infra --ignore-not-found
	@echo "All scenarios removed."
