#!/usr/bin/env bash
# Stub check for snapshot/push.sh. Fake curl, temp HOME, fake token, no network.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d)
ORIG_PATH=$PATH
TOKEN=fake-ingest-token
CALLER_TOKEN=caller-only-token
DEPLOY_URL=https://stats.example.com
CALLER_URL=http://127.0.0.1:8080
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

mkdir -p "$TMP/bin" "$TMP/log" "$TMP/saved" "$TMP/home/.config/xstats"
printf '%s\n' "$TOKEN" > "$TMP/home/.config/xstats/hourly_token"
chmod 600 "$TMP/home/.config/xstats/hourly_token"
printf '%s\n' "$CALLER_TOKEN" > "$TMP/caller_token"
chmod 600 "$TMP/caller_token"
printf '%s\n' '{"date":"2026-10-14"}' > "$TMP/bundle.json"
# The readonly attribute is not part of the environment, so a child process
# would see a normal FOO. bash sources BASH_ENV in push.sh's own process
# before the script body, which is what makes FOO readonly inside push.sh.
cat > "$TMP/readonly.bash" << 'EOF'
readonly FOO=1
export FOO
EOF

# $HOME stays literal here and expands when push.sh sources the file.
cat > "$TMP/home/.config/xstats/deploy.env" << 'EOF'
# Hourly snapshot config. Example values only; curl is stubbed.
XSTATS_HOST=203.0.113.10
STATS_URL=https://stats.example.com
XSTATS_TOKEN_FILE=$HOME/.config/xstats/hourly_token
EOF
chmod 600 "$TMP/home/.config/xstats/deploy.env"

cat > "$TMP/bin/curl" << 'EOF'
#!/usr/bin/env bash
# Record argv and stdin. Do not resolve hosts or open a connection.
set -euo pipefail
log=${STUB_LOG:?}
printf '%s\n' "$@" > "$log/curl.args"
cat > "$log/curl.stdin"
exit 0
EOF
chmod +x "$TMP/bin/curl"

redact() { sed -e "s/${TOKEN}/[redacted]/g" -e "s/${CALLER_TOKEN}/[redacted]/g"; }

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  if [ -f "$TMP/stderr" ]; then
    printf '--- stderr ---\n' >&2
    redact < "$TMP/stderr" >&2
  fi
  if [ -f "$TMP/stdout" ] && [ -s "$TMP/stdout" ]; then
    printf '--- stdout ---\n' >&2
    redact < "$TMP/stdout" >&2
  fi
  exit 1
}

reset_logs() {
  rm -rf "$TMP/log"
  mkdir -p "$TMP/log"
  : > "$TMP/stdout"
  : > "$TMP/stderr"
}

# run_push runs push.sh with the curl stub first on PATH.
# Extra KEY=VALUE arguments are the caller's command-line environment.
# --readonly exports FOO and marks it readonly inside push.sh via BASH_ENV.
run_push() {
  local bash_env=""
  if [ "${1:-}" = "--readonly" ]; then
    bash_env=$TMP/readonly.bash
    shift
  fi
  reset_logs
  set +e
  (
    export PATH="$TMP/bin:$ORIG_PATH"
    export HOME="$TMP/home"
    export STUB_LOG="$TMP/log"
    unset STATS_URL XSTATS_DEPLOY_ENV XSTATS_TOKEN_FILE CURL_EXTRA
    if [ -n "$bash_env" ]; then
      export BASH_ENV=$bash_env
    fi
    if [ "$(command -v curl)" != "$TMP/bin/curl" ]; then
      printf 'refusing to run push.sh without the curl stub\n' >&2
      exit 1
    fi
    case " $* " in
      *scottmiller*) printf 'refusing production host\n' >&2; exit 1 ;;
    esac
    env "$@" bash "$ROOT/snapshot/push.sh" "$TMP/bundle.json"
  ) >"$TMP/stdout" 2>"$TMP/stderr"
  rc=$?
  set -e
}

save_run() {
  local name=$1
  cp "$TMP/stdout" "$TMP/saved/$name.stdout"
  cp "$TMP/stderr" "$TMP/saved/$name.stderr"
  if [ -f "$TMP/log/curl.args" ]; then
    cp "$TMP/log/curl.args" "$TMP/saved/$name.args"
  fi
  if [ -f "$TMP/log/curl.stdin" ]; then
    cp "$TMP/log/curl.stdin" "$TMP/saved/$name.stdin"
  fi
}

assert_url() {
  local expected=$1
  local got
  if [ ! -f "$TMP/log/curl.args" ]; then
    fail "curl was not run"
  fi
  got=$(tail -n 1 "$TMP/log/curl.args")
  case "$got" in
    *"$TOKEN"*|*"$CALLER_TOKEN"*) fail "url argument contains the token" ;;
  esac
  if [ "$got" != "$expected" ]; then
    printf 'expected url %s\ngot url %s\n' "$expected" "$got" >&2
    fail "unexpected POST url"
  fi
}

assert_token_on_stdin() {
  local needle=$1
  if [ ! -f "$TMP/log/curl.stdin" ]; then
    fail "curl did not receive a config on stdin"
  fi
  if ! grep -F -q -- 'header = "Authorization: Bearer ' "$TMP/log/curl.stdin"; then
    fail "curl stdin is not an Authorization config"
  fi
  if ! grep -F -q -- "$needle" "$TMP/log/curl.stdin"; then
    fail "token was not passed to curl on stdin"
  fi
  if ! grep -F -x -q -- '-K' "$TMP/log/curl.args" || ! grep -F -x -q -- '-' "$TMP/log/curl.args"; then
    fail "curl was not told to read its config from stdin"
  fi
  if grep -F -q -e "$TOKEN" -e "$CALLER_TOKEN" "$TMP/log/curl.args"; then
    fail "token appeared in curl argv"
  fi
}

echo "=== 1. hourly job: only deploy.env sets STATS_URL ==="
run_push
save_run hourly
if [ "$rc" -ne 0 ]; then
  fail "hourly job should post using deploy.env (exit $rc)"
fi
assert_url "$DEPLOY_URL/api/ingest"
assert_token_on_stdin "$TOKEN"
if [ -s "$TMP/stdout" ] || [ -s "$TMP/stderr" ]; then
  fail "hourly job wrote output"
fi
echo "exit: $rc"
echo "url: $DEPLOY_URL/api/ingest"

echo "=== 2. caller STATS_URL plus deploy.env ==="
run_push "STATS_URL=$CALLER_URL"
save_run caller
if [ "$rc" -ne 0 ]; then
  fail "caller STATS_URL should win (exit $rc)"
fi
assert_url "$CALLER_URL/api/ingest"
assert_token_on_stdin "$TOKEN"
if grep -F -x -q -- "$DEPLOY_URL/api/ingest" "$TMP/log/curl.args"; then
  fail "caller run still posted to the deploy.env URL"
fi
if [ -s "$TMP/stdout" ] || [ -s "$TMP/stderr" ]; then
  fail "caller run wrote output"
fi
echo "exit: $rc"
echo "url: $CALLER_URL/api/ingest"

echo "=== 3. no deploy.env and no STATS_URL ==="
mv "$TMP/home/.config/xstats/deploy.env" "$TMP/deploy.env.bak"
run_push
save_run missing
mv "$TMP/deploy.env.bak" "$TMP/home/.config/xstats/deploy.env"
if [ "$rc" -eq 0 ]; then
  fail "missing STATS_URL should fail"
fi
if [ -f "$TMP/log/curl.args" ]; then
  fail "curl ran without STATS_URL"
fi
if ! grep -F -q -- 'set STATS_URL, e.g. https://stats.example.com' "$TMP/stderr"; then
  fail "missing the STATS_URL usage message"
fi
echo "exit: $rc"
echo "stderr: $(redact < "$TMP/stderr")"
echo "curl: not run"

echo "=== 4. fake token never appears in curl argv ==="
for name in hourly caller; do
  if grep -F -q -e "$TOKEN" -e "$CALLER_TOKEN" "$TMP/saved/$name.args"; then
    fail "token appeared in curl argv ($name)"
  fi
  if grep -F -q -e 'Authorization' -e 'Bearer' "$TMP/saved/$name.args"; then
    fail "Authorization was passed on curl argv ($name)"
  fi
done
for name in hourly caller missing; do
  if grep -F -q -e "$TOKEN" -e "$CALLER_TOKEN" "$TMP/saved/$name.stdout" "$TMP/saved/$name.stderr"; then
    fail "token appeared in push.sh output ($name)"
  fi
done
echo "token absent from curl argv and from push.sh output"

echo "=== 5. exported readonly variable ==="
run_push --readonly "STATS_URL=$CALLER_URL"
save_run readonly
if [ "$rc" -ne 0 ]; then
  fail "readonly caller variable should not stop the post (exit $rc)"
fi
assert_url "$CALLER_URL/api/ingest"
assert_token_on_stdin "$TOKEN"
if [ -s "$TMP/stdout" ] || [ -s "$TMP/stderr" ]; then
  fail "readonly run wrote output"
fi
echo "exit: $rc"
echo "url: $CALLER_URL/api/ingest"

echo "=== 6. empty STATS_URL counts as unset ==="
run_push "STATS_URL="
save_run empty
if [ "$rc" -ne 0 ]; then
  fail "empty STATS_URL should be filled from deploy.env (exit $rc)"
fi
assert_url "$DEPLOY_URL/api/ingest"
assert_token_on_stdin "$TOKEN"
if grep -F -x -q -- "$CALLER_URL/api/ingest" "$TMP/log/curl.args"; then
  fail "empty STATS_URL posted to the caller URL"
fi
if [ -s "$TMP/stdout" ] || [ -s "$TMP/stderr" ]; then
  fail "empty STATS_URL run wrote output"
fi
echo "exit: $rc"
echo "url: $DEPLOY_URL/api/ingest"

echo "=== 7. caller XSTATS_TOKEN_FILE wins over deploy.env ==="
run_push "XSTATS_TOKEN_FILE=$TMP/caller_token"
save_run tokenfile
if [ "$rc" -ne 0 ]; then
  fail "caller XSTATS_TOKEN_FILE should win (exit $rc)"
fi
assert_url "$DEPLOY_URL/api/ingest"
assert_token_on_stdin "$CALLER_TOKEN"
if grep -F -q -- "$TOKEN" "$TMP/log/curl.stdin"; then
  fail "stdin held the deploy.env token"
fi
if [ -s "$TMP/stdout" ] || [ -s "$TMP/stderr" ]; then
  fail "caller token-file run wrote output"
fi
echo "exit: $rc"
echo "url: $DEPLOY_URL/api/ingest"
echo "stdin: caller token"

for name in readonly empty tokenfile; do
  if grep -F -q -e "$TOKEN" -e "$CALLER_TOKEN" "$TMP/saved/$name.args"; then
    fail "token appeared in curl argv ($name)"
  fi
  if grep -F -q -e 'Authorization' -e 'Bearer' "$TMP/saved/$name.args"; then
    fail "Authorization was passed on curl argv ($name)"
  fi
  if grep -F -q -e "$TOKEN" -e "$CALLER_TOKEN" "$TMP/saved/$name.stdout" "$TMP/saved/$name.stderr"; then
    fail "token appeared in push.sh output ($name)"
  fi
done
echo "ok"
