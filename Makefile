# Makefile for voicx
# Common developer tasks. Targets are phony unless they produce a file.

GO          ?= go
DOCKER      ?= docker
IMAGE       ?= voicx:dev
BINARY       = bin/voicx
PKG          = ./cmd/server
LDFLAGS      = -ldflags="-s -w"

.PHONY: all build run migrate tidy test cover fmt vet docker-build docker-run docker-stop compose-up compose-down compose-logs clean help

all: build

## build: compile the server binary into ./bin
build:
	$(GO) build $(LDFLAGS) -o $(BINARY) $(PKG)

## run: run the server locally (go run)
run:
	$(GO) run $(PKG)

## migrate: run database migrations (go run ./cmd/migrate)
migrate:
	$(GO) run ./cmd/migrate

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
	$(DOCKER) build -t $(IMAGE) .

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

## clean: remove local build artifacts
clean:
	rm -rf bin out dist

## help: print this help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make <target>\n\nTargets:\n"} \
	/^[a-zA-Z_-]+:.*##/ { printf "  %-16s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
