package guestseed

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
)

const (
	GuestBinariesUnit = cloudinit.GuestBinariesUnit

	GuestBinariesScript = "/usr/local/bin/setup-guest-binaries.sh"

	GuestBinariesDir = "/usr/local/bin"

	GuestBinaryOpenTunnel = "opentunnel"

	GuestBinaryAgent = "socialweb-guest-agent"

	GuestBinaryWootty = "wootty"

	GuestAgentUnit = "socialweb-guest-agent.service"

	GuestSecretsUnit = cloudinit.GuestSecretsUnit

	GuestAgentBinary = GuestBinariesDir + "/" + GuestBinaryAgent

	GuestAgentBidderPath = cloudinit.GuestAgentConfigDir + "/bidder.json"

	GuestDownloadsRoute = "/v1/downloads"

	guestBinaryAttempts = 60
)

var (
	GuestArches = []string{"x86_64", "aarch64"}

	sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

	guestBinaryNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

	unitNameRE = regexp.MustCompile(`^[A-Za-z0-9@._-]+\.service$`)
)

type GuestBinaryPin struct {
	Name string

	Arch string

	Sha256 string
}

type GuestBinaryNeeds struct {
	OpenTunnel bool

	Agent bool

	Wootty bool

	Secrets bool

	Units []string
}

type guestAgentBidder struct {
	Base string `json:"base"`

	Audience string `json:"audience"`

	AcceptPath string `json:"acceptPath"`
}

func GuestDownloadName(name, arch string) string {
	return name + "-" + arch
}

func (n GuestBinaryNeeds) Any() bool {
	return n.OpenTunnel || n.Agent || n.Wootty
}

func (n GuestBinaryNeeds) Names() []string {
	var out []string
	if n.OpenTunnel {
		out = append(out, GuestBinaryOpenTunnel)
	}
	if n.Wootty {
		out = append(out, GuestBinaryWootty)
	}
	if n.Agent {
		out = append(out, GuestBinaryAgent)
	}
	return out
}

func GuestBinariesNeeded(userData string) GuestBinaryNeeds {
	const unitDir = "/etc/systemd/system/"
	out := GuestBinaryNeeds{}
	for _, p := range cloudinit.WriteFilePaths(userData) {
		switch {
		case p == cloudinit.GuestAgentTunnelsPath:
			out.Agent = true
		case p == cloudinit.GuestAgentSecretsPath:
			out.Agent = true
			out.Secrets = true
		case strings.HasPrefix(p, unitDir+"guest-tunnel-") && !strings.HasPrefix(p, unitDir+"guest-tunnel-report-") && strings.HasSuffix(p, ".service"):
			out.OpenTunnel = true
			out.Units = append(out.Units, strings.TrimPrefix(p, unitDir))
		case strings.HasPrefix(p, unitDir+cloudinit.ShellUnitPrefix) && strings.HasSuffix(p, ".service"):
			out.Wootty = true
			out.Units = append(out.Units, strings.TrimPrefix(p, unitDir))
		}
	}
	if out.Agent {
		out.Units = append(out.Units, GuestAgentUnit)
	}
	if out.Secrets {
		out.Units = append(out.Units, GuestSecretsUnit)
	}
	sort.Strings(out.Units)
	return out
}

func GuestBinaryPins(downloads map[string]string, names []string) ([]GuestBinaryPin, error) {
	var out []GuestBinaryPin
	for _, name := range names {
		found := false
		for _, arch := range GuestArches {
			sum, ok := downloads[GuestDownloadName(name, arch)]
			if !ok {
				continue
			}
			out = append(out, GuestBinaryPin{Name: name, Arch: arch, Sha256: sum})
			found = true
		}
		if !found {
			return nil, fmt.Errorf("guestseed: the bidder serves no %s download for any of %s", name, strings.Join(GuestArches, ", "))
		}
	}
	return out, nil
}

func GuestBinariesModule(base string, pins []GuestBinaryPin, units []string) (cloudinit.UserDataModule, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if err := checkURLLike(base, "guest downloads base"); err != nil {
		return nil, err
	}
	if len(pins) == 0 {
		return nil, fmt.Errorf("guestseed: the guest binaries module names no binary")
	}
	var wants strings.Builder
	for _, pin := range pins {
		if !guestBinaryNameRE.MatchString(pin.Name) {
			return nil, fmt.Errorf("guestseed: guest binary name %q is not a file name", pin.Name)
		}
		if !slices.Contains(GuestArches, pin.Arch) {
			return nil, fmt.Errorf("guestseed: guest binary %s arch %q is not an arch", pin.Name, pin.Arch)
		}
		if !sha256HexRE.MatchString(pin.Sha256) {
			return nil, fmt.Errorf("guestseed: guest binary %s-%s sha256 %q is not 64 lowercase hex characters", pin.Name, pin.Arch, pin.Sha256)
		}
		fmt.Fprintf(&wants, "want %s %s %s\n", pin.Name, pin.Arch, pin.Sha256)
	}
	for _, unit := range units {
		if !unitNameRE.MatchString(unit) {
			return nil, fmt.Errorf("guestseed: %q is not a systemd service name", unit)
		}
	}
	script := `#!/usr/bin/env bash
set -uo pipefail
BASE="` + base + `"
case "$(uname -m)" in
  x86_64|amd64) ARCH=x86_64 ;;
  aarch64|arm64) ARCH=aarch64 ;;
  *) echo "setup-guest-binaries: the bidder serves no binaries for $(uname -m)"; exit 1 ;;
esac
FAIL=0
fetch() {
  local name="$1" sum="$2" dest="` + GuestBinariesDir + `/$1" tmp got
  if [ -x "${dest}" ] && [ "$(sha256sum "${dest}" | cut -d' ' -f1)" = "${sum}" ]; then
    echo "setup-guest-binaries: installed ${name} sha256 ${sum}"
    return 0
  fi
  tmp="$(mktemp "` + GuestBinariesDir + `/.${name}.XXXXXX")" || return 1
  for attempt in $(seq 1 ` + fmt.Sprint(guestBinaryAttempts) + `); do
    if curl -q -fsS --noproxy '*' --connect-timeout 10 -m 300 -o "${tmp}" "${BASE}` + GuestDownloadsRoute + `/${sum}/${name}-${ARCH}"; then
      got="$(sha256sum "${tmp}" | cut -d' ' -f1)"
      if [ "${got}" = "${sum}" ]; then
        chmod 0755 "${tmp}" && mv -f "${tmp}" "${dest}"
        echo "setup-guest-binaries: installed ${name} sha256 ${got}"
        return 0
      fi
      echo "setup-guest-binaries: ${name} from ${BASE} has sha256 ${got}, not ${sum} (attempt ${attempt})"
    else
      echo "setup-guest-binaries: fetching ${name} from ${BASE} failed (attempt ${attempt})"
    fi
    sleep 3
  done
  rm -f "${tmp}"
  echo "setup-guest-binaries: gave up on ${name}"
  return 1
}
want() {
  [ "$2" = "${ARCH}" ] || return 0
  fetch "$1" "$3" || FAIL=1
}
` + wants.String() + `exit "${FAIL}"
`
	unit := []string{
		"[Unit]",
		"Description=Install the guest binaries the bidder serves",
		"After=network-online.target",
		"Wants=network-online.target",
	}
	if len(units) > 0 {
		unit = append(unit, "Before="+strings.Join(units, " "))
	}
	unit = append(unit,
		"StartLimitIntervalSec=0",
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"TimeoutStartSec=0",
		"ExecStart="+GuestBinariesScript,
		"Restart=on-failure",
		"RestartSec=10",
		"StandardOutput=journal+console",
		"StandardError=journal+console",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	)
	content := strings.Join(unit, "\n")
	return func(cloudinit.Context) (cloudinit.UserDataPatch, error) {
		return cloudinit.UserDataPatch{
			WriteFiles: []cloudinit.WriteFileEntry{
				{Path: GuestBinariesScript, Owner: "root:root", Permissions: "0700", Content: script},
				{Path: "/etc/systemd/system/" + GuestBinariesUnit, Owner: "root:root", Permissions: "0644", Content: content},
			},
			RuncmdPrepend: []any{
				"systemctl daemon-reload",
				"systemctl enable --no-block " + GuestBinariesUnit,
				"systemctl start --no-block " + GuestBinariesUnit,
			},
		}, nil
	}, nil
}

func GuestAgentModule(base, audience, acceptPath string, secrets bool) (cloudinit.UserDataModule, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if err := checkURLLike(base, "guest agent base"); err != nil {
		return nil, err
	}
	if err := checkAbsolute(acceptPath); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(guestAgentBidder{Base: base, Audience: audience, AcceptPath: acceptPath})
	if err != nil {
		return nil, fmt.Errorf("guestseed: encoding %s: %w", GuestAgentBidderPath, err)
	}
	unit := strings.Join([]string{
		"[Unit]",
		"Description=socialweb guest agent: the webrtc tunnels and the sealed secrets fallback of the guest",
		"After=network-online.target " + GuestBinariesUnit,
		"Wants=network-online.target",
		"Requires=" + GuestBinariesUnit,
		"ConditionPathExists=|" + cloudinit.GuestAgentTunnelsPath,
		"ConditionPathExists=|" + cloudinit.GuestAgentSecretsPath,
		"StartLimitIntervalSec=0",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + GuestAgentBinary,
		"Restart=always",
		"RestartSec=2",
		"StandardOutput=journal+console",
		"StandardError=journal+console",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
	files := []cloudinit.WriteFileEntry{
		{Path: GuestAgentBidderPath, Owner: "root:root", Permissions: "0644", Content: string(raw) + "\n"},
		{Path: "/etc/systemd/system/" + GuestAgentUnit, Owner: "root:root", Permissions: "0644", Content: unit},
	}
	runcmd := []any{
		"systemctl daemon-reload",
		"systemctl enable --no-block " + GuestAgentUnit,
		"systemctl start --no-block " + GuestAgentUnit,
	}
	if secrets {
		files = append(files, cloudinit.WriteFileEntry{Path: "/etc/systemd/system/" + GuestSecretsUnit, Owner: "root:root", Permissions: "0644", Content: guestSecretsUnit})
		runcmd = append(runcmd, "systemctl enable --no-block "+GuestSecretsUnit, "systemctl start --no-block "+GuestSecretsUnit)
	}
	return func(cloudinit.Context) (cloudinit.UserDataPatch, error) {
		return cloudinit.UserDataPatch{WriteFiles: files, RuncmdPrepend: runcmd}, nil
	}, nil
}

var guestSecretsUnit = strings.Join([]string{
	"[Unit]",
	"Description=socialweb guest secrets: wait until the bundle is written, by setup-secrets.service (getSecrets) or by the guest agent (the sealed fallback)",
	"After=" + GuestBinariesUnit,
	"Requires=" + GuestBinariesUnit,
	"ConditionPathExists=" + cloudinit.GuestAgentSecretsPath,
	"",
	"[Service]",
	"Type=oneshot",
	"RemainAfterExit=yes",
	"TimeoutStartSec=infinity",
	"ExecStart=" + GuestAgentBinary + " wait-secrets",
	"StandardOutput=journal+console",
	"StandardError=journal+console",
	"",
	"[Install]",
	"WantedBy=multi-user.target",
	"",
}, "\n")
