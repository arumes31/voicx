#!/bin/sh
set -eu
umask 077

. "${VOICX_SECRET_ENV_LIB:-/usr/local/lib/voicx/secret-env.sh}"

require_backup_value() {
	backup_value_name=$1
	eval "backup_value=\${$backup_value_name-}"
	if [ -z "$backup_value" ]; then
		echo "postgres-backup: $backup_value_name must be set for an external database" >&2
		exit 1
	fi
}

if [ -n "${BACKUP_EXTERNAL_DATABASE:-}" ]; then
	for backup_setting in BACKUP_PGHOST BACKUP_PGPORT BACKUP_PGUSER BACKUP_PGDATABASE BACKUP_PGSSLMODE; do
		require_backup_value "$backup_setting"
	done
	secret_env_load BACKUP_POSTGRES_PASSWORD required
	unset POSTGRES_PASSWORD POSTGRES_PASSWORD_FILE
	POSTGRES_PASSWORD=$BACKUP_POSTGRES_PASSWORD
	export POSTGRES_PASSWORD
	unset BACKUP_POSTGRES_PASSWORD BACKUP_POSTGRES_PASSWORD_FILE
	PGHOST=$BACKUP_PGHOST
	PGPORT=$BACKUP_PGPORT
	PGUSER=$BACKUP_PGUSER
	PGDATABASE=$BACKUP_PGDATABASE
	PGSSLMODE=$BACKUP_PGSSLMODE
	case "$PGSSLMODE" in require | verify-ca | verify-full) ;; *)
		echo 'postgres-backup: external BACKUP_PGSSLMODE must require TLS verification' >&2
		exit 1
	;; esac
	for ssl_file in BACKUP_PGSSLROOTCERT BACKUP_PGSSLCERT BACKUP_PGSSLKEY; do
		eval "ssl_path=\${$ssl_file-}"
		if [ -n "$ssl_path" ] && { [ ! -f "$ssl_path" ] || [ ! -r "$ssl_path" ]; }; then
			echo "postgres-backup: $ssl_file is not a readable regular file" >&2
			exit 1
		fi
	done
	PGSSLROOTCERT=${BACKUP_PGSSLROOTCERT:-}
	PGSSLCERT=${BACKUP_PGSSLCERT:-}
	PGSSLKEY=${BACKUP_PGSSLKEY:-}
	export PGSSLROOTCERT PGSSLCERT PGSSLKEY
else
	secret_env_load POSTGRES_PASSWORD required
	: "${PGHOST:=postgres}"
	: "${PGPORT:=5432}"
	: "${PGUSER:=voicx}"
	: "${PGDATABASE:=voicx}"
	: "${PGSSLMODE:=disable}"
fi
export PGHOST PGPORT PGUSER PGDATABASE PGSSLMODE

for pgpass_field in POSTGRES_PASSWORD PGHOST PGPORT PGUSER PGDATABASE; do
	secret_env_require_single_line "$pgpass_field"
done

pgpass_escape() {
	printf '%s' "$1" | sed 's/\\/\\\\/g; s/:/\\:/g'
}

pgpass_dir=${PGPASS_RUNTIME_DIR:-/tmp/voicx-pgpass}
pgpass_file=${PGPASSFILE_PATH:-$pgpass_dir/.pgpass}
backup_dir=${BACKUP_DIR:-/backups/postgres}
case "$backup_dir" in /*) ;; *)
	echo 'postgres-backup: BACKUP_DIR must be an absolute non-root directory' >&2
	exit 1
;; esac
[ "$backup_dir" != / ] || {
	echo 'postgres-backup: BACKUP_DIR must not be /' >&2
	exit 1
}
case "$pgpass_dir" in /*) ;; *)
	echo 'postgres-backup: PGPASS_RUNTIME_DIR must be absolute' >&2
	exit 1
;; esac
case "/$backup_dir/" in */../* | */./*)
	echo 'postgres-backup: backup paths must not contain dot components' >&2
	exit 1
	;;
esac
case "/$pgpass_dir/" in */../* | */./*)
	echo 'postgres-backup: PGPASS_RUNTIME_DIR must not contain dot components' >&2
	exit 1
	;;
esac
case "/$pgpass_file/" in */../* | */./*)
	echo 'postgres-backup: PGPASSFILE_PATH must not contain dot components' >&2
	exit 1
	;;
esac
case "$pgpass_file" in "$pgpass_dir"/*) ;; *)
	echo 'postgres-backup: PGPASSFILE_PATH must be inside PGPASS_RUNTIME_DIR' >&2
	exit 1
	;; esac
if [ "$(id -u)" -eq 0 ]; then
	case "$backup_dir" in /backups/postgres) ;; *)
		echo 'postgres-backup: root requires BACKUP_DIR=/backups/postgres' >&2
		exit 1
	;; esac
	case "$pgpass_dir" in /tmp/voicx-pgpass) ;; *)
		echo 'postgres-backup: root requires PGPASS_RUNTIME_DIR=/tmp/voicx-pgpass' >&2
		exit 1
	;; esac
	for protected_path in /backups /tmp "$backup_dir" "$pgpass_dir" "$pgpass_file"; do
		if [ -L "$protected_path" ]; then
			echo 'postgres-backup: refusing symlinked privileged path' >&2
			exit 1
		fi
	done
fi
mkdir -p "$pgpass_dir"
mkdir -p "$backup_dir"
if [ "$(id -u)" -eq 0 ] && { [ -L "$pgpass_dir" ] || [ -L "$backup_dir" ] || [ -L "$pgpass_file" ]; }; then
	echo 'postgres-backup: refusing symlinked privileged path' >&2
	exit 1
fi
chmod 700 "$pgpass_dir"
chmod 700 "$backup_dir"
if [ -n "${PGSSLKEY:-}" ]; then
	private_ssl_key="$pgpass_dir/client.key"
	cp "$PGSSLKEY" "$private_ssl_key"
	chmod 600 "$private_ssl_key"
	PGSSLKEY=$private_ssl_key
	export PGSSLKEY
fi
printf '%s:%s:%s:%s:%s\n' \
	"$(pgpass_escape "$PGHOST")" \
	"$(pgpass_escape "$PGPORT")" \
	"$(pgpass_escape "$PGDATABASE")" \
	"$(pgpass_escape "$PGUSER")" \
	"$(pgpass_escape "$POSTGRES_PASSWORD")" > "$pgpass_file"
chmod 600 "$pgpass_file"
export PGPASSFILE=$pgpass_file

unset POSTGRES_PASSWORD POSTGRES_PASSWORD_FILE

if [ "$(id -u)" -eq 0 ] && command -v gosu >/dev/null 2>&1; then
	chown postgres:postgres "$pgpass_dir" "$pgpass_file" "$backup_dir"
	if [ -n "${PGSSLKEY:-}" ]; then
		chown postgres:postgres "$PGSSLKEY"
	fi
	exec gosu postgres "$@"
fi

exec "$@"
