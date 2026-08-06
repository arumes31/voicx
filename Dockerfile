# syntax=docker/dockerfile:1.24.0@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89
# =============================================================================
# voicx - voice/video server (Phase 1 base image)
# =============================================================================
# Multi-stage build:
#   1. builder  - cross-compiles a static Go binary from Go 1.26.5/Alpine 3.24
#   2. runtime  - minimal Alpine 3.24 image running as a non-root user
#
# NOTE on CGO: Phase 1 keeps CGO_ENABLED=0 to produce a fully static binary
# that runs on scratch/alpine without libc dependencies. Later phases that
# require FFmpeg/cgo bindings will need to switch to CGO_ENABLED=1 with an
# appropriate toolchain (e.g. gcc/musl-gcc) and a matching runtime base.
# =============================================================================

# -----------------------------------------------------------------------------
# Builder stage
# -----------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

ARG TARGETOS
ARG TARGETARCH

# Version metadata: .git is excluded from the build context (see
# .dockerignore), so it is injected via --build-arg (from Makefile/compose).
ARG VOICX_VERSION=0.0.0-dev
ARG VOICX_BUILD=0
ARG VOICX_COMMIT=none
ARG VOICX_DIRTY=false
ARG VOICX_BUILD_DATE=unknown
ARG VOICX_UPDATE_REPO=voicx/voicx

# git is required by `go mod download` for modules that reference VCS sources.
RUN apk add --no-cache git

WORKDIR /src

# Cache module downloads: copy only the module manifests first so that
# subsequent source changes do not invalidate the mod-download layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

# Copy the rest of the source tree.
COPY . .

# Build a stripped, static binary with embedded version metadata.
#   -s : strip symbol table
#   -w : strip DWARF debug info
# CGO_ENABLED=0 ensures a static binary with no libc dependency.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -mod=readonly -trimpath \
    -ldflags="-s -w -buildid= \
      -X voicx/internal/version.Version=${VOICX_VERSION} \
      -X voicx/internal/version.Build=${VOICX_BUILD} \
      -X voicx/internal/version.Commit=${VOICX_COMMIT} \
      -X voicx/internal/version.BuildDate=${VOICX_BUILD_DATE} \
      -X voicx/internal/version.Dirty=${VOICX_DIRTY} \
      -X voicx/internal/version.UpdateRepo=${VOICX_UPDATE_REPO}" \
    -o /out/voicx ./cmd/server

# -----------------------------------------------------------------------------
# Runtime stage
# -----------------------------------------------------------------------------
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS runtime

# ca-certificates: required for outbound TLS (e.g. HTTPS clients, webhooks).
# tzdata:         required for proper timezone handling in logs/scheduling.
# wget:           used by the HEALTHCHECK below.
RUN apk add --no-cache ca-certificates tzdata wget

# Create a non-root user/group with a fixed UID for predictable permissions.
# uid 10001 avoids clashes with common alpine system users.
RUN addgroup -S -g 10001 voicx \
    && adduser  -S -G voicx -u 10001 -h /home/voicx voicx

# Data directories for uploaded files, avatars/icons, and recordings. The
# compose stack mounts a named volume at /data; creating it here (owned by
# the runtime user) makes fresh volumes initialize with the right ownership
# even with read_only rootfs + cap_drop.
RUN mkdir -p /data/files /data/recordings \
    && chown -R 10001:10001 /data

# Copy the compiled binary from the builder stage.
COPY --from=builder --chown=10001:10001 /out/voicx /out/voicx

# Drop privileges: run as the non-root voicx user.
USER 10001:10001

# Expose the service ports:
#   12333/tcp  - TLS control channel
#   12334/udp  - UDP keepalive/media support
#   12335/tcp  - ServerQuery admin/bot text protocol
#   12336/tcp  - file transfer (token-authorized)
#   12337/tcp  - health/readiness HTTP endpoint (/healthz, /readyz)
#   12338/tcp  - gRPC signaling/control (loopback-only by default)
#   12339/tcp  - ServerQuery over SSH (disabled by default)
EXPOSE 12333/tcp 12334/udp 12335/tcp 12336/tcp 12337/tcp 12338/tcp 12339/tcp

# Liveness probe against the health endpoint.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:12337/healthz || exit 1

STOPSIGNAL SIGTERM

ENTRYPOINT ["/out/voicx"]
