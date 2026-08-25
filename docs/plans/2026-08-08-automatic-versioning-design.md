# Automatic versioning design

## Context and decision

VoicX previously had disconnected version surfaces. `VERSION` contained
`0.4.0`, npm declared `0.0.0`, Wails defaulted desktop metadata to `1.0.0`, and
the Go runtime defaulted to `0.0.0-dev` unless callers happened to use a
Makefile target. The documented direct Go and Wails commands therefore showed
the fallback, while the Docker publication job silently embedded it in shipped
images.

The selected design keeps intentional SemVer releases and automates every
intermediate build identity. `VERSION` remains a stable, human-chosen
`MAJOR.MINOR.PATCH` release line. A clean matching tag produces that exact
version. An untagged commit adds Git build metadata, and a dirty tree adds a
deterministic content fingerprint. Builds do not modify tracked files, avoiding
recursive bumps and merge conflicts. Patch-per-commit mutation and timestamps
were rejected because they make branches conflict or builds irreproducible.

## Components and data flow

`internal/version` owns SemVer validation/comparison, runtime fallback, source
detection, and deterministic fingerprints. `cmd/version` exposes that logic as
human, JSON, linker, GitHub Actions, and Docker projections. Make invokes the
tool for server, Wails, Docker, and Compose builds. CI invokes it once in each
publishing job and passes the resulting values unchanged to all artifacts.

The Go runtime uses linker-injected metadata when present. Otherwise it reads
Go's embedded `vcs.revision`, `vcs.time`, and `vcs.modified` settings, adding a
compiled-binary fingerprint for dirty direct builds. This makes the raw commands
safe while preserving a reproducible source hash in the preferred wrapper.
Wails and npm keep the stable numeric base for platform/package metadata; the
application UI continues to read the authoritative Go value rather than adding
a second frontend runtime source.

## Failure handling and verification

The calculator fails closed when `VERSION` is not strict three-part SemVer,
when multiple release tags point at `HEAD`, or when a release tag's numeric base
does not match `VERSION`. Git operations have bounded timeouts. Dirty detection
includes staged, unstaged, deleted, renamed, and untracked nonignored files.
Source archives receive a deterministic filtered-tree hash instead of claiming
an unverifiable commit.

Unit tests cover SemVer precedence, invalid input, stable/development/dirty
formatting, content-hash determinism, ignored outputs, root discovery, and every
machine-output format. The repository check synchronizes all tracked version
declarations. Verification also builds an actually stamped binary and inspects
its module/linker metadata, then runs the existing Go, frontend, and workflow
quality gates.
