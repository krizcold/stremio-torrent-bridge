FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Copy go mod files first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/

# Build the binary (static, no CGO)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bridge ./cmd/bridge/

# Runtime stage
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

# Create non-root user and data directory
RUN adduser -D -u 1000 bridge && \
    mkdir -p /data && \
    chown bridge:bridge /data

USER bridge
WORKDIR /app

COPY --from=builder /bridge /app/bridge

EXPOSE 8080

ENTRYPOINT ["/app/bridge"]
