FROM golang:1.25-alpine AS builder

WORKDIR /app

# Download deps separately for layer caching
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o mimir ./cmd/mimir

# Distroless scratch image — no shell, no libc, minimal attack surface
FROM scratch

COPY --from=builder /app/mimir /mimir

# Copy CA certificates for HTTPS to LLM provider APIs
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENTRYPOINT ["/mimir"]
