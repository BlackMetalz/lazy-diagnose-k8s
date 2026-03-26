FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bot ./cmd/bot

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /bot /bot
COPY configs/ /etc/lazy-diagnose-k8s/

ENTRYPOINT ["/bot"]
