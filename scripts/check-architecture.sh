#!/usr/bin/env bash
set -euo pipefail

strict_layers="api contract"

backend_prefixes="internal/compute internal/vault internal/pds internal/atprotorepo internal/events internal/vmruntime internal/hostsvc internal/fcapi internal/guestnet internal/guestdisk internal/guestimage internal/policyengine/runs internal/relay internal/ingress internal/ssh internal/dispatcher internal/gateway internal/guestcp"

backend_banned="k8s.io/client-go sigs.k8s.io/controller-runtime sigs.k8s.io/multicluster-runtime github.com/kcp-dev k8s.io/apimachinery/pkg/apis/meta/v1/unstructured internal/bus internal/controllers internal/app internal/appctl internal/kcpserver internal/kcpapply internal/kcpclient internal/kcpdatastore internal/stack"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
module="$(go list -m)"

layer_of() {
  local rel="${1#"$module"/}"
  if [ "$1" = "$module" ]; then
    echo cmd
    return
  fi
  case "$rel" in
    api | api/*) echo api ;;
    internal/contract | internal/contract/*) echo contract ;;
    internal/kit | internal/kit/*) echo kit ;;
    internal/controllers | internal/controllers/*) echo controllers ;;
    internal/app | internal/app/*) echo app ;;
    internal/*) echo impl ;;
    cmd | cmd/* | test | test/*) echo cmd ;;
    *) echo assets ;;
  esac
}

rank_of() {
  case "$1" in
    api | assets) echo 0 ;;
    contract) echo 1 ;;
    kit) echo 2 ;;
    impl) echo 3 ;;
    controllers) echo 4 ;;
    app) echo 5 ;;
    *) echo 6 ;;
  esac
}

is_strict() {
  local l
  for l in $strict_layers; do
    if [ "$l" = "$1" ]; then
      return 0
    fi
  done
  return 1
}

errors=0
warnings=0

report() {
  local layer="$1" msg="$2"
  if is_strict "$layer"; then
    echo "check-architecture: FAIL [$layer] $msg" >&2
    errors=$((errors + 1))
  else
    echo "check-architecture: warn [$layer] $msg" >&2
    warnings=$((warnings + 1))
  fi
}

listing="$(go list -e -f '{{.ImportPath}} {{join .Imports " "}}' ./...)"

while read -r pkg imports; do
  [ -n "$pkg" ] || continue
  from="$(layer_of "$pkg")"
  from_rank="$(rank_of "$from")"
  for imp in $imports; do
    case "$imp" in
      "$module" | "$module"/*)
        to="$(layer_of "$imp")"
        to_rank="$(rank_of "$to")"
        if [ "$from" = contract ] && [ "$to" != api ] && [ "$to" != contract ]; then
          report "$from" "$pkg imports $imp ($to); a contract imports api and the standard library only"
        elif [ "$to_rank" -gt "$from_rank" ]; then
          report "$from" "$pkg imports $imp ($to); imports point down api <- contract <- kit <- impl <- controllers <- app <- cmd"
        fi
        ;;
      *)
        first="${imp%%/*}"
        if [ "$from" = contract ] && [[ "$first" == *.* ]]; then
          report "$from" "$pkg imports $imp; a contract imports api and the standard library only"
        fi
        ;;
    esac
  done
done <<<"$listing"

is_backend() {
  local rel="${1#"$module"/}" p
  for p in $backend_prefixes; do
    case "$rel" in
      "$p" | "$p"/*) return 0 ;;
    esac
  done
  return 1
}

backend_listing=""
for b in $backend_prefixes; do
  if [ -d "$b" ]; then
    backend_listing+="$(go list -e -f '{{.ImportPath}} {{join .Deps " "}}' "./$b/...")"$'\n'
  fi
done

while read -r pkg deps; do
  [ -n "$pkg" ] || continue
  is_backend "$pkg" || continue
  for banned in $backend_banned; do
    for dep in $deps; do
      rel="${dep#"$module"/}"
      case "$rel" in
        "$banned" | "$banned"/*)
          report backend "$pkg depends on $banned (via $dep); a backend package runs without kcp, the bus or a controller runtime"
          break
          ;;
      esac
    done
  done
done <<<"$backend_listing"

echo "check-architecture: strict layers: $strict_layers; $errors errors, $warnings warnings"
if [ "$errors" -gt 0 ]; then
  exit 1
fi
