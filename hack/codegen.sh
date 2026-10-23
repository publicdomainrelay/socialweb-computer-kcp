#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

go mod download k8s.io/code-generator
codegen_dir="$(go list -m -f '{{.Dir}}' k8s.io/code-generator)"
codegen_version="$(go list -m -f '{{.Version}}' k8s.io/code-generator)"
GOBIN="${XDG_CACHE_HOME:-$HOME/.cache}/socialweb-computer-kcp/code-generator-${codegen_version}/bin"
export GOBIN

source "${codegen_dir}/kube_codegen.sh"

kube::codegen::gen_helpers --boilerplate /dev/null "${root}/api"
