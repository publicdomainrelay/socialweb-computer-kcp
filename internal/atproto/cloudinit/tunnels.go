package cloudinit

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	TunnelsModuleID = "tunnels"

	OnNetworkScriptPath = "/usr/local/bin/send-onnetwork.sh"

	DefaultTunnelServer = "https://opentunnel.xyz"

	GuestTunnelsSetupPath = "/usr/local/bin/setup-guest-tunnels.sh"

	GuestTunnelScriptPath = "/usr/local/bin/guest-tunnel.sh"

	GuestTunnelReportPath = "/usr/local/bin/guest-tunnel-report.sh"

	GuestTunnelRunDir = "/run/guest-tunnels"

	GuestTunnelStateDir = "/var/lib/guest-tunnels"

	OpenTunnelBinary = "/usr/local/bin/opentunnel"
)

var (
	tunnelTagRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

	tunnelServerRE = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

	pinURLRE = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?=&%+-]+$`)

	pinNameRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Tunnel struct {
	Tag string

	Port int

	Server string
}

type GuestBinaryPin struct {
	Arch string

	URL string

	SHA256 string

	Member string
}

type TunnelsOptions struct {
	Tunnels []Tunnel

	OpenTunnel []GuestBinaryPin
}

func TunnelURLPath(tag string) string {
	return GuestTunnelRunDir + "/" + tag + ".url"
}

func GuestBinarySetupUnit(name string) string {
	return "setup-guest-" + name + ".service"
}

func GuestBinaryStamp(name string) string {
	return "/var/lib/setup-guest-" + name + ".done"
}

func TunnelUnit(tag string) string {
	return "guest-tunnel-" + tag + ".service"
}

func TunnelReportUnit(tag string) string {
	return "guest-tunnel-report-" + tag + ".service"
}

func (t Tunnel) server() string {
	return strings.TrimRight(cmp.Or(strings.TrimSpace(t.Server), DefaultTunnelServer), "/")
}

func (opts TunnelsOptions) Validate() error {
	if len(opts.Tunnels) == 0 {
		return errors.New("cloudinit tunnels: no tunnel is asked for")
	}
	tags := map[string]bool{}
	for _, t := range opts.Tunnels {
		if !tunnelTagRE.MatchString(t.Tag) {
			return fmt.Errorf("cloudinit tunnels: tag %q is not a DNS label of at most 32 characters", t.Tag)
		}
		if tags[t.Tag] {
			return fmt.Errorf("cloudinit tunnels: tag %q is asked for twice", t.Tag)
		}
		tags[t.Tag] = true
		if t.Port < 1 || t.Port > 65535 {
			return fmt.Errorf("cloudinit tunnels: tunnel %s port %d is not a port", t.Tag, t.Port)
		}
		if !tunnelServerRE.MatchString(t.server()) {
			return fmt.Errorf("cloudinit tunnels: tunnel %s server %q is not an http(s) origin", t.Tag, t.Server)
		}
	}
	return checkPins("opentunnel", opts.OpenTunnel)
}

func checkPins(name string, pins []GuestBinaryPin) error {
	if len(pins) == 0 {
		return fmt.Errorf("cloudinit tunnels: no %s pin", name)
	}
	for _, p := range pins {
		switch {
		case p.Arch != "x86_64" && p.Arch != "aarch64":
			return fmt.Errorf("cloudinit tunnels: %s pin arch %q is not x86_64 or aarch64", name, p.Arch)
		case !sha256RE.MatchString(p.SHA256):
			return fmt.Errorf("cloudinit tunnels: %s pin for %s has no sha256", name, p.Arch)
		case !pinNameRE.MatchString(p.Member) || strings.Contains(p.Member, ".."):
			return fmt.Errorf("cloudinit tunnels: %s pin for %s member %q is not a path", name, p.Arch, p.Member)
		case !pinURLRE.MatchString(p.URL):
			return fmt.Errorf("cloudinit tunnels: %s pin for %s url %q is not an https URL", name, p.Arch, p.URL)
		}
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
	binaries := []string{"opentunnel"}
	files := []WriteFileEntry{
		{Path: GuestTunnelsSetupPath, Owner: "root:root", Permissions: "0700", Content: tunnelsSetupScript(opts)},
	}
	for _, name := range binaries {
		files = append(files, WriteFileEntry{Path: "/etc/systemd/system/" + GuestBinarySetupUnit(name), Owner: "root:root", Permissions: "0644", Content: unitFile(
			[]string{
				"Description=Install the pinned " + name + " binary of the contract's tunnels",
				"After=network-online.target",
				"Wants=network-online.target",
			},
			[]string{
				"Type=oneshot",
				"RemainAfterExit=yes",
				"TimeoutStartSec=900",
				"Restart=on-failure",
				"RestartSec=5",
				"ExecStart=" + GuestTunnelsSetupPath + " " + name,
				"StandardOutput=journal+console",
				"StandardError=journal+console",
			})})
	}
	files = append(files,
		WriteFileEntry{Path: GuestTunnelScriptPath, Owner: "root:root", Permissions: "0700", Content: guestTunnelScript},
		WriteFileEntry{Path: GuestTunnelReportPath, Owner: "root:root", Permissions: "0700", Content: guestTunnelReportScript},
	)
	early := []string{}
	for _, name := range binaries {
		early = append(early, GuestBinarySetupUnit(name))
	}
	units := slices.Clone(early)
	openTunnelSetup := GuestBinarySetupUnit("opentunnel")
	for _, t := range opts.Tunnels {
		port := strconv.Itoa(t.Port)
		files = append(files,
			WriteFileEntry{Path: "/etc/systemd/system/" + TunnelUnit(t.Tag), Owner: "root:root", Permissions: "0644", Content: unitFile(
				[]string{
					"Description=opentunnel route of guest tunnel " + t.Tag + " to 127.0.0.1:" + port,
					"After=network-online.target " + openTunnelSetup,
					"Wants=network-online.target",
					"Requires=" + openTunnelSetup,
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
		early = append(early, TunnelUnit(t.Tag))
		units = append(units, TunnelUnit(t.Tag))
	}
	list := strings.Join(units, " ")
	return UserDataPatch{
		Packages:   []string{"jq", "curl", "ca-certificates"},
		WriteFiles: files,
		RuncmdPrepend: []any{
			"systemctl daemon-reload",
			"systemctl start --no-block " + strings.Join(early, " "),
		},
		Runcmd: []any{
			"systemctl enable " + list,
			"systemctl start --no-block " + list,
		},
	}, nil
}

func unitFile(unit, service []string) string {
	lines := append([]string{"[Unit]"}, unit...)
	lines = append(lines, "", "[Service]")
	lines = append(lines, service...)
	lines = append(lines, "", "[Install]", "WantedBy=multi-user.target", "")
	return strings.Join(lines, "\n")
}

func tunnelsSetupScript(opts TunnelsOptions) string {
	var arches strings.Builder
	for _, arch := range []string{"x86_64", "aarch64"} {
		for _, p := range opts.OpenTunnel {
			if p.Arch == arch {
				fmt.Fprintf(&arches, "  %s:opentunnel) fetch opentunnel %q %q %q ;;\n", arch, p.URL, p.SHA256, p.Member)
			}
		}
	}
	return `#!/usr/bin/env bash
set -euo pipefail

NAME="$1"
STAMP="` + GuestBinaryStamp("${NAME}") + `"
[ -f "${STAMP}" ] && exit 0

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

fetch() {
  local name="$1" url="$2" sum="$3" member="$4"
  local args
  for attempt in 1 2 3 4 5; do
    args=(-fsSL --connect-timeout 20 --max-time 300)
    rm -rf "${TMP:?}/${name}" "${TMP}/${name}.tar.gz"
    if curl "${args[@]}" -o "${TMP}/${name}.tar.gz" "${url}" \
        && printf '%s  %s\n' "${sum}" "${TMP}/${name}.tar.gz" | sha256sum -c --quiet - \
        && mkdir -p "${TMP}/${name}" \
        && tar -xzf "${TMP}/${name}.tar.gz" -C "${TMP}/${name}" "${member}"; then
      install -m 0755 "${TMP}/${name}/${member}" "/usr/local/bin/${name}"
      echo "setup-guest-tunnels: installed /usr/local/bin/${name} from ${url} (sha256 ${sum})"
      return 0
    fi
    echo "setup-guest-tunnels: ${name} fetch or check failed (attempt ${attempt}): ${url}" >&2
    if [ "${attempt}" -lt 5 ]; then sleep "${attempt}"; fi
  done
  echo "setup-guest-tunnels: ${name} failed after 5 attempts: ${url}" >&2
  exit 1
}

case "$(uname -m)" in
  amd64) ARCH=x86_64 ;;
  arm64) ARCH=aarch64 ;;
  *) ARCH="$(uname -m)" ;;
esac

case "${ARCH}:${NAME}" in
` + arches.String() + `  *) echo "setup-guest-tunnels: no ${NAME} pin for machine type ${ARCH}" >&2; exit 1 ;;
esac

touch "${STAMP}"
`
}

const guestTunnelScript = `#!/usr/bin/env bash
set -euo pipefail
TAG="$1"
PORT="$2"
API="$3"
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
