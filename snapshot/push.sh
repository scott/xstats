#!/usr/bin/env bash
# POST one day's raw bundle to the live ingest API (token auth, no basic auth).
# Usage: snapshot/push.sh raw/YYYY-MM-DD.json
set -euo pipefail
FILE="${1:?usage: push.sh raw/YYYY-MM-DD.json}"
DEPLOY_ENV="${XSTATS_DEPLOY_ENV:-$HOME/.config/xstats/deploy.env}"
# A non-empty caller value wins. An empty value counts as unset, so deploy.env
# fills it. Restore only the keys this script reads after sourcing: STATS_URL,
# XSTATS_TOKEN_FILE, CURL_EXTRA, and FILE and HOME (the upload path and the
# default token path). Leave every other caller variable alone, including
# readonly ones.
if [ -f "$DEPLOY_ENV" ]; then
  unset _xstats_STATS_URL _xstats_XSTATS_TOKEN_FILE _xstats_CURL_EXTRA _xstats_FILE _xstats_HOME
  if [ -n "${STATS_URL:-}" ]; then _xstats_STATS_URL=$STATS_URL; fi
  if [ -n "${XSTATS_TOKEN_FILE:-}" ]; then _xstats_XSTATS_TOKEN_FILE=$XSTATS_TOKEN_FILE; fi
  if [ -n "${CURL_EXTRA:-}" ]; then _xstats_CURL_EXTRA=$CURL_EXTRA; fi
  if [ -n "${FILE:-}" ]; then _xstats_FILE=$FILE; fi
  if [ -n "${HOME:-}" ]; then _xstats_HOME=$HOME; fi
  set -a
  . "$DEPLOY_ENV"
  set +a
  if [ -n "${_xstats_STATS_URL:-}" ]; then STATS_URL="$_xstats_STATS_URL"; fi
  if [ -n "${_xstats_XSTATS_TOKEN_FILE:-}" ]; then XSTATS_TOKEN_FILE="$_xstats_XSTATS_TOKEN_FILE"; fi
  if [ -n "${_xstats_CURL_EXTRA:-}" ]; then CURL_EXTRA="$_xstats_CURL_EXTRA"; fi
  if [ -n "${_xstats_FILE:-}" ]; then FILE="$_xstats_FILE"; fi
  if [ -n "${_xstats_HOME:-}" ]; then HOME="$_xstats_HOME"; fi
fi
URL="${STATS_URL:?set STATS_URL, e.g. https://stats.example.com}/api/ingest"
TOKEN_FILE="${XSTATS_TOKEN_FILE:-$HOME/.config/xstats/ingest_token}"
# Token goes in a curl config on stdin, never argv.
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$TOKEN_FILE")" |
  curl -fsS -K - -X POST -H 'Content-Type: application/json' --data-binary "@$FILE" ${CURL_EXTRA:-} "$URL"
