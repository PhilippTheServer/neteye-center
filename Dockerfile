# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /neteye-center ./cmd/center

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.21

RUN apk add --no-cache wget ca-certificates

COPY --from=builder /neteye-center /neteye-center

EXPOSE 8080 9090

ENTRYPOINT ["/neteye-center"]
