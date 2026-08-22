# Hardening 200 Design

Date: 2026-08-21
Status: approved for implementation by the user's direct request
Source: `temp/quickwins-200.md` / supplied audit attachment

## Objective

Reconcile the 200 reported security, correctness, API, test, CI, and operations
findings with the current `dev3` tree, implement every still-valid finding in
reviewable batches, and preserve compatibility where the proposed remedy is
unsafe or stale.

The implementation loop for every batch is:

1. Revalidate each finding against the current source.
2. Add or strengthen the behavioral safety net.
3. Make one coherent behavioral change set.
4. Run targeted tests and proportional race/security checks.
5. Have an independent Sol reviewer inspect the diff and evidence.
6. Return all review findings to the Terra implementer for correction.

## Audit corrections

- #5 is already enforced by migration 023; only stale comments/query behavior
  and regression coverage may need cleanup.
- #12 already validates decoded X25519 keys at exactly 32 bytes.
- #42 is rejected: an unmeasured `sync.Pool` can retain user file data and is
  unnecessary once file-transfer concurrency is bounded.
- #55 requires per-reply-type serialization, not removal of serialization,
  because pending requests are keyed by reply type.
- #57 already deduplicates scope-key pulls; bound only the remaining DM work.
- #61 can stream verification, but the legacy base64-returning download API
  inherently buffers and must instead be capped/deprecated.
- #71 retains the Wails minimization poll but makes its lifecycle cancellable.
- #100-109 preserve protobuf field numbers and use deprecation/documentation
  rather than destructive field reuse.
- #110 should surface data loss rather than silently hide corrupt rows.
- #140 adds checked-in corpus files only for real regression inputs.
- #153 aligns and documents separate Docker/Compose health checks.
- #167 does not hard-code a transient working branch as repository policy.
- #170 keeps gosec in the standalone reusable security gate.
- #188 keeps optional Redis non-fatal to readiness while exposing degradation.
- #198 tunes and documents sampling rather than adding a logging-to-metrics dependency.

## Implementation batches

1. Authentication perimeter and bounded failure state (#1-9, #37, #180).
2. Certificates, crypto, secure config, and file ingress (#10-23, #43-44).
3. Server context, error, and lifecycle correctness (#24-41).
4. gRPC shutdown and recovery infrastructure (#24, #38, #114-116, #192,
   #196).
5. Client connection-manager concurrency (#45-57).
6. Client persistence, transfers, and deterministic UI state (#58-74).
7. Frontend resilience and accessibility cleanup (#75-99).
8. Protobuf contract stabilization and generated-code gates (#100-124).
9. Missing behavioral tests (#125-135).
10. Fuzzing and deterministic test repair (#136-152).
11. Container and secret-handling operations (#153-159).
12. CI/security workflow consolidation (#160-170).
13. Local build and release tooling (#171-176).
14. Metrics and opt-in local profiling (#177-187).
15. Readiness composition (#188-191).
16. Process shutdown, goroutine recovery, and logging (#193-200).

## Batch 1 design

Batch 1 creates a bounded, expiry-pruned login failure limiter with an injected
clock. It scopes failures by both transport source and normalized principal:
TCP control uses remote IP plus identity; loopback gRPC uses the principal so a
single local process cannot lock out every administrator. ServerQuery reuses
the same bounded semantics instead of permanent per-IP entries.

Credentials are rejected before expensive work: passwords are capped at 256
bytes in hash and verify paths, Basic metadata is capped before base64 decode,
and banned IPs are checked before server-password Argon2 verification. New
account registration requires at least 12 bytes, while existing short hashes
remain valid for login compatibility. Unknown accounts verify against a
precomputed syntactically valid dummy Argon2 hash to reduce enumeration timing
differences.

Authentication failures increment a low-cardinality counter labelled only by
transport and stable reason. IP addresses, user IDs, nicknames, and raw errors
must never become metric labels.

Tests cover limiter threshold/expiry/eviction/reset/concurrency, principal
isolation, banned-IP ordering, oversized credentials, dummy-hash use, nickname
uniqueness behavior, and metric cardinality. Concurrency changes receive
targeted `go test -race` coverage.

## Compatibility and safety rules

- Never reuse or renumber protobuf fields.
- Do not silently weaken existing authentication or TLS defaults.
- Do not emit secrets, credentials, identities, addresses, paths, or raw
  unbounded errors as metric labels.
- Preserve the user's untracked `temp/quickwins-200.md` unchanged.
- Keep this design document local and uncommitted.
- Treat generated files, schema/default changes, deletions, and public API
  changes as explicit batch boundaries with dedicated verification.

## Verification gates

Each Go batch runs targeted tests, root `go test ./...`, client `go test ./...`,
and targeted race tests. Frontend batches run lint, unit coverage, build, and
targeted Playwright checks. Proto batches run Buf lint/breaking/generation and a
clean-tree drift check. Final integration additionally runs golangci-lint,
gosec, govulncheck, Linux/Windows CI, and Docker Compose smoke tests for auth,
readiness, Redis authentication, file transfer, and graceful shutdown.
