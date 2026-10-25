#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

org_root_git_pull_status() {
  infra_stub_status "git-pull.timer git-pull.service"
  infra_ro_root "systemctl show git-pull.service -p Result --no-pager; git -c safe.directory='*' -C /home/johnandersen777/prod-org-root log -1 --format='prod-org-root: %h %cs %s' | cut -c1-90; journalctl -u git-pull.service -n 3 --no-pager -o cat | cut -c1-160"
}

org_root_git_pull_deploy() {
  infra_stub_todo "git-pull.timer runs ~/bin/git-pull-and-rebuild.sh every 5 min as johnandersen777: git fetch, reset --hard origin/main and submodule update of prod-org-root (org-root-dispatcher-typescript), then a compute-spa rebuild. It fails on the atproto-relay submodule checkout. A pull-based deploy is not this tree's model (local ssh deploys only); retire the timer once atproto-relay moves to Go"
}

org_root_git_pull_rollback() {
  infra_stub_todo "nothing to roll back"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main org-root-git-pull root@mini-cloud-0002.chadig.com "$HOME/.ssh/id_ed25519_no_password" "" "" "$@"
fi
