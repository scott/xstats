#!/usr/bin/env bash
# Local check for deploy/deploy.sh. Stubs ssh and scp so nothing is copied
# to a real host. Uses dummy secrets only.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d)
ORIG_PATH=$PATH
created_bin=0
cleanup() {
  rm -rf "$TMP"
  if [ "$created_bin" = 1 ]; then
    rm -f "$ROOT/bin/xstats"
    rmdir "$ROOT/bin" 2>/dev/null || true
  fi
}
trap cleanup EXIT

if [ ! -x "$ROOT/bin/xstats" ]; then
  mkdir -p "$ROOT/bin"
  printf '#!/bin/sh\nexit 0\n' > "$ROOT/bin/xstats"
  chmod +x "$ROOT/bin/xstats"
  created_bin=1
fi

mkdir -p "$TMP/bin" "$TMP/log" "$TMP/home"
printf 'dummy-ingest-token\n' > "$TMP/token"
: > "$TMP/empty.env"
: > "$TMP/dummy.key"

cat > "$TMP/bin/ssh" << 'EOF'
#!/usr/bin/env bash
set -euo pipefail
log=${STUB_LOG:?}
n=$(cat "$log/ssh.count" 2>/dev/null || echo 0)
n=$((n + 1))
printf '%s\n' "$n" > "$log/ssh.count"
printf '%s\n' "$*" > "$log/ssh.args.$n"
cat > "$log/ssh.stdin.$n"
if [[ " $* " == *"/etc/xstats/env"* ]]; then
  cp "$log/ssh.stdin.$n" "$log/env.file"
  cp "$log/ssh.args.$n" "$log/env.cmd"
fi
exit 0
EOF
cat > "$TMP/bin/scp" << 'EOF'
#!/usr/bin/env bash
printf 'scp\n' >> "${STUB_LOG:?}/scp.txt"
exit 0
EOF
chmod +x "$TMP/bin/ssh" "$TMP/bin/scp"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  if [ -f "$TMP/stderr" ]; then
    printf '--- stderr ---\n' >&2
    cat "$TMP/stderr" >&2
  fi
  if [ -f "$TMP/log/env.file" ]; then
    printf '--- env.file ---\n' >&2
    cat "$TMP/log/env.file" >&2
  fi
  exit 1
}

reset_logs() {
  rm -rf "$TMP/log"
  mkdir -p "$TMP/log"
  : > "$TMP/stdout"
  : > "$TMP/stderr"
}

# run_deploy runs deploy.sh with stub ssh/scp. Extra env assignments may
# be passed as arguments (for example XSTATS_TZ=Europe/Berlin).
run_deploy() {
  reset_logs
  (
    export PATH="$TMP/bin:$ORIG_PATH"
    export STUB_LOG="$TMP/log"
    export HOME="$TMP/home"
    export XSTATS_DEPLOY_ENV="$TMP/empty.env"
    export XSTATS_HOST="203.0.113.10"
    export XSTATS_DOMAIN="stats.example.com"
    export XSTATS_SSH_USER="root"
    export XSTATS_AUTH_USER="admin"
    export BASIC_AUTH_PASSWORD="dummy-password"
    export XSTATS_TOKEN_FILE="$TMP/token"
    export SSH_KEY="$TMP/dummy.key"
    export XSTATS_ACCOUNTS="alice,bob"
    unset XSTATS_TZ
    if [ "$#" -gt 0 ]; then
      export "$@"
    fi
    case "$XSTATS_HOST" in
      *scottmiller*) printf 'refusing production host\n' >&2; exit 1 ;;
    esac
    if [ "$(command -v ssh)" != "$TMP/bin/ssh" ] || [ "$(command -v scp)" != "$TMP/bin/scp" ]; then
      printf 'refusing to run deploy.sh without ssh/scp stubs\n' >&2
      exit 1
    fi
    bash "$ROOT/deploy/deploy.sh" >"$TMP/stdout" 2>"$TMP/stderr"
  )
}

ssh_count() {
  if [ -f "$TMP/log/ssh.count" ]; then
    cat "$TMP/log/ssh.count"
  else
    printf '0\n'
  fi
}

scp_count() {
  if [ -f "$TMP/log/scp.txt" ]; then
    wc -l < "$TMP/log/scp.txt" | tr -d ' '
  else
    printf '0\n'
  fi
}

assert_env() {
  local expected=$1 got bit
  # The trailing x keeps the env file's final newline; $(cat) would drop it.
  got=$(cat "$TMP/log/env.file"; printf x)
  got=${got%x}
  if [ "$got" != "$expected" ]; then
    printf 'expected env file:\n%s\n--- got ---\n%s\n' "$expected" "$got" >&2
    fail "env file mismatch"
  fi
  if [ ! -f "$TMP/log/env.cmd" ]; then
    fail "remote command was not the env write"
  fi
  for bit in 'umask 027' 'cat > /etc/xstats/env' 'chgrp xstats' 'systemctl restart xstats'; do
    if ! grep -q "$bit" "$TMP/log/env.cmd"; then
      fail "remote env command missing: $bit"
    fi
  done
  if grep -q 'dummy-ingest-token' "$TMP/log/env.cmd"; then
    fail "token appeared on the ssh command line"
  fi
  if [ "$(ssh_count)" != "3" ] || [ "$(scp_count)" != "1" ]; then
    fail "expected 3 ssh and 1 scp, got ssh=$(ssh_count) scp=$(scp_count)"
  fi
}

base_env='XSTATS_INGEST_TOKEN=dummy-ingest-token
XSTATS_ACCOUNTS=alice,bob
'

echo "=== XSTATS_TZ=Europe/Berlin ==="
run_deploy XSTATS_TZ=Europe/Berlin
assert_env "${base_env}XSTATS_TZ=Europe/Berlin"$'\n'
echo "remote env file:"
cat "$TMP/log/env.file"
echo "stdout: $(cat "$TMP/stdout")"

echo "=== XSTATS_TZ with surrounding whitespace ==="
run_deploy XSTATS_TZ='  Europe/Berlin  '
assert_env "${base_env}XSTATS_TZ=Europe/Berlin"$'\n'
echo "remote env file:"
cat "$TMP/log/env.file"

echo "=== XSTATS_TZ=Canada/Eastern (embedded tzdata, no system zone file required) ==="
run_deploy XSTATS_TZ=Canada/Eastern
assert_env "${base_env}XSTATS_TZ=Canada/Eastern"$'\n'
echo "remote env file:"
cat "$TMP/log/env.file"

echo "=== XSTATS_TZ unset ==="
run_deploy
assert_env "$base_env"
if grep -q 'XSTATS_TZ' "$TMP/log/env.file"; then
  fail "unset XSTATS_TZ should not write a line"
fi
echo "remote env file:"
cat "$TMP/log/env.file"

echo "=== XSTATS_TZ empty ==="
run_deploy XSTATS_TZ=
assert_env "$base_env"
echo "remote env file:"
cat "$TMP/log/env.file"

echo "=== XSTATS_TZ whitespace only ==="
run_deploy XSTATS_TZ='   '
assert_env "$base_env"
echo "remote env file:"
cat "$TMP/log/env.file"

echo "=== missing token file ==="
mv "$TMP/token" "$TMP/token.bak"
set +e
run_deploy
rc=$?
set -e
mv "$TMP/token.bak" "$TMP/token"
if [ "$rc" = 0 ]; then
  fail "missing token file should fail the deploy"
fi
if [ "$(ssh_count)" != "0" ] || [ "$(scp_count)" != "0" ]; then
  fail "missing token touched the server stub: ssh=$(ssh_count) scp=$(scp_count)"
fi
if [ -f "$TMP/log/env.file" ]; then
  fail "missing token file still sent an env file"
fi
echo "exit: $rc"
echo "stderr: $(cat "$TMP/stderr")"
echo "ssh: $(ssh_count) scp: $(scp_count)"

echo "=== XSTATS_TZ=Not/AZone ==="
set +e
run_deploy XSTATS_TZ=Not/AZone
rc=$?
set -e
if [ "$rc" = 0 ]; then
  fail "invalid zone should fail the deploy"
fi
if [ "$(ssh_count)" != "0" ] || [ "$(scp_count)" != "0" ]; then
  fail "invalid zone touched the server stub: ssh=$(ssh_count) scp=$(scp_count)"
fi
if ! grep -q 'invalid XSTATS_TZ "Not/AZone"' "$TMP/stderr"; then
  fail "missing a clear invalid-zone message"
fi
echo "exit: $rc"
echo "stderr: $(cat "$TMP/stderr")"
echo "ssh: $(ssh_count) scp: $(scp_count)"

echo "ok"
