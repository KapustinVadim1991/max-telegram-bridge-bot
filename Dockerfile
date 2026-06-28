# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 go build -o /max-telegram-bridge-bot .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates
RUN adduser -D -h /app bridge
USER bridge
WORKDIR /app

COPY --from=builder /max-telegram-bridge-bot /usr/local/bin/max-telegram-bridge-bot

ENTRYPOINT ["max-telegram-bridge-bot"]
