#!/bin/sh
set -eu
umask 077

# Backups deliberately live outside PostgreSQL's data volume. The destination
# can be a bind mount or a dedicated volume, and only the newest seven daily
# archives are retained locally.
backup_dir="${BACKUP_DIR:-/backups/postgres}"
retention_days="${BACKUP_RETENTION_DAYS:-7}"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
database="${PGDATABASE:-voicx}"
database_id=$(printf '%s' "$database" | sha256sum | awk '{print substr($1, 1, 16)}')
archive_prefix="postgres-${database_id}"
mkdir -p "$backup_dir"

target="$backup_dir/${archive_prefix}-${timestamp}.dump.gz"
dump_tmp="$target.dump.tmp"
compressed_tmp="$target.tmp"
child_pid=
cleanup() {
	rm -f "$dump_tmp" "$compressed_tmp"
}
stop() {
	if [ -n "$child_pid" ]; then
		kill -TERM "$child_pid" 2>/dev/null || true
		wait "$child_pid" 2>/dev/null || true
	fi
	cleanup
	trap - EXIT HUP INT TERM
	exit 0
}
trap cleanup EXIT
trap stop HUP INT TERM

run_child() {
	"$@" &
	child_pid=$!
	if ! wait "$child_pid"; then
		child_pid=
		return 1
	fi
	child_pid=
}

gzip_file() {
	gzip -9 -c "$1" > "$2" &
	child_pid=$!
	if ! wait "$child_pid"; then
		child_pid=
		return 1
	fi
	child_pid=
}

run_child pg_dump --format=custom --no-owner --no-privileges \
	--host="${PGHOST:?PGHOST is required}" \
	--port="${PGPORT:?PGPORT is required}" \
	--username="${PGUSER:?PGUSER is required}" \
	--dbname="$database" \
	--file="$dump_tmp"
gzip_file "$dump_tmp" "$compressed_tmp"
mv "$compressed_tmp" "$target"
rm -f "$dump_tmp"

find "$backup_dir" -type f -name "${archive_prefix}-*.dump.gz" -mtime "+$retention_days" -delete

# Optional object-storage upload. Configure rclone and set BACKUP_REMOTE to a
# destination such as s3:voicx-backups/production.
if [ -n "${BACKUP_REMOTE:-}" ]; then
	run_child rclone copyto "$target" "${BACKUP_REMOTE%/}/$(basename "$target")"
fi

trap - EXIT HUP INT TERM
