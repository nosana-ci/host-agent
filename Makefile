GO      ?= $(shell command -v go 2>/dev/null || echo $(HOME)/.local/go/bin/go)
VERSION ?= 0.0.0-dev
COMMIT   = $(shell git rev-parse --short HEAD 2>/dev/null)
# The base64 ed25519 public key that signs releases; a build without it cannot install or update.
RELEASE_PUBLIC_KEY ?=
PKG     := github.com/nosana-ci/nosana-host/internal/version
LDFLAGS  = -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).ReleasePublicKey=$(RELEASE_PUBLIC_KEY)
RUNTIME ?= 5.8.8

.PHONY: build runtime test lint check api-types clean

## build: bin/nosana-host for linux-amd64 (VERSION=x.y.z RELEASE_PUBLIC_KEY=...)
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/nosana-host ./cmd/nosana-host

## runtime: bin/nosana-runtime-$(RUNTIME)-linux-amd64.tar.gz, the podman bundle
runtime:
	scripts/build-runtime.sh $(RUNTIME) bin

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...
	golangci-lint run ./...

check: lint test

## api-types: regenerate internal/api from host-manager's OpenAPI document (HOST_MANAGER=http://localhost:3005)
api-types:
	scripts/api-types.sh $(HOST_MANAGER)

clean:
	rm -rf bin
