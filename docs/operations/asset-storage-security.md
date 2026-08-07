# Asset storage security

VoicX treats `FileRoot` as a security boundary. Run exactly one writer process
per `FileRoot`, and grant write access only to the VoicX service identity and
trusted administrators. Asset file names are confined with `os.Root`, but no
cross-process lock coordinates extension replacement or group-icon metadata.

## Windows ACL requirement

On Windows, Go's `os.Chmod(0700/0600)` calls do not rewrite NTFS DACLs. Before
starting VoicX, provision `FileRoot` with a restricted **inheritable** DACL so
new directories and files inherit the same protection. Verify the effective
permissions as the service identity. VoicX emits `ASSET STORAGE SECURITY
LIMITATION` error-level log entries on Windows because the portable runtime
cannot prove that the DACL is restricted; those warnings must be treated as a
deployment gate.

On POSIX systems, VoicX normalizes the root and asset directories to `0700` and
regular asset files to `0600`. Parent-directory permissions and host/container
mount policy remain the operator's responsibility.

## Group-icon recovery journals

Group-icon changes use a canonical recovery-baseline journal. On a valid admin
upload, database/disk drift is normalized to either the one valid
metadata-linked image or no icon, but only after that baseline is durable.
Recovery validates canonical group metadata, image extensions, complete
static-image decodes, per-image hashes, and a checksum over the whole journal
before restoring anything. The checksum detects torn writes and inconsistent
manual edits; it is not authentication.

Server-group deletion is serialized with icon reads and writes. A strict
`.group-icon-delete-*.json` tombstone is durable before the database delete and
is removed only after all known icon variants and any update journal are gone.
At startup, a tombstone for a live group is canceled without touching its icon;
a tombstone for a missing group completes cleanup. Group-icon reads also require
a live authoritative group row and serve exactly the file named by `Group.Icon`,
so an orphan or alternate extension is never exposed as a fallback.

Uploaded PNG, GIF, and WebP containers must be static. APNG animation chunks,
multi-frame GIFs, and animated WebP feature flags are rejected before a full
decode.

There is no configured operator secret from which VoicX could derive a sound
MAC, so an attacker who can rewrite both `FileRoot` and the database is already
inside this trust boundary and can recompute the checksum.

Never delete a `.group-icon-txn-*.json` or `.group-icon-delete-*.json` file to
make startup succeed. Preserve the recovery file and matching database state
for investigation, correct the storage or permissions problem, and restart
recovery.

## Durability and power loss

Asset files are written to a temporary file, synced, and atomically renamed.
Directory metadata is synced where the platform supports it. Windows does not
provide the directory-sync behavior used by this path, so recovery across a
sudden power loss is best-effort: a rename or journal deletion may not be
durable even after the preceding file sync. Use resilient storage, clean
shutdowns, and backups of both `FileRoot` and the database.
