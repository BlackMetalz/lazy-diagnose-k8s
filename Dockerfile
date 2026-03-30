FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bot ./cmd/bot

FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata curl gcc librdkafka-dev \
    && curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/$(dpkg --print-architecture)/kubectl" \
    && install kubectl /usr/local/bin/kubectl \
    && rm kubectl \
    && pip install --no-cache-dir holmesgpt \
    && apt-get purge -y gcc \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /bot /bot
COPY configs/ /etc/lazy-diagnose-k8s/

ENTRYPOINT ["/bot"]
