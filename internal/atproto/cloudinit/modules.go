package cloudinit

import (
	"regexp"
	"strconv"
	"strings"
)

const K3sVersion = "v1.36.4+k3s1"

const K3sSetupPath = "/usr/local/bin/setup-k3s.sh"

var secretsModule UserDataModule = func(ctx Context) (UserDataPatch, error) {
	return UserDataPatch{
		Packages: []string{"jq", "curl"},
		WriteFiles: []WriteFileEntry{
			{
				Path:        "/usr/local/bin/setup-secrets.sh",
				Owner:       "root:root",
				Permissions: "0700",
				Content: `#!/usr/bin/env bash
set -euo pipefail

STAMP=/var/lib/setup-secrets.done
[ -f "${STAMP}" ] && exit 0

SECRETS_URL="` + ctx.SecretsURL + `"
SECRETS_ROUTE="` + ctx.SecretsRoute + `"
SECRETS_AUD="` + ctx.SecretsAud + `"
GUEST_LOGIN="` + GuestLoginScriptPath + `"

if [ -z "${SECRETS_URL}" ] || [ -z "${SECRETS_AUD}" ]; then
  echo "secrets module misconfigured: missing url or aud" >&2
  exit 1
fi

for _ in $(seq 1 60); do
  [ -x "${GUEST_LOGIN}" ] && break
  sleep 2
done
[ -x "${GUEST_LOGIN}" ] || { echo "the guest login helper ${GUEST_LOGIN} never appeared" >&2; exit 1; }

EXCHANGED=""
for attempt in $(seq 1 10); do
  EXCHANGED="$("${GUEST_LOGIN}" token "${SECRETS_AUD}")" || true
  [ -n "${EXCHANGED}" ] && break
  echo "workload identity token exchange failed (attempt ${attempt}); retrying" >&2
  sleep 5
done
[ -n "${EXCHANGED}" ] || { echo "no workload identity token from the bidder's OpenBao for ${SECRETS_AUD}" >&2; exit 1; }

BUNDLE=""
for attempt in $(seq 1 10); do
  BUNDLE="$(curl -sf -H "Authorization: Bearer ${EXCHANGED}" "${SECRETS_URL}${SECRETS_ROUTE}")" || true
  [ -n "${BUNDLE}" ] && break
  echo "secrets fetch failed (attempt ${attempt}); retrying" >&2
  sleep 5
done
[ -n "${BUNDLE}" ] || { echo "secrets fetch failed against ${SECRETS_URL}${SECRETS_ROUTE}" >&2; exit 1; }

umask 077
COUNT="$(printf '%s' "${BUNDLE}" | jq 'length')"
i=0
while [ "${i}" -lt "${COUNT}" ]; do
  SECRET_PATH="$(printf '%s' "${BUNDLE}" | jq -r ".[${i}].path")"
  install -d -m 0700 -o root -g root "$(dirname "${SECRET_PATH}")"
  printf '%s' "${BUNDLE}" | jq -j ".[${i}].value" > "${SECRET_PATH}"
  chown root:root "${SECRET_PATH}"
  chmod 0600 "${SECRET_PATH}"
  i=$((i + 1))
done

echo "wrote ${COUNT} secrets"
touch "${STAMP}"
`,
			},
			{
				Path:        "/etc/systemd/system/setup-secrets.service",
				Owner:       "root:root",
				Permissions: "0644",
				Content: strings.Join([]string{
					"[Unit]",
					"Description=Fetch compute contract secrets via workload identity exchange",
					"After=network-online.target guest-login.service",
					"Wants=network-online.target guest-login.service",
					"ConditionPathExists=!/var/lib/setup-secrets.done",
					"",
					"[Service]",
					"Type=oneshot",
					"RemainAfterExit=yes",
					"User=root",
					"ExecStart=/usr/local/bin/setup-secrets.sh",
					"StandardOutput=journal",
					"StandardError=journal",
					"",
					"[Install]",
					"WantedBy=multi-user.target",
					"",
				}, "\n"),
			},
		},
		RuncmdPrepend: []any{
			"systemctl daemon-reload",
			"systemctl enable setup-secrets.service",
			"systemctl start --no-block setup-secrets.service",
		},
	}, nil
}

var k3sModule UserDataModule = func(Context) (UserDataPatch, error) {
	return UserDataPatch{
		WriteFiles: []WriteFileEntry{{
			Path:        K3sSetupPath,
			Owner:       "root:root",
			Permissions: "0755",
			Content: `#!/bin/bash
set -eux -o pipefail

STAMP=/var/lib/setup-k3s.done
[ -f "${STAMP}" ] && exit 0

if [ -x /usr/local/bin/k3s ] && /usr/local/bin/k3s --version 2>/dev/null | grep -qF '` + K3sVersion + `'; then
  /usr/local/bin/k3s --version
  touch "${STAMP}"
  exit 0
fi

command -v curl >/dev/null || { apt-get update && apt-get install -y curl ca-certificates; }

curl -sfL https://get.k3s.io -o /tmp/k3s-install.sh
INSTALL_K3S_VERSION=` + K3sVersion + ` \
  INSTALL_K3S_SKIP_START=true \
  INSTALL_K3S_SKIP_ENABLE=true \
  sh /tmp/k3s-install.sh

/usr/local/bin/k3s --version

touch "${STAMP}"
`,
		}},
		Runcmd: []any{K3sSetupPath},
	}, nil
}

var aliasRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]+\s+[A-Za-z0-9_.-]+$`)

var irohModule UserDataModule = func(ctx Context) (UserDataPatch, error) {
	targetPort := ctx.TargetPort
	if targetPort == 0 {
		targetPort = 22
	}
	version := ctx.DumbpipeVersion
	if version == "" {
		version = DumbpipeVersionDefault
	}
	aliases := []string{}
	for _, alias := range ctx.HostAliases {
		if aliasRE.MatchString(alias) {
			aliases = append(aliases, alias)
		}
	}
	writeFiles := []WriteFileEntry{
		{
			Path:        "/root/.ssh/authorized_keys",
			Owner:       "root:root",
			Permissions: "0600",
			Content:     ctx.SSHAuthorizedKey + "\n",
		},
		{
			Path:        "/etc/ssh/sshd_config.d/10-iroh.conf",
			Owner:       "root:root",
			Permissions: "0644",
			Content: strings.Join([]string{
				"# Key-only root login; reached over the guest's own iroh endpoint (the",
				"# compute-provider harness also TCP-probes :22 directly for readiness).",
				"PermitRootLogin prohibit-password",
				"PasswordAuthentication no",
			}, "\n") + "\n",
		},
		{
			Path:        "/usr/local/bin/setup-iroh.sh",
			Owner:       "root:root",
			Permissions: "0755",
			Content: `#!/bin/bash
set -euo pipefail

STAMP=/var/lib/setup-iroh.done
[ -f "${STAMP}" ] && exit 0

DUMBPIPE_VERSION="` + version + `"

if [ -x /usr/local/bin/dumbpipe ] && [ "$(cat /usr/local/bin/dumbpipe.version 2>/dev/null)" = "${DUMBPIPE_VERSION}" ]; then
  touch "${STAMP}"
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64) _arch=x86_64 ;;
  aarch64|arm64) _arch=aarch64 ;;
  *) echo "unsupported machine type for dumbpipe: $(uname -m)" >&2; exit 1 ;;
esac

URL="https://github.com/n0-computer/dumbpipe/releases/download/v${DUMBPIPE_VERSION}/dumbpipe-v${DUMBPIPE_VERSION}-linux-${_arch}.tar.gz"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

for attempt in $(seq 1 5); do
  if curl -fsSL "${URL}" -o "${TMP}/dumbpipe.tar.gz" \
      && tar -xzf "${TMP}/dumbpipe.tar.gz" -C "${TMP}"; then
    install -m 0755 "${TMP}/dumbpipe" /usr/local/bin/dumbpipe
    touch "${STAMP}"
    exit 0
  fi
  echo "dumbpipe install failed (attempt ${attempt}): ${URL}" >&2
  sleep 5
done

echo "dumbpipe install failed after 5 attempts: ${URL}" >&2
exit 1
`,
		},
		{
			Path:        "/usr/local/bin/iroh-listener.sh",
			Owner:       "root:root",
			Permissions: "0755",
			Content: `#!/bin/bash
set -euo pipefail

SECRET_FILE=/root/.iroh-secret
if [ ! -s "${SECRET_FILE}" ]; then
  umask 077
  head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "${SECRET_FILE}"
  chmod 0600 "${SECRET_FILE}"
fi
IROH_SECRET="$(cat "${SECRET_FILE}")"
export IROH_SECRET

TICKET_FILE=/run/guest-iroh-ticket
LOG=/run/guest-iroh.log
rm -f "${TICKET_FILE}" "${LOG}"

/usr/local/bin/dumbpipe listen-tcp --host 127.0.0.1:` + strconv.Itoa(targetPort) + ` 2>>"${LOG}" &
DUMBPIPE_PID=$!

TICKET=""
for _ in $(seq 1 150); do
  if [ -s "${LOG}" ]; then
    TICKET="$(grep -a 'connect-tcp' "${LOG}" | tail -n 1 | awk '{print $NF}')"
  fi
  if [ -n "${TICKET}" ]; then
    break
  fi
  if ! kill -0 "${DUMBPIPE_PID}" 2>/dev/null; then
    break
  fi
  sleep 1
done

if [ -n "${TICKET}" ]; then
  printf 'iroh://%s\n' "${TICKET}" > "${TICKET_FILE}"
  chmod 0600 "${TICKET_FILE}"
  printf 'iroh://%s\n' "${TICKET}" > /run/guest-fqdn.tmp
  mv -f /run/guest-fqdn.tmp /run/guest-fqdn
fi

wait "${DUMBPIPE_PID}"
`,
		},
		{
			Path:        "/etc/systemd/system/iroh.service",
			Owner:       "root:root",
			Permissions: "0644",
			Content: strings.Join([]string{
				"[Unit]",
				"Description=iroh dumbpipe listener in front of local sshd",
				"After=network-online.target",
				"Wants=network-online.target",
				"",
				"[Service]",
				"Type=simple",
				"User=root",
				"ExecStart=/usr/local/bin/iroh-listener.sh",
				"Restart=always",
				"RestartSec=5",
				"TimeoutStopSec=10",
				"StandardOutput=journal",
				"StandardError=journal",
				"",
				"[Install]",
				"WantedBy=multi-user.target",
				"",
			}, "\n"),
		},
	}
	bootcmd := make([]any, 0, len(aliases))
	for _, alias := range aliases {
		bootcmd = append(bootcmd, []any{"sh", "-c", "grep -qxF '" + alias + "' /etc/hosts || echo '" + alias + "' >> /etc/hosts"})
	}
	disabledRoot := false
	pwAuth := false
	return UserDataPatch{
		Apt:         map[string]any{"preserve_sources_list": true},
		Packages:    []string{"openssh-server"},
		DisableRoot: &disabledRoot,
		SSHPwAuth:   &pwAuth,
		Bootcmd:     bootcmd,
		WriteFiles:  writeFiles,
		Runcmd: []any{
			"/usr/local/bin/setup-iroh.sh",
			"systemctl daemon-reload",
			"systemctl enable iroh.service",
			"systemctl start --no-block iroh.service",
			"systemctl enable --now --no-block ssh || systemctl enable --now --no-block sshd",
		},
	}, nil
}

type GuestLoginOptions struct {
	URL           string
	Role          string
	BindSecret    string
	Mount         string
	SSHDir        string
	TokenPath     string
	Attempts      int
	RetryDelaySec int
}

func GuestLoginScript(opts GuestLoginOptions) string {
	sshDir := opts.SSHDir
	if sshDir == "" {
		sshDir = "/etc/ssh"
	}
	tokenPath := opts.TokenPath
	if tokenPath == "" {
		tokenPath = GuestLoginTokenPath
	}
	attempts := opts.Attempts
	if attempts == 0 {
		attempts = 30
	}
	delay := opts.RetryDelaySec
	if delay == 0 {
		delay = 4
	}
	mount := opts.Mount
	if mount == "" {
		mount = GuestLoginDefaultMount
	}
	script := `#!/usr/bin/env bash
set -uo pipefail
URL="__URL__"
ROLE="__ROLE__"
MOUNT="__MOUNT__"
BIND_SECRET="__BIND_SECRET__"
TOKEN_PATH="__TOKEN_PATH__"
ED25519="__SSH_DIR__/ssh_host_ed25519_key"
OUT="$(mktemp)"
trap 'rm -f "${OUT}"' EXIT
wait_key() {
  for _ in $(seq 1 150); do
    [ -f "${ED25519}" ] && [ -f "${ED25519}.pub" ] && return 0
    sleep 2
  done
  echo "guest-login: no ${ED25519}" >&2
  return 1
}
post() {
  local step="$1" body="$2" code
  for _ in $(seq 1 __ATTEMPTS__); do
    code="$(curl -sS -m 20 -o "${OUT}" -w '%{http_code}' -X POST "${URL}/auth/${MOUNT}/${step}" -H 'Content-Type: application/json' -d "${body}" 2>/dev/null || echo 000)"
    case "${code}" in
      200|204) return 0 ;;
      4??) echo "guest-login: ${step} refused (${code}): $(cat "${OUT}" 2>/dev/null)" >&2; return 1 ;;
    esac
    echo "guest-login: ${step} http ${code}, retrying" >&2
    sleep __DELAY__
  done
  echo "guest-login: ${step} gave up" >&2
  return 1
}
sign() {
  printf '%s' "$1" | ssh-keygen -Y sign -n __NAMESPACE__ -f "${ED25519}"
}
bind() {
  local host_key sig
  host_key="$(awk '{print $1" "$2}' "${ED25519}.pub")"
  sig="$(sign "${BIND_SECRET}")"
  [ -n "${sig}" ] || { echo "guest-login: signing the bind secret failed" >&2; return 1; }
  post bind "$(jq -nc --arg r "${ROLE}" --arg s "${BIND_SECRET}" --arg k "${host_key}" --arg g "${sig}" '{role: $r, bind_secret: $s, host_key: $k, signature: $g}')" || return 1
  echo "guest-login: bound $(jq -r '.data.host_key_fingerprint // empty' "${OUT}") to role ${ROLE}" >&2
}
login() {
  local challenge sig token
  post challenge "$(jq -nc --arg r "${ROLE}" '{role: $r}')" || return 1
  challenge="$(jq -r '.data.challenge // empty' "${OUT}")"
  [ -n "${challenge}" ] || { echo "guest-login: no challenge in $(cat "${OUT}")" >&2; return 1; }
  sig="$(sign "${challenge}")"
  post login "$(jq -nc --arg r "${ROLE}" --arg c "${challenge}" --arg g "${sig}" '{role: $r, challenge: $c, signature: $g}')" || return 1
  token="$(jq -r '.auth.client_token // empty' "${OUT}")"
  [ -n "${token}" ] || { echo "guest-login: no token in the login reply" >&2; return 1; }
  install -d -m 0700 "$(dirname "${TOKEN_PATH}")"
  ( umask 077; printf '%s\n' "${token}" > "${TOKEN_PATH}" )
  echo "guest-login: logged in to the bidder's OpenBao as $(jq -r '.auth.metadata.role // empty' "${OUT}"), policies $(jq -c '.auth.policies' "${OUT}"), ttl $(jq -r '.auth.lease_duration' "${OUT}")s" >&2
}
role_for() {
  printf 'guest-%s-%s' "${ROLE}" "$(printf '%s' "$1" | sha256sum | cut -c1-16)"
}
token() {
  local name code
  name="$(role_for "$1")"
  [ -s "${TOKEN_PATH}" ] || login || return 1
  for _ in $(seq 1 __ATTEMPTS__); do
    code="$(curl -sS -m 20 -o "${OUT}" -w '%{http_code}' -H "X-Vault-Token: $(cat "${TOKEN_PATH}")" "${URL}/identity/oidc/token/${name}" 2>/dev/null || echo 000)"
    case "${code}" in
      200) jq -r '.data.token // empty' "${OUT}"; return 0 ;;
      403) login || sleep __DELAY__ ;;
      *) sleep __DELAY__ ;;
    esac
  done
  echo "guest-login: no token for $1 from the bidder's OpenBao (last http ${code})" >&2
  return 1
}
case "${1:-}" in
  "") wait_key && { bind || echo "guest-login: the bind was refused; logging in with the key already bound" >&2; login; } ;;
  login) wait_key && login ;;
  token) wait_key && token "${2:?an audience}" ;;
  *) echo "usage: guest-login.sh [login|token <audience>]" >&2; exit 2 ;;
esac
`
	return strings.NewReplacer(
		"__URL__", opts.URL,
		"__ROLE__", opts.Role,
		"__MOUNT__", mount,
		"__BIND_SECRET__", opts.BindSecret,
		"__TOKEN_PATH__", tokenPath,
		"__SSH_DIR__", sshDir,
		"__ATTEMPTS__", strconv.Itoa(attempts),
		"__DELAY__", strconv.Itoa(delay),
		"__NAMESPACE__", GuestLoginNamespace,
	).Replace(script)
}

func GuestLoginModule(opts GuestLoginOptions) UserDataModule {
	unit := strings.Join([]string{
		"[Unit]",
		"Description=Log this guest in to its bidder's OpenBao by its ssh host key",
		"After=network-online.target ssh.service",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"TimeoutStartSec=600",
		"ExecStart=" + GuestLoginScriptPath,
		"StandardOutput=journal+console",
		"StandardError=journal+console",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
	return func(Context) (UserDataPatch, error) {
		return UserDataPatch{
			WriteFiles: []WriteFileEntry{
				{Path: GuestLoginScriptPath, Owner: "root:root", Permissions: "0700", Content: GuestLoginScript(opts)},
				{Path: "/etc/systemd/system/" + GuestLoginUnit, Owner: "root:root", Permissions: "0644", Content: unit},
			},
			RuncmdPrepend: []any{
				"systemctl daemon-reload",
				"systemctl enable " + GuestLoginUnit,
				"systemctl start --no-block " + GuestLoginUnit,
			},
		}, nil
	}
}

var poweroffEndsVmModule UserDataModule = func(Context) (UserDataPatch, error) {
	return UserDataPatch{
		WriteFiles: []WriteFileEntry{{
			Path:        PoweroffEndsVMPath,
			Owner:       "root:root",
			Permissions: "0755",
			Content: "#!/bin/sh\n" +
				"case \"$1\" in poweroff|halt) ;; *) exit 0 ;; esac\n" +
				"[ -e /sys/class/dmi/id/sys_vendor ] && exit 0\n" +
				"echo b > /proc/sysrq-trigger\n",
		}},
	}, nil
}

func init() {
	RegisterUserDataModule("secrets", secretsModule)
	RegisterUserDataModule("k3s", k3sModule)
	RegisterUserDataModule("iroh", irohModule)
	RegisterUserDataModule(PoweroffEndsVMModule, poweroffEndsVmModule)
}
