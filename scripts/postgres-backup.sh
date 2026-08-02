#!/bin/sh
set -eu

# Backups deliberately live outside PostgreSQL's data volume. The destination
# can be a bind mount or a dedicated volume, and only the newest seven daily
# archives are retained locally.
backup_dir="${BACKUP_DIR:-/backups/postgres}"
retention_days="${BACKUP_RETENTION_DAYS:-7}"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
database="${POSTGRES_DB:-voicx}"
mkdir -p "$backup_dir"

target="$backup_dir/${database}-${timestamp}.dump.gz"
pg_dump --format=custom --no-owner --no-privileges \
  --dbname="${DATABASE_URL:?DATABASE_URL is required}" | gzip -9 > "$target.tmp"
mv "$target.tmp" "$target"

find "$backup_dir" -type f -name "${database}-*.dump.gz" -mtime "+$retention_days" -delete

# Optional object-storage upload. Configure rclone and set BACKUP_REMOTE to a
# destination such as s3:voicx-backups/production.
if [ -n "${BACKUP_REMOTE:-}" ]; then
  rclone copyto "$target" "${BACKUP_REMOTE%/}/$(basename "$target")"
fi
