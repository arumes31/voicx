# VoicX incident runbook

Use this runbook for production availability, integrity, or confidentiality
incidents. Preserve evidence before changing state, and record every command and
timestamp in UTC.

## First ten minutes

1. Declare an incident owner and a separate communications owner. Record the
   affected deployment, start time, symptoms, and most recent change.
2. Stop deployments and migrations. Do not restart every dependency at once.
3. From the server network namespace, capture the operational endpoints:

   ```sh
   curl --fail --silent --show-error http://127.0.0.1:12337/healthz
   curl --fail --silent --show-error http://127.0.0.1:12337/readyz
   curl --fail --silent --show-error http://127.0.0.1:12337/api/v1/schema/version
   curl --fail --silent --show-error http://127.0.0.1:12337/metrics > voicx-metrics.txt
   ```

   `/metrics` and schema diagnostics are loopback-only by default. Keep them
   private; metrics can reveal capacity and traffic patterns.
4. Capture application logs, container/runtime events, PostgreSQL health and
   replication state, disk usage, and the deployed image digest. Do not paste
   credentials, bearer tokens, chat keys, or message bodies into the incident
   channel.
5. Run one authenticated synthetic control/chat probe. Run media and file probes
   only if they cannot worsen saturation.

## Triage map

| Signal | Likely area | First safe action |
| --- | --- | --- |
| `/healthz` fails | process/listener/runtime | inspect the service exit and resource limits; roll back the last release if correlated |
| `/healthz` succeeds, `/readyz` fails | PostgreSQL or migration | inspect DB reachability, TLS, pool saturation, locks, and schema version |
| rising `voicx_db_pool_wait_*` | pool exhaustion/slow query | identify blocked and long-running queries; scale only after ruling out a lock storm |
| rising UDP drops with healthy control | media admission/network | reduce load, inspect CPU/socket pressure and TURN health, preserve control traffic |
| event-bus drops only | diagnostic consumers | disconnect slow consumers; this stream must not impair the product path |
| file errors only | file root, permissions, disk, TLS | stop new uploads if integrity is uncertain; verify free space and root confinement |
| authentication failures after deploy | config, clock, TLS, schema | compare redacted config and certificate fingerprint; do not disable verification |

## Recovery order

1. Remove or rate-limit the harmful input when an attack or runaway client is
   confirmed.
2. Roll back the application to a known image digest if the schema remains
   backward compatible. Never reverse a database migration by hand during an
   incident.
3. Restore a database only after isolating the broken deployment and preserving
   WAL/log evidence. Restore into a new database first, validate migration ledger
   checksums and required indexes, then switch traffic deliberately.
4. Rotate credentials or signing/chat keys only when compromise is suspected or
   confirmed. Follow the documented overlapping-key rotation procedure; deleting
   an old key prematurely can make stored data unrecoverable.
5. Re-enable traffic gradually and watch every user-journey SLI plus DB pool,
   event-bus, UDP-drop, and file-transfer metrics.

## Integrity and security incidents

- Treat an unexpected migration checksum, invalid index, asset digest mismatch,
  path escape, or plaintext secret in logs as an integrity incident.
- Quarantine suspect files and snapshots; do not overwrite them while testing a
  repair.
- Revoke exposed tokens and certificates through their owning system. Never
  commit replacement secrets to this repository.
- Preserve original logs and artifacts read-only, calculate cryptographic
  digests, and restrict access to responders who need them.

## Closure

Before resolving the incident, confirm every affected SLI has recovered from all
probe locations, drains and shutdowns are clean, migrations are consistent, and
the working deployment matches a recorded image digest. Within two business days,
record the timeline, root cause, contributing controls, user impact, detection
gap, and owners/dates for corrective actions. Add a regression test or executable
check wherever the failure can be reproduced safely.
