#!/usr/bin/env bash
# chaos-db.sh — database chaos drill (467).
#
# Runs the full e2e checklist plus the -chaos check, which stops PostgreSQL
# while traffic is in flight and verifies the server degrades and recovers.
# The chaos check needs the channel the checklist creates, so the whole
# checklist runs; only the last check is destructive.
#
# Requires a running stack (make compose-up) and the three e2e accounts
# (cmd/adduser). Credentials come from the environment:
#
#   E2E_ALICE_UID E2E_ALICE_PASS E2E_BOB_UID E2E_BOB_PASS
#   E2E_ADMIN_UID E2E_ADMIN_PASS
#
# Optional: CHAOS_STOP_CMD / CHAOS_START_CMD (default: docker compose
# stop/start postgres), and any extra e2e flags passed as arguments.
set -euo pipefail

cd "$(dirname "$0")/.."

missing=()
for var in E2E_ALICE_UID E2E_ALICE_PASS E2E_BOB_UID E2E_BOB_PASS E2E_ADMIN_UID E2E_ADMIN_PASS; do
	if [ -z "${!var:-}" ]; then
		missing+=("$var")
	fi
done
if [ ${#missing[@]} -ne 0 ]; then
	echo "chaos-db: missing required environment: ${missing[*]}" >&2
	echo "chaos-db: create the accounts with 'go run ./cmd/adduser' and export the printed unique IDs" >&2
	exit 2
fi

exec go run ./cmd/e2e -tls-insecure -chaos \
	-alice-uid "$E2E_ALICE_UID" -alice-pass "$E2E_ALICE_PASS" \
	-bob-uid "$E2E_BOB_UID" -bob-pass "$E2E_BOB_PASS" \
	-admin-uid "$E2E_ADMIN_UID" -admin-pass "$E2E_ADMIN_PASS" \
	-chaos-stop-cmd "${CHAOS_STOP_CMD:-docker compose stop postgres}" \
	-chaos-start-cmd "${CHAOS_START_CMD:-docker compose start postgres}" \
	"$@"
