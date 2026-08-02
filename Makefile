# Makefile for voicx
# Common developer tasks. Targets are phony unless they produce a file.

GO          ?= go
DOCKER      ?= docker
IMAGE       ?= voicx:dev
BINARY       = bin/voicx
PKG          = ./cmd/server

# Embedded version metadata (see internal/version). Build = commit count, so
# every commit bumps the version; dirty flag for uncommitted changes.
VOICX_VERSION    ?= $(shell cat VERSION 2>/dev/null || echo 0.0.0-dev)
VOICX_BUILD      ?= $(shell git rev-list --count HEAD 2>/dev/null || echo 0)
VOICX_COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
VOICX_DIRTY      ?= $(shell git diff --quiet 2>/dev/null && echo false || echo true)
VOICX_BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VOICX_UPDATE_REPO ?= voicx/voicx

VERSION_FLAGS = -ldflags="-s -w \
	-X voicx/internal/version.Version=$(VOICX_VERSION) \
	-X voicx/internal/version.Build=$(VOICX_BUILD) \
	-X voicx/internal/version.Commit=$(VOICX_COMMIT) \
	-X voicx/internal/version.BuildDate=$(VOICX_BUILD_DATE) \
	-X voicx/internal/version.Dirty=$(VOICX_DIRTY) \
	-X voicx/internal/version.UpdateRepo=$(VOICX_UPDATE_REPO)"

.PHONY: all build run migrate proto tidy test cover fmt vet docker-build docker-run docker-stop compose-up compose-down compose-logs chaos clean help client-build

all: build

## build: compile the server binary into ./bin
build:
	$(GO) build $(VERSION_FLAGS) -o $(BINARY) $(PKG)

## client-build: build the Wails client with embedded version metadata
client-build:
	cd client && wails build $(VERSION_FLAGS)

## run: run the server locally (go run)
run:
	$(GO) run $(VERSION_FLAGS) $(PKG)

## migrate: run database migrations (go run ./cmd/migrate)
migrate:
	$(GO) run ./cmd/migrate

## proto: regenerate the gRPC stubs in ./v1 from proto/ (needs buf, 232)
proto:
	buf generate

## tidy: run go mod tidy
tidy:
	$(GO) mod tidy

## test: run the full test suite
test:
	$(GO) test ./...

## cover: run the full test suite with coverage report
cover:
	$(GO) test -cover ./...

## fmt: format all Go sources
fmt:
	$(GO) fmt ./...

## vet: run go vet across the module
vet:
	$(GO) vet ./...

## docker-build: build the voicx:dev image from the Dockerfile
docker-build:
	$(DOCKER) build \
		--build-arg VOICX_VERSION=$(VOICX_VERSION) \
		--build-arg VOICX_BUILD=$(VOICX_BUILD) \
		--build-arg VOICX_COMMIT=$(VOICX_COMMIT) \
		--build-arg VOICX_DIRTY=$(VOICX_DIRTY) \
		--build-arg VOICX_BUILD_DATE=$(VOICX_BUILD_DATE) \
		--build-arg VOICX_UPDATE_REPO=$(VOICX_UPDATE_REPO) \
		-t $(IMAGE) .

## docker-run: run the voicx:dev image with default ports published
docker-run:
	$(DOCKER) run --rm -p 9987:9987/udp -p 10011:10011 -p 30033:30033 -p 50051:50051 -p 9090:9090 $(IMAGE)

## docker-stop: stop and remove any running voicx containers
docker-stop:
	-$(DOCKER) rm -f voicx 2>/dev/null || true

## compose-up: build and start the full stack (voicx + postgres + redis) detached
compose-up:
	$(DOCKER) compose up -d --build

## compose-down: stop and remove the compose stack (containers, networks)
compose-down:
	$(DOCKER) compose down

## compose-logs: tail logs from all compose services
compose-logs:
	$(DOCKER) compose logs -f --tail=100

## chaos: run the database chaos drill against the running compose stack (467)
chaos:
	./scripts/chaos-db.sh

## clean: remove local build artifacts
clean:
	rm -rf bin out dist

## help: print this help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make <target>\n\nTargets:\n"} \
	/^[a-zA-Z_-]+:.*##/ { printf "  %-16s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
