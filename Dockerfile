FROM golang:1.25-alpine@sha256:c4ea15b4a7912716eb362a022e2b12317762eca387423760bc59c0f9ae69423c AS builder
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
