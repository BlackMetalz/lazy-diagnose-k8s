.PHONY: build run test lint clean

BINARY=lazy-diagnose-k8s

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
