.PHONY: build run test lint clean docker-build docker-load deploy

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
