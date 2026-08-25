#!/bin/sh
set -eu

. "${VOICX_SECRET_ENV_LIB:-/usr/local/lib/voicx/secret-env.sh}"

# A complete URL is useful for external databases. Compose otherwise provides
# the discrete PostgreSQL settings below, which keep the password out of its
# generated YAML and command line.
secret_env_load VOICX_DATABASE_URL optional
secret_env_load VOICX_REDIS_PASSWORD optional
secret_env_load VOICX_TURN_SECRET optional

url_escape() {
	# Percent-encode every byte. This is valid URI userinfo and avoids treating
	# punctuation in usernames or passwords as URL delimiters.
	printf '%s' "$1" | od -An -tx1 | tr -d ' \n' | sed 's/../%&/g'
}

if [ -z "$VOICX_DATABASE_URL" ] && [ -n "${POSTGRES_HOST:-}" ]; then
	secret_env_load POSTGRES_PASSWORD required
	postgres_host=${POSTGRES_HOST:-postgres}
	postgres_port=${POSTGRES_PORT:-5432}
	postgres_user=${POSTGRES_USER:-voicx}
	postgres_database=${POSTGRES_DB:-voicx}
	postgres_sslmode=${POSTGRES_SSLMODE:-disable}
	VOICX_DATABASE_URL="postgres://$(url_escape "$postgres_user"):$(url_escape "$POSTGRES_PASSWORD")@${postgres_host}:${postgres_port}/${postgres_database}?sslmode=${postgres_sslmode}"
	export VOICX_DATABASE_URL
fi

# The password has been incorporated into the connection URL; do not leave an
# extra copy in the process environment passed to the server.
unset POSTGRES_PASSWORD POSTGRES_PASSWORD_FILE

case "${1:-}" in
	-*) set -- "${VOICX_SERVER_BIN:-/out/voicx}" "$@" ;;
esac

exec "$@"
