# Automatic versioning

`VERSION` is the single release-line declaration. It contains exactly three
numeric components, currently `0.4.0`. Builds never rewrite it.

`go run ./cmd/version` projects that release line onto the current source:

| Source state | Canonical version |
| --- | --- |
| Exact clean tag `v0.4.0` | `0.4.0` |
| Clean untagged commit | `0.4.0-dev+g996a4344dcd6` |
| Dirty tracked/untracked source | `0.4.0-dev+g996a4344dcd6.dirty.hb99ce64cf0ba` |
| Source archive without Git | `0.4.0-dev+src.h<content-hash>` |

The commit and content fragments are deterministic 12-character hashes. A
different commit or effective dirty tree produces a different identity;
reverting the tree restores its previous identity. Ignored build outputs do
not affect it.

## Build commands

Use the stamped targets for production-equivalent local artifacts:

```bash
make version
make version-check
make build
make client-build
make docker-build
make compose-up
```

Windows PowerShell uses the same calculator through the native wrapper:

```powershell
./scripts/build.ps1 version
./scripts/build.ps1 check
./scripts/build.ps1 server
./scripts/build.ps1 client
```

The calculator also supports machine-readable projections:

```bash
go run ./cmd/version -format json
go run ./cmd/version -format runtime
go run ./cmd/version -format ldflags
go run ./cmd/version -format github
go run ./cmd/version -format docker
```

Plain `go build`, `go run`, `wails dev`, and `wails build` use the VCS settings
embedded by the Go toolchain. When a direct build lacks those settings, or is
dirty, the runtime adds a fingerprint of the compiled executable. The shared
calculator is preferred for release-equivalent builds because its dirty
fingerprint is source-derived and reproducible.

A direct `docker build` or `docker compose build` cannot see `.git`; when no
version build arguments are supplied, the Dockerfile automatically embeds the
deterministic `src.h<content-hash>` archive identity instead.

## Release rules

Create a clean `vMAJOR.MINOR.PATCH` tag whose numeric base matches `VERSION`.
Prerelease tags such as `v0.4.0-rc.1` are accepted when their numeric base still
matches. CI fetches complete tag history, validates the tag, and sends the same
canonical version to the server, desktop client, and container image. Invalid,
ambiguous, dirty, or mismatched release states fail before artifact publication.

After a stable release, change `VERSION` to the next intended release line and
update the synchronized package declarations. `go run ./cmd/version -check`
enforces the root version, Go fallback, npm/lock metadata, local Go-module
placeholder, and Wails product version as one set.

Wails desktop package metadata intentionally uses the stable three-part release
line. Windows and macOS both impose numeric bundle-version constraints that
cannot carry SemVer commit metadata. The in-app version, logs, health endpoint,
metrics, updater, and server-info response use the full canonical identity.
