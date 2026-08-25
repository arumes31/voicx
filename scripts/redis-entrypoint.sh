#!/bin/sh
set -eu
umask 077

. "${VOICX_SECRET_ENV_LIB:-/usr/local/lib/voicx/secret-env.sh}"

secret_env_load REDIS_PASSWORD optional

runtime_dir=${REDIS_RUNTIME_DIR:-/tmp/voicx-redis}
config_file=${REDIS_CONFIG_FILE:-$runtime_dir/redis.conf}
mkdir -p "$runtime_dir"
chmod 700 "$runtime_dir"

{
	echo 'appendonly yes'
	echo 'dir /data'
	if [ -n "$REDIS_PASSWORD" ]; then
		password_hash=$(printf '%s' "$REDIS_PASSWORD" | sha256sum | awk '{print $1}')
		printf 'user default on #%s ~* &* +@all\n' "$password_hash"
	else
		echo 'user default on nopass ~* &* +@all'
	fi
} > "$config_file"
chmod 600 "$config_file"

# The official entrypoint drops from root to redis. Make the private generated
# config readable by that account without loosening its mode.
if [ "$(id -u)" -eq 0 ] && id redis >/dev/null 2>&1; then
	chown redis:redis "$runtime_dir" "$config_file"
fi

unset REDIS_PASSWORD REDIS_PASSWORD_FILE
redis_entrypoint=${REDIS_ENTRYPOINT:-/usr/local/bin/docker-entrypoint.sh}
if [ "${1:-}" = redis-server ]; then
	shift
fi
exec "$redis_entrypoint" redis-server "$config_file" "$@"
