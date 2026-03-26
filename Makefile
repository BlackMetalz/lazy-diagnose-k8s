.PHONY: build run test lint clean docker-build docker-load deploy scenarios scenarios-clean scenarios-status

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
	@kubectl create namespace prod --dry-run=client -o yaml | kubectl apply -f -
	@kubectl apply -f deploy/test-workloads/
	@echo ""
	@echo "All scenarios deployed. Wait ~30s then check:"
	@echo "  make scenarios-status"

scenarios-status:
	@echo "=== Pod Status ==="
	@kubectl get pods -n prod -o wide
	@echo ""
	@echo "=== Expected ==="
	@echo "  checkout           CrashLoopBackOff  (OOMKilled)"
	@echo "  worker             Pending           (insufficient resources)"
	@echo "  payment            Running           (healthy)"
	@echo "  api-config-missing CrashLoopBackOff  (missing env)"
	@echo "  api-probe-fail     Running+restarts  (liveness probe fail)"
	@echo "  api-bad-image      ErrImagePull      (bad image tag)"
	@echo "  api-dependency-fail CrashLoopBackOff (connection refused)"
	@echo "  ml-worker-taint    Pending           (nodeSelector mismatch)"
	@echo "  db-pvc-pending     Pending           (PVC not bound)"

scenarios-clean:
	@kubectl delete -f deploy/test-workloads/ --ignore-not-found
	@kubectl delete pvc data-pvc-test -n prod --ignore-not-found
	@echo "All scenarios removed."
