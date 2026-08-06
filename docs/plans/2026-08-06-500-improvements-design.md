# Implementing the 500-improvement program

Date: 2026-08-06
Status: approved for implementation
Scope: items 1 through 500 from the repository audit

## Outcome

VoicX will move from a strong but uneven development baseline to a release-ready,
observable, accessible, and maintainable communications platform. Every audit item
must finish in one of three states:

- `done`: implemented and verified;
- `superseded`: a mutually exclusive item was satisfied by the safer or more
  complete alternative, with the decision recorded here;
- `blocked`: completion requires an external credential, production dataset,
  signing key, platform entitlement, or other authority that cannot safely be
  invented. A blocked item must include a tested local seam and exact operator step.

No item may disappear from the program merely because it is large.

## Delivery strategy

The program lands as small, test-gated commits on the existing `dev` branch.
Structural and behavioral changes remain separate. File-disjoint work may proceed
in parallel; changes touching the same subsystem remain sequential. The order is:

| Milestone | Items | Primary outcome | Initial state |
| --- | ---: | --- | --- |
| M1 | 1-100 | Security, configuration, lint, TLS, updater, hygiene | in progress |
| M2 | 101-200 | Tests, fuzzing, coverage, race checks, benchmarks | pending |
| M3 | 201-250 | Go structure, lifecycle, errors, API clarity | pending |
| M4 | 251-300 | Tenancy, persistence, migrations, retention, keys | pending |
| M5 | 301-350 | Network admission, WebRTC resilience, telemetry | pending |
| M6 | 351-400 | File transfer and protocol evolution | pending |
| M7 | 401-450 | Frontend modularity, safety, accessibility, UX | pending |
| M8 | 451-490 | Localization, identity, moderation, media features | pending |
| M9 | 491-500 | Supply chain, releases, SLOs, tracing, ADRs | pending |
| M10 | 1-500 | Final cross-platform verification and graph refresh | pending |

The implementation ledger is the audit numbering itself. Commits and this table
record completed ranges; exceptions are listed in the decision log below. This
keeps the source of truth compact while preserving one-to-one numbering.

## Architecture

Security and correctness foundations land before feature growth. Configuration is
validated at startup and exposes a redacted operational view. External input is
bounded at transport, decoding, database-conversion, and filesystem boundaries.
Long-running components share an explicit lifecycle: start, readiness, drain,
shutdown, and joined error reporting. Database changes preserve explicit SQL and
transaction boundaries. Network/media work adds admission control and telemetry
before adaptive behavior. Frontend work first introduces safe rendering and module
boundaries, then accessibility and product features.

Large source-file splits are behavior-preserving commits backed by characterization
tests. Cross-package moves retain compatibility aliases until callers migrate.
Protocol evolution is additive and version-negotiated; existing clients receive a
clear unsupported-version response rather than undefined behavior.

## Data and trust boundaries

Untrusted data enters through TCP frames, UDP datagrams, gRPC, ServerQuery,
WebRTC/SDP, file uploads, configuration, release metadata, and persisted rows.
Each boundary validates size, syntax, numeric range, identity, authorization, and
resource budget before work or allocation. Filesystem access is rooted and rejects
traversal and symlink escape. Database identifiers are converted only after range
checks. Secrets are never present in summaries, metrics, protocol errors, or stable
log messages.

Tenant identity becomes explicit before the multi-tenant claim is retained. Data
retention, export, deletion, and cryptographic erasure share one documented model.
Key and envelope versions remain durable so interrupted rotations can resume.

## Errors, operations, and observability

Expected domain failures use stable codes; internal errors retain wrapped context
and are translated at system boundaries. An error is logged or returned, not both.
Best-effort work such as auditing gains explicit failure metrics and a durable path
where the product promise requires it. Every service publishes bounded-cardinality
health, saturation, latency, drop, and shutdown signals.

Production operations include secure defaults, migration locking, restore drills,
SBOMs, signed artifacts, provenance, SLOs, traces, and authenticated diagnostics.
When completion needs operator-owned material such as an update signing private
key, code and CI will implement verification and fail closed; only secret creation
and custody remain an operator step.

## Verification gates

Every behavioral batch must pass targeted tests, `go test ./...`, `go vet ./...`,
and relevant client/frontend tests. Concurrency batches also pass `go test -race`.
Security batches pass `govulncheck` and `gosec` with reviewed, narrow suppressions.
Frontend batches pass build, unit, Playwright, lint, and accessibility checks.
Performance changes require before/after benchmarks. Release completion requires
clean git status, generated-code reproducibility, container build, migration test,
backup restore test, and an updated knowledge graph.

## Decision log

- 2026-08-06: The user approved implementation of all 500 items.
- 2026-08-06: Use staged, independently verifiable commits rather than a monolith.
- 2026-08-06: Conditional alternatives resolve to the safer complete behavior;
  documentation is then corrected to match what is actually delivered.
- 2026-08-06: External secrets are never generated or committed on the user's
  behalf. Verification paths fail closed until operator-owned keys are configured.

