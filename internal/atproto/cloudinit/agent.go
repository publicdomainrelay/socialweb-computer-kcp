package cloudinit

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	RequestReleaseWait = 3 * time.Minute

	RequestRevokeWait = 15 * time.Minute

	GuestRequesterReleaseEach = RequestReleaseWait + RequestRevokeWait + 2*time.Minute

	GuestRequesterReleaseUnitTimeout = GuestRequesterReleaseEach + time.Minute

	GuestRequesterStopTimeout = 2 * time.Minute

	GuestRequesterReleaseTimeout = GuestRequesterStopTimeout + GuestRequesterReleaseUnitTimeout + time.Minute
)

const (
	DefaultAgentEnvFile = "/etc/atirc/atirc.env"

	AgentIrohKeyFile = "/etc/atirc/iroh_key.hex"

	AgentConfigFile = "/etc/atirc/config.toml"

	AssociationRefDir = "/etc/socialweb-computer"

	DefaultAssociationRefFile = AssociationRefDir + "/association-ref"

	DefaultAssociationRequestFile = AssociationRefDir + "/association-request.json"

	GuestRequesterAtircEnvFile = AssociationRefDir + "/r2-atirc.env"

	GuestRequesterAgentEnvFile = AssociationRefDir + "/r2-agent.env"

	GuestRequesterGroup = "socialweb-r2"

	GuestRequesterDir = "/run/socialweb-r2"

	GuestRequesterUser = "r2"

	GuestRequesterUnit = "socialweb-r2.service"

	GuestRequesterReleaseUnit = "socialweb-r2-release.service"

	GuestRequesterRunPath = "/usr/local/bin/socialweb-r2-run.sh"

	GuestRequesterReleasePath = "/usr/local/bin/socialweb-r2-release.sh"

	GuestRequesterStartEnv = GuestRequesterDir + "/r2-start.env"

	GuestAgentAssociationPath = GuestAgentConfigDir + "/association.json"

	GuestRequesterDIDPath = GuestAgentStateDir + "/requester-did"

	SecretsFetchScriptPath = "/usr/local/bin/setup-secrets.sh"

	SecretsRefetchEnv = "SECRETS_REFETCH"

	AgentUnit = "atirc-agent.service"

	AgentRunPath = "/usr/local/bin/atirc-agent-run.sh"

	AgentEnvPathUnit = "atirc-agent-env.path"

	AgentEnvUnit = "atirc-agent-env.service"

	AgentEnvRestartPath = "/usr/local/bin/atirc-agent-env.sh"

	AgentEnvStampDir = "/run/atirc-agent"

	AgentEnvStampPath = AgentEnvStampDir + "/env.sha256"

	AgentEnvSnapshotPath = AgentEnvStampDir + "/env"

	AtircBinary = "/usr/local/bin/atirc"

	AgentUserPath = "/usr/local/bin/atirc-agent-user.sh"

	AgentExecPath = "/usr/local/bin/atirc-agent-exec.sh"

	GuestAgentAgentPath = GuestAgentConfigDir + "/agent.json"
)

type AgentOptions struct {
	EnvFile string
}

type guestAgentAgent struct {
	Unit string `json:"unit"`
}

func (opts AgentOptions) envFile() string {
	if opts.EnvFile == "" {
		return DefaultAgentEnvFile
	}
	return opts.EnvFile
}

func (opts AgentOptions) Validate() error {
	if !tunnelPathRE.MatchString(opts.envFile()) {
		return fmt.Errorf("cloudinit agent: envFile %q is not an absolute path of [A-Za-z0-9._/-]", opts.EnvFile)
	}
	return nil
}

func AgentModule(opts AgentOptions) UserDataModule {
	return func(Context) (UserDataPatch, error) {
		return agentPatch(opts)
	}
}

func agentPatch(opts AgentOptions) (UserDataPatch, error) {
	if err := opts.Validate(); err != nil {
		return UserDataPatch{}, err
	}
	raw, err := json.Marshal(guestAgentAgent{Unit: AgentUnit})
	if err != nil {
		return UserDataPatch{}, fmt.Errorf("cloudinit agent: encoding %s: %w", GuestAgentAgentPath, err)
	}
	return UserDataPatch{
		WriteFiles: []WriteFileEntry{
			{Path: "/etc/systemd/system/" + AgentUnit, Owner: "root:root", Permissions: "0644", Content: unitFile(
				[]string{
					"Description=atirc agent",
					"After=network-online.target " + GuestBinariesUnit + " " + strings.Join(SecretsReadyUnits, " "),
					"Wants=network-online.target " + GuestBinariesUnit + " " + GuestSecretsUnit,
					"StartLimitIntervalSec=0",
				},
				[]string{
					"Type=simple",
					"ExecStart=" + AgentRunPath + " " + opts.envFile(),
					"Restart=always",
					"RestartSec=30",
					"StandardOutput=journal",
					"StandardError=journal",
				})},
			{Path: AgentRunPath, Owner: "root:root", Permissions: "0700", Content: agentRunScript},
			{Path: AgentUserPath, Owner: "root:root", Permissions: "0700", Content: agentUserScript},
			{Path: AgentExecPath, Owner: "root:root", Permissions: "0700", Content: agentExecScript},
			{Path: AgentEnvRestartPath, Owner: "root:root", Permissions: "0700", Content: agentEnvRestartScript},
			{Path: "/etc/systemd/system/" + AgentEnvUnit, Owner: "root:root", Permissions: "0644", Content: strings.Join([]string{
				"[Unit]",
				"Description=restart a running atirc agent when its env file changed",
				"",
				"[Service]",
				"Type=oneshot",
				"ExecStart=" + AgentEnvRestartPath + " " + opts.envFile(),
				"StandardOutput=journal",
				"StandardError=journal",
				"",
			}, "\n")},
			{Path: "/etc/systemd/system/" + AgentEnvPathUnit, Owner: "root:root", Permissions: "0644", Content: strings.Join([]string{
				"[Unit]",
				"Description=watch the atirc agent env file",
				"",
				"[Path]",
				"PathChanged=" + opts.envFile(),
				"Unit=" + AgentEnvUnit,
				"",
				"[Install]",
				"WantedBy=paths.target",
				"",
			}, "\n")},
			{Path: GuestAgentAgentPath, Owner: "root:root", Permissions: "0644", Content: string(raw) + "\n"},
		},
		RuncmdPrepend: []any{"systemctl daemon-reload", "systemctl enable --now " + AgentEnvPathUnit},
	}, nil
}

const agentEnvRestartScript = `#!/usr/bin/env bash
set -euo pipefail
ENV_FILE="$1"
now="$(sha256sum < "${ENV_FILE}" 2>/dev/null || sha256sum < /dev/null)"
if [ "${now}" = "$(cat "` + AgentEnvStampPath + `" 2>/dev/null || true)" ]; then
  exit 0
fi
echo "atirc-agent-env: ${ENV_FILE} changed; restarting ` + AgentUnit + ` if it runs" >&2
exec systemctl try-restart --no-block ` + AgentUnit + `
`

const agentRunScript = `#!/usr/bin/env bash
set -euo pipefail
ENV_FILE="$1"
install -d -m 0700 -o root -g root "` + AgentEnvStampDir + `"
if [ -f "${ENV_FILE}" ]; then cp -f "${ENV_FILE}" "` + AgentEnvSnapshotPath + `"; else : > "` + AgentEnvSnapshotPath + `"; fi
chmod 0600 "` + AgentEnvSnapshotPath + `"
sha256sum < "` + AgentEnvSnapshotPath + `" > "` + AgentEnvStampPath + `"
while IFS= read -r line || [ -n "${line}" ]; do
  if [[ "${line}" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then
    if ! (export "${line}") 2>/dev/null; then
      echo "atirc-agent: skipped ${line%%=*} from the env file (not exportable)" >&2
      continue
    fi
    export "${line}"
  fi
done < "` + AgentEnvSnapshotPath + `"
if [ ! -f "` + GuestSecretsReadyPath + `" ] && [ ! -f "` + SecretsDoneStamp + `" ]; then
  echo "atirc-agent: the secrets bundle is not written yet (` + GuestSecretsReadyPath + `); the agent waits" >&2
  exit 1
fi
if [ ! -x "` + AtircBinary + `" ]; then
  echo "atirc-agent: ` + AtircBinary + ` is not installed yet (` + GuestBinariesUnit + `); the agent waits" >&2
  exit 1
fi
if [ -z "${ATP_USERNAME:-}" ] || [ -z "${ATIRC_NICK:-}" ] || [ ! -s "` + AgentIrohKeyFile + `" ]; then
  echo "atirc-agent: the mesh identity (ATP_USERNAME, ATIRC_NICK, ` + AgentIrohKeyFile + `) is not in the secrets bundle yet; the agent waits" >&2
  exit 1
fi
hex="$(tr -d '[:space:]' < "` + AgentIrohKeyFile + `")"
if ! [[ "${hex}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "atirc-agent: ` + AgentIrohKeyFile + ` is not 32 bytes of hex" >&2
  exit 1
fi
export ATP_USERNAME ATIRC_NICK
nick="$(` + AgentUserPath + `)"
uid="$(id -u "${nick}")"
home="$(getent passwd "${nick}" | cut -d: -f6)"
dir="${home}/.config/atirc"
install -d -m 0700 -o "${nick}" -g "${nick}" "${home}/.config" "${dir}"
(umask 077; printf '%b' "$(printf '%s' "${hex}" | sed 's/../\\x&/g')" > "${dir}/iroh_key.tmp")
chown "${nick}:${nick}" "${dir}/iroh_key.tmp"
chmod 0600 "${dir}/iroh_key.tmp"
mv -f "${dir}/iroh_key.tmp" "${dir}/iroh_key"
if [ -s "` + AgentConfigFile + `" ]; then
  install -m 0600 -o "${nick}" -g "${nick}" "` + AgentConfigFile + `" "${dir}/config.toml"
fi
run=(runuser -u "${nick}" -- env XDG_RUNTIME_DIR="/run/user/${uid}" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${uid}/bus" ATIRC_CONFIG_DIR="${dir}")
"${run[@]}" ` + AtircBinary + ` agent run &
exec "${run[@]}" ` + AtircBinary + ` mesh run --no-publish
`

const agentUserScript = `#!/usr/bin/env bash
set -euo pipefail
nick="${ATIRC_NICK:-$(` + AtircBinary + ` nick)}"
if ! [[ "${nick}" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]]; then
  echo "atirc-agent: atirc nick printed ${nick@Q}, not a user name" >&2
  exit 1
fi
getent group wheel >/dev/null || groupadd wheel
mkdir -p /etc/sudoers.d
printf '%%wheel ALL=(ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/wheel
chmod 440 /etc/sudoers.d/wheel
id "${nick}" >/dev/null 2>&1 || useradd -m -G wheel -s /bin/bash "${nick}"
if getent group ` + GuestRequesterGroup + ` >/dev/null; then usermod -aG ` + GuestRequesterGroup + ` "${nick}"; fi
loginctl enable-linger "${nick}" >&2
uid="$(id -u "${nick}")"
for _ in $(seq 1 30); do
  [ -S "/run/user/${uid}/bus" ] && break
  sleep 1
done
printf '%s\n' "${nick}"
`

const agentExecScript = `#!/usr/bin/env bash
set -euo pipefail
if [ -z "${ATIRC_TOKEN:-}" ]; then
  echo "atirc-agent-exec: ATIRC_TOKEN is empty" >&2
  exit 1
fi
nick="$(` + AgentUserPath + `)"
uid="$(id -u "${nick}")"
home="$(getent passwd "${nick}" | cut -d: -f6)"
cd "${home}"
exec runuser -u "${nick}" -- env XDG_RUNTIME_DIR="/run/user/${uid}" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${uid}/bus" bash -c "$1"
`

func AssociationRefPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultAssociationRefFile, nil
	}
	clean := path.Clean(raw)
	if !strings.HasPrefix(raw, "/") || slices.Contains(strings.Split(raw, "/"), "..") || !strings.HasPrefix(clean, AssociationRefDir+"/") {
		return "", fmt.Errorf("cloudinit association: refFile %q is not an absolute path under %s/ without a .. segment", raw, AssociationRefDir)
	}
	return clean, nil
}

type GuestAgentAssociation struct {
	RefFile string `json:"refFile"`
}

func AssociationModule(refFile string) UserDataModule {
	return func(Context) (UserDataPatch, error) {
		clean, err := AssociationRefPath(refFile)
		if err != nil {
			return UserDataPatch{}, err
		}
		raw, err := json.Marshal(GuestAgentAssociation{RefFile: clean})
		if err != nil {
			return UserDataPatch{}, fmt.Errorf("cloudinit association: encoding %s: %w", GuestAgentAssociationPath, err)
		}
		after := []string{"After=network-online.target " + GuestBinariesUnit + " " + strings.Join(SecretsReadyUnits, " "),
			"Wants=network-online.target " + GuestBinariesUnit + " " + GuestSecretsUnit}
		return UserDataPatch{WriteFiles: []WriteFileEntry{
			{Path: GuestAgentAssociationPath, Owner: "root:root", Permissions: "0644", Content: string(raw) + "\n"},
			{Path: GuestRequesterRunPath, Owner: "root:root", Permissions: "0755", Content: guestRequesterRunScript},
			{Path: GuestRequesterReleasePath, Owner: "root:root", Permissions: "0755", Content: guestRequesterReleaseScript},
			{Path: "/etc/systemd/system/" + GuestRequesterUnit, Owner: "root:root", Permissions: "0644", Content: unitFile(
				append([]string{"Description=the guest requester (plan 0036 A7), started by guest.r2Start"}, after...),
				[]string{"Type=exec", "User=" + GuestRequesterUser, "ExecStart=" + GuestRequesterRunPath,
					"TimeoutStopSec=" + seconds(GuestRequesterStopTimeout-30*time.Second), "StandardOutput=journal", "StandardError=journal"})},
			{Path: "/etc/systemd/system/" + GuestRequesterReleaseUnit, Owner: "root:root", Permissions: "0644", Content: unitFile(
				append([]string{"Description=release every contract of the guest requester (plan 0036 A8), started by guest.r2Release"}, after...),
				[]string{"Type=oneshot", "User=" + GuestRequesterUser, "ExecStart=" + GuestRequesterReleasePath,
					"TimeoutStartSec=" + seconds(GuestRequesterReleaseUnitTimeout), "StandardOutput=journal", "StandardError=journal"})},
		}, RuncmdPrepend: []any{
			"groupadd -f " + GuestRequesterGroup,
			"id " + GuestRequesterUser + " >/dev/null 2>&1 || useradd -m -s /bin/bash -G " + GuestRequesterGroup + " " + GuestRequesterUser,
			"loginctl enable-linger " + GuestRequesterUser,
			"systemctl daemon-reload",
		}}, nil
	}
}

const guestRequesterEnv = `uid="$(id -u)"
export XDG_RUNTIME_DIR="/run/user/${uid}" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${uid}/bus"
for _ in $(seq 1 30); do
  [ -S "${XDG_RUNTIME_DIR}/bus" ] && break
  sleep 1
done
`

const guestRequesterRunScript = `#!/usr/bin/env bash
set -euo pipefail
. ` + GuestRequesterStartEnv + `
` + guestRequesterEnv + `args=(run request --name "${R2_NAME}" --guest-requester)
[ "${R2_AGENT:-}" = 1 ] && args+=(--agent)
[ "${R2_HOLD:-}" = 1 ] && args+=(--hold)
[ -n "${R2_COLOCATED:-}" ] && args+=(--colocated "${R2_COLOCATED}")
[ "${R2_IDENTITY:-}" = 1 ] && args+=(--identity-only)
socialweb-computer "${args[@]}"
if [ "${R2_AGENT:-}" = 1 ] && [ "${R2_HOLD:-}" = 1 ]; then
  exec socialweb-computer run request agent "${R2_NAME}"
fi
`

var guestRequesterReleaseScript = `#!/usr/bin/env bash
set -uo pipefail
command -v socialweb-computer >/dev/null || exit 0
` + guestRequesterEnv + `fail=0
pids=()
for name in $(socialweb-computer run request list --json | jq -r '.[] | select(.phase != "Released") | .name'); do
  timeout --kill-after=30 ` + seconds(GuestRequesterReleaseEach) + ` socialweb-computer run request release --contract-timeout ` + seconds(RequestReleaseWait) + `s "${name}" &
  pids+=("$!")
done
for pid in "${pids[@]}"; do
  wait "${pid}" || fail=1
done
exit "${fail}"
`

func seconds(d time.Duration) string {
	return strconv.Itoa(int(d / time.Second))
}
