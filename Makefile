# Huginn — build, test, and release helpers.
#
# Build provenance is stamped into internal/version at link time via -ldflags -X.
# Without these args the binary reports VERSION=dev, GIT_SHA=unknown,
# BUILD_TIME=unknown (see internal/version/version.go).

VERSION   ?= dev
GIT_SHA   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG       := github.com/lgreene03/huginn/internal/version

LDFLAGS := -X $(PKG).Version=$(VERSION) \
           -X $(PKG).GitSHA=$(GIT_SHA) \
           -X $(PKG).BuildTime=$(BUILD_TIME)

.PHONY: build build-release test vet docker print-version help

## build: compile the huginn binary with build provenance stamped in.
build:
	go build -ldflags "$(LDFLAGS)" -o huginn ./cmd/huginn

## build-release: stripped, statically-linked release build (matches the Dockerfile).
build-release:
	CGO_ENABLED=0 GOOS=linux go build -ldflags "-w -s $(LDFLAGS)" -o huginn ./cmd/huginn

## test: run the unit test suite.
test:
	go test ./...

## vet: run go vet across all packages.
vet:
	go vet ./...

## docker: build the container image, passing build provenance as --build-arg.
docker:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg GIT_SHA=$(GIT_SHA) \
	  --build-arg BUILD_TIME=$(BUILD_TIME) \
	  -t huginn:$(GIT_SHA) .

## print-version: echo the ldflags values that would be stamped.
print-version:
	@echo "VERSION=$(VERSION) GIT_SHA=$(GIT_SHA) BUILD_TIME=$(BUILD_TIME)"

## help: list available targets.
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
