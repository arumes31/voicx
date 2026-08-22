#!/bin/sh
# Exercises the pinned backup image's root entrypoint and real gosu drop.
set -eu

image=${BACKUP_IMAGE:-voicx-backup:dev}

stop_signal=$(docker image inspect --format '{{.Config.StopSignal}}' "$image")
[ "$stop_signal" = SIGTERM ] || {
	echo "test-backup-image: expected SIGTERM stop signal, got $stop_signal" >&2
	exit 1
}

output=$(docker run --rm \
	-e POSTGRES_PASSWORD=container-test-password \
	-e BACKUP_RUN_ONCE=1 \
	-e BACKUP_COMMAND=id \
	"$image")
case "$output" in
	*'uid=70(postgres)'*) ;;
	*)
		echo 'test-backup-image: backup command did not run as postgres' >&2
		exit 1
		;;
esac

if symlink_output=$(docker run --rm --entrypoint sh \
	-e POSTGRES_PASSWORD=container-test-password \
	"$image" -c 'mkdir -p /backups; ln -s /etc /backups/postgres; exec postgres-backup-entrypoint.sh true' 2>&1); then
	echo 'test-backup-image: accepted a symlinked backup directory' >&2
	exit 1
fi
case "$symlink_output" in
	*'postgres-backup: refusing symlinked privileged path'*) ;;
	*)
		echo 'test-backup-image: symlink check failed for an unexpected reason' >&2
		echo "$symlink_output" >&2
		exit 1
		;;
esac

echo 'backup image harness passed'
