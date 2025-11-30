# ─────────────────────────────────────────────
# 1) Build stage (Go 1.25)
# ─────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o server .


# ─────────────────────────────────────────────
# 2) Production stage — Distroless static (no shell, no root)
# ─────────────────────────────────────────────
FROM gcr.io/distroless/static:latest

WORKDIR /app

COPY --from=builder /app/server .
COPY .env .env
COPY web ./web

EXPOSE 8080

ENTRYPOINT ["/app/server"]
