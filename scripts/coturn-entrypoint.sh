#!/bin/sh
set -eu
umask 077

. "${VOICX_SECRET_ENV_LIB:-/usr/local/lib/voicx/secret-env.sh}"

secret_env_load TURN_SECRET required
secret_env_require_single_line TURN_SECRET
config_dir=${TURN_CONFIG_DIR:-/tmp/voicx-coturn}
config_file=${TURN_CONFIG_FILE:-$config_dir/turnserver.conf}
mkdir -p "$config_dir"
chmod 700 "$config_dir"
printf 'static-auth-secret=%s\n' "$TURN_SECRET" > "$config_file"
chmod 600 "$config_file"
unset TURN_SECRET TURN_SECRET_FILE

turnserver_bin=${TURN_SERVER_BIN:-turnserver}
exec "$turnserver_bin" -c "$config_file" "$@"
