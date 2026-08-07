# VoicX service-level objectives

This document defines the production objectives for one VoicX deployment. It
does not turn process uptime into user availability: control, chat, media, and
file transfer are measured independently with authenticated synthetic clients.

## Objectives

The compliance window is a rolling 30 days. Planned maintenance consumes the
same error budget as an unplanned outage.

| User journey | Good event | Objective | 30-day error budget |
| --- | --- | ---: | ---: |
| Control session | TLS connection, authentication, and initial snapshot finish within 2 seconds | 99.90% | 43m 12s equivalent |
| Channel chat | An authenticated message is accepted and observed by a second member within 1 second | 99.90% | 43m 12s equivalent |
| Voice setup | Two authenticated members establish media and receive RTP within 5 seconds | 99.50% | 3h 36m equivalent |
| File transfer | A 1 MiB upload and download complete with matching digest within 30 seconds | 99.50% | 3h 36m equivalent |

Latency objectives apply only to successful events:

- control authentication: p95 below 500 ms and p99 below 1.5 s;
- channel-chat delivery: p95 below 250 ms and p99 below 1 s;
- voice setup: p95 below 3 s and p99 below 5 s;
- one-MiB file transfer on the deployment's reference network: p95 below 10 s.

## Measurement

Run the probes from at least two failure domains. A probe account must have only
the permissions needed for its journey and must never be an administrator.
Exclude probe traffic from product analytics, but not from reliability metrics.

The authoritative availability ratio is:

```text
good probe attempts / all valid probe attempts
```

Timeouts, protocol errors, digest mismatches, and missing observations are bad
events. Probe-runner or network failures outside the VoicX deployment are
labelled `invalid` and audited; they are not silently counted as good.

Prometheus process metrics supplement, but do not replace, the synthetic SLIs:

- `up` and `/readyz` distinguish process and dependency health;
- `voicx_udp_packets_dropped_total` detects media admission pressure;
- `voicx_db_pool_*` exposes connection-pool saturation;
- `voicx_eventbus_*` exposes diagnostic-stream drops;
- `voicx_file_transfers_total{result="error"}` corroborates file-probe failures.

Labels must stay bounded. Never add user, channel, filename, IP address, or
request identifiers to a Prometheus label.

## Alert policy

Page on both fast and slow error-budget burn:

- fast burn: 14.4 times the allowed rate for 5 minutes and 1 hour;
- slow burn: 6 times the allowed rate for 30 minutes and 6 hours;
- immediate infrastructure page: `/readyz` fails from every probe location for
  5 minutes, PostgreSQL pool use reaches its configured maximum, or the process
  repeatedly restarts.

Ticket-only conditions include a single probe-location failure, sustained UDP
drops below the voice SLO threshold, and non-growing event-stream drops.

## Release and review policy

- Stop routine releases when a journey has consumed 50% of its monthly budget.
- At 75%, permit only reliability, security, or rollback changes.
- At 100%, freeze feature releases until the owner records a remediation plan.
- Review targets quarterly and after every severity-1 incident. Tighten a target
  only after instrumentation shows the current target is met consistently.
