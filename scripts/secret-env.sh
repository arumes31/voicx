#!/bin/sh
# Shared secret loading for container entrypoints. A value can come from either
# NAME or NAME_FILE. Values are never written to stdout or stderr.

secret_env_load() {
	secret_env_name=$1
	secret_env_required=${2:-required}
	case "$secret_env_name" in
		*[!A-Za-z0-9_]* | '')
			echo "secret-env: invalid secret name" >&2
			return 1
			;;
	esac

	eval "secret_env_value=\${$secret_env_name-}"
	eval "secret_env_file=\${${secret_env_name}_FILE-}"

	# Compose commonly passes empty optional variables. Treat those as absent,
	# while rejecting two actual secret sources.
	if [ -n "$secret_env_value" ] && [ -n "$secret_env_file" ]; then
		echo "secret-env: $secret_env_name and ${secret_env_name}_FILE are mutually exclusive" >&2
		return 1
	fi

	if [ -n "$secret_env_file" ]; then
		if [ ! -f "$secret_env_file" ] || [ ! -r "$secret_env_file" ]; then
			echo "secret-env: ${secret_env_name}_FILE is not a readable regular file" >&2
			return 1
		fi
		secret_env_value=$(cat "$secret_env_file") || {
			echo "secret-env: unable to read ${secret_env_name}_FILE" >&2
			return 1
		}
	fi

	if [ "$secret_env_required" = required ] && [ -z "$secret_env_value" ]; then
		echo "secret-env: $secret_env_name must not be empty" >&2
		return 1
	fi

	export "$secret_env_name=$secret_env_value"
	unset "${secret_env_name}_FILE"
}

# Values written to line-oriented configuration formats must not inject extra
# records. Docker secrets commonly end with one newline, which command
# substitution above removes; any remaining CR/LF is embedded content.
secret_env_require_single_line() {
	secret_env_name=$1
	case "$secret_env_name" in
		*[!A-Za-z0-9_]* | '')
			echo "secret-env: invalid secret name" >&2
			return 1
			;;
	esac
	eval "secret_env_value=\${$secret_env_name-}"
	secret_env_lf=$(printf '\nx')
	secret_env_lf=${secret_env_lf%x}
	secret_env_cr=$(printf '\rx')
	secret_env_cr=${secret_env_cr%x}
	case "$secret_env_value" in
		*"$secret_env_lf"* | *"$secret_env_cr"*)
			echo "secret-env: $secret_env_name must not contain a line break" >&2
			return 1
			;;
	esac
}
