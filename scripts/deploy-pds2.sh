#!/usr/bin/env bash
set -euo pipefail

# Stand up a second AT Protocol PDS on the production host, at
# pds2.socialweb.computer, and create one test account on it.
#
#   ./scripts/deploy-pds2.sh --dry-run
#   ./scripts/deploy-pds2.sh
#
# Why a second PDS at all: the k8s door's node contracts are written into the
# market's own collections (com.publicdomainrelay.temp.*). A public PDS refuses
# records in a collection it does not know, so the identity that drives the door
# has to live on a PDS we run. This script produces that identity.
#
# WHY THE CODE COMES FROM THE HOST'S OWN TREE
#
# The unit runs ts/hono-pds/main.ts out of a socialweb-computer-kcp checkout. Its
# @publicdomainrelay/* imports resolve through the root deno.json workspace, so
# the shipped set is that workspace: deno.json, deno.lock, ts/ and
# socialweb-computer-k8s/.
#
# The tree it is copied from is a variable (SOURCE_ROOT, default
# /opt/socialweb-computer-kcp) and it is the one already on this host. That is not a shortcut: the version of
# hono-pds in this repo keeps accounts in a `new Map`
# (ts/hono-pds/lib/atproto-repo-deno/account-store.ts), so every restart would erase the
# account and every re-run of this script would mint a second did:plc for one
# handle. The tree already on the host persists accounts to Deno KV
# (accountStorage in main.ts, "accounts" prefix in account-store.ts). A tree
# without that wiring is refused below rather than deployed.
#
# WHY THE HANDLE NEEDS NO DNS RECORD TO RESOLVE
#
# A handle proves itself to a client either by a DNS TXT at _atproto.<handle> or
# by https://<handle>/.well-known/atproto-did. This PDS answers that path with
# its OWN did (hono-pds factory.ts), not with the account's, so the HTTPS method
# cannot come from the PDS process. It comes from Caddy instead: a second site
# block for the handle's hostname answers that one path with the account's did
# and proxies everything else to the PDS. *.socialweb.computer already resolves
# here, so Caddy can get a certificate for the handle host over HTTP-01 and no
# operator action is needed. The equivalent TXT record is printed at the end
# anyway, because the DNS method is the one that survives this site block being
# removed.
#
# NOTHING HERE TOUCHES AN EXISTING UNIT. The only shared unit it reloads is
# Caddy, and only through `caddy validate` + `systemctl reload` after adding a
# drop-in under /etc/caddy/sites/. Every other unit on the host is left alone.

SSH_TARGET="${SSH_TARGET:-root@socialweb.computer}"
SSH_PORT="${SSH_PORT:-1997}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_no_password}"
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=20
  -o ServerAliveInterval=15)

SERVICE_NAME="pds2"
SERVICE_USER="pds2"
ORIGIN="pds2.socialweb.computer"
HANDLE="test.pds2.socialweb.computer"
ACCOUNT_EMAIL="test@pds2.socialweb.computer"
PORT="8794"
PREFIX="/opt/socialweb-pds2"
SOURCE_ROOT="/opt/socialweb-computer-kcp"
ENTRY_CONFIG="deno.json"
ENTRYPOINT="ts/hono-pds/main.ts"
STATE_DIR="/var/lib/pds2"
DENO="/usr/local/bin/deno"
PLC_DIRECTORY_URL="https://plc.directory"
BIND="127.0.0.1"
PROBE_COLLECTION="com.publicdomainrelay.temp.badgeBlueKeys"
CADDY_SITES_DIR="/etc/caddy/sites"
CADDYFILE="/etc/caddy/Caddyfile"

ENV_FILE="${STATE_DIR}/pds.env"
CONFIG_PATH="${STATE_DIR}/config.json"
PASSWORD_FILE="${STATE_DIR}/account-password"
DID_FILE="${STATE_DIR}/account-did"

DRY_RUN=false
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    -h|--help) sed -n '2,45p' "$0"; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

say() { printf '\n=== %s ===\n' "$*"; }

# Every remote command goes through here, so --dry-run is one branch rather than
# a flag threaded through every step.
remote() {
  if ${DRY_RUN}; then
    printf -- '--- [dry-run] ssh %s -p %s %s\n' "${SSH_TARGET}" "${SSH_PORT}" "$*"
  else
    ssh "${SSH_OPTS[@]}" -i "${SSH_KEY}" -p "${SSH_PORT}" "${SSH_TARGET}" "$@"
  fi
}

# The local values are handed to the remote script as shell assignments so the
# previewed script reads the way it will run.
REMOTE_VARS="$(cat <<EOF
SERVICE_NAME='${SERVICE_NAME}'
SERVICE_USER='${SERVICE_USER}'
ORIGIN='${ORIGIN}'
HANDLE='${HANDLE}'
ACCOUNT_EMAIL='${ACCOUNT_EMAIL}'
PORT='${PORT}'
PREFIX='${PREFIX}'
SOURCE_ROOT='${SOURCE_ROOT}'
ENTRY_CONFIG='${ENTRY_CONFIG}'
ENTRYPOINT='${ENTRYPOINT}'
STATE_DIR='${STATE_DIR}'
DENO='${DENO}'
PLC_DIRECTORY_URL='${PLC_DIRECTORY_URL}'
BIND='${BIND}'
PROBE_COLLECTION='${PROBE_COLLECTION}'
CADDY_SITES_DIR='${CADDY_SITES_DIR}'
CADDYFILE='${CADDYFILE}'
ENV_FILE='${ENV_FILE}'
CONFIG_PATH='${CONFIG_PATH}'
PASSWORD_FILE='${PASSWORD_FILE}'
DID_FILE='${DID_FILE}'
EOF
)"

# A multi-line remote script, read from stdin.
remote_script() {
  local desc="$1"
  local body full
  body="$(cat)"
  full="${REMOTE_VARS}
${body}"
  if ${DRY_RUN}; then
    printf -- '--- [dry-run] ssh %s -p %s %s <<REMOTE\n%s\n--- end\n' \
      "${SSH_TARGET}" "${SSH_PORT}" "${desc}" "${full}"
  else
    printf '%s\n' "${full}" | ssh "${SSH_OPTS[@]}" -i "${SSH_KEY}" -p "${SSH_PORT}" \
      "${SSH_TARGET}" "bash -s"
  fi
}

# ── Host preflight ────────────────────────────────────────────────────────────
say "checking ${SSH_TARGET}"
remote 'test "$(id -u)" = 0 && echo "root on $(hostname)"'

# ── The PDS answers on the loopback port before Caddy is told about it ────────
say "shipping the PDS workspace to ${PREFIX} and starting ${SERVICE_NAME}"
remote_script "install ${SERVICE_NAME}" <<REMOTE
set -euo pipefail

for tool in rsync openssl curl jq useradd runuser; do
  command -v "\$tool" >/dev/null 2>&1 || { echo "missing on the host: \$tool" >&2; exit 1; }
done
[ -x "\${DENO}" ] || { echo "no deno at \${DENO}" >&2; exit 1; }
[ -f "\${SOURCE_ROOT}/\${ENTRY_CONFIG}" ] && [ -f "\${SOURCE_ROOT}/\${ENTRYPOINT}" ] || {
  echo "no socialweb-computer-kcp checkout at \${SOURCE_ROOT}" >&2; exit 1; }

install -d -m 0755 "\${PREFIX}"
cp "\${SOURCE_ROOT}/deno.json" "\${SOURCE_ROOT}/deno.lock" "\${PREFIX}/"
for tree in ts socialweb-computer-k8s; do
  rsync -a --delete --exclude node_modules --exclude web/downloads "\${SOURCE_ROOT}/\${tree}/" "\${PREFIX}/\${tree}/"
done

# Refuse a tree whose accounts do not survive a restart. See the header.
grep -q 'accountStorage' "\${PREFIX}/\${ENTRYPOINT}" || {
  echo "\${PREFIX}/\${ENTRYPOINT} keeps accounts in memory (no accountStorage)." >&2
  echo "Accounts would vanish on every restart and every re-run of this deploy would" >&2
  echo "mint a second did:plc for \${HANDLE}. Ship a tree that persists them." >&2
  exit 1
}

id -u "\${SERVICE_USER}" >/dev/null 2>&1 || \
  useradd --system --create-home --home-dir "\${STATE_DIR}" --shell /usr/sbin/nologin "\${SERVICE_USER}"
install -d -m 0700 -o "\${SERVICE_USER}" -g "\${SERVICE_USER}" "\${STATE_DIR}"

# The PDS key is the issuer of every access token it mints, and the admin
# password gates createAccount (without it the route is unauthenticated). Both
# are generated once and kept: regenerating either invalidates sessions or
# reopens account creation.
if [ ! -f "\${ENV_FILE}" ]; then
  umask 077
  {
    printf 'PDS_PRIVATE_KEY_HEX=%s\n' "\$(openssl rand -hex 32)"
    printf 'PDS_ADMIN_PASSWORD=%s\n' "\$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-24)"
    printf 'PDS_STORAGE_PATH=%s\n' "\${STATE_DIR}/pds.kv"
  } > "\${ENV_FILE}"
  echo "generated \${ENV_FILE}"
fi
chmod 0600 "\${ENV_FILE}"
chown "\${SERVICE_USER}:\${SERVICE_USER}" "\${ENV_FILE}"

# The CLI reads config.json from CONFIG_PATH_HONO_PDS. Keeping it in the state
# dir leaves the shipped tree read-only.
if [ ! -f "\${CONFIG_PATH}" ]; then
  printf '{\n  "crawlers": ""\n}\n' > "\${CONFIG_PATH}"
fi
chown "\${SERVICE_USER}:\${SERVICE_USER}" "\${CONFIG_PATH}"
chmod 0644 "\${CONFIG_PATH}"

# Deno updates the lockfile next to its config whenever the resolved
# dependencies move, and the shipped tree is read-only -- that is the point of
# ProtectSystem=strict, and on a host where the sibling copy of this tree is
# writable deno would never have complained. So the lock lives in the state dir:
# copied from the tree on first run, updated there afterwards, and pointed at
# with --lock. Pinning survives; the tree stays read-only.
if [ ! -f "\${STATE_DIR}/deno.lock" ]; then
  cp "\${PREFIX}/deno.lock" "\${STATE_DIR}/deno.lock"
fi
chown "\${SERVICE_USER}:\${SERVICE_USER}" "\${STATE_DIR}/deno.lock"
chmod 0644 "\${STATE_DIR}/deno.lock"

cat > "\${STATE_DIR}/\${SERVICE_NAME}.service.new" <<UNIT
[Unit]
Description=\${SERVICE_NAME} — AT Protocol PDS for \${ORIGIN}
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
User=\${SERVICE_USER}
Group=\${SERVICE_USER}
WorkingDirectory=\${STATE_DIR}
EnvironmentFile=\${ENV_FILE}
Environment=HOME=\${STATE_DIR}
Environment=DENO_DIR=\${STATE_DIR}/deno
Environment=CONFIG_PATH_HONO_PDS=\${CONFIG_PATH}
# --node-modules-dir=none is what lets this run from a source tree that is never
# written: npm deps resolve out of DENO_DIR instead of a node_modules beside
# deno.json. One line, so that neither heredoc level has to carry an escape.
ExecStart=\${DENO} run -A --node-modules-dir=none --unstable-kv --lock \${STATE_DIR}/deno.lock --config \${PREFIX}/\${ENTRY_CONFIG} \${PREFIX}/\${ENTRYPOINT} --port \${PORT} --hostname \${BIND} --public-hostname \${ORIGIN} --plc-directory-url \${PLC_DIRECTORY_URL}

Restart=always
RestartSec=5
LimitNOFILE=65536

NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
ReadWritePaths=\${STATE_DIR}

[Install]
WantedBy=multi-user.target
UNIT

install -d -m 0700 -o "\${SERVICE_USER}" -g "\${SERVICE_USER}" "\${STATE_DIR}/deno"
runuser -u "\${SERVICE_USER}" -- env HOME="\${STATE_DIR}" DENO_DIR="\${STATE_DIR}/deno" \\
  "\${DENO}" cache --node-modules-dir=none --unstable-kv --lock "\${STATE_DIR}/deno.lock" \\
  --config "\${PREFIX}/\${ENTRY_CONFIG}" "\${PREFIX}/\${ENTRYPOINT}" >/dev/null 2>&1 || true

changed=no
if cmp -s "\${STATE_DIR}/\${SERVICE_NAME}.service.new" "/etc/systemd/system/\${SERVICE_NAME}.service"; then
  rm -f "\${STATE_DIR}/\${SERVICE_NAME}.service.new"
else
  mv "\${STATE_DIR}/\${SERVICE_NAME}.service.new" "/etc/systemd/system/\${SERVICE_NAME}.service"
  chmod 644 "/etc/systemd/system/\${SERVICE_NAME}.service"
  changed=yes
fi

systemctl daemon-reload
systemctl enable "\${SERVICE_NAME}" >/dev/null
if [ "\${changed}" = yes ]; then
  systemctl restart "\${SERVICE_NAME}"
  echo "unit changed; restarted \${SERVICE_NAME}"
elif ! systemctl is-active --quiet "\${SERVICE_NAME}"; then
  systemctl start "\${SERVICE_NAME}"
  echo "unit unchanged; started \${SERVICE_NAME}"
else
  echo "unit unchanged and running; left alone"
fi

for _ in \$(seq 1 60); do
  if curl -fsS "http://\${BIND}:\${PORT}/xrpc/_health" >/dev/null 2>&1; then
    echo "health on \${BIND}:\${PORT} ok"
    exit 0
  fi
  sleep 2
done
echo "\${SERVICE_NAME} never answered on \${BIND}:\${PORT}; last log lines:" >&2
journalctl -u "\${SERVICE_NAME}" -n 40 --no-pager >&2 || true
exit 1
REMOTE

# ── Caddy: the origin, then the account ───────────────────────────────────────
say "pointing ${ORIGIN} at the PDS and reloading Caddy"
remote_script "caddy site for ${ORIGIN}" <<REMOTE
set -euo pipefail

install -d -m 0755 "\${CADDY_SITES_DIR}"
cat > "\${CADDY_SITES_DIR}/\${ORIGIN}.caddy" <<SITE
\${ORIGIN} {
  reverse_proxy http://\${BIND}:\${PORT}
}
SITE

# The drop-in directory is imported by the Caddyfile. Adding the import if it is
# somehow absent, once, with the backup the sibling deploys take.
if ! grep -qE '^[[:space:]]*import[[:space:]]+/etc/caddy/sites/\*\.caddy' "\${CADDYFILE}"; then
  cp "\${CADDYFILE}" "\${CADDYFILE}.bak.\$(date +%s)"
  printf '\nimport /etc/caddy/sites/*.caddy\n' >> "\${CADDYFILE}"
fi

if ! grep -qE '^[[:space:]]*skip_install_trust[[:space:]]*\$' "\${CADDYFILE}"; then
  cp "\${CADDYFILE}" "\${CADDYFILE}.bak.\$(date +%s).trust"
  if [ "\$(grep -vE '^[[:space:]]*(#|\$)' "\${CADDYFILE}" | awk 'NR==1' | tr -d '[:space:]')" = "{" ]; then
    awk '!done && /^[[:space:]]*\{[[:space:]]*\$/ { print; print "\tskip_install_trust"; done = 1; next } { print }' "\${CADDYFILE}" > "\${CADDYFILE}.trust"
  else
    { printf '{\n\tskip_install_trust\n}\n\n'; cat "\${CADDYFILE}"; } > "\${CADDYFILE}.trust"
  fi
  mv "\${CADDYFILE}.trust" "\${CADDYFILE}"
fi

caddy validate --config "\${CADDYFILE}" >/dev/null
systemctl reload caddy || systemctl restart caddy

for _ in \$(seq 1 45); do
  if curl -fsS "https://\${ORIGIN}/xrpc/_health" >/dev/null 2>&1; then
    echo "https://\${ORIGIN} answers (certificate obtained)"
    exit 0
  fi
  sleep 2
done
echo "https://\${ORIGIN} did not answer within 90s" >&2
journalctl -u caddy -n 30 --no-pager >&2 || true
exit 1
REMOTE

# ── The account ───────────────────────────────────────────────────────────────
say "creating the test account if it is not already there"
remote_script "create account ${HANDLE}" <<REMOTE
set -euo pipefail

if [ ! -f "\${PASSWORD_FILE}" ]; then
  umask 077
  openssl rand -base64 24 | tr -d '/+=' | cut -c1-24 > "\${PASSWORD_FILE}"
fi
chmod 0600 "\${PASSWORD_FILE}"

PW="\$(cat "\${PASSWORD_FILE}")"
ADMIN_B64="\$(grep '^PDS_ADMIN_PASSWORD=' "\${ENV_FILE}" | cut -d= -f2- | tr -d '\n' | { printf 'admin:'; cat; } | base64 -w0)"

session="\$(curl -fsS -X POST "https://\${ORIGIN}/xrpc/com.atproto.server.createSession" \\
  -H 'content-type: application/json' \\
  --data "{\\"identifier\\":\\"\${HANDLE}\\",\\"password\\":\\"\${PW}\\"}" 2>/dev/null || true)"
did="\$(printf '%s' "\${session}" | jq -r '.did // empty')"

if [ -z "\${did}" ]; then
  if [ -f "\${DID_FILE}" ]; then
    echo "an account for \${HANDLE} was already created on this PDS but the stored" >&2
    echo "password no longer authenticates it. Refusing to create a second account" >&2
    echo "for one handle. To start over: systemctl stop \${SERVICE_NAME}, remove" >&2
    echo "\${DID_FILE}, \${PASSWORD_FILE} and \${STATE_DIR}/pds.kv, then re-run." >&2
    exit 1
  fi
  created="\$(curl -fsS -X POST "https://\${ORIGIN}/xrpc/com.atproto.server.createAccount" \\
    -H "authorization: Basic \${ADMIN_B64}" \\
    -H 'content-type: application/json' \\
    --data "{\\"handle\\":\\"\${HANDLE}\\",\\"email\\":\\"\${ACCOUNT_EMAIL}\\",\\"password\\":\\"\${PW}\\"}")"
  did="\$(printf '%s' "\${created}" | jq -r '.did')"
  echo "created \${HANDLE} as \${did}"
else
  echo "\${HANDLE} already exists as \${did}"
fi

printf '%s' "\${did}" > "\${DID_FILE}"
chmod 0644 "\${DID_FILE}"
echo "password is in \${PASSWORD_FILE} (0600), not printed here"
REMOTE

# ── The handle resolves without a DNS record ──────────────────────────────────
say "serving the handle's did at https://${HANDLE}/.well-known/atproto-did"
remote_script "caddy site for ${HANDLE}" <<REMOTE
set -euo pipefail

cat > "\${CADDY_SITES_DIR}/\${HANDLE}.caddy" <<SITE
\${HANDLE} {
  handle /.well-known/atproto-did {
    respond "\$(cat \${DID_FILE})" 200
  }
  handle {
    reverse_proxy http://\${BIND}:\${PORT}
  }
}
SITE

caddy validate --config "\${CADDYFILE}" >/dev/null
systemctl reload caddy || systemctl restart caddy

resolved=''
for _ in \$(seq 1 45); do
  resolved="\$(curl -fsS "https://\${HANDLE}/.well-known/atproto-did" 2>/dev/null || true)"
  [ "\${resolved}" = "\$(cat \${DID_FILE})" ] && { echo "handle resolves to \${resolved}"; exit 0; }
  sleep 2
done
echo "warning: https://\${HANDLE}/.well-known/atproto-did did not come up." >&2
echo "The PDS itself is up; a client that resolves \${HANDLE} by DNS still needs the" >&2
echo "_atproto TXT record printed at the end of this deploy." >&2
exit 0
REMOTE

# ── Prove it: a session, and a write into a collection no public PDS takes ────
say "verifying: describeServer, session, createRecord"
remote_script "verify ${SERVICE_NAME}" <<REMOTE
set -euo pipefail

echo "--- GET https://\${ORIGIN}/xrpc/_health"
curl -fsS "https://\${ORIGIN}/xrpc/_health"; echo
echo "--- GET https://\${ORIGIN}/xrpc/com.atproto.server.describeServer"
curl -fsS "https://\${ORIGIN}/xrpc/com.atproto.server.describeServer"; echo
echo "--- GET https://\${HANDLE}/.well-known/atproto-did"
curl -fsS "https://\${HANDLE}/.well-known/atproto-did" || echo "(not served)"
echo
echo "--- POST createSession as \${HANDLE}"
PW="\$(cat "\${PASSWORD_FILE}")"
token="\$(curl -fsS -X POST "https://\${ORIGIN}/xrpc/com.atproto.server.createSession" \\
  -H 'content-type: application/json' \\
  --data "{\\"identifier\\":\\"\${HANDLE}\\",\\"password\\":\\"\${PW}\\"}" | jq -r '.accessJwt')"
[ -n "\${token}" ] && [ "\${token}" != null ] || { echo "createSession gave no token" >&2; exit 1; }
echo "session created"

echo "--- POST putRecord into \${PROBE_COLLECTION}"
record="{\\"repo\\":\\"self\\",\\"collection\\":\\"\${PROBE_COLLECTION}\\",\\"rkey\\":\\"deploy-check\\",\\"record\\":{\\"\\\$type\\":\\"\${PROBE_COLLECTION}\\",\\"note\\":\\"pds2 deploy probe\\",\\"createdAt\\":\\"\$(date -u +%Y-%m-%dT%H:%M:%S.000Z)\\"}}"
curl -fsS -X POST "https://\${ORIGIN}/xrpc/com.atproto.repo.putRecord" \\
  -H "authorization: Bearer \${token}" \\
  -H 'content-type: application/json' \\
  --data "\${record}"; echo

echo "--- GET listRecords \${PROBE_COLLECTION} for \$(cat \${DID_FILE})"
# Not repo=self: this PDS resolves "self" on the write routes only, so a read
# has to name the account the way a client would.
curl -fsS "https://\${ORIGIN}/xrpc/com.atproto.repo.listRecords?repo=\$(cat \${DID_FILE})&collection=\${PROBE_COLLECTION}" \\
  -H "authorization: Bearer \${token}"; echo
REMOTE

# ── What an operator still has to know ────────────────────────────────────────
say "report"
remote_script "report" <<'REMOTE'
set -euo pipefail
echo "service    ${SERVICE_NAME}.service   ($(systemctl is-active ${SERVICE_NAME}))"
echo "listening  ${BIND}:${PORT}           (loopback only)"
echo "origin     https://${ORIGIN}"
echo "handle     ${HANDLE}"
echo "account    $(cat ${DID_FILE})"
echo "password   ${PASSWORD_FILE}  (0600, root; never printed)"
echo "state      ${STATE_DIR}  (deno dir, config, pds.env, pds.kv, account-did)"
echo "code       ${PREFIX}  (the ${ENTRY_CONFIG} workspace, copied from ${SOURCE_ROOT})"
echo
echo "verify:"
echo "  curl -fsS https://${ORIGIN}/xrpc/_health"
echo "  curl -fsS https://${ORIGIN}/xrpc/com.atproto.server.describeServer"
echo "  curl -fsS https://${HANDLE}/.well-known/atproto-did"
echo
echo "no DNS record is needed: https://${HANDLE}/.well-known/atproto-did answers"
echo "with the account's did, which is one of the two ways a client resolves a"
echo "handle. The DNS method is the other, and it is the one that still works if"
echo "that Caddy site block is ever removed -- add this record to take it:"
echo
echo "  _atproto.${HANDLE}.  IN  TXT  \"did=$(cat ${DID_FILE})\""
echo
echo "sign in at https://k8s.socialweb.computer with the handle above; the password"
echo "is in ${PASSWORD_FILE} on this host."
REMOTE

say "done"
