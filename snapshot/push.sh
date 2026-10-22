#!/usr/bin/env bash
# POST one day's raw bundle to the live ingest API (token auth, no basic auth).
# Usage: snapshot/push.sh raw/YYYY-MM-DD.json
set -euo pipefail
FILE="${1:?usage: push.sh raw/YYYY-MM-DD.json}"
DEPLOY_ENV="${XSTATS_DEPLOY_ENV:-$HOME/.config/xstats/deploy.env}"
# Caller-set values win. deploy.env only fills variables that are unset,
# so an hourly run with no STATS_URL still posts to the URL in the file.
if [ -f "$DEPLOY_ENV" ]; then
  _xstats_caller_env=$(export -p)
  set -a
  . "$DEPLOY_ENV"
  set +a
  eval "$_xstats_caller_env"
fi
URL="${STATS_URL:?set STATS_URL, e.g. https://stats.example.com}/api/ingest"
TOKEN_FILE="${XSTATS_TOKEN_FILE:-$HOME/.config/xstats/ingest_token}"
# Token goes in a curl config on stdin, never argv.
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$TOKEN_FILE")" |
  curl -fsS -K - -X POST -H 'Content-Type: application/json' --data-binary "@$FILE" ${CURL_EXTRA:-} "$URL"
