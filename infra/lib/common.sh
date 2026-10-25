#!/bin/bash

INFRA_LIB=1
INFRA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INFRA_REPO="$(cd "$INFRA_DIR/.." && pwd)"
INFRA_TS="${INFRA_TS:-$(date -u +%Y%m%dT%H%M%SZ)}"
INFRA_APPLY="${INFRA_APPLY:-0}"
INFRA_PRINT_RO="${INFRA_PRINT_RO:-1}"
INFRA_ACTION="${INFRA_ACTION:-deploy}"
INFRA_ONLY=()
INFRA_LIST=0

infra_q() {
  printf '%q' "$1"
}

infra_log() {
  echo "infra[${INFRA_COMPONENT:-${INFRA_HOST:-infra}}]: $*"
}

infra_die() {
  infra_log "$*" >&2
  exit 1
}

infra_fn() {
  printf '%s' "${1//-/_}"
}

infra_is_local() {
  [ "${SSH_TARGET:-}" = local ]
}

infra_target() {
  if infra_is_local; then
    printf 'local'
  else
    printf '%s%s' "${SSH_TARGET:-?}" "${SSH_PORT:+:$SSH_PORT}"
  fi
}

infra_ssh_opts() {
  INFRA_SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=15 -o StrictHostKeyChecking=accept-new)
  if [ -n "${SSH_KEY:-}" ]; then
    INFRA_SSH_OPTS+=(-o IdentitiesOnly=yes -i "$SSH_KEY")
  fi
  if [ -n "${SSH_PORT:-}" ]; then
    INFRA_SSH_OPTS+=(-p "$SSH_PORT")
  fi
}

infra_indent() {
  sed 's/^/    /'
}

infra_wrap_root() {
  if [ "$2" = root ] && [ -n "${INFRA_SUDO:-}" ]; then
    printf '%s bash -c %q' "$INFRA_SUDO" "$1"
  else
    printf '%s' "$1"
  fi
}

infra_exec() {
  local kind="$1" who="$2" input="$3" cmd print=0
  cmd="$(infra_wrap_root "$4" "$who")"
  if [ "$kind" = rw ] && [ "$INFRA_APPLY" != 1 ]; then
    print=1
  fi
  if [ "$kind" = ro ] && [ "$INFRA_PRINT_RO" = 1 ]; then
    print=1
  fi
  if [ "$print" = 1 ]; then
    if [ "$who" = root ] && [ -n "${INFRA_SUDO:-}" ]; then
      infra_log "[dry-run] $kind on $(infra_target) as root ($INFRA_SUDO bash -c)${input:+ (stdin $input)}:" >&2
    else
      infra_log "[dry-run] $kind on $(infra_target)${input:+ (stdin $input)}:" >&2
    fi
    printf '%s\n' "$4" | infra_indent >&2
    return 0
  fi
  if [ -z "$input" ]; then
    input=/dev/null
  fi
  if infra_is_local; then
    bash -c "$cmd" <"$input"
  else
    [ -n "${SSH_TARGET:-}" ] || infra_die "SSH_TARGET is not set"
    infra_ssh_opts
    ssh "${INFRA_SSH_OPTS[@]}" "$SSH_TARGET" "$cmd" <"$input"
  fi
}

infra_ro() {
  infra_exec ro user "" "$1"
}

infra_ro_root() {
  infra_exec ro root "" "$1"
}

infra_rw() {
  infra_exec rw user "" "$1"
}

infra_rw_root() {
  infra_exec rw root "" "$1"
}

infra_rw_root_stdin() {
  infra_exec rw root "$1" "$2"
}

infra_local_rw() {
  local desc="$1"
  shift
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "[dry-run] local: $desc:"
    printf '%q ' "$@" | sed 's/ $//' | infra_indent
    echo
    return 0
  fi
  infra_log "local: $desc"
  "$@"
}

infra_sha256() {
  sha256sum "$1" | cut -d' ' -f1
}

infra_upload() {
  local src="$1" dest="$2" mode="${3:-0644}" who="${4:-root}" sha got
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "[dry-run] upload $src -> $(infra_target):$dest (mode $mode, as $who, sha256 checked after)"
    return 0
  fi
  [ -f "$src" ] || infra_die "upload: no file at $src"
  sha="$(infra_sha256 "$src")"
  infra_exec rw "$who" "$src" "install -d -m 0755 $(infra_q "$(dirname "$dest")") && cat > $(infra_q "$dest.partial") && chmod $mode $(infra_q "$dest.partial") && mv -f $(infra_q "$dest.partial") $(infra_q "$dest")"
  got="$(infra_exec ro "$who" "" "sha256sum $(infra_q "$dest") | cut -d' ' -f1")"
  [ "$got" = "$sha" ] || infra_die "upload: $(infra_target):$dest is $got, not $sha"
  infra_log "uploaded $src -> $(infra_target):$dest ($sha)"
}

infra_file_install() {
  local dest="$1" mode="$2" content="$3"
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "[dry-run] write $(infra_target):$dest (mode $mode, the old file kept as $dest.infra-bak.$INFRA_TS):"
    printf '%s\n' "$content" | infra_indent
    return 0
  fi
  infra_rw_root_stdin <(printf '%s\n' "$content") "set -e
install -d -m 0755 $(infra_q "$(dirname "$dest")")
if [ -f $(infra_q "$dest") ]; then cp -a $(infra_q "$dest") $(infra_q "$dest.infra-bak.$INFRA_TS"); fi
cat > $(infra_q "$dest.partial")
chmod $mode $(infra_q "$dest.partial")
mv -f $(infra_q "$dest.partial") $(infra_q "$dest")"
  infra_log "wrote $(infra_target):$dest"
}

infra_unit_install() {
  infra_file_install "/etc/systemd/system/$1" 0644 "$2"
  infra_rw_root "systemctl daemon-reload"
}

infra_unit_dropin() {
  infra_file_install "/etc/systemd/system/$1.d/$2" 0644 "$3"
  infra_rw_root "systemctl daemon-reload"
}

infra_unit_restart() {
  infra_rw_root "systemctl daemon-reload && systemctl restart $(infra_q "$1")"
}

infra_unit_state() {
  local u
  for u in "$@"; do
    infra_ro "printf '%s: ' $(infra_q "$u"); systemctl show $(infra_q "$u") -p ActiveState -p SubState -p UnitFileState -p MainPID -p ExecMainStartTimestamp --no-pager | paste -sd' '"
  done
}

infra_caddy_site_install() {
  local name="$1" content="$2" sites="${INFRA_CADDY_SITES:-/etc/caddy/sites}" caddyfile="${INFRA_CADDYFILE:-/etc/caddy/Caddyfile}" bak
  bak="${INFRA_BACKUP_ROOT:-/var/backups/infra}/caddy/$INFRA_TS"
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "[dry-run] caddy site $(infra_target):$sites/$name.caddy (backup $bak, caddy validate, restore on failure, reload):"
    printf '%s\n' "$content" | infra_indent
    return 0
  fi
  infra_rw_root_stdin <(printf '%s\n' "$content") "set -e
sites=$(infra_q "$sites")
f=\"\$sites\"/$(infra_q "$name.caddy")
bak=$(infra_q "$bak")
cf=$(infra_q "$caddyfile")
install -d -m 0755 \"\$sites\" \"\$bak\"
cp -a \"\$cf\" \"\$bak/Caddyfile\"
had=0
if [ -f \"\$f\" ]; then cp -a \"\$f\" \"\$bak/\"; had=1; fi
cat > \"\$f.partial\"
mv -f \"\$f.partial\" \"\$f\"
if ! grep -qF \"import \$sites/*.caddy\" \"\$cf\"; then printf '\\nimport %s/*.caddy\\n' \"\$sites\" >> \"\$cf\"; fi
if ! caddy validate --config \"\$cf\" --adapter caddyfile >\"\$bak/validate.log\" 2>&1; then
  cp -a \"\$bak/Caddyfile\" \"\$cf\"
  if [ \"\$had\" = 1 ]; then cp -a \"\$bak/\$(basename \"\$f\")\" \"\$f\"; else rm -f \"\$f\"; fi
  echo \"caddy validate failed; restored from \$bak\" >&2
  tail -n 20 \"\$bak/validate.log\" >&2
  exit 1
fi
systemctl reload caddy || systemctl restart caddy
echo \"caddy site \$f installed (backup \$bak)\""
}

infra_caddy_restore() {
  local bak="$1" caddyfile="${INFRA_CADDYFILE:-/etc/caddy/Caddyfile}"
  infra_rw_root "set -e
cp -a $(infra_q "$bak/Caddyfile") $(infra_q "$caddyfile")
caddy validate --config $(infra_q "$caddyfile") --adapter caddyfile >/dev/null
systemctl reload caddy || systemctl restart caddy
echo restored caddy from $(infra_q "$bak")"
}

infra_caddy_sites() {
  infra_ro_root "for f in ${INFRA_CADDYFILE:-/etc/caddy/Caddyfile} ${INFRA_CADDY_SITES:-/etc/caddy/sites}/*.caddy; do [ -f \"\$f\" ] || continue; echo \"\$f:\"; grep -nE '^[^[:space:]#(][^{]*\\{[[:space:]]*\$|reverse_proxy|^[[:space:]]*root[[:space:]]' \"\$f\" | grep -v api_token | sed 's/^/  /'; done"
}

infra_fetch() {
  local url="$1" body code
  if [ "$INFRA_PRINT_RO" = 1 ]; then
    infra_log "[dry-run] ro local: GET $url" >&2
    printf 'dry-run -\n'
    return 0
  fi
  command -v curl >/dev/null || infra_die "curl is required for the health checks"
  body="$(curl -sS --max-time 15 -w '\n%{http_code}' "$url" 2>/dev/null || true)"
  code="${body##*$'\n'}"
  body="${body%$'\n'*}"
  printf '%s %s\n' "$code" "$(printf '%s' "$body" | tr -d '\r\n' | cut -c1-160)"
}

infra_health_wait() {
  local url="$1" want="$2" tries="${3:-30}" delay="${4:-2}" got=""
  if [ "$INFRA_PRINT_RO" = 1 ]; then
    infra_log "[dry-run] wait up to $((tries * delay)) s for GET $url to answer '$want'"
    return 0
  fi
  for _ in $(seq 1 "$tries"); do
    got="$(infra_fetch "$url")"
    case "$got" in
    $want*)
      infra_log "GET $url: ${got:0:120}"
      return 0
      ;;
    esac
    sleep "$delay"
  done
  infra_log "GET $url never answered '$want' (last: ${got:0:160})"
  return 1
}

infra_report_urls() {
  local url
  for url in "$@"; do
    infra_log "GET $url: $(infra_fetch "$url")"
  done
}

infra_release_binary() {
  local platform="${1:-linux/amd64}" name commit release
  name="socialweb-computer-${platform%/*}-${platform#*/}"
  if [ -n "${INFRA_BINARY:-}" ]; then
    if [ -z "${INFRA_SHA256:-}" ] && [ -f "$INFRA_BINARY" ]; then
      INFRA_SHA256="$(infra_sha256 "$INFRA_BINARY")"
    fi
    [ -n "${INFRA_SHA256:-}" ] || INFRA_SHA256="<sha256 of $INFRA_BINARY>"
    return 0
  fi
  commit="${INFRA_COMMIT:-$(git -C "$INFRA_REPO" rev-parse HEAD)}"
  release="$HOME/.tmp/socialweb-computer-build/release/$commit"
  if ! awk -v n="$name" '$2 == n { found = 1 } END { exit !found }' "$release/SHA256SUMS" 2>/dev/null; then
    infra_local_rw "build the release binary of $commit for $platform" "$INFRA_REPO/scripts/build-socialweb-computer.sh" --platform "$platform" --commit "$commit"
  fi
  INFRA_BINARY="$release/$name"
  INFRA_SHA256="$(awk -v n="$name" '$2 == n { print $1 }' "$release/SHA256SUMS" 2>/dev/null || true)"
  if [ -z "$INFRA_SHA256" ]; then
    [ "$INFRA_APPLY" != 1 ] || infra_die "no sha256 for $name in $release/SHA256SUMS"
    INFRA_SHA256="<sha256 of $name at $commit>"
  fi
  if [ "$INFRA_APPLY" = 1 ]; then
    [ "$(infra_sha256 "$INFRA_BINARY")" = "$INFRA_SHA256" ] || infra_die "$INFRA_BINARY is not $INFRA_SHA256"
  fi
}

infra_stub_status() {
  local list=()
  read -r -a list <<<"$1"
  shift
  if [ "${#list[@]}" -gt 0 ]; then
    infra_unit_state "${list[@]}"
  fi
  infra_report_urls "$@"
}

infra_stub_todo() {
  infra_log "not managed yet: $*"
  if [ "$INFRA_APPLY" = 1 ]; then
    infra_die "refusing --apply on an unmanaged component; see infra/README.md and docs/infra/inventory.md"
  fi
}

infra_usage() {
  cat <<EOF
usage: $1 [--dry-run | --apply] [--status | --rollback]${2:+ [--only <component>]... [--list]}

  (default)     the deploy, printed only (dry-run): no ssh, no build, nothing changes
  --apply       run it: build, ship, install, restart, health check
  --status      read-only: unit state, versions, public health (runs ssh unless --dry-run is given too)
  --rollback    print the rollback; with --apply, run it
  --dry-run     print every command, read-only ones included, and run none
${2:+  --only <c>     only this component (repeat, or comma separated)
  --list        print the components of this host
}
env: SSH_TARGET (user@host, or local), SSH_KEY (identity file; empty uses ~/.ssh/config), SSH_PORT,
     INFRA_SUDO (prefix for root commands, e.g. "sudo -n"), and each component's own (infra/README.md)
EOF
}

infra_parse_args() {
  local host="$1" explicit_dry=0 parts=()
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
    --apply) INFRA_APPLY=1 ;;
    --dry-run) explicit_dry=1 ;;
    --status) INFRA_ACTION=status ;;
    --rollback) INFRA_ACTION=rollback ;;
    --deploy) INFRA_ACTION=deploy ;;
    --list)
      [ -n "$host" ] || { infra_usage "$0" >&2; exit 2; }
      INFRA_LIST=1
      ;;
    --only)
      [ -n "$host" ] && [ $# -ge 2 ] || { infra_usage "$0" "$host" >&2; exit 2; }
      IFS=, read -r -a parts <<<"$2"
      INFRA_ONLY+=("${parts[@]}")
      shift
      ;;
    --only=*)
      [ -n "$host" ] || { infra_usage "$0" >&2; exit 2; }
      IFS=, read -r -a parts <<<"${1#--only=}"
      INFRA_ONLY+=("${parts[@]}")
      ;;
    -h | --help)
      infra_usage "$0" "$host"
      exit 0
      ;;
    *)
      infra_usage "$0" "$host" >&2
      exit 2
      ;;
    esac
    shift
  done
  if [ "$explicit_dry" = 1 ]; then
    INFRA_APPLY=0
  fi
  if [ "$explicit_dry" = 1 ] || { [ "$INFRA_ACTION" != status ] && [ "$INFRA_APPLY" != 1 ]; }; then
    INFRA_PRINT_RO=1
  else
    INFRA_PRINT_RO=0
  fi
  if [ "$INFRA_ACTION" = status ]; then
    INFRA_APPLY=0
  fi
}

infra_mode_line() {
  local mode=dry-run
  if [ "$INFRA_APPLY" = 1 ]; then
    mode=apply
  elif [ "$INFRA_ACTION" = status ] && [ "$INFRA_PRINT_RO" = 0 ]; then
    mode=read-only
  fi
  infra_log "$INFRA_ACTION ($mode) on $(infra_target)"
}

infra_component_main() {
  local name="$1" target="$2" key="$3" port="$4" sudo="$5" fn
  shift 5
  INFRA_COMPONENT="$name"
  SSH_TARGET="${SSH_TARGET:-$target}"
  SSH_KEY="${SSH_KEY-$key}"
  SSH_PORT="${SSH_PORT-$port}"
  INFRA_SUDO="${INFRA_SUDO-$sudo}"
  infra_parse_args "" "$@"
  fn="$(infra_fn "$name")"
  infra_mode_line
  "${fn}_${INFRA_ACTION}"
}

infra_host_main() {
  local c fn rc=0 selected=()
  infra_parse_args "$INFRA_HOST" "$@"
  if [ "$INFRA_LIST" = 1 ]; then
    printf '%s\n' "${INFRA_COMPONENTS[@]}"
    return 0
  fi
  if [ "${#INFRA_ONLY[@]}" -gt 0 ]; then
    for c in "${INFRA_ONLY[@]}"; do
      case " ${INFRA_COMPONENTS[*]} " in
      *" $c "*) selected+=("$c") ;;
      *) infra_die "$c is not a component of $INFRA_HOST (${INFRA_COMPONENTS[*]})" ;;
      esac
    done
  else
    selected=("${INFRA_COMPONENTS[@]}")
  fi
  for c in "${selected[@]}"; do
    INFRA_COMPONENT="$c"
    fn="$(infra_fn "$c")"
    infra_mode_line
    if [ "$INFRA_ACTION" = status ]; then
      "${fn}_status" || {
        infra_log "status failed"
        rc=1
      }
    else
      "${fn}_${INFRA_ACTION}"
    fi
  done
  INFRA_COMPONENT=""
  infra_log "$INFRA_HOST: done (${selected[*]})"
  return "$rc"
}
