#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: scripts/dev-droplet.sh create [name] | setup [name] | push <publish-dir> [name] | swc [name] -- <socialweb-computer args>
       scripts/dev-droplet.sh ssh [name] [cmd...] | list | down
       scripts/dev-droplet.sh sync [name] [ref] [work] | build [name] [work]
EOF
  exit 2
}

sizes="${DEV_DROPLET_SIZES:-c-60-intel c2-60vcpu-120gb-intel c-48-intel c2-48vcpu-96gb-intel c-48 c2-48vcpu-96gb c-32-intel c-32 c-16-intel c-16}"
regions="${DEV_DROPLET_REGIONS:-sfo3 nyc3 atl1 tor1 nyc2 sfo2 nyc1 lon1 fra1 ams3}"
image="${DEV_DROPLET_IMAGE:-ubuntu-24-04-x64}"
key_file="${DEV_DROPLET_KEY:-$HOME/.ssh/id_ed25519}"
[ -f "$key_file" ] || key_file="$HOME/.ssh/id_ed25519_no_password"
state="$HOME/.tmp/dev-droplet"
guest_image="${SOCIALWEB_IT_GUEST_IMAGE_DIR:-/var/lib/socialweb-computer/guest-image}"
mkdir -p "$state"

say() { echo "dev-droplet: $*" >&2; }
ip_of() { cat "$state/${1:-dev-work-0001}.ip" 2>/dev/null || { say "no droplet ${1:-dev-work-0001}; run create"; exit 1; }; }
rssh() { local n="$1"; shift; ssh -i "$key_file" -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 "root@$(ip_of "$n")" "$@"; }

dssh() { local n="$1"; shift; ssh -i "$key_file" -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 "dev@$(ip_of "$n")" "$@"; }

cmd="${1:-}"
[ -n "$cmd" ] || usage
shift

case "$cmd" in
  create)
    name="${1:-dev-work-0001}"
    [ "$(doctl compute droplet list --tag-name dev-work --no-header --format ID | grep -c .)" -lt 8 ] || { say "eight dev-work droplets exist already"; exit 1; }
    key_id="$(doctl compute ssh-key list --no-header --format ID,FingerPrint | awk -v fp="$(ssh-keygen -E md5 -lf "$key_file.pub" | sed 's/^[0-9]* MD5://; s/ .*//')" '$2 == fp { print $1 }')"
    [ -n "$key_id" ] || { say "key $key_file.pub is not registered with doctl"; exit 1; }
    for size in $sizes; do
      for region in $regions; do
        if out="$(doctl compute droplet create "$name" --size "$size" --image "$image" --region "$region" --tag-name dev-work --ssh-keys "$key_id" --wait --no-header --format ID 2>&1)"; then
          ip=""
          until [ -n "$ip" ]; do ip="$(doctl compute droplet get "$out" --no-header --format PublicIPv4)"; sleep 2; done
          echo "$ip" >"$state/$name.ip"
          say "$name created: $size in $region at $ip"
          break 2
        fi
        say "$size in $region: ${out##*: }"
      done
    done
    [ -f "$state/$name.ip" ] || exit 1
    until rssh "$name" true 2>/dev/null; do sleep 3; done
    ;;
  setup)
    name="${1:-dev-work-0001}"
    repo="$(cd "$(dirname "$0")/.." && pwd)"
    go_ver="$(awk '/^go /{print $2}' "$repo/go.mod")"
    deno_ver="$(jq -r '.assets.deno.version' "$repo/internal/versions/versions.json")"
    rssh "$name" env GO_VER="$go_ver" DENO_VER="$deno_ver" bash -s <<'EOF'
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive
apt_opts="-o DPkg::Lock::Timeout=600"
apt-get $apt_opts update -qq
apt-get $apt_opts install -y -qq uidmap rsync zstd acl jq curl unzip xz-utils dbus-user-session git build-essential gnupg binutils passt podman iproute2 util-linux
modprobe tun
echo tun >/etc/modules-load.d/tun.conf
printf 'kernel.apparmor_restrict_unprivileged_userns=0\nuser.max_user_namespaces=1000000\n' >/etc/sysctl.d/60-swc.conf
sysctl --system >/dev/null
id dev >/dev/null 2>&1 || useradd -m -s /bin/bash dev
id test-0001 >/dev/null 2>&1 || useradd -m -s /bin/bash -G kvm test-0001
sed -i '/^test-0001:/d;/^dev:/d' /etc/subuid /etc/subgid
echo 'dev:100000:65536' >>/etc/subuid
echo 'dev:100000:65536' >>/etc/subgid
install -d -m 700 -o dev -g dev /home/dev/.ssh
install -m 600 -o dev -g dev /root/.ssh/authorized_keys /home/dev/.ssh/authorized_keys
echo 'test-0001:655360:1048576' >>/etc/subuid
echo 'test-0001:655360:1048576' >>/etc/subgid
mkdir -p /etc/systemd/system/user@.service.d
printf '[Service]\nDelegate=cpu memory pids\n' >/etc/systemd/system/user@.service.d/swc-delegate.conf
systemctl daemon-reload
loginctl enable-linger test-0001
loginctl enable-linger dev
sleep 2
systemctl restart "user@$(id -u test-0001).service"
mkdir -p /etc/rancher/node
test -s /etc/rancher/node/password || head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n' >/etc/rancher/node/password
chown test-0001:test-0001 /etc/rancher/node/password
chmod 600 /etc/rancher/node/password
mkdir -p /var/lib/socialweb-computer /var/tmp/swcit
chmod 1777 /var/tmp/swcit
chown dev:dev /var/lib/socialweb-computer
test -c /dev/kvm
test -x /usr/local/go/bin/go && /usr/local/go/bin/go version | grep -q "go$GO_VER " || { curl -fsSL "https://go.dev/dl/go$GO_VER.linux-amd64.tar.gz" | tar -C /usr/local -xzf - --transform "s,^go,go.new," && rm -rf /usr/local/go && mv /usr/local/go.new /usr/local/go; }
ln -sf /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/
test -x /usr/local/bin/deno && deno --version | grep -q "deno ${DENO_VER#v} " || { curl -fsSL -o /var/tmp/deno.zip "https://github.com/denoland/deno/releases/download/$DENO_VER/deno-x86_64-unknown-linux-gnu.zip" && unzip -oq /var/tmp/deno.zip -d /usr/local/bin && rm /var/tmp/deno.zip; }
runuser -u dev -- bash -c 'mkdir -p ~/.deno/bin && ln -sf /usr/local/bin/deno ~/.deno/bin/deno && { test -x ~/.cargo/bin/rustup || curl -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain none; }'
grep -q CONTAINER /home/dev/.profile || echo 'export PATH=$HOME/.cargo/bin:$PATH CONTAINER=podman' >>/home/dev/.profile
EOF
    ;;
  push)
    publish="${1:?publish dir}"
    name="${2:-dev-work-0001}"
    ssh_e="ssh -i $key_file -o StrictHostKeyChecking=accept-new"
    rsync -a --delete -e "$ssh_e" "$publish/" "root@$(ip_of "$name"):$publish/"
    rsync -a -z -e "$ssh_e" "$guest_image/" "root@$(ip_of "$name"):/var/lib/socialweb-computer/guest-image/"
    ;;
  swc)
    name="dev-work-0001"
    [ "${1:-}" = "--" ] || { name="$1"; shift; }
    shift
    publish="${SOCIALWEB_PUBLISH_DIR:?set SOCIALWEB_PUBLISH_DIR to the pushed publish dir}"
    uid="$(rssh "$name" id -u test-0001)"
    rssh "$name" cd /var/tmp '&&' runuser -u test-0001 -- env HOME=/home/test-0001 XDG_RUNTIME_DIR=/run/user/$uid DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus PATH=/usr/local/bin:/usr/bin:/bin LANG=C.UTF-8 "$publish/bin/socialweb-computer" $(printf '%q ' "$@")
    ;;
  sync)
    name="${1:-dev-work-0001}"
    ref="${2:-origin/main}"
    work="${3:-swc-dev}"
    dssh "$name" "bash -lc $(printf %q "cd ~; [ -d socialweb-computer-kcp ] || git clone -q https://github.com/publicdomainrelay/socialweb-computer-kcp; cd socialweb-computer-kcp && git fetch -q origin && { [ -d ~/w/$work ] && git -C ~/w/$work checkout -qf --detach $ref || git worktree add -qf --detach ~/w/$work $ref; } && git -C ~/w/$work log --oneline -1")"
    ;;
  build)
    name="${1:-dev-work-0001}"
    work="${2:-swc-dev}"
    dssh "$name" "bash -lc $(printf %q "cd ~/w/$work && scripts/build-socialweb-computer.sh --dev --publish /var/tmp/$work")"
    ;;
  ssh)
    name="${1:-dev-work-0001}"
    [ $# -eq 0 ] || shift
    rssh "$name" "$@"
    ;;
  list)
    doctl compute droplet list --tag-name dev-work --format ID,Name,PublicIPv4,Memory,VCPUs,Region,Status
    ;;
  down)
    doctl compute droplet delete --tag-name dev-work --force
    rm -f "$state"/*.ip
    doctl compute droplet list --tag-name dev-work --no-header --format ID | grep -c . | grep -qx 0 && say "no dev-work droplet is left"
    ;;
  *)
    usage
    ;;
esac
