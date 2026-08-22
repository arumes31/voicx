# Backup and restore drill

Back up PostgreSQL and the configured file/asset roots as one recovery point.
A database-only backup can restore metadata that references missing content;
a filesystem-only backup can restore content with no authorization or ownership
record. Keep encryption keys and TLS/update-signing keys in the deployment's
secret backup system, never in the archive described here.

## Create a recovery point

1. Record the deployed image digest, VoicX version, UTC timestamp, database
   server version, and redacted configuration.
2. Quiesce writes or use a storage/database snapshot mechanism that provides a
   documented common point in time. Do not copy a live mutable directory with a
   best-effort recursive file command.
3. Create a PostgreSQL custom-format dump with `pg_dump --format=custom` and
   capture the file root plus server/channel/group asset roots from the same
   snapshot boundary.
4. Hash every artifact with SHA-256, encrypt it with the operator-owned backup
   key, upload it to immutable storage, and verify the uploaded size and digest.
5. Record retention and deletion dates. A successful upload is not a verified
   backup until the restore drill below passes.

## Restore drill

Run this at least quarterly and before a migration or storage-layout release.
Use an isolated network and new database, file roots, credentials, and ports.

1. Verify archive signatures/digests before decrypting or extracting. Reject
   absolute paths, `..` components, links, devices, and unexpected owners while
   extracting filesystem archives.
2. Restore the PostgreSQL dump into an empty database owned by a non-superuser.
3. Restore file and asset roots to new empty directories with restrictive
   ownership. Do not overlay the production directories.
   VoicX treats the restored channel table as authoritative and removes
   canonical numeric file directories for channels absent from it at startup.
   Never start against a filesystem snapshot paired with an older or incomplete
   database restore.
4. Start the exact backed-up VoicX image against the isolated restore. Confirm
   `/readyz` and `/api/v1/schema/version`, then stop it cleanly.
5. Start the candidate image. Its migration runner must accept every ledger
   checksum and required index before readiness succeeds.
6. With non-administrator synthetic accounts, verify authentication, channel
   membership, encrypted chat history, one upload/download digest, and every
   server/channel/group asset class. Confirm a cross-channel file request and a
   path traversal request are denied.
7. Compare row counts and sampled content digests with the recovery-point
   manifest. Scan logs for migration, journal-recovery, permission, and missing
   file errors.
8. Destroy the isolated credentials and restored plaintext after recording the
   drill result. Keep only the timestamp, recovery-point identifier, versions,
   duration, checks performed, and remediation owners.

## Compose backup container

The optional `backup` Compose profile runs the dedicated `voicx-backup` image.
It connects with `PGHOST`, `PGPORT`, `PGUSER`, `PGDATABASE`, and `PGSSLMODE`;
the password is supplied through `POSTGRES_PASSWORD` or
`POSTGRES_PASSWORD_FILE`, never a connection URL or `pg_dump` argument. The
entrypoint writes a mode-600 `.pgpass` file, drops to the PostgreSQL user, and
the scheduler handles `TERM` cleanly. For a one-shot job, do not set
`BACKUP_RUN_ONCE=1` on the long-running service (its normal restart policy
would restart it). Use:

```sh
docker compose --profile backup run --rm --no-deps -e BACKUP_RUN_ONCE=1 postgres-backup
```

`BACKUP_RUN_ONCE` remains a harness/test seam. `BACKUP_RESTART_POLICY` defaults
to `unless-stopped` for the scheduled service and can be set to `no` for a
manually managed job.

`*_FILE` values are bind-mounted read-only. On Linux, keep the host secret
directory root-owned mode `0700` and individual files root-owned mode `0444`.
Compose mounts only the selected file, never its parent, so non-root containers
can read the bound file without host users being able to traverse the source
directory. Keep sources outside the repository where possible; only
`docker/secrets/.empty` is tracked.

For a rootless Docker daemon, make the `0700` source directory owned by the
daemon/operator account instead of root; retain the same direct-file mounts and
keep the directory inaccessible to unrelated host users.

If VoicX uses `VOICX_COMPOSE_DATABASE_URL` or its `_FILE` form for an external
database, the profile fails closed until `BACKUP_PGHOST`, `BACKUP_PGPORT`,
`BACKUP_PGUSER`, `BACKUP_PGDATABASE`, `BACKUP_PGSSLMODE`, and exactly one of
`BACKUP_POSTGRES_PASSWORD` / `BACKUP_POSTGRES_PASSWORD_FILE` are provided.
These discrete settings intentionally prevent a password-bearing URL from
being passed to `pg_dump`. The profile does not depend on the internal
PostgreSQL service in this mode. External backups require `require`,
`verify-ca`, or `verify-full`; mount `BACKUP_PGSSLROOTCERT_FILE`,
`BACKUP_PGSSLCERT_FILE`, and `BACKUP_PGSSLKEY_FILE` when libpq needs custom CA
or client credentials. For an object remote, set `RCLONE_CONFIG_FILE`; it is
mounted as `RCLONE_CONFIG` only in the backup container.

Use the separate external topology whenever the application URL is external:

```sh
docker compose -f docker-compose.yml -f docker-compose.external-db.yml --profile backup up -d
```

It removes VoicX's internal PostgreSQL health dependency and leaves the bundled
database inactive, while retaining Redis and the optional backup profile.

## Recovery objectives

Set deployment-specific RPO and RTO values beside the service SLOs. Measure RPO
from the newest verified common recovery point and RTO from restore declaration
until authenticated synthetic probes pass. If either target is missed, the drill
is failed even when the process eventually starts.

## Fail-closed conditions

Do not switch production traffic to a restore when a migration checksum differs,
a required index is missing or invalid, an asset recovery journal cannot be
reconciled, startup orphan cleanup fails, content digests differ, key material is
unavailable, or the restore requires hand-editing the migration ledger. Preserve
the failed restore for forensics and escalate through the incident runbook.
