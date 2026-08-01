# syntax=docker/dockerfile:1.6
# =============================================================================
# voicx - TeamSpeak-like voice/video server (Phase 1 base image)
# =============================================================================
# Multi-stage build:
#   1. builder  - compiles a static Go binary from golang:1.22-alpine
#   2. runtime  - minimal alpine:3.20 image running as a non-root user
#
# NOTE on CGO: Phase 1 keeps CGO_ENABLED=0 to produce a fully static binary
# that runs on scratch/alpine without libc dependencies. Later phases that
# require FFmpeg/cgo bindings will need to switch to CGO_ENABLED=1 with an
# appropriate toolchain (e.g. gcc/musl-gcc) and a matching runtime base.
# =============================================================================

# -----------------------------------------------------------------------------
# Builder stage
# -----------------------------------------------------------------------------
FROM golang:1.25-alpine AS builder

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
RUN go mod download

# Copy the rest of the source tree.
COPY . .

# Build a stripped, static binary with embedded version metadata.
#   -s : strip symbol table
#   -w : strip DWARF debug info
# CGO_ENABLED=0 ensures a static binary with no libc dependency.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w \
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
FROM alpine:3.20 AS runtime

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
COPY --from=builder /out/voicx /out/voicx

# Drop privileges: run as the non-root voicx user.
USER 10001:10001

# Expose the service ports:
#   9987/udp   - voice transport (TeamSpeak-compatible default)
#   10011/tcp  - ServerQuery / control (TeamSpeak-compatible default)
#   10012/tcp  - ServerQuery admin/bot text protocol
#   30033/tcp  - file transfer (token-authorized)
#   50051/tcp  - gRPC signaling/control (reserved; listener arrives in a later phase)
#   9090/tcp   - health/readiness HTTP endpoint (/healthz, /readyz)
EXPOSE 9987/udp 10011/tcp 10012/tcp 30033/tcp 50051/tcp 9090/tcp

# Liveness probe against the health endpoint.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:9090/healthz || exit 1

ENTRYPOINT ["/out/voicx"]
