# syntax=docker/dockerfile:1

FROM golang:1.25 AS builder
WORKDIR /build

RUN apt-get update && apt-get install -y git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GONOSUMDB=* GOFLAGS=-mod=mod \
    go build -o mm-bot ./cmd/main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY --from=builder /build/mm-bot .

EXPOSE 8080
CMD ["./mm-bot"]