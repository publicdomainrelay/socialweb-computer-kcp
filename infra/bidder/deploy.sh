#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

bidder_defaults() {
  BIDDER_ACCOUNT="${BIDDER_ACCOUNT:-swc-bidder}"
  BIDDER_STATE="${BIDDER_STATE:-/var/lib/socialweb-computer/$BIDDER_ACCOUNT}"
  BIDDER_PLATFORM="${BIDDER_PLATFORM:-linux/amd64}"
  BIDDER_HANDLE="${BIDDER_HANDLE:-}"
  BIDDER_ALLOW_DEV_BINARY="${BIDDER_ALLOW_DEV_BINARY:-}"
  BIDDER_STAGE_DIR="${BIDDER_STAGE_DIR:-/var/tmp/swc-release}"
}

bidder_as_account() {
  printf 'runuser -u %q -- env XDG_RUNTIME_DIR=/run/user/$(id -u %q) CONFIG_PATH_SOCIALWEB_COMPUTER=%q' \
    "$BIDDER_ACCOUNT" "$BIDDER_ACCOUNT" "$BIDDER_STATE/config.json"
}

bidder_status() {
  local st
  bidder_defaults
  st="$(infra_q "$BIDDER_STATE")"
  infra_ro_root "rec=/var/lib/socialweb-computer/install-$(infra_q "$BIDDER_ACCOUNT").json
if [ -f \"\$rec\" ]; then echo \"install record: \$rec\"; else echo \"install record: none (not installed)\"; exit 0; fi
echo \"bin/current: \$(readlink $st/bin/current)\"
echo \"bin/previous: \$(readlink $st/bin/previous 2>/dev/null || echo none)\"
$st/bin/current/socialweb-computer version 2>&1 | head -3 | sed 's/^/version: /'
systemctl --user -M $(infra_q "$BIDDER_ACCOUNT")@ list-units --all --no-legend --plain 'socialweb-computer-*' 2>&1 | cut -c1-160"
}

bidder_deploy() {
  local staged dev="" install="" sha
  bidder_defaults
  INFRA_BINARY="${BIDDER_BINARY:-}"
  INFRA_SHA256="${BIDDER_SHA256:-}"
  infra_release_binary "$BIDDER_PLATFORM"
  sha="$INFRA_SHA256"
  staged="$BIDDER_STAGE_DIR/socialweb-computer-${sha:0:12}"
  case "$sha" in
  "<"*) staged="$BIDDER_STAGE_DIR/socialweb-computer-<sha12>" ;;
  esac
  if [ -n "$BIDDER_ALLOW_DEV_BINARY" ]; then
    dev=" --allow-dev-binary"
  fi
  if [ -n "$BIDDER_HANDLE" ]; then
    install="$(infra_q "$staged") install bidder --handle $(infra_q "$BIDDER_HANDLE")$dev"
  fi
  infra_log "binary $INFRA_BINARY ($sha) -> $(infra_target):$staged"
  infra_upload "$INFRA_BINARY" "$staged" 0755 root
  infra_rw_root "set -e
rec=/var/lib/socialweb-computer/install-$(infra_q "$BIDDER_ACCOUNT").json
if [ -f \"\$rec\" ]; then
  echo \"upgrade $BIDDER_ACCOUNT to $sha\"
  $(bidder_as_account) $(infra_q "$BIDDER_STATE/bin/current/socialweb-computer") upgrade --binary $(infra_q "$staged") --sha256 $(infra_q "$sha")$dev
  ${install:-echo \"no BIDDER_HANDLE: upgrade only, install bidder not run again\"}
else
  ${install:-echo \"not installed and no BIDDER_HANDLE: set BIDDER_HANDLE for the first install bidder\" >&2; exit 1}
fi"
  if [ "$INFRA_APPLY" = 1 ]; then
    bidder_status
  fi
}

bidder_rollback() {
  bidder_defaults
  infra_rw_root "$(bidder_as_account) $(infra_q "$BIDDER_STATE/bin/current/socialweb-computer") rollback"
  if [ "$INFRA_APPLY" = 1 ]; then
    bidder_status
  fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main bidder "" "" "" "sudo -n" "$@"
fi
