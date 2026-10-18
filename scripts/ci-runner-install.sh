#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: scripts/ci-runner-install.sh [--remove] [--account NAME] [--token-file FILE]
  Installs the CI runner of socialweb-computer-kcp on this host: a dedicated unprivileged system account
  (no docker group, no sudo; kvm for the VM guests), one gitlab-runner with the shell executor as a systemd
  user unit of that account, registered to this project only, tagged socialweb-computer-kcp, one job at a
  time, 90 minutes at most. The host needs /usr/bin/google-chrome and /usr/local/bin/kubectl (the web app
  sign-in and kubeconfig tests).
  --remove      stop the unit, delete the runner on the GitLab server, remove the account and its home
  --account     the runner account (default socialweb-ci)
  --token-file  a 0600 file holding a GitLab token with the create_runner scope (default: the password of
                this checkout's origin remote URL); the token is read in-process and never printed
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

die() { echo "ci-runner-install: $*" >&2; exit 1; }
say() { echo "ci-runner-install: $*" >&2; }

for tool in sudo jq curl xz unshare git useradd loginctl zstd /usr/bin/gitlab-runner /usr/bin/google-chrome /usr/local/bin/kubectl; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done
sudo -n true 2>/dev/null || die "needs sudo -n (run as an administrator of this host)"

root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
remote="$(git -C "$root" remote get-url origin)"
case "$remote" in https://*) ;; *) die "origin must be an https URL of the GitLab server" ;; esac
hostpart="${remote#https://}"
creds=""
case "$hostpart" in *@*) creds="${hostpart%%@*}"; hostpart="${hostpart#*@}" ;; esac
server_host="${hostpart%%/*}"
project_path="${hostpart#*/}"
project_path="${project_path%.git}"
server_url="https://$server_host/"
api="https://$server_host/api/v4"
project_enc="$(printf '%s' "$project_path" | jq -sRr @uri)"

gl_token=""
if [ -n "$token_file" ]; then
  [ "$(stat -c %a "$token_file")" = 600 ] || die "$token_file must be mode 0600"
  gl_token="$(tr -d '[:space:]' <"$token_file")"
else
  case "$creds" in *:*) gl_token="${creds#*:}" ;; esac
fi
[ -n "$gl_token" ] || die "no GitLab token: pass --token-file or use an origin URL that holds one"

gl() {
  local method="$1" path="$2"
  shift 2
  printf 'header = "PRIVATE-TOKEN: %s"\n' "$gl_token" |
    curl -sS --max-time 30 --fail-with-body -K - -X "$method" "$api$path" "$@"
}

home="/var/lib/$account"
config_dir="$home/.config/gitlab-runner"
config="$config_dir/config.toml"
unit_dir="$home/.config/systemd/user"
unit=gitlab-runner-socialweb-computer-kcp.service
tag=socialweb-computer-kcp

as_account() { sudo -u "$account" -- "$@"; }
user_systemctl() { sudo systemctl --user -M "$account@" "$@"; }

if [ "$remove" = 1 ]; then
  if id "$account" >/dev/null 2>&1; then
    user_systemctl disable --now "$unit" >/dev/null 2>&1 || true
    runner_id="$(sudo sed -n 's/^ *id *= *\([0-9][0-9]*\)$/\1/p' "$config" 2>/dev/null | head -1)"
    if [ -n "$runner_id" ]; then
      gl DELETE "/runners/$runner_id" >/dev/null
      say "deleted runner $runner_id on $server_host"
    fi
    sudo loginctl disable-linger "$account"
    sudo userdel --remove "$account" 2>/dev/null || sudo userdel "$account"
    sudo rm -rf "$home"
    say "removed account $account and $home"
  else
    say "no account $account; nothing to remove"
  fi
  exit 0
fi

if ! id "$account" >/dev/null 2>&1; then
  sudo useradd --system --user-group --create-home --home-dir "$home" --shell /usr/sbin/nologin \
    --comment "gitlab-runner for socialweb-computer-kcp" "$account"
  say "created account $account (home $home)"
fi
sudo rm -f "$home/.bash_logout"
id -nG "$account" | tr ' ' '\n' | grep -qx kvm || sudo usermod -aG kvm "$account"
for group in $(id -nG "$account"); do
  case "$group" in
    "$account"|kvm) ;;
    *) die "$account is in group $group; the runner account must be in no group but its own and kvm" ;;
  esac
done
sudo chmod 0700 "$home"
uid="$(id -u "$account")"
# Integration tests run market networks as this account: VM slots (a subuid/subgid range, as
# install bidder reserves), a short run root, and the account's user bus and runtime dir.
if ! grep -q "^$account:" /etc/subuid; then
  sudo usermod --add-subuids 19005440-20054015 --add-subgids 19005440-20054015 "$account"
fi
sudo install -d -o "$account" -g "$account" -m 0755 /var/tmp/swcci
sudo loginctl enable-linger "$account"
for _ in $(seq 1 50); do
  sudo test -S "/run/user/$uid/systemd/private" && break
  sleep 0.2
done
sudo test -S "/run/user/$uid/systemd/private" || die "the user manager of $account did not start"
manager_pid="$(systemctl show -p MainPID --value "user@$uid.service")"
sudo sed -n 's/^Groups://p' "/proc/$manager_pid/status" | tr ' \t' '\n\n' | grep -qx "$(getent group kvm | cut -d: -f3)" ||
  die "user@$uid started before $account joined kvm; no job may run then: sudo systemctl restart user@$uid.service, then run this again"

as_account mkdir -p "$config_dir" "$unit_dir" "$home/.tmp"
as_account chmod 0700 "$home/.config" "$config_dir" "$home/.tmp"

if sudo test -s "$config"; then
  say "kept the existing registration in $config"
else
  project_id="$(gl GET "/projects/$project_enc" | jq -r .id)"
  [ -n "$project_id" ] && [ "$project_id" != null ] || die "project $project_path not found on $server_host"
  resp="$(gl POST /user/runners \
    --data-urlencode runner_type=project_type \
    --data-urlencode "project_id=$project_id" \
    --data-urlencode "description=socialweb-computer-kcp gate on $(hostname -s) ($account, shell)" \
    --data-urlencode "tag_list=$tag" \
    --data-urlencode run_untagged=false \
    --data-urlencode locked=true \
    --data-urlencode maximum_timeout=5400)"
  runner_id="$(printf '%s' "$resp" | jq -r .id)"
  runner_token="$(printf '%s' "$resp" | jq -r .token)"
  case "$runner_token" in glrt-*) ;; *) die "the GitLab server did not return a runner token" ;; esac
  printf '%s\n' \
    'concurrent = 1' \
    'check_interval = 3' \
    'shutdown_timeout = 30' \
    '' \
    '[[runners]]' \
    "  name = \"socialweb-computer-kcp gate on $(hostname -s)\"" \
    "  url = \"$server_url\"" \
    "  id = $runner_id" \
    "  token = \"$runner_token\"" \
    '  executor = "shell"' \
    '  shell = "bash"' \
    "  builds_dir = \"$home/builds\"" \
    "  cache_dir = \"$home/cache\"" \
    '  output_limit = 65536' |
    as_account bash -c 'umask 077; cat >"$1"' _ "$config"
  unset runner_token resp
  say "registered runner $runner_id to $project_path (tag $tag, locked, shell executor)"
fi
sudo chmod 0600 "$config"

# No sandboxing settings in this user unit: in a user manager ProtectSystem/ProtectHome/PrivateTmp put
# the service in its own user namespace with only this uid mapped, so rootless podman (builder images,
# swc-userns-stage) cannot use the account's subuid/subgid range through newuidmap. The integration
# tests start market networks through this account's user manager, which runs outside any unit sandbox
# anyway. Isolation is the account: its own uid, home mode 0700, no sudo, no docker group.
printf '%s\n' \
  '[Unit]' \
  'Description=gitlab-runner for socialweb-computer-kcp (shell executor, one job)' \
  '' \
  '[Service]' \
  "ExecStart=/usr/bin/gitlab-runner run --config $config --working-directory $home" \
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
user_systemctl is-active --quiet "$unit" || die "$unit is not active; read: sudo journalctl _UID=$uid"
say "$unit is active for $account; logs: sudo journalctl _UID=$uid"
