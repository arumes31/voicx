#!/bin/sh
set -eu

backup_command=${BACKUP_COMMAND:-/usr/local/bin/postgres-backup.sh}
child_pid=

cleanup_private_key() {
	case "${PGSSLKEY:-}" in
		/tmp/voicx-pgpass/client.key) rm -f "$PGSSLKEY" ;;
	esac
}

stop() {
	if [ -n "$child_pid" ]; then
		kill -TERM "$child_pid" 2>/dev/null || true
		wait "$child_pid" 2>/dev/null || true
	fi
	cleanup_private_key
	exit 0
}
trap stop INT TERM

run_backup() {
	"$backup_command" &
	child_pid=$!
	if ! wait "$child_pid"; then
		child_pid=
		return 1
	fi
	child_pid=
}

if [ "${BACKUP_RUN_ONCE:-0}" = 1 ]; then
	run_backup
	cleanup_private_key
	exit 0
fi

next_delay() {
	if [ -n "${BACKUP_DELAY_SECONDS:-}" ]; then
		printf '%s\n' "$BACKUP_DELAY_SECONDS"
		return
	fi

	hour=$(date -u +%H)
	minute=$(date -u +%M)
	second=$(date -u +%S)
	hour=${hour#0}
	minute=${minute#0}
	second=${second#0}
	now=$((hour * 3600 + minute * 60 + second))
	target=$((2 * 3600 + 15 * 60))
	if [ "$now" -lt "$target" ]; then
		printf '%s\n' $((target - now))
	else
		printf '%s\n' $((24 * 3600 - now + target))
	fi
}

while :; do
	delay=$(next_delay)
	sleep "$delay" &
	child_pid=$!
	if ! wait "$child_pid"; then
		exit 1
	fi
	child_pid=
	run_backup
done
