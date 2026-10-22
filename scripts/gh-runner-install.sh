#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: scripts/gh-runner-install.sh [--remove] [--account NAME] [--token-file FILE]
  Installs the GitHub Actions runner of socialweb-computer-kcp on this host: a dedicated unprivileged
  system account (no docker group, no sudo), one runner as a systemd user unit of that account,
  registered to this repository only, labelled self-hosted,linux,x64,socialweb-computer-kcp, one job
  at a time. The pins and binaries jobs run here, because the assets they stage are pinned by the
  bytes this host's toolchain produces (MESHD_CC, the C compiler of the meshd build).
  --remove      stop the unit, delete the runner on GitHub, remove the account and its home
  --account     the runner account (default socialweb-ci)
  --token-file  a 0600 file holding a GitHub runner registration token (install) or a removal token
                (--remove); create one with:
                  gh api -X POST repos/<owner>/<repo>/actions/runners/registration-token --jq .token
                The token is read in-process and never printed.
EOF
  exit 2
}

account=socialweb-ci
token_file=""
remove=0
while [ $# -gt 0 ]; do
  case "$1" in
    --remove) remove=1 ;;
    --account) account="$2"; shift ;;
    --token-file) token_file="$2"; shift ;;
    *) usage ;;
  esac
  shift
done

die() { echo "gh-runner-install: $*" >&2; exit 1; }
say() { echo "gh-runner-install: $*" >&2; }

runner_version=v2.338.0
runner_sha=af4b794c1bc41d73d40535e3fe092a39f9679cd8d965954c2aca25a05ca41d32
labels=self-hosted,linux,x64,socialweb-computer-kcp
unit=gh-actions-runner-socialweb-computer-kcp.service

for tool in sudo curl git useradd loginctl tar sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done
sudo -n true 2>/dev/null || die "needs sudo -n (run as an administrator of this host)"

root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
remote="$(git -C "$root" remote get-url origin)"
case "$remote" in
  https://github.com/*.git|https://github.com/*) repo_path="${remote#https://github.com/}"; repo_path="${repo_path%.git}" ;;
  *) die "origin is not a github.com URL: $remote" ;;
esac
repo_url="https://github.com/$repo_path"
runner_name="socialweb-computer-kcp on $(hostname -s) ($account)"

home="/var/lib/$account"
runner_dir="$home/actions-runner"
unit_dir="$home/.config/systemd/user"
uid=""
if id "$account" >/dev/null 2>&1; then
  uid="$(id -u "$account")"
fi

as_account() { sudo -u "$account" -- "$@"; }
user_systemctl() { sudo systemctl --user -M "$account@" "$@"; }

token=""
if [ -n "$token_file" ]; then
  [ "$(stat -c %a "$token_file")" = 600 ] || die "$token_file must be mode 0600"
  token="$(tr -d '[:space:]' <"$token_file")"
fi

if [ "$remove" = 1 ]; then
  if ! id "$account" >/dev/null 2>&1; then
    say "no account $account; nothing to remove"
    exit 0
  fi
  user_systemctl disable --now "$unit" >/dev/null 2>&1 || true
  if [ -x "$runner_dir/config.sh" ]; then
    [ -n "$token" ] || die "--remove needs --token-file with a removal token"
    as_account "$runner_dir/config.sh" remove --token "$token" || say "the runner was not removed on GitHub; remove it on the repository's settings page"
  fi
  sudo loginctl disable-linger "$account"
  sudo userdel --remove "$account" 2>/dev/null || sudo userdel "$account"
  sudo rm -rf "$home"
  say "removed account $account and $home"
  exit 0
fi

[ -n "$token" ] || die "--token-file is required: a GitHub runner registration token"

if ! id "$account" >/dev/null 2>&1; then
  sudo useradd --system --user-group --create-home --home-dir "$home" --shell /usr/sbin/nologin \
    --comment "GitHub Actions runner for socialweb-computer-kcp" "$account"
  uid="$(id -u "$account")"
  say "created account $account (home $home)"
fi
if ! grep -q "^$account:" /etc/subuid; then
  sudo usermod --add-subuids 19005440-20054015 --add-subgids 19005440-20054015 "$account"
fi
sudo loginctl enable-linger "$account"
for _ in $(seq 1 50); do
  sudo test -S "/run/user/$uid/systemd/private" && break
  sleep 0.2
done
sudo test -S "/run/user/$uid/systemd/private" || die "the user manager of $account did not start"

as_account mkdir -p "$home/.tmp" "$home/.cache" "$unit_dir"
as_account chmod 0700 "$home/.tmp" "$home/.cache" "$home/.config"

tarball="actions-runner-linux-x64-${runner_version#v}.tar.gz"
if [ ! -x "$runner_dir/run.sh" ]; then
  as_account curl -fL --max-time 900 --progress-bar -o "$home/$tarball" "https://github.com/actions/runner/releases/download/$runner_version/$tarball"
  echo "$runner_sha  $home/$tarball" | sha256sum -c --quiet -
  as_account mkdir -p "$runner_dir"
  sudo tar -xzf "$home/$tarball" -C "$runner_dir"
  as_account rm -f "$home/$tarball"
  sudo chown -R "$account:$account" "$runner_dir"
  say "installed the runner $runner_version into $runner_dir"
fi

if sudo test -s "$runner_dir/.runner"; then
  say "kept the existing registration in $runner_dir/.runner"
else
  as_account "$runner_dir/config.sh" --unattended --url "$repo_url" --token "$token" \
    --name "$runner_name" --labels "$labels" --work "$home/_work" --replace
  say "registered $runner_name to $repo_path (labels $labels)"
fi
token=""

printf '%s\n' \
  '[Unit]' \
  'Description=GitHub Actions runner for socialweb-computer-kcp (one job at a time)' \
  'After=network-online.target' \
  '' \
  '[Service]' \
  "ExecStart=$runner_dir/run.sh" \
  "WorkingDirectory=$runner_dir" \
  "Environment=HOME=$home" \
  "Environment=TMPDIR=$home/.tmp" \
  'Restart=on-failure' \
  'RestartSec=5' \
  'Nice=10' \
  'KillMode=mixed' \
  'TimeoutStopSec=40' \
  '' \
  '[Install]' \
  'WantedBy=default.target' |
  as_account bash -c 'cat >"$1"' _ "$unit_dir/$unit"

user_systemctl daemon-reload
user_systemctl enable "$unit" >/dev/null 2>&1
user_systemctl restart "$unit"
sleep 3
user_systemctl is-active --quiet "$unit" || die "$unit is not active; read: sudo journalctl _UID=$uid"
say "$unit is active for $account; logs: sudo journalctl _UID=$uid"
say "the runner is listed at $repo_url/settings/actions/runners"
