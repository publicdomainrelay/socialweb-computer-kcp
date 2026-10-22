#!/bin/bash
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

SSH_TARGET="${SSH_TARGET:-root@socialweb.computer}"
SSH_PORT="${SSH_PORT:-1997}"
SSH_KEY="${SSH_KEY:-}"
DOMAIN="${DOMAIN:-socialweb.computer}"
ORIGIN="${ORIGIN:-k8s.${DOMAIN}}"
KCP_ORIGIN="${KCP_ORIGIN:-kcp.${DOMAIN}}"
ACCOUNT="${ACCOUNT:-socialweb}"
STATE_DIR="${STATE_DIR:-/var/lib/socialweb-computer/${ACCOUNT}}"
GATEWAY_ENV_FILE="${GATEWAY_ENV_FILE:-}"
GATEWAY_SPEC_FILE="${GATEWAY_SPEC_FILE:-}"
BINARY="${BINARY:-}"
SHA256="${SHA256:-}"
WEB_APP_PORT=8788
KCP_PORT=6443
GATEWAY_TLS_PORT=8443
CERT_FRONT_PORT=8444
SSH_OPTS=(-p "$SSH_PORT" -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=20
  -o ControlMaster=auto -o "ControlPath=${HOME}/.ssh/cm-swc-%r@%h:%p" -o ControlPersist=10m -o ServerAliveInterval=15)
if [ -n "$SSH_KEY" ]; then
  SSH_OPTS+=(-i "$SSH_KEY" -o IdentitiesOnly=yes)
fi

say() {
  echo "deploy: $*"
}

q() {
  printf "%q" "$1"
}

if [ -z "$BINARY" ]; then
  commit="$(git -C "$REPO" rev-parse HEAD)"
  release="$HOME/.tmp/socialweb-computer-build/release/$commit"
  if [ ! -f "$release/SHA256SUMS" ]; then
    say "building the release binary of $commit"
    "$REPO/scripts/build-socialweb-computer.sh" --platform linux/amd64 --commit "$commit"
  fi
  BINARY="$release/socialweb-computer-linux-amd64"
  SHA256="$(awk '$2 == "socialweb-computer-linux-amd64" { print $1 }' "$release/SHA256SUMS")"
fi
[ -f "$BINARY" ] || { say "no binary at $BINARY"; exit 1; }
[ -n "$SHA256" ] || { say "SHA256 names the published sha256 of $BINARY"; exit 1; }
[ "$(sha256sum "$BINARY" | cut -d' ' -f1)" = "$SHA256" ] || { say "$BINARY is not $SHA256"; exit 1; }

INCOMING="/var/lib/socialweb-computer/incoming-${SHA256}"
say "shipping $BINARY ($SHA256) to $SSH_TARGET"
ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "install -d -m 0755 /var/lib/socialweb-computer && cat > $(q "$INCOMING.partial") && chmod 0755 $(q "$INCOMING.partial") && mv $(q "$INCOMING.partial") $(q "$INCOMING")" < "$BINARY"
if [ -n "$GATEWAY_ENV_FILE" ]; then
  say "shipping $GATEWAY_ENV_FILE as $STATE_DIR/gateway.env"
  ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "cat > $(q "$INCOMING.env")" < "$GATEWAY_ENV_FILE"
fi

if [ -n "$GATEWAY_SPEC_FILE" ]; then
  say "shipping $GATEWAY_SPEC_FILE as the Gateway spec"
  ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "cat > $(q "$INCOMING.spec")" < "$GATEWAY_SPEC_FILE"
fi

REMOTE_ENV="ACCOUNT=$(q "$ACCOUNT") STATE_DIR=$(q "$STATE_DIR") INCOMING=$(q "$INCOMING") SHA256=$(q "$SHA256") ORIGIN=$(q "$ORIGIN") KCP_ORIGIN=$(q "$KCP_ORIGIN") WEB_APP_PORT=$(q "$WEB_APP_PORT") KCP_PORT=$(q "$KCP_PORT") GATEWAY_TLS_PORT=$(q "$GATEWAY_TLS_PORT") CERT_FRONT_PORT=$(q "$CERT_FRONT_PORT")"

ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "$REMOTE_ENV bash -s" <<'REMOTE_EOF'
set -euo pipefail

say() {
  echo "remote: $*"
}

command -v caddy >/dev/null || { say "no caddy on this host: the public names are served by its automatic HTTPS"; exit 1; }
trap 'rm -f "$INCOMING" "$INCOMING.env" "$INCOMING.spec"' EXIT
SPEC="/var/lib/socialweb-computer/gateway-spec-${ACCOUNT}.yaml"
spec_args=()
if [ -f "$INCOMING.spec" ]; then
  install -m 0644 "$INCOMING.spec" "$SPEC"
fi
if [ -f "$SPEC" ]; then
  spec_args=(--gateway-spec-file "$SPEC")
fi

# gateway.env is read by the account's user manager (EnvironmentFile): it must belong to the account
# BEFORE the core starts, or systemd skips it and the gateway runs on built-in defaults (audience
# did:web:socialweb.local, every real token refused). A first install creates the account, so there
# the file is handed over and the core restarted once right after install gateway.
if [ -f "$INCOMING.env" ]; then
  install -d -m 0700 "$STATE_DIR"
  if id -u "$ACCOUNT" >/dev/null 2>&1; then
    install -o "$ACCOUNT" -g "$ACCOUNT" -m 0600 "$INCOMING.env" "$STATE_DIR/gateway.env"
  else
    install -m 0600 "$INCOMING.env" "$STATE_DIR/gateway.env"
  fi
fi
if [ ! -f "/var/lib/socialweb-computer/install-${ACCOUNT}.json" ]; then
  say "install gateway (the one root step)"
  "$INCOMING" install gateway --account "$ACCOUNT" --state-dir "$STATE_DIR" "${spec_args[@]}"
  if [ -f "$STATE_DIR/gateway.env" ] && [ "$(stat -c %U "$STATE_DIR/gateway.env")" != "$ACCOUNT" ]; then
    chown "$ACCOUNT:$ACCOUNT" "$STATE_DIR/gateway.env"
    say "gateway.env handed to $ACCOUNT; restarting the core so it reads it"
    systemctl --user -M "$ACCOUNT@" restart socialweb-computer-gateway.service
  fi
else
  uid="$(id -u "$ACCOUNT")"
  copy="$STATE_DIR/incoming-$SHA256"
  install -o "$ACCOUNT" -g "$ACCOUNT" -m 0755 "$INCOMING" "$copy"
  say "upgrade to $SHA256 as $ACCOUNT"
  upgrade_ok=1
  runuser -u "$ACCOUNT" -- env XDG_RUNTIME_DIR="/run/user/$uid" CONFIG_PATH_SOCIALWEB_COMPUTER="$STATE_DIR/config.json" \
    "$copy" upgrade --binary "$copy" --sha256 "$SHA256" || upgrade_ok=0
  rm -f "$copy"
  if [ "$upgrade_ok" != "1" ]; then
    # The first start after a restart can fail (systemd 255 exit 219/CGROUP) and Restart=always starts it again.
    say "upgrade reported a failed job; waiting up to 60 s for the core unit to become active"
    recovered=0
    for _ in $(seq 1 60); do
      state="$(systemctl --user -M "$ACCOUNT@" show -p ActiveState -p SubState --value socialweb-computer-gateway.service | paste -sd' ')"
      case "$state" in
        "active "*) recovered=1; break ;;
        "activating start"*|"activating start-pre"*) recovered=1; break ;;
      esac
      sleep 1
    done
    if [ "$recovered" != "1" ]; then
      say "the core unit never became active (state: $state); last log lines:"
      journalctl --user -M "${ACCOUNT}@" -u socialweb-computer-gateway.service -n 40 --no-pager 2>&1 || true
      exit 1
    fi
    say "the core unit is $state; continuing"
  fi
  say "install gateway again with bin/current (renders the unit files, restarts a core whose unit changed)"
  "$STATE_DIR/bin/current/socialweb-computer" install gateway --account "$ACCOUNT" --state-dir "$STATE_DIR" "${spec_args[@]}"
fi

say "waiting for the web app to answer"
ready=0
for _ in $(seq 1 150); do
  if curl -fsS "http://127.0.0.1:${WEB_APP_PORT}/api/config" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 2
done
if [ "$ready" != "1" ]; then
  say "the web app never answered; last log lines:"
  journalctl --user -M "${ACCOUNT}@" -u socialweb-computer-gateway.service -n 60 --no-pager 2>&1 || true
  exit 1
fi
say "the web app answers"

CA_DIR="/etc/caddy/socialweb-computer"
install -d -m 0755 "$CA_DIR"
admin="$(sed -n 's/.*"adminKubeconfig": *"\([^"]*\)".*/\1/p' "$STATE_DIR/gateway.json" | head -n1)"
[ -n "$admin" ] || { say "gateway.json names no admin kubeconfig"; exit 1; }
sed -n 's/^ *certificate-authority-data: *//p' "$admin" | head -n1 | base64 -d > "$CA_DIR/kcp.crt"
[ -s "$CA_DIR/kcp.crt" ] || { say "kcp's serving CA could not be read out of $admin"; exit 1; }
install -m 0644 "$STATE_DIR/tls/tls.crt" "$CA_DIR/gateway.crt"
[ -s "$STATE_DIR/tls/edge-client.crt" ] && [ -s "$STATE_DIR/tls/edge-client.key" ] || { say "the gateway made no edge client certificate"; exit 1; }
getent group caddy >/dev/null || { say "no caddy group: the edge client key cannot be given to caddy only"; exit 1; }
install -m 0644 "$STATE_DIR/tls/edge-client.crt" "$CA_DIR/edge-client.crt"
install -m 0640 -o root -g caddy "$STATE_DIR/tls/edge-client.key" "$CA_DIR/edge-client.key"
chmod 0644 "$CA_DIR"/*.crt

install -d -m 0755 /etc/caddy/sites
cat > "/etc/caddy/sites/${ORIGIN}.caddy" <<SITE
${ORIGIN} {
  handle {
    reverse_proxy http://127.0.0.1:${WEB_APP_PORT}
  }
}
SITE
cat > "/etc/caddy/sites/${KCP_ORIGIN}.caddy" <<SITE
${KCP_ORIGIN} {
  tls {
    client_auth {
      mode request
    }
  }
  @clientcert expression {tls_client_certificate_der_base64} != ""
  handle @clientcert {
    reverse_proxy https://127.0.0.1:${CERT_FRONT_PORT} {
      header_up X-Socialweb-Client-Certificate {tls_client_certificate_der_base64}
      flush_interval -1
      transport http {
        tls_trusted_ca_certs ${CA_DIR}/gateway.crt
        tls_client_auth ${CA_DIR}/edge-client.crt ${CA_DIR}/edge-client.key
        tls_server_name 127.0.0.1
        versions 1.1
      }
    }
  }
  handle {
    reverse_proxy https://127.0.0.1:${KCP_PORT} {
      header_up -X-Socialweb-Client-Certificate
      flush_interval -1
      transport http {
        tls_trusted_ca_certs ${CA_DIR}/kcp.crt
        tls_server_name 127.0.0.1
        versions 1.1
      }
    }
  }
}
SITE
if ! grep -qE '^[[:space:]]*import[[:space:]]+/etc/caddy/sites/\*\.caddy' /etc/caddy/Caddyfile; then
  cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%s)"
  printf '\nimport /etc/caddy/sites/*.caddy\n' >> /etc/caddy/Caddyfile
fi
if ! grep -qE '^[[:space:]]*skip_install_trust[[:space:]]*$' /etc/caddy/Caddyfile; then
  cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%s).trust"
  if [ "$(grep -vE '^[[:space:]]*(#|$)' /etc/caddy/Caddyfile | head -n1 | tr -d '[:space:]')" = "{" ]; then
    awk '!done && /^[[:space:]]*\{[[:space:]]*$/ { print; print "\tskip_install_trust"; done = 1; next } { print }' /etc/caddy/Caddyfile > /etc/caddy/Caddyfile.trust
  else
    { printf '{\n\tskip_install_trust\n}\n\n'; cat /etc/caddy/Caddyfile; } > /etc/caddy/Caddyfile.trust
  fi
  mv /etc/caddy/Caddyfile.trust /etc/caddy/Caddyfile
fi
caddy validate --config /etc/caddy/Caddyfile >/dev/null
systemctl reload caddy || systemctl restart caddy

say "https://${ORIGIN} -> web app, https://${KCP_ORIGIN} -> kcp front"
REMOTE_EOF

say "done: https://${ORIGIN} and https://${KCP_ORIGIN}"
