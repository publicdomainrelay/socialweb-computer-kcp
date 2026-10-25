package cloudinit

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	TunnelsModuleID = "tunnels"

	OnNetworkScriptPath = "/usr/local/bin/send-onnetwork.sh"

	DefaultTunnelServer = "https://opentunnel.xyz"

	GuestTunnelScriptPath = "/usr/local/bin/guest-tunnel.sh"

	GuestTunnelReportPath = "/usr/local/bin/guest-tunnel-report.sh"

	GuestTunnelRunDir = "/run/guest-tunnels"

	GuestTunnelStateDir = "/var/lib/guest-tunnels"

	OpenTunnelBinary = "/usr/local/bin/opentunnel"

	GuestBinariesUnit = "setup-guest-binaries.service"

	GuestAgentConfigDir = "/etc/socialweb-guest-agent"

	GuestAgentTunnelsPath = GuestAgentConfigDir + "/tunnels.json"

	TunnelTransportOpenTunnel = "opentunnel"

	TunnelTransportWebRTC = "webrtc"

	DefaultShellEnvFile = "/etc/wootty/wootty.env"

	ShellTokenMinLength = 32

	WoottyRunPath = "/usr/local/bin/wootty-run.sh"

	WoottyBinary = "/usr/local/bin/wootty"

	ShellUnitPrefix = "wootty-"
)

var (
	tunnelTagRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

	tunnelServerRE = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

	tunnelPathRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

type Tunnel struct {
	Tag string

	Port int

	Server string

	Transport string

	ShellEnvFile string
}

type TunnelsOptions struct {
	Tunnels []Tunnel
}

type guestAgentTunnel struct {
	Tag string `json:"tag"`

	Port int `json:"port"`
}

type guestAgentTunnels struct {
	Tunnels []guestAgentTunnel `json:"tunnels"`
}

func TunnelURLPath(tag string) string {
	return GuestTunnelRunDir + "/" + tag + ".url"
}

func TunnelUnit(tag string) string {
	return "guest-tunnel-" + tag + ".service"
}

func TunnelReportUnit(tag string) string {
	return "guest-tunnel-report-" + tag + ".service"
}

func ShellUnit(tag string) string {
	return ShellUnitPrefix + tag + ".service"
}

func (t Tunnel) server() string {
	return strings.TrimRight(cmp.Or(strings.TrimSpace(t.Server), DefaultTunnelServer), "/")
}

func (t Tunnel) transport() string {
	return cmp.Or(strings.TrimSpace(t.Transport), TunnelTransportOpenTunnel)
}

func (opts TunnelsOptions) Validate() error {
	if len(opts.Tunnels) == 0 {
		return errors.New("cloudinit tunnels: no tunnel is asked for")
	}
	tags := map[string]bool{}
	shellPorts := map[int]string{}
	for _, t := range opts.Tunnels {
		if !tunnelTagRE.MatchString(t.Tag) {
			return fmt.Errorf("cloudinit tunnels: tag %q is not a DNS label of at most 32 characters", t.Tag)
		}
		if tags[t.Tag] {
			return fmt.Errorf("cloudinit tunnels: tag %q is asked for twice", t.Tag)
		}
		if t.Tag == GuestSecretsTag {
			return fmt.Errorf("cloudinit tunnels: tag %q names the guest's secrets key, not a tunnel", t.Tag)
		}
		tags[t.Tag] = true
		if t.Port < 1 || t.Port > 65535 {
			return fmt.Errorf("cloudinit tunnels: tunnel %s port %d is not a port", t.Tag, t.Port)
		}
		switch t.transport() {
		case TunnelTransportOpenTunnel:
			if !tunnelServerRE.MatchString(t.server()) {
				return fmt.Errorf("cloudinit tunnels: tunnel %s server %q is not an http(s) origin", t.Tag, t.Server)
			}
		case TunnelTransportWebRTC:
			if strings.TrimSpace(t.Server) != "" {
				return fmt.Errorf("cloudinit tunnels: tunnel %s names a server; only an opentunnel tunnel has one", t.Tag)
			}
		default:
			return fmt.Errorf("cloudinit tunnels: tunnel %s transport %q is not opentunnel or webrtc", t.Tag, t.Transport)
		}
		if t.ShellEnvFile == "" {
			continue
		}
		if t.transport() != TunnelTransportOpenTunnel {
			return fmt.Errorf("cloudinit tunnels: tunnel %s asks for a wootty shell on transport %s; a shell is served over opentunnel only", t.Tag, t.transport())
		}
		if !tunnelPathRE.MatchString(t.ShellEnvFile) || strings.Contains(t.ShellEnvFile, "..") {
			return fmt.Errorf("cloudinit tunnels: tunnel %s envFile %q is not an absolute path", t.Tag, t.ShellEnvFile)
		}
		if other, ok := shellPorts[t.Port]; ok {
			return fmt.Errorf("cloudinit tunnels: shells %s and %s both ask for port %d", other, t.Tag, t.Port)
		}
		shellPorts[t.Port] = t.Tag
	}
	return nil
}

func TunnelsModule(opts TunnelsOptions) UserDataModule {
	return func(Context) (UserDataPatch, error) {
		return tunnelsPatch(opts)
	}
}

var tunnelsModule UserDataModule = func(ctx Context) (UserDataPatch, error) {
	if ctx.Tunnels == nil {
		return UserDataPatch{}, errors.New("cloudinit tunnels: the tunnels module needs Context.Tunnels")
	}
	return tunnelsPatch(*ctx.Tunnels)
}

func tunnelsPatch(opts TunnelsOptions) (UserDataPatch, error) {
	if err := opts.Validate(); err != nil {
		return UserDataPatch{}, err
	}
	patch := UserDataPatch{}
	webrtc := guestAgentTunnels{Tunnels: []guestAgentTunnel{}}
	units := []string{}
	shells := false
	for _, t := range opts.Tunnels {
		if t.transport() == TunnelTransportWebRTC {
			webrtc.Tunnels = append(webrtc.Tunnels, guestAgentTunnel{Tag: t.Tag, Port: t.Port})
			continue
		}
		port := strconv.Itoa(t.Port)
		patch.WriteFiles = append(patch.WriteFiles,
			WriteFileEntry{Path: "/etc/systemd/system/" + TunnelUnit(t.Tag), Owner: "root:root", Permissions: "0644", Content: unitFile(
				[]string{
					"Description=opentunnel route of guest tunnel " + t.Tag + " to 127.0.0.1:" + port,
					"After=network-online.target " + GuestBinariesUnit,
					"Wants=network-online.target " + GuestBinariesUnit,
					"StartLimitIntervalSec=0",
				},
				[]string{
					"Type=simple",
					"ExecStart=" + GuestTunnelScriptPath + " " + t.Tag + " " + port + " " + t.server(),
					"Restart=always",
					"RestartSec=10",
					"StandardOutput=journal+console",
					"StandardError=journal+console",
				})},
			WriteFileEntry{Path: "/etc/systemd/system/" + TunnelReportUnit(t.Tag), Owner: "root:root", Permissions: "0644", Content: unitFile(
				[]string{
					"Description=Report the URL of guest tunnel " + t.Tag + " to the market",
					"After=network-online.target " + GuestLoginUnit,
				},
				[]string{
					"Type=oneshot",
					"TimeoutStartSec=600",
					"ExecStart=" + GuestTunnelReportPath + " " + t.Tag,
					"StandardOutput=journal+console",
					"StandardError=journal+console",
				})},
		)
		units = append(units, TunnelUnit(t.Tag))
		if t.ShellEnvFile == "" {
			continue
		}
		shells = true
		patch.WriteFiles = append(patch.WriteFiles, WriteFileEntry{Path: "/etc/systemd/system/" + ShellUnit(t.Tag), Owner: "root:root", Permissions: "0644", Content: unitFile(
			[]string{
				"Description=wootty browser shell of guest tunnel " + t.Tag + " on 127.0.0.1:" + port,
				"After=network-online.target " + GuestBinariesUnit + " " + strings.Join(SecretsReadyUnits, " "),
				"Wants=network-online.target " + GuestBinariesUnit + " " + GuestSecretsUnit,
				"StartLimitIntervalSec=0",
			},
			[]string{
				"Type=simple",
				"EnvironmentFile=-" + t.ShellEnvFile,
				"Environment=HOME=/root TERM=xterm-256color",
				"WorkingDirectory=/root",
				"ExecStart=" + WoottyRunPath + " " + port,
				"Restart=always",
				"RestartSec=5",
				"StandardOutput=journal",
				"StandardError=journal",
			})})
		units = append(units, ShellUnit(t.Tag))
	}
	if shells {
		patch.WriteFiles = append(patch.WriteFiles, WriteFileEntry{Path: WoottyRunPath, Owner: "root:root", Permissions: "0700", Content: woottyRunScript})
	}
	if len(webrtc.Tunnels) > 0 {
		raw, err := json.Marshal(webrtc)
		if err != nil {
			return UserDataPatch{}, fmt.Errorf("cloudinit tunnels: encoding %s: %w", GuestAgentTunnelsPath, err)
		}
		patch.WriteFiles = append(patch.WriteFiles,
			WriteFileEntry{Path: GuestAgentTunnelsPath, Owner: "root:root", Permissions: "0644", Content: string(raw) + "\n"})
	}
	if len(units) == 0 {
		return patch, nil
	}
	patch.Packages = []string{"jq", "curl", "ca-certificates"}
	patch.WriteFiles = append(patch.WriteFiles,
		WriteFileEntry{Path: GuestTunnelScriptPath, Owner: "root:root", Permissions: "0700", Content: guestTunnelScript},
		WriteFileEntry{Path: GuestTunnelReportPath, Owner: "root:root", Permissions: "0700", Content: guestTunnelReportScript},
	)
	list := strings.Join(units, " ")
	patch.RuncmdPrepend = []any{
		"systemctl daemon-reload",
		"systemctl start --no-block " + list,
	}
	patch.Runcmd = []any{
		"systemctl enable " + list,
		"systemctl start --no-block " + list,
	}
	return patch, nil
}

func unitFile(unit, service []string) string {
	lines := append([]string{"[Unit]"}, unit...)
	lines = append(lines, "", "[Service]")
	lines = append(lines, service...)
	lines = append(lines, "", "[Install]", "WantedBy=multi-user.target", "")
	return strings.Join(lines, "\n")
}

const guestTunnelScript = `#!/usr/bin/env bash
set -euo pipefail
TAG="$1"
PORT="$2"
API="$3"
if [ ! -x "` + OpenTunnelBinary + `" ]; then
  echo "guest-tunnel: ` + OpenTunnelBinary + ` is not installed yet (` + GuestBinariesUnit + `); ${TAG} retries" >&2
  exit 1
fi
BASE="` + GuestTunnelStateDir + `/${TAG}"
export HOME=/root
export XDG_CONFIG_HOME="${BASE}/config"
export XDG_DATA_HOME="${BASE}/data"
export XDG_STATE_HOME="${BASE}/state"
export XDG_RUNTIME_DIR="` + GuestTunnelRunDir + `/${TAG}"
install -d -m 0700 -o root -g root "` + GuestTunnelStateDir + `" "${BASE}" "${XDG_CONFIG_HOME}" "${XDG_CONFIG_HOME}/opentunnel" \
  "${XDG_DATA_HOME}" "${XDG_STATE_HOME}" "` + GuestTunnelRunDir + `" "${XDG_RUNTIME_DIR}"
CONF="${XDG_CONFIG_HOME}/opentunnel/${TAG}.toml"
if [ ! -s "${CONF}" ]; then
  ROUTE="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  ( umask 077; printf '[routes]\n%s = "127.0.0.1:%s"\n' "${ROUTE}" "${PORT}" > "${CONF}.tmp" )
  mv -f "${CONF}.tmp" "${CONF}"
fi
ROUTE="$(sed -n 's/^\([0-9a-f]\{16\}\) = .*/\1/p' "${CONF}" | head -n 1)"
[ -n "${ROUTE}" ] || { echo "guest-tunnel: ${CONF} names no route" >&2; exit 1; }
OT=(` + OpenTunnelBinary + ` --profile "${TAG}" --api "${API}")
IDENTITY="${XDG_DATA_HOME}/opentunnel/${TAG}/tunnel.json"
if [ ! -s "${IDENTITY}" ]; then
  echo "guest-tunnel: creating the ${TAG} tunnel at ${API} (the certificate can take minutes)"
  "${OT[@]}" up
  "${OT[@]}" down || true
fi
HOST="$(jq -r '.hostname // empty' "${IDENTITY}")"
[ -n "${HOST}" ] || { echo "guest-tunnel: ${IDENTITY} names no hostname" >&2; exit 1; }
URL="https://${ROUTE}.${HOST}"
printf '%s\n' "${URL}" > "` + GuestTunnelRunDir + `/${TAG}.url.tmp"
mv -f "` + GuestTunnelRunDir + `/${TAG}.url.tmp" "` + GuestTunnelRunDir + `/${TAG}.url"
echo "guest-tunnel: ${TAG} serves 127.0.0.1:${PORT} at ${URL}"
systemctl start --no-block "guest-tunnel-report-${TAG}.service"
exec "${OT[@]}" serve
`

const guestTunnelReportScript = `#!/usr/bin/env bash
set -uo pipefail
TAG="$1"
URL="$(cat "` + GuestTunnelRunDir + `/${TAG}.url" 2>/dev/null || true)"
if [ -z "${URL}" ]; then echo "guest-tunnel-report: no URL for ${TAG} yet" >&2; exit 1; fi
if [ ! -x "` + OnNetworkScriptPath + `" ]; then
  echo "guest-tunnel-report: the bidder installed no ` + OnNetworkScriptPath + `; ${TAG} at ${URL} stays unreported" >&2
  exit 0
fi
exec "` + OnNetworkScriptPath + `" "${TAG}" "${URL}"
`

var woottyRunScript = `#!/usr/bin/env bash
set -euo pipefail
PORT="$1"
if [ ! -f "` + GuestSecretsReadyPath + `" ] && [ ! -f "` + SecretsDoneStamp + `" ]; then
  echo "wootty: the secrets bundle is not written yet (` + GuestSecretsReadyPath + `); the shell waits" >&2
  exit 1
fi
if [ ! -x "` + WoottyBinary + `" ]; then
  echo "wootty: ` + WoottyBinary + ` is not installed yet (` + GuestBinariesUnit + `); the shell waits" >&2
  exit 1
fi
TOKEN="${WOOTTY_AUTH_TOKEN:-}"
if [ "${#TOKEN}" -lt ` + strconv.Itoa(ShellTokenMinLength) + ` ]; then
  echo "wootty: WOOTTY_AUTH_TOKEN from the secrets bundle is missing or shorter than ` + strconv.Itoa(ShellTokenMinLength) + ` characters; no shell starts without auth" >&2
  exit 1
fi
unset TOKEN
export WOOTTY_HOST=127.0.0.1
export WOOTTY_PORT="${PORT}"
unset WOOTTY_ALLOW_INSECURE_NO_AUTH
exec ` + WoottyBinary + ` run /bin/bash -l
`
