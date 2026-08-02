#!/usr/bin/env bash
# Runs real Opus RTP publishers through TURN/TCP behind Toxiproxy. The latency
# toxic provides jitter; a short periodic timeout toxic models packet-loss
# bursts on the TCP relay while keeping sessions recoverable.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -z "${TURN_SECRET:-}" ]; then
	echo "chaos-webrtc: TURN_SECRET must be set explicitly" >&2
	exit 2
fi
if [ -z "${LOADTEST_ARGS:-}" ]; then
	echo "chaos-webrtc: set LOADTEST_ARGS (for example: -anonymous -tls-insecure -channel 1)" >&2
	exit 2
fi

export TURN_SECRET
export VOICX_TURN_URIS="turn:127.0.0.1:8666?transport=tcp"
docker compose --profile turn --profile chaos-network up -d --build voicx coturn toxiproxy

api=http://127.0.0.1:8474
for _ in $(seq 1 60); do
	if curl -fsS "$api/version" >/dev/null; then break; fi
	sleep 1
done

curl -fsS -X POST "$api/proxies" -H 'Content-Type: application/json' \
	-d '{"name":"turn_tcp","listen":"0.0.0.0:8666","upstream":"coturn:3478"}' >/dev/null || true
curl -fsS -X POST "$api/proxies/turn_tcp/toxics" -H 'Content-Type: application/json' \
	-d '{"name":"wan_latency","type":"latency","stream":"downstream","toxicity":1,"attributes":{"latency":90,"jitter":45}}' >/dev/null

flap() {
	while true; do
		sleep 8
		curl -fsS -X POST "$api/proxies/turn_tcp/toxics" -H 'Content-Type: application/json' \
			-d '{"name":"loss_burst","type":"timeout","stream":"downstream","toxicity":1,"attributes":{"timeout":0}}' >/dev/null || true
		sleep 1
		curl -fsS -X DELETE "$api/proxies/turn_tcp/toxics/loss_burst" >/dev/null || true
	done
}
flap &
flap_pid=$!
cleanup() {
	kill "$flap_pid" 2>/dev/null || true
	curl -fsS -X DELETE "$api/proxies/turn_tcp" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Intentional word splitting: LOADTEST_ARGS is an operator-owned list of flags.
# shellcheck disable=SC2086
go run ./cmd/loadtest -clients 100 -duration 45s -webrtc -ice-relay-only $LOADTEST_ARGS
