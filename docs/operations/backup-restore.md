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
