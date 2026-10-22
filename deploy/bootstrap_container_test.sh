#!/usr/bin/env bash
# Runs inside an Ubuntu container. systemctl, caddy, apt-get, and curl are
# stubbed. curl refuses anything except the local release server.
set -euo pipefail

: "${SRC:?}"
: "${WORK:?}"
: "${XSTATS_RELEASE_URL:?}"

SCRIPT="$SRC/deploy/bootstrap.sh"
RELEASE_URL="$XSTATS_RELEASE_URL"

fail() {
  printf '%s\n' "FAIL: $1" >&2
  if [ -f "$WORK/stdout" ]; then
    printf '%s\n' '--- stdout ---' >&2
    scrub "$WORK/stdout" >&2 || true
  fi
  if [ -f "$WORK/stderr" ]; then
    printf '%s\n' '--- stderr ---' >&2
    scrub "$WORK/stderr" >&2 || true
  fi
  exit 1
}

scrub() {
  grep -a -F -v -f "$WORK/token" -f "$WORK/password" -f "$WORK/token2" "$1" || true
}

scan_file() {
  local f=$1
  [ -f "$f" ] || return 0
  if grep -a -F -q -f "$WORK/token" "$f" \
    || grep -a -F -q -f "$WORK/password" "$f" \
    || grep -a -F -q -f "$WORK/token2" "$f"; then
    fail "secret appeared in $f"
  fi
}

start_scan() {
  rm -f "$WORK/leak"
  : > "$WORK/scan.on"
  python3 "$WORK/scan.py" </dev/null >/dev/null 2>&1 &
  echo $! > "$WORK/scan.pid"
}

stop_scan() {
  rm -f "$WORK/scan.on"
  if [ -f "$WORK/scan.pid" ]; then
    local pid i
    pid=$(cat "$WORK/scan.pid")
    for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
      if ! kill -0 "$pid" 2>/dev/null; then
        break
      fi
      sleep 0.05
    done
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  if [ -s "$WORK/leak" ]; then
    fail "secret appeared in a process cmdline or environ"
  fi
}

run_boot() {
  : > /var/log/xstats-systemctl.log
  : > /var/log/xstats-caddy.log
  : > /var/log/xstats-apt.log
  : > "$WORK/stdout"
  : > "$WORK/stderr"
  start_scan
  set +e
  "$@" >"$WORK/stdout" 2>"$WORK/stderr"
  RC=$?
  set -e
  stop_scan
  scan_file "$WORK/stdout"
  scan_file "$WORK/stderr"
  scan_file /var/log/xstats-systemctl.log
  scan_file /var/log/xstats-caddy.log
  scan_file /var/log/xstats-apt.log
  if [ -s /var/log/xstats-apt.log ]; then
    fail "apt-get was invoked: $(cat /var/log/xstats-apt.log)"
  fi
}

expect_fail() {
  local needle=$1
  shift
  run_boot "$@"
  if [ "$RC" -eq 0 ]; then
    fail "expected failure ($needle)"
  fi
  if ! grep -q "$needle" "$WORK/stderr" && ! grep -q "$needle" "$WORK/stdout"; then
    fail "missing error text: $needle"
  fi
}

expect_ok() {
  run_boot "$@"
  if [ "$RC" -ne 0 ]; then
    fail "expected success"
  fi
}

arch_name() {
  case "$(/usr/bin/uname -m)" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) fail "unsupported architecture: $(/usr/bin/uname -m)" ;;
  esac
}

publish_rel() {
  local dir=$1
  cp "$dir/xstats-linux-amd64" "$dir/xstats-linux-arm64" \
    "$dir/xstats.service" "$dir/Caddyfile.tmpl" "$dir/set-auth.sh" \
    "$dir/SHA256SUMS" "$WORK/www/"
  if [ -f "$dir/bootstrap.sh" ]; then
    cp "$dir/bootstrap.sh" "$WORK/www/"
  fi
  chmod a+r "$WORK/www/xstats-linux-amd64" "$WORK/www/xstats-linux-arm64" \
    "$WORK/www/xstats.service" "$WORK/www/Caddyfile.tmpl" "$WORK/www/set-auth.sh" \
    "$WORK/www/SHA256SUMS"
}

assert_env() {
  local accounts=$1
  local tz_line=${2:-}
  local token_file=$3
  local expected=$WORK/expected-env
  {
    printf 'XSTATS_INGEST_TOKEN=%s\n' "$(cat "$token_file")"
    printf 'XSTATS_ACCOUNTS=%s\n' "$accounts"
    if [ -n "$tz_line" ]; then
      printf 'XSTATS_TZ=%s\n' "$tz_line"
    fi
  } > "$expected"
  if ! cmp -s "$expected" /etc/xstats/env; then
    diff -u "$expected" /etc/xstats/env | scrub /dev/stdin >&2 || true
    fail "env file mismatch"
  fi
  [ "$(stat -c '%a %U %G' /etc/xstats/env)" = "640 root xstats" ]
  [ "$(stat -c '%a %U %G' /etc/xstats)" = "750 root xstats" ]
  scan_file /etc/caddy/Caddyfile
  if grep -a -F -q -f "$WORK/password" /etc/xstats/env; then
    fail "password stored in env file"
  fi
}

assert_layout() {
  local asset=$1
  local host=$2
  local user=$3
  cmp "/usr/local/bin/xstats" "$asset"
  [ "$(stat -c '%a' /usr/local/bin/xstats)" = "755" ]
  cmp /etc/systemd/system/xstats.service "$SRC/deploy/xstats.service"
  grep -q "$host" /etc/caddy/Caddyfile
  grep -q "$user" /etc/caddy/Caddyfile
  grep -q 'basic_auth' /etc/caddy/Caddyfile
  grep -q 'reverse_proxy 127.0.0.1:8080' /etc/caddy/Caddyfile
  grep -q 'favicon.ico' /etc/caddy/Caddyfile
  grep -q '\$2' /etc/caddy/Caddyfile
  if grep -q '__DOMAIN__\|__USER__\|__HASH__' /etc/caddy/Caddyfile; then
    fail "Caddyfile still has template placeholders"
  fi
  id xstats >/dev/null
  [ "$(stat -c '%U %G' /var/lib/xstats)" = "xstats xstats" ]
  [ "$(stat -c '%U %G' /var/lib/xstats/inbox)" = "xstats xstats" ]
  grep -q 'daemon-reload' /var/log/xstats-systemctl.log
  grep -q 'enable xstats' /var/log/xstats-systemctl.log
  grep -q 'restart xstats' /var/log/xstats-systemctl.log
  grep -q 'enable caddy' /var/log/xstats-systemctl.log
  grep -E -q 'reload caddy|restart caddy|start caddy' /var/log/xstats-systemctl.log
  grep -q 'hash-password' /var/log/xstats-caddy.log
  grep -q 'validate' /var/log/xstats-caddy.log
  [ ! -e /etc/apt/sources.list.d/caddy-stable.list ]
  if grep -q 'kept existing ingest token' "$WORK/stdout"; then
    fail "first install reported an existing token"
  fi
  grep -q 'checksum ok' "$WORK/stdout"
}

assert_unchanged_binary() {
  local asset=$1
  cmp /usr/local/bin/xstats "$asset"
}

install_stubs() {
  cat > /usr/local/bin/systemctl <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> /var/log/xstats-systemctl.log
exit 0
EOF
  cat > /usr/local/bin/caddy <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> /var/log/xstats-caddy.log
case "$1" in
  hash-password)
    cat >/dev/null
    printf '%s\n' '$2a$14$0123456789abcdef0123456789abcdef0123456789abcdef01234'
    ;;
esac
exit 0
EOF
  cat > /usr/local/bin/apt-get <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> /var/log/xstats-apt.log
exit 0
EOF
  cat > /usr/local/bin/curl <<'EOF'
#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    *github.com*|*cloudsmith.io*|*scottmiller*)
      printf 'refusing network: %s\n' "$arg" >&2
      exit 99
      ;;
  esac
done
exec /usr/bin/curl "$@"
EOF
  chmod 0755 /usr/local/bin/systemctl /usr/local/bin/caddy /usr/local/bin/apt-get /usr/local/bin/curl
  : > /var/log/xstats-systemctl.log
  : > /var/log/xstats-caddy.log
  : > /var/log/xstats-apt.log
}

cat > "$WORK/scan.py" <<'PY'
import os, time
patterns = []
for name in ("token", "password", "token2"):
    path = os.path.join(os.environ["WORK"], name)
    with open(path, "rb") as fh:
        for line in fh.read().splitlines():
            if line:
                patterns.append(line)
flag = os.path.join(os.environ["WORK"], "scan.on")
leak = os.path.join(os.environ["WORK"], "leak")
while os.path.exists(flag):
    try:
        pids = os.listdir("/proc")
    except OSError:
        break
    for pid in pids:
        if not pid.isdigit():
            continue
        for kind in ("cmdline", "environ"):
            path = f"/proc/{pid}/{kind}"
            try:
                data = open(path, "rb").read()
            except OSError:
                continue
            for pat in patterns:
                if pat in data:
                    with open(leak, "ab") as fh:
                        fh.write(b"leak\n")
                    raise SystemExit(0)
    time.sleep(0.002)
PY

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/tmp/apt-setup.log 2>&1
apt-get install -y -qq curl ca-certificates python3 >>/tmp/apt-setup.log 2>&1
# Drop zone files without removing the tzdata package. Python on 24.04
# depends on that package, and the release binary carries its own zones.
rm -rf /usr/share/zoneinfo

install_stubs
ARCH=$(arch_name)
printf 'container %s arch %s\n' "$(. /etc/os-release && printf '%s' "$VERSION_ID")" "$ARCH"

if [ -e /usr/share/zoneinfo/Europe/Berlin ] || [ -e /usr/share/zoneinfo/Canada/Eastern ]; then
  fail "zoneinfo files are still installed"
fi

# --- xtrace is refused before secrets are read ---
set +e
bash -x "$SCRIPT" \
  --hostname xstats.example.com \
  --token-file "$WORK/token" \
  --password-file "$WORK/password" \
  >"$WORK/stdout" 2>"$WORK/stderr"
RC=$?
set -e
[ "$RC" -ne 0 ]
grep -q 'xtrace' "$WORK/stderr"
scan_file "$WORK/stdout"
scan_file "$WORK/stderr"
[ ! -e /usr/local/bin/xstats ]
[ ! -e /etc/xstats/env ]
echo "ok: xtrace refused"

run_boot bash "$SCRIPT" --help
[ "$RC" -eq 0 ]
grep -q 'xstats.<domain>' "$WORK/stdout"
echo "ok: help"

expect_fail "arguments" bash "$SCRIPT" --hostname xstats.example.com --password 'not-a-real-secret'
[ ! -e /usr/local/bin/xstats ]
echo "ok: password flag rejected"

# --- missing secrets do not touch the system ---
expect_fail "password-file" env -u XSTATS_TOKEN_FILE -u XSTATS_PASSWORD_FILE -u XSTATS_TZ \
  XSTATS_DOMAIN=xstats.example.com \
  XSTATS_RELEASE_URL="$RELEASE_URL" \
  bash "$SCRIPT"
[ ! -e /usr/local/bin/xstats ]
[ ! -e /etc/xstats/env ]
! id xstats >/dev/null 2>&1
[ ! -s /var/log/xstats-systemctl.log ]
echo "ok: missing secrets"

# --- tampered binary fails closed before install ---
publish_rel "$WORK/rel1"
cp "$WORK/www/xstats-linux-$ARCH" "$WORK/www/xstats-linux-$ARCH.good"
printf 'tamper\n' >> "$WORK/www/xstats-linux-$ARCH"
chmod a+r "$WORK/www/xstats-linux-$ARCH"
expect_fail "checksum mismatch" env -u XSTATS_TZ \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash "$SCRIPT" --hostname xstats.example.com --release-url "$RELEASE_URL"
[ ! -e /usr/local/bin/xstats ]
[ ! -e /etc/xstats/env ]
! id xstats >/dev/null 2>&1
mv "$WORK/www/xstats-linux-$ARCH.good" "$WORK/www/xstats-linux-$ARCH"
chmod a+r "$WORK/www/xstats-linux-$ARCH"
echo "ok: tampered binary before install"

assert_nothing_installed() {
  [ ! -e /usr/local/bin/xstats ]
  [ ! -e /usr/local/bin/xstats.prev ]
  [ ! -e /etc/xstats/env ]
  [ ! -e /etc/systemd/system/xstats.service ]
  [ ! -e /etc/caddy/Caddyfile ]
  ! id xstats >/dev/null 2>&1
  [ ! -s /var/log/xstats-systemctl.log ]
}

# Companion downloads only happen when the script is not beside the files.
mkdir -p /tmp/alone
cp "$SCRIPT" /tmp/alone/bootstrap.sh
chmod a+r /tmp/alone/bootstrap.sh

# --- tampered set-auth.sh fails closed before install ---
cp "$WORK/www/set-auth.sh" "$WORK/www/set-auth.sh.good"
printf '\n# tamper\n' >> "$WORK/www/set-auth.sh"
chmod a+r "$WORK/www/set-auth.sh"
expect_fail "checksum mismatch" env -u XSTATS_TZ \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash /tmp/alone/bootstrap.sh --hostname xstats.example.com --release-url "$RELEASE_URL"
assert_nothing_installed
mv "$WORK/www/set-auth.sh.good" "$WORK/www/set-auth.sh"
chmod a+r "$WORK/www/set-auth.sh"
echo "ok: tampered set-auth.sh"

# --- tampered Caddyfile.tmpl fails closed before install ---
cp "$WORK/www/Caddyfile.tmpl" "$WORK/www/Caddyfile.tmpl.good"
printf '\n# tamper\n' >> "$WORK/www/Caddyfile.tmpl"
chmod a+r "$WORK/www/Caddyfile.tmpl"
expect_fail "checksum mismatch" env -u XSTATS_TZ \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash /tmp/alone/bootstrap.sh --hostname xstats.example.com --release-url "$RELEASE_URL"
assert_nothing_installed
mv "$WORK/www/Caddyfile.tmpl.good" "$WORK/www/Caddyfile.tmpl"
chmod a+r "$WORK/www/Caddyfile.tmpl"
echo "ok: tampered Caddyfile.tmpl"

# --- SHA256SUMS with no entry for xstats.service fails closed ---
cp "$WORK/www/SHA256SUMS" "$WORK/www/SHA256SUMS.good"
grep -v '[[:space:]]xstats\.service$' "$WORK/www/SHA256SUMS.good" > "$WORK/www/SHA256SUMS"
chmod a+r "$WORK/www/SHA256SUMS"
expect_fail "no entry for xstats.service" env -u XSTATS_TZ \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash /tmp/alone/bootstrap.sh --hostname xstats.example.com --release-url "$RELEASE_URL"
assert_nothing_installed
mv "$WORK/www/SHA256SUMS.good" "$WORK/www/SHA256SUMS"
chmod a+r "$WORK/www/SHA256SUMS"
echo "ok: missing xstats.service checksum"

# --- invalid zone fails before any xstats file is installed ---
expect_fail "invalid XSTATS_TZ" env \
  XSTATS_TZ=Not/AZone \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash "$SCRIPT" --hostname xstats.example.com --release-url "$RELEASE_URL"
[ ! -e /usr/local/bin/xstats ]
[ ! -e /usr/local/bin/xstats.prev ]
[ ! -e /etc/xstats/env ]
[ ! -e /etc/systemd/system/xstats.service ]
! id xstats >/dev/null 2>&1
[ ! -s /var/log/xstats-systemctl.log ]
echo "ok: invalid zone"

# --- first install: flag overrides, trimmed zone, file secrets ---
expect_ok env \
  XSTATS_RELEASE_URL="http://127.0.0.1:1" \
  XSTATS_TZ='  Europe/Berlin  ' \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  XSTATS_ACCOUNTS=from-env \
  XSTATS_DOMAIN=wrong.example \
  bash "$SCRIPT" \
  --release-url "$RELEASE_URL" \
  --hostname xstats.example.com \
  --accounts alice,bob \
  --auth-user dashuser
assert_env alice,bob Europe/Berlin "$WORK/token"
assert_layout "$WORK/rel1/xstats-linux-$ARCH" xstats.example.com dashuser
[ ! -e /usr/local/bin/xstats.prev ]
if grep -q 'wrong.example\|from-env' /etc/caddy/Caddyfile /etc/xstats/env; then
  fail "flag did not override env"
fi
echo "ok: first install"
printf 'env mode %s\n' "$(stat -c '%a %U %G' /etc/xstats/env)"
scrub "$WORK/stdout"

# --- password on stdin, existing token kept ---
expect_ok env -u XSTATS_PASSWORD_FILE -u XSTATS_TOKEN_FILE -u XSTATS_TZ \
  XSTATS_ACCOUNTS=alice,bob \
  bash "$SCRIPT" --hostname xstats.example.com --release-url "$RELEASE_URL" --auth-user dashuser \
  < "$WORK/password"
assert_env alice,bob "" "$WORK/token"
grep -q 'kept existing ingest token' "$WORK/stdout" || fail "did not keep the ingest token"
echo "ok: password on stdin"

# --- rerun keeps the token, previous binary, and a DB backup ---
printf 'db-marker-v1\n' > /var/lib/xstats/xstats.db
cp /var/lib/xstats/xstats.db "$WORK/db-marker"
publish_rel "$WORK/rel2"
if cmp -s "$WORK/rel1/xstats-linux-$ARCH" "$WORK/rel2/xstats-linux-$ARCH"; then
  fail "release builds are identical"
fi
expect_ok env -u XSTATS_TOKEN_FILE \
  XSTATS_TZ='   ' \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  XSTATS_DOMAIN=xstats.example.com \
  XSTATS_ACCOUNTS=carol \
  bash "$SCRIPT" --release-url "$RELEASE_URL"
assert_env carol "" "$WORK/token"
if grep -q 'XSTATS_TZ' /etc/xstats/env; then
  fail "blank XSTATS_TZ was written"
fi
grep -q 'kept existing ingest token' "$WORK/stdout" || fail "rerun did not keep the ingest token"
cmp /usr/local/bin/xstats.prev "$WORK/rel1/xstats-linux-$ARCH"
cmp /usr/local/bin/xstats "$WORK/rel2/xstats-linux-$ARCH"
shopt -s nullglob
baks=(/var/lib/xstats/xstats.db.bak-*)
[ "${#baks[@]}" -ge 1 ]
found=0
for bak in "${baks[@]}"; do
  base=$(basename "$bak")
  [[ "$base" =~ ^xstats\.db\.bak-[0-9]{12}$ ]]
  if cmp -s "$bak" "$WORK/db-marker"; then
    found=1
  fi
done
[ "$found" -eq 1 ]
cmp /var/lib/xstats/xstats.db "$WORK/db-marker"
echo "ok: rerun keeps token, xstats.prev, and db backup"
printf 'prev %s\n' "$(sha256sum /usr/local/bin/xstats.prev | awk '{print $1}')"
printf 'bin  %s\n' "$(sha256sum /usr/local/bin/xstats | awk '{print $1}')"

# --- a bad checksum on rerun does not replace the binary or prev ---
prev_sum=$(sha256sum /usr/local/bin/xstats.prev | awk '{print $1}')
bin_sum=$(sha256sum /usr/local/bin/xstats | awk '{print $1}')
cp "$WORK/www/xstats-linux-$ARCH" "$WORK/www/xstats-linux-$ARCH.good"
printf 'tamper\n' >> "$WORK/www/xstats-linux-$ARCH"
chmod a+r "$WORK/www/xstats-linux-$ARCH"
expect_fail "checksum mismatch" env -u XSTATS_TZ \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash "$SCRIPT" --hostname xstats.example.com --release-url "$RELEASE_URL"
[ "$(sha256sum /usr/local/bin/xstats | awk '{print $1}')" = "$bin_sum" ]
[ "$(sha256sum /usr/local/bin/xstats.prev | awk '{print $1}')" = "$prev_sum" ]
assert_env carol "" "$WORK/token"
mv "$WORK/www/xstats-linux-$ARCH.good" "$WORK/www/xstats-linux-$ARCH"
chmod a+r "$WORK/www/xstats-linux-$ARCH"
echo "ok: tampered binary on rerun"

# --- missing SHA256SUMS fails closed ---
mv "$WORK/www/SHA256SUMS" "$WORK/www/SHA256SUMS.good"
expect_fail "download failed" env -u XSTATS_TZ \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash "$SCRIPT" --hostname xstats.example.com --release-url "$RELEASE_URL"
assert_unchanged_binary "$WORK/rel2/xstats-linux-$ARCH"
mv "$WORK/www/SHA256SUMS.good" "$WORK/www/SHA256SUMS"
chmod a+r "$WORK/www/SHA256SUMS"
echo "ok: missing SHA256SUMS"

# --- sums file with no entry for this asset fails closed ---
cp "$WORK/www/SHA256SUMS" "$WORK/www/SHA256SUMS.good"
printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  not-the-asset\n' > "$WORK/www/SHA256SUMS"
chmod a+r "$WORK/www/SHA256SUMS"
expect_fail "no entry" env -u XSTATS_TZ \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash "$SCRIPT" --hostname xstats.example.com --release-url "$RELEASE_URL"
assert_unchanged_binary "$WORK/rel2/xstats-linux-$ARCH"
mv "$WORK/www/SHA256SUMS.good" "$WORK/www/SHA256SUMS"
chmod a+r "$WORK/www/SHA256SUMS"
echo "ok: SHA256SUMS missing this asset"

# --- rotate without a new token does not invent one or drop the old one ---
expect_fail "token-file" env -u XSTATS_TOKEN_FILE -u XSTATS_TZ \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  bash "$SCRIPT" --rotate-token --hostname xstats.example.com --release-url "$RELEASE_URL"
assert_env carol "" "$WORK/token"
assert_unchanged_binary "$WORK/rel2/xstats-linux-$ARCH"
echo "ok: rotate without a token keeps the old one"

# --- token on stdin replaces the saved token when asked ---
expect_ok env -u XSTATS_TOKEN_FILE -u XSTATS_TZ \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  XSTATS_ACCOUNTS=carol \
  bash "$SCRIPT" --rotate-token --hostname xstats.example.com --release-url "$RELEASE_URL" \
  < "$WORK/token2"
assert_env carol "" "$WORK/token2"
if grep -q 'kept existing ingest token' "$WORK/stdout"; then
  fail "stdin rotate reported the old token was kept"
fi
echo "ok: token on stdin"

# --- a new token file replaces the token ---
expect_ok env -u XSTATS_TZ \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  XSTATS_ACCOUNTS=carol \
  bash "$SCRIPT" --rotate-token --hostname xstats.example.com --release-url "$RELEASE_URL"
assert_env carol "" "$WORK/token"
if grep -q 'kept existing ingest token' "$WORK/stdout"; then
  fail "rotate reported the old token was kept"
fi
echo "ok: rotate with a token file"

# --- embedded tzdata accepts Canada/Eastern with no zoneinfo files ---
expect_ok env \
  XSTATS_TZ=Canada/Eastern \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  XSTATS_ACCOUNTS=carol \
  bash "$SCRIPT" --hostname xstats.example.com --release-url "$RELEASE_URL"
assert_env carol Canada/Eastern "$WORK/token"
echo "ok: Canada/Eastern"

# --- hostname prompt suggests xstats.<domain> ---
cat > "$WORK/prompt.py" <<'PY'
import os, pty, select, sys, time
script = os.environ["BOOTSTRAP"]
env = os.environ.copy()
env.pop("XSTATS_DOMAIN", None)
env["XSTATS_TZ"] = ""
env["XSTATS_TOKEN_FILE"] = os.environ["WORK"] + "/token"
env["XSTATS_PASSWORD_FILE"] = os.environ["WORK"] + "/password"
env["XSTATS_ACCOUNTS"] = "carol"
env["XSTATS_RELEASE_URL"] = os.environ["XSTATS_RELEASE_URL"]
pid, fd = pty.fork()
if pid == 0:
    os.execvpe("bash", ["bash", script, "--release-url", env["XSTATS_RELEASE_URL"]], env)
buf = b""
def wait_for(token, timeout):
    global buf
    end = time.time() + timeout
    while time.time() < end:
        if token in buf:
            return True
        ready, _, _ = select.select([fd], [], [], 0.2)
        if not ready:
            continue
        try:
            data = os.read(fd, 4096)
        except OSError:
            return token in buf
        if not data:
            return token in buf
        buf += data
    return token in buf
if not wait_for(b"Domain (example.com):", 20):
    sys.stderr.buffer.write(buf)
    sys.exit(2)
os.write(fd, b"example.com\n")
if not wait_for(b"Dashboard hostname [xstats.example.com]:", 20):
    sys.stderr.buffer.write(buf)
    sys.exit(3)
os.write(fd, b"\n")
end = time.time() + 60
status = None
while time.time() < end:
    ready, _, _ = select.select([fd], [], [], 0.2)
    if ready:
        try:
            data = os.read(fd, 4096)
        except OSError:
            data = b""
        if data:
            buf += data
    wpid, st = os.waitpid(pid, os.WNOHANG)
    if wpid == pid:
        status = st
        break
open(os.environ["WORK"] + "/prompt-transcript", "wb").write(buf)
if status is None:
    sys.exit(4)
if os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0:
    sys.exit(0)
sys.stderr.buffer.write(buf)
sys.exit(os.WEXITSTATUS(status) if os.WIFEXITED(status) else 1)
PY
start_scan
set +e
WORK="$WORK" BOOTSTRAP="$SCRIPT" python3 "$WORK/prompt.py" >"$WORK/stdout" 2>"$WORK/stderr"
RC=$?
set -e
stop_scan
scan_file "$WORK/stdout"
scan_file "$WORK/stderr"
if [ -f "$WORK/prompt-transcript" ]; then
  scan_file "$WORK/prompt-transcript"
fi
if [ "$RC" -ne 0 ]; then
  fail "hostname prompt failed"
fi
grep -q 'xstats.example.com' /etc/caddy/Caddyfile
assert_env carol "" "$WORK/token"
echo "ok: hostname prompt suggests xstats.<domain>"

# --- script alone downloads checksummed companions from the release URL ---
mkdir -p /tmp/alone
cp "$SCRIPT" /tmp/alone/bootstrap.sh
chmod a+r /tmp/alone/bootstrap.sh
publish_rel "$WORK/rel2"
expect_ok env -u XSTATS_TZ \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  XSTATS_ACCOUNTS=carol \
  bash /tmp/alone/bootstrap.sh --hostname xstats.example.com --release-url "$RELEASE_URL"
cmp /etc/systemd/system/xstats.service "$WORK/www/xstats.service"
grep -q 'xstats.example.com' /etc/caddy/Caddyfile
grep -q 'basic_auth' /etc/caddy/Caddyfile
echo "ok: companion files downloaded and verified"

# --- arm64 selection does not execute the foreign binary ---
other=arm64
if [ "$ARCH" = "arm64" ]; then
  other=amd64
fi
mkdir -p "$WORK/fakebin"
cat > "$WORK/fakebin/uname" <<EOF
#!/bin/sh
if [ "\$1" = "-m" ]; then
  if [ "$other" = "arm64" ]; then
    printf 'aarch64\n'
  else
    printf 'x86_64\n'
  fi
  exit 0
fi
exec /usr/bin/uname "\$@"
EOF
chmod 0755 "$WORK/fakebin/uname"
expect_ok env -u XSTATS_TZ \
  PATH="$WORK/fakebin:$PATH" \
  XSTATS_TOKEN_FILE="$WORK/token" \
  XSTATS_PASSWORD_FILE="$WORK/password" \
  XSTATS_ACCOUNTS=carol \
  bash "$SCRIPT" --hostname xstats.example.com --release-url "$RELEASE_URL"
cmp "/usr/local/bin/xstats" "$WORK/rel2/xstats-linux-$other"
if cmp -s "/usr/local/bin/xstats" "$WORK/rel2/xstats-linux-$ARCH"; then
  fail "arch override still installed the host binary"
fi
echo "ok: arch $other"

echo "container ok"
