#!/bin/sh
set -eu
: "${TURN_SECRET:?TURN_SECRET must be set for the turn profile}"
exec turnserver --static-auth-secret="$TURN_SECRET" "$@"
