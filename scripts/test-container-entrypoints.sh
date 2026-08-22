#!/bin/sh
# Bounded, image-independent checks for secret source and entrypoint behavior.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/voicx-entrypoints.XXXXXX")
scheduler_pid=
cleanup() {
	if [ -n "$scheduler_pid" ]; then
		kill -TERM "$scheduler_pid" 2>/dev/null || true
		wait "$scheduler_pid" 2>/dev/null || true
	fi
	rm -rf "$tmpdir"
}
trap cleanup EXIT HUP INT TERM

mode_checks=1
mode_probe="$tmpdir/mode-probe"
: > "$mode_probe"
chmod 600 "$mode_probe"
if [ "$(stat -c '%a' "$mode_probe" 2>/dev/null || stat -f '%Lp' "$mode_probe")" != 600 ]; then
	# Git Bash on a Windows-mounted workspace cannot represent POSIX modes. The
	# same assertions remain mandatory on Linux and inside the container images.
	mode_checks=0
	echo 'test-container-entrypoints: POSIX mode assertions unavailable on this filesystem' >&2
fi

fail() {
	echo "test-container-entrypoints: $*" >&2
	exit 1
}

assert_mode() {
	[ "$mode_checks" -eq 1 ] || return 0
	actual=$(stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1")
	[ "$actual" = "$2" ] || fail "expected mode $2 on $1, got $actual"
}

secret_file="$tmpdir/secret"
printf 'from-file\n' > "$secret_file"

# Linux bind mounts preserve source ownership and mode. Verify the equivalent
# mounted target through a traversable container-side parent: the private host
# source parent is deliberately not mounted. BusyBox setpriv lacks these flags,
# so retain this as an optional Linux capability check.
if [ "$(id -u)" -eq 0 ] && command -v setpriv >/dev/null 2>&1 && setpriv --help 2>&1 | grep -q -- '--reuid'; then
	chmod 755 "$tmpdir"
	bind_probe_dir="$tmpdir/bind-probe"
	mkdir "$bind_probe_dir"
	chmod 755 "$bind_probe_dir"
	bind_probe_file="$bind_probe_dir/secret"
	printf 'probe\n' > "$bind_probe_file"
	chmod 444 "$bind_probe_file"
	chown 0:0 "$bind_probe_file"
	setpriv --reuid=10001 --regid=10001 --clear-groups test -r "$bind_probe_file" || fail 'UID 10001 cannot read a mounted 0444 secret source'
fi

if SECRET=plain SECRET_FILE="$secret_file" sh -c '. "$1"; secret_env_load SECRET required' sh "$repo_root/scripts/secret-env.sh"; then
	fail 'accepted conflicting secret sources'
fi
if SECRET= sh -c '. "$1"; secret_env_load SECRET required' sh "$repo_root/scripts/secret-env.sh"; then
	fail 'accepted an empty required secret'
fi
if SECRET_FILE="$tmpdir" sh -c '. "$1"; secret_env_load SECRET required' sh "$repo_root/scripts/secret-env.sh"; then
	fail 'accepted a non-regular secret file'
fi
if SECRET='line
break' sh -c '. "$1"; secret_env_require_single_line SECRET' sh "$repo_root/scripts/secret-env.sh"; then
	fail 'accepted a multiline line-oriented secret'
fi

argv_file="$tmpdir/voicx.argv"
cat > "$tmpdir/capture-argv" <<'EOF'
#!/bin/sh
printf '%s\n' "$0" "$@" > "$ARGV_FILE"
EOF
chmod +x "$tmpdir/capture-argv"
ARGV_FILE="$argv_file" POSTGRES_PASSWORD='not-in-argv' POSTGRES_HOST=db \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" \
	"$repo_root/scripts/voicx-entrypoint.sh" "$tmpdir/capture-argv" serve
! grep -q 'not-in-argv' "$argv_file" || fail 'voicx secret appeared in argv'
if POSTGRES_HOST=db VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" \
	"$repo_root/scripts/voicx-entrypoint.sh" /bin/true; then
	fail 'accepted missing PostgreSQL password while generating a URL'
fi
VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" "$repo_root/scripts/voicx-entrypoint.sh" /bin/true
ARGV_FILE="$argv_file" VOICX_SERVER_BIN="$tmpdir/capture-argv" VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" \
	"$repo_root/scripts/voicx-entrypoint.sh" --version
head -n 1 "$argv_file" | grep -qx "$tmpdir/capture-argv" || fail 'voicx binary flags were not prefixed with the server binary'
sed -n '2p' "$argv_file" | grep -qx -- '--version' || fail 'voicx binary flag was not preserved'

if [ "$(id -u)" -ne 0 ]; then
pgpass_file="$tmpdir/pgpass-dir/.pgpass"
cat > "$tmpdir/check-pgpass" <<'EOF'
#!/bin/sh
test -n "$PGPASSFILE"
test ! "${POSTGRES_PASSWORD+x}"
EOF
chmod +x "$tmpdir/check-pgpass"
POSTGRES_PASSWORD='pa:ss\word' PGPASSFILE_PATH="$pgpass_file" \
	PGPASS_RUNTIME_DIR="$tmpdir/pgpass-dir" BACKUP_DIR="$tmpdir/backups" \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" \
	"$repo_root/scripts/postgres-backup-entrypoint.sh" "$tmpdir/check-pgpass"
assert_mode "$pgpass_file" 600
expected_pgpass='postgres:5432:voicx:voicx:pa\:ss\\word'
[ "$(cat "$pgpass_file")" = "$expected_pgpass" ] || fail '.pgpass escaping mismatch'
if POSTGRES_PASSWORD='bad
password' PGPASS_RUNTIME_DIR="$tmpdir/pgpass-invalid" BACKUP_DIR="$tmpdir/backups-invalid" \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" \
	"$repo_root/scripts/postgres-backup-entrypoint.sh" /bin/true; then
	fail 'accepted a multiline .pgpass password'
fi
if POSTGRES_PASSWORD=good PGHOST='bad
host' PGPASS_RUNTIME_DIR="$tmpdir/pgpass-invalid-host" BACKUP_DIR="$tmpdir/backups-invalid-host" \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" "$repo_root/scripts/postgres-backup-entrypoint.sh" /bin/true; then
	fail 'accepted a multiline .pgpass host'
fi
if POSTGRES_PASSWORD=good PGPASS_RUNTIME_DIR='/tmp/voicx-pgpass/../../etc' BACKUP_DIR="$tmpdir/backups-invalid-path" \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" "$repo_root/scripts/postgres-backup-entrypoint.sh" /bin/true; then
	fail 'accepted a dot-component escape from the .pgpass runtime directory'
fi
if BACKUP_EXTERNAL_DATABASE=1 POSTGRES_PASSWORD=ignored \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" "$repo_root/scripts/postgres-backup-entrypoint.sh" /bin/true; then
	fail 'accepted an external database without dedicated backup settings'
fi
external_pgpass_dir="$tmpdir/external-pgpass"
BACKUP_EXTERNAL_DATABASE=1 BACKUP_PGHOST=external-db BACKUP_PGPORT=6543 BACKUP_PGUSER=backup \
	BACKUP_PGDATABASE=archive BACKUP_PGSSLMODE=verify-full BACKUP_POSTGRES_PASSWORD=backup-secret \
	PGPASS_RUNTIME_DIR="$external_pgpass_dir" PGPASSFILE_PATH="$external_pgpass_dir/.pgpass" BACKUP_DIR="$tmpdir/external-backups" \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" "$repo_root/scripts/postgres-backup-entrypoint.sh" "$tmpdir/check-pgpass"
grep -q '^external-db:6543:archive:backup:' "$external_pgpass_dir/.pgpass" || fail 'external backup did not use dedicated libpq settings'
else
	echo 'test-container-entrypoints: root skips generic backup entrypoint cases; run scripts/test-backup-image.sh for gosu coverage' >&2
fi

redis_config="$tmpdir/redis.conf"
cat > "$tmpdir/capture-redis" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "$REDIS_ARGV_FILE"
EOF
chmod +x "$tmpdir/capture-redis"
REDIS_PASSWORD='redis-secret' REDIS_RUNTIME_DIR="$tmpdir/redis-runtime" REDIS_CONFIG_FILE="$redis_config" \
	REDIS_ENTRYPOINT="$tmpdir/capture-redis" REDIS_ARGV_FILE="$tmpdir/redis.argv" \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" "$repo_root/scripts/redis-entrypoint.sh" redis-server --save ''
assert_mode "$redis_config" 600
! grep -q 'redis-secret' "$redis_config" || fail 'Redis config retained plaintext password'
grep -q '^user default on #[0-9a-f][0-9a-f]* ~\* &\* +@all$' "$redis_config" || fail 'Redis config lacks ACL hash'
! grep -q 'redis-secret' "$tmpdir/redis.argv" || fail 'Redis secret appeared in argv'
[ "$(grep -cx 'redis-server' "$tmpdir/redis.argv")" -eq 1 ] || fail 'Redis command duplicated redis-server'

turn_config_dir="$tmpdir/turn"
cat > "$tmpdir/capture-turn" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "$TURN_ARGV_FILE"
EOF
chmod +x "$tmpdir/capture-turn"
TURN_SECRET='turn-secret' TURN_CONFIG_DIR="$turn_config_dir" TURN_SERVER_BIN="$tmpdir/capture-turn" \
	TURN_ARGV_FILE="$tmpdir/turn.argv" VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" \
	"$repo_root/scripts/coturn-entrypoint.sh" --listening-port=12340
assert_mode "$turn_config_dir/turnserver.conf" 600
grep -q '^static-auth-secret=turn-secret$' "$turn_config_dir/turnserver.conf" || fail 'TURN config missing secret'
! grep -q 'turn-secret' "$tmpdir/turn.argv" || fail 'TURN secret appeared in argv'
if TURN_SECRET='bad
secret' TURN_CONFIG_DIR="$tmpdir/turn-invalid" TURN_SERVER_BIN="$tmpdir/capture-turn" \
	VOICX_SECRET_ENV_LIB="$repo_root/scripts/secret-env.sh" "$repo_root/scripts/coturn-entrypoint.sh"; then
	fail 'accepted a multiline TURN secret'
fi

backup_marker="$tmpdir/backup-ran"
cat > "$tmpdir/backup-command" <<'EOF'
#!/bin/sh
: > "$BACKUP_MARKER"
EOF
chmod +x "$tmpdir/backup-command"
BACKUP_RUN_ONCE=1 BACKUP_COMMAND="$tmpdir/backup-command" BACKUP_MARKER="$backup_marker" \
	"$repo_root/scripts/postgres-backup-scheduler.sh"
[ -f "$backup_marker" ] || fail 'run-once scheduler did not run backup'

mkdir -p "$tmpdir/fakebin"
cat > "$tmpdir/fakebin/pg_dump" <<'EOF'
#!/bin/sh
trap ': > "$BACKUP_WORK_TERM_MARKER"; exit 0' TERM INT
: > "$BACKUP_WORK_READY_MARKER"
while :; do
  sleep 1
done
EOF
chmod +x "$tmpdir/fakebin/pg_dump"
BACKUP_DELAY_SECONDS=0 BACKUP_COMMAND="$repo_root/scripts/postgres-backup.sh" \
	BACKUP_DIR="$tmpdir/active-backup" PGHOST=postgres PGPORT=5432 PGUSER=voicx PGDATABASE=voicx \
	BACKUP_WORK_READY_MARKER="$tmpdir/backup-ready" BACKUP_WORK_TERM_MARKER="$tmpdir/backup-term" \
	PATH="$tmpdir/fakebin:$PATH" \
	"$repo_root/scripts/postgres-backup-scheduler.sh" &
scheduler_pid=$!
attempt=0
while [ ! -f "$tmpdir/backup-ready" ] && [ "$attempt" -lt 50 ]; do
	sleep 0.1
	attempt=$((attempt + 1))
done
[ -f "$tmpdir/backup-ready" ] || fail 'scheduler did not start blocking backup child'
kill -TERM "$scheduler_pid"
wait "$scheduler_pid" || fail 'scheduler did not exit cleanly on TERM'
scheduler_pid=
[ -f "$tmpdir/backup-term" ] || fail 'scheduler did not forward TERM through backup wrapper to pg_dump'

grep -q 'COPY --from=rclone /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt' "$repo_root/Dockerfile.backup" || fail 'backup image does not copy a CA bundle for rclone'

echo 'container entrypoint harness passed'
