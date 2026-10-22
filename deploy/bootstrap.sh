#!/usr/bin/env bash
# Install xstats on a fresh Ubuntu server from a GitHub release.
# Run as root on the machine, or over SSH. Example:
#
#   sudo bash deploy/bootstrap.sh --hostname xstats.example.com \
#     --token-file ./ingest_token --password-file ./dash_pass
#
# The ingest token and the dashboard password come from a file, a prompt,
# or (when this file is executed directly) a single stdin line. They are
# never read from arguments.
#
# XSTATS_RELEASE_URL or --release-url replaces the download base so this
# can be tested without a published release. The default is
# https://github.com/scott/xstats/releases/latest/download
# with assets xstats-linux-amd64, xstats-linux-arm64, xstats.service,
# Caddyfile.tmpl, set-auth.sh, and SHA256SUMS.
#
# set-auth.sh, Caddyfile.tmpl, and xstats.service are used from this
# directory when they sit beside the script. Otherwise they are downloaded
# from the same release URL as the binary and checked against SHA256SUMS.

# Refuse xtrace before any secret is read. bash -x would print expansions.
case $- in
  *x*) printf 'refusing to run with xtrace enabled\n' >&2; exit 1 ;;
esac
set +x
set -euo pipefail

usage() {
  cat <<'EOF'
usage: bootstrap.sh [--hostname HOST] [--accounts LIST] [--auth-user USER]
                    [--tz ZONE] [--token-file PATH] [--password-file PATH]
                    [--release-url URL] [--rotate-token]

Hostname and accounts may also be set with XSTATS_DOMAIN and XSTATS_ACCOUNTS.
XSTATS_AUTH_USER defaults to admin. XSTATS_TZ and XSTATS_START_DATES are optional.

The ingest token and the dashboard password come from --token-file /
XSTATS_TOKEN_FILE, --password-file / XSTATS_PASSWORD_FILE, a prompt, or
stdin. They are never accepted as arguments.

With no hostname, a terminal prompt asks for the domain and suggests
xstats.<domain>.

When XSTATS_TZ is set, it is trimmed and checked before the install
changes the system. When it is unset or blank, it is left out of
/etc/xstats/env.

XSTATS_START_DATES is not a flag. When it is set, it is trimmed, checked
with the binary being installed, and written to /etc/xstats/env. When it
is unset or blank, the line is left out. A bad value stops the install
before the system is changed. The service reads it at startup, so a later
edit needs systemctl restart xstats.

A rerun keeps /usr/local/bin/xstats.prev and a database backup, and keeps
an existing ingest token unless --token-file or --rotate-token is given.
EOF
}

trim_space() {
  local s=$1
  s=${s//$'\r'/}
  s=${s#"${s%%[![:space:]]*}"}
  s=${s%"${s##*[![:space:]]}"}
  printf '%s' "$s"
}

die() {
  printf '%s\n' "$1" >&2
  exit 1
}

usage_err() {
  printf '%s\n' "$1" >&2
  exit 2
}

valid_hostname() {
  local s=$1
  [ "${#s}" -le 253 ] || return 1
  [[ "$s" =~ ^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$ ]]
}

valid_user() {
  [[ "$1" =~ ^[A-Za-z0-9._-]{1,64}$ ]]
}

require_oneline() {
  local label=$1
  local value=$2
  case "$value" in
    *$'\n'*) die "$label must be a single line" ;;
  esac
  [ -n "$value" ] || die "$label is empty"
}

read_secret_file() {
  local path=$1
  local value
  [ -f "$path" ] && [ -r "$path" ] || die "cannot read secret file: $path"
  value=$(cat -- "$path")
  printf '%s' "$value"
}

# Print one stdin line. Return 1 on EOF with no data.
read_piped_line() {
  local line=
  if IFS= read -r line || [ -n "$line" ]; then
    printf '%s' "$line"
    return 0
  fi
  return 1
}

prompt_secret() {
  local prompt=$1
  local value
  [ -r /dev/tty ] || return 1
  printf '%s' "$prompt" >/dev/tty
  if ! IFS= read -rs value </dev/tty; then
    return 1
  fi
  printf '\n' >/dev/tty
  printf '%s' "$value"
}

prompt_line() {
  local prompt=$1
  local value
  [ -r /dev/tty ] || return 1
  printf '%s' "$prompt" >/dev/tty
  if ! IFS= read -r value </dev/tty; then
    return 1
  fi
  value=${value//$'\r'/}
  printf '%s' "$value"
}

download() {
  local url=$1
  local dest=$2
  rm -f "$dest.partial"
  if ! curl -fsSL --retry 2 --retry-delay 1 -o "$dest.partial" "$url"; then
    rm -f "$dest.partial"
    die "download failed: $url"
  fi
  if [ ! -s "$dest.partial" ]; then
    rm -f "$dest.partial"
    die "download was empty: $url"
  fi
  mv "$dest.partial" "$dest"
}

verify_checksum() {
  local file=$1
  local sums=$2
  local name=$3
  local stripped matches count expected actual pattern
  if [ ! -s "$file" ]; then
    die "missing release file: $name"
  fi
  if [ ! -s "$sums" ]; then
    die "missing or empty SHA256SUMS"
  fi
  stripped=$(tr -d '\r' < "$sums")
  pattern="^[0-9a-fA-F]{64}[[:space:]]+\\*?${name}\$"
  matches=$(printf '%s\n' "$stripped" | grep -E "$pattern" || true)
  if [ -z "$matches" ]; then
    die "SHA256SUMS has no entry for $name"
  fi
  count=$(printf '%s\n' "$matches" | wc -l | tr -d ' ')
  if [ "$count" != "1" ]; then
    die "SHA256SUMS has multiple entries for $name"
  fi
  expected=$(printf '%s\n' "$matches" | awk '{print tolower($1)}')
  actual=$(sha256sum -- "$file" | awk '{print tolower($1)}')
  if [ -z "$expected" ] || [ -z "$actual" ] || [ "$expected" != "$actual" ]; then
    printf 'checksum mismatch for %s\n' "$name" >&2
    exit 1
  fi
}

detect_arch() {
  local machine
  machine=$(uname -m)
  case "$machine" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) die "unsupported architecture: $machine" ;;
  esac
}

ensure_curl() {
  if command -v curl >/dev/null 2>&1 && command -v sha256sum >/dev/null 2>&1; then
    return 0
  fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq ca-certificates curl coreutils
}

# validate_start_dates runs the release binary so a bad pair fails the
# same way a later service start would. The binary is still only in the
# temp dir. Output names the pair.
validate_start_dates() {
  local value=$1
  local bin=$2
  local err=
  rm -f "$tmp/startcheck.db" "$tmp/startcheck.db-wal" "$tmp/startcheck.db-shm"
  if ! err=$(XSTATS_START_DATES="$value" "$bin" ingest --db "$tmp/startcheck.db" 2>&1 >/dev/null); then
    if [ -n "$err" ]; then
      printf '%s\n' "$err" >&2
    else
      printf 'invalid XSTATS_START_DATES %q\n' "$value" >&2
    fi
    return 1
  fi
  rm -f "$tmp/startcheck.db" "$tmp/startcheck.db-wal" "$tmp/startcheck.db-shm"
}

# validate_tz runs the release binary, which uses the same embedded zone
# data as a normal start. The binary is still only in the temp dir.
validate_tz() {
  local zone=$1
  local bin=$2
  local err=
  rm -f "$tmp/tzcheck.db" "$tmp/tzcheck.db-wal" "$tmp/tzcheck.db-shm"
  if ! err=$(XSTATS_TZ="$zone" "$bin" ingest --db "$tmp/tzcheck.db" 2>&1 >/dev/null); then
    printf 'invalid XSTATS_TZ %q\n' "$zone" >&2
    if [ -n "$err" ]; then
      printf '%s\n' "$err" >&2
    fi
    return 1
  fi
  rm -f "$tmp/tzcheck.db" "$tmp/tzcheck.db-wal" "$tmp/tzcheck.db-shm"
}

stage_companion_files() {
  if [ -n "$script_dir" ] \
    && [ -f "$script_dir/set-auth.sh" ] \
    && [ -f "$script_dir/Caddyfile.tmpl" ] \
    && [ -f "$script_dir/xstats.service" ]; then
    unit=$script_dir/xstats.service
    tmpl=$script_dir/Caddyfile.tmpl
    auth=$script_dir/set-auth.sh
    return 0
  fi
  local name
  for name in xstats.service Caddyfile.tmpl set-auth.sh; do
    download "$release_url/$name" "$tmp/$name"
    verify_checksum "$tmp/$name" "$tmp/SHA256SUMS" "$name"
  done
  unit=$tmp/xstats.service
  tmpl=$tmp/Caddyfile.tmpl
  auth=$tmp/set-auth.sh
}

install_caddy() {
  if command -v caddy >/dev/null 2>&1; then
    return 0
  fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq && apt-get install -y -qq caddy
}

read_existing_token() {
  existing_token=
  if [ ! -f /etc/xstats/env ]; then
    return 0
  fi
  [ -r /etc/xstats/env ] || die "cannot read /etc/xstats/env"
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      XSTATS_INGEST_TOKEN=*) existing_token=${line#XSTATS_INGEST_TOKEN=} ;;
    esac
  done < /etc/xstats/env
}

load_password() {
  local piped
  password=
  password_from_stdin=0
  if [ -n "$password_file" ]; then
    password=$(read_secret_file "$password_file")
  elif [ "$script_is_file" -eq 1 ] && [ ! -t 0 ]; then
    if piped=$(read_piped_line); then
      password=$piped
      password_from_stdin=1
    fi
    unset piped
  fi
  if [ -z "$password" ]; then
    password_from_stdin=0
    if piped=$(prompt_secret "Dashboard password (will not echo): "); then
      password=$piped
      unset piped
    else
      die "set --password-file or XSTATS_PASSWORD_FILE, or run from a terminal"
    fi
  fi
  require_oneline "dashboard password" "$password"
}

load_token() {
  local piped
  token=
  kept_token=0
  if [ -n "$token_file" ]; then
    token=$(read_secret_file "$token_file")
  elif [ "$rotate_token" -eq 0 ] && [ -n "$existing_token" ]; then
    token=$existing_token
    kept_token=1
  elif [ "$script_is_file" -eq 1 ] && [ ! -t 0 ] && [ "$password_from_stdin" -eq 0 ]; then
    if piped=$(read_piped_line); then
      token=$piped
    fi
    unset piped
  fi
  if [ -z "$token" ]; then
    if piped=$(prompt_secret "Ingest token (will not echo): "); then
      token=$piped
      unset piped
    else
      die "set --token-file or XSTATS_TOKEN_FILE, or run from a terminal"
    fi
  fi
  require_oneline "ingest token" "$token"
}

resolve_hostname() {
  local domain= entered= suggestion=
  if [ -n "$hostname" ]; then
    return 0
  fi
  if ! domain=$(prompt_line "Domain (example.com): "); then
    die "set --hostname or XSTATS_DOMAIN (for example xstats.example.com)"
  fi
  domain=$(trim_space "$domain")
  [ -n "$domain" ] || die "domain is empty"
  suggestion="xstats.${domain}"
  if ! valid_hostname "$suggestion"; then
    die "invalid domain"
  fi
  if ! entered=$(prompt_line "Dashboard hostname [${suggestion}]: "); then
    die "failed to read dashboard hostname"
  fi
  entered=$(trim_space "$entered")
  if [ -n "$entered" ]; then
    hostname=$entered
  else
    hostname=$suggestion
  fi
}

write_env() {
  install -d -m 0750 -o root -g xstats /etc/xstats
  tmpenv=$(mktemp /etc/xstats/.env.XXXXXX)
  {
    printf 'XSTATS_INGEST_TOKEN=%s\n' "$token"
    printf 'XSTATS_ACCOUNTS=%s\n' "$accounts"
    if [ -n "$tz" ]; then
      printf 'XSTATS_TZ=%s\n' "$tz"
    fi
    if [ -n "$start_dates" ]; then
      printf 'XSTATS_START_DATES=%s\n' "$start_dates"
    fi
  } > "$tmpenv"
  chown root:xstats "$tmpenv"
  chmod 0640 "$tmpenv"
  mv -f "$tmpenv" /etc/xstats/env
  tmpenv=
}

apply_auth() {
  install -d -m 0755 /etc/caddy
  install -m 0644 "$tmpl" /tmp/Caddyfile.tmpl
  install -m 0755 "$auth" /tmp/set-auth.sh
  # Keep the password on the pipe. env -u drops inherited xtrace options.
  printf '%s\n' "$password" | env -u SHELLOPTS bash /tmp/set-auth.sh "$hostname" "$auth_user"
}

tmp=
tmpenv=
cleanup() {
  [ -n "$tmpenv" ] && rm -f "$tmpenv"
  [ -n "$tmp" ] && rm -rf "$tmp"
  return 0
}
trap cleanup EXIT

script_dir=
script_is_file=0
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
  script_is_file=1
  script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fi

hostname=${XSTATS_DOMAIN:-}
accounts=${XSTATS_ACCOUNTS:-}
auth_user=${XSTATS_AUTH_USER:-admin}
token_file=${XSTATS_TOKEN_FILE:-}
password_file=${XSTATS_PASSWORD_FILE:-}
release_url=${XSTATS_RELEASE_URL:-https://github.com/scott/xstats/releases/latest/download}
rotate_token=0
tz=
start_dates=
if [ -n "${XSTATS_TZ+x}" ]; then
  tz=$(trim_space "$XSTATS_TZ")
fi
if [ -n "${XSTATS_START_DATES+x}" ]; then
  start_dates=$(trim_space "$XSTATS_START_DATES")
fi

while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --hostname)
      [ $# -ge 2 ] || usage_err "missing value for $1"
      hostname=$2
      shift 2
      ;;
    --hostname=*)
      hostname=${1#--hostname=}
      shift
      ;;
    --accounts)
      [ $# -ge 2 ] || usage_err "missing value for $1"
      accounts=$2
      shift 2
      ;;
    --accounts=*)
      accounts=${1#--accounts=}
      shift
      ;;
    --auth-user)
      [ $# -ge 2 ] || usage_err "missing value for $1"
      auth_user=$2
      shift 2
      ;;
    --auth-user=*)
      auth_user=${1#--auth-user=}
      shift
      ;;
    --tz)
      [ $# -ge 2 ] || usage_err "missing value for $1"
      tz=$(trim_space "$2")
      shift 2
      ;;
    --tz=*)
      tz=$(trim_space "${1#--tz=}")
      shift
      ;;
    --token-file)
      [ $# -ge 2 ] || usage_err "missing value for $1"
      token_file=$2
      shift 2
      ;;
    --token-file=*)
      token_file=${1#--token-file=}
      shift
      ;;
    --password-file)
      [ $# -ge 2 ] || usage_err "missing value for $1"
      password_file=$2
      shift 2
      ;;
    --password-file=*)
      password_file=${1#--password-file=}
      shift
      ;;
    --release-url)
      [ $# -ge 2 ] || usage_err "missing value for $1"
      release_url=$2
      shift 2
      ;;
    --release-url=*)
      release_url=${1#--release-url=}
      shift
      ;;
    --rotate-token)
      rotate_token=1
      shift
      ;;
    --password|--password=*|--token|--token=*|--ingest-token|--ingest-token=*|--basic-auth-password|--basic-auth-password=*)
      usage_err "pass secrets via a file or a prompt, not as arguments"
      ;;
    --)
      shift
      if [ $# -gt 0 ]; then
        usage_err "unexpected argument: $1"
      fi
      break
      ;;
    -*)
      usage_err "unknown flag: $1"
      ;;
    *)
      usage_err "unexpected argument: $1"
      ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "bootstrap.sh must run as root"
[ -n "$release_url" ] || die "release URL is empty"
release_url=${release_url%/}
hostname=$(trim_space "$hostname")

case "$accounts" in
  *$'\n'*) die "XSTATS_ACCOUNTS must be a single line" ;;
esac
valid_user "$auth_user" || die "invalid auth user"
if [ -n "$tz" ]; then
  case "$tz" in
    *$'\n'*|*'='*) die "invalid XSTATS_TZ" ;;
  esac
fi
if [ -n "$start_dates" ]; then
  case "$start_dates" in
    *$'\n'*|*'='*) die "invalid XSTATS_START_DATES" ;;
  esac
fi

resolve_hostname
valid_hostname "$hostname" || die "invalid hostname"

# Secrets, the zone, and start dates are settled before the install writes
# anywhere except a temp download (and curl, if the base image has neither
# curl nor sha256sum). An invalid zone, a bad start date, or a bad checksum
# stops first.
read_existing_token
load_password
load_token

tmp=$(mktemp -d)
ensure_curl
arch=$(detect_arch)
asset_name="xstats-linux-${arch}"
download "$release_url/$asset_name" "$tmp/$asset_name"
download "$release_url/SHA256SUMS" "$tmp/SHA256SUMS"
verify_checksum "$tmp/$asset_name" "$tmp/SHA256SUMS" "$asset_name"
chmod 0755 "$tmp/$asset_name"
printf 'checksum ok for %s\n' "$asset_name"

if [ -n "$tz" ]; then
  validate_tz "$tz" "$tmp/$asset_name" || exit 1
fi
if [ -n "$start_dates" ]; then
  validate_start_dates "$start_dates" "$tmp/$asset_name" || exit 1
fi

stage_companion_files

id xstats >/dev/null 2>&1 || useradd --system --home /var/lib/xstats --shell /usr/sbin/nologin xstats
install -d -o xstats -g xstats /var/lib/xstats /var/lib/xstats/inbox
if [ -f /usr/local/bin/xstats ]; then
  cp -p /usr/local/bin/xstats /usr/local/bin/xstats.prev
fi
if [ -f /var/lib/xstats/xstats.db ]; then
  cp -p /var/lib/xstats/xstats.db "/var/lib/xstats/xstats.db.bak-$(date +%Y%m%d%H%M)"
fi
install -m 0755 "$tmp/$asset_name" /usr/local/bin/xstats
install -m 0644 "$unit" /etc/systemd/system/xstats.service

install_caddy
write_env
if [ "$kept_token" -eq 1 ]; then
  printf 'kept existing ingest token\n'
fi

systemctl daemon-reload
systemctl enable xstats
systemctl restart xstats
systemctl enable caddy
apply_auth
systemctl start caddy
ufw allow OpenSSH >/dev/null 2>&1 || true
ufw allow 80,443/tcp >/dev/null 2>&1 || true

printf 'Installed. Check: curl -u %s https://%s/api/data\n' "$auth_user" "$hostname"
