# Go 1.26.10, matching go.mod. 1.25 is out of support; 1.26.10 is the first
# release with the stdlib fixes for GO-2026-6809 (crypto/x509) and GO-2026-6617
# (net/http HTTP/2). The tag names the version; the digest pins the bytes.
FROM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS builder
WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG GIT_SHA=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-w -s \
        -X github.com/lgreene03/huginn/internal/version.Version=${VERSION} \
        -X github.com/lgreene03/huginn/internal/version.GitSHA=${GIT_SHA} \
        -X github.com/lgreene03/huginn/internal/version.BuildTime=${BUILD_TIME}" \
      -o huginn ./cmd/huginn

FROM alpine:3.20@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e
WORKDIR /app

RUN apk --no-cache add ca-certificates tzdata

# Run as an unprivileged user rather than root. Create a dedicated app user
# and own the working dir so the binary and configs are readable at runtime.
RUN addgroup -S huginn && adduser -S -G huginn -H -h /app huginn

COPY --from=builder --chown=huginn:huginn /app/huginn .
COPY --from=builder --chown=huginn:huginn /app/configs ./configs

USER huginn

EXPOSE 8081

CMD ["./huginn"]
