package cloudinit

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodeagent"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/systemdunit"
)

const (
	NodeSecretsDir = "/etc/socialweb/node"

	NodeOpenBaoTicketPath = NodeSecretsDir + "/openbao-ticket"

	NodeOpenBaoCAPath = NodeSecretsDir + "/openbao-ca.pem"

	NodeClaimDocPath = NodeSecretsDir + "/claim.json"

	NodeLinkEnvPath = NodeSecretsDir + "/apiserver.env"

	NodeOpenBaoLinkAddress = "127.0.0.1:18200"

	NodeKVMount = "socialweb-nodes"

	NodeJWTUserClaim = "wid_sub"

	NodeJWTMountPrefix = "jwt-"

	NodeJWTMountHashHex = 12

	NodeUnitK3sAgent = nodeagent.UnitK3sAgent

	NodeUnitDNSLink = nodeagent.UnitDNSLink

	NodeUnitMasquerade = nodeagent.UnitMasquerade

	NodeUnitCNI = nodeagent.UnitCNI

	NodeUnitMeshdInstall = nodeagent.UnitMeshdInstall

	NodeUnitMeshd = nodeagent.UnitMeshd

	NodeUnitDiagnose = nodeagent.UnitDiagnose

	NodeUnitBootstrap = nodeagent.UnitBootstrap

	NodeScriptBootstrap = "/usr/local/bin/socialweb-node-bootstrap"

	NodeSecretsFetchUnit = "setup-secrets.service"

	K3sNodePasswordPath = "/etc/rancher/node/password"

	NodeLocalDNSAddress = nodeagent.NodeLocalDNSAddress

	NodeLocalDNSLabel = "socialweb.computer/node-local-dns"

	NodeLocalDNSLink = nodeagent.NodeLocalDNSLink

	PodClusterCIDR = nodeagent.PodClusterCIDR

	PodMTU = nodeagent.PodMTU

	PodBridge = nodeagent.PodBridge

	PodEgressComment = nodeagent.PodEgressComment

	CNIConflistName = nodeagent.CNIConflistName

	CNIConfDir = nodeagent.DefaultCNIConfDir

	CNIBinDir = nodeagent.DefaultCNIBinDir

	MeshdPath = nodeagent.DefaultMeshdPath

	MeshdEnvPath = NodeSecretsDir + "/meshd.env"

	HostnamePath = nodeagent.DefaultHostnamePath

	SysctlPath = "/etc/sysctl.d/90-socialweb-node.conf"

	K3sJoinModuleID = "k3s-join"

	NodeJoinConfigPath = NodeSecretsDir + "/join.json"
)

const (
	DefaultCNIWaitAttempts = nodeagent.DefaultCNIWaitAttempts

	DefaultDiagnoseAttempts = nodeagent.DefaultDiagnoseAttempts

	DefaultLinkWaitAttempts = nodeagent.DefaultLinkWaitAttempts
)

type MeshdPin = nodeagent.MeshdPin

type NodeAgentPin = nodeagent.NodeAgentPin

type K3sJoinOptions struct {
	DNSNameservers []string

	K3sDataDir string

	AcceptPath string

	DumbpipePath string

	K3sPath string

	NodeDir string

	K3sNodePasswordPath string

	DoneStamp string

	OpenBaoAddr string

	Meshd *MeshdPin

	NodeAgent *NodeAgentPin

	MeshdPath string

	NodeBinaryPath string

	CNIConfDir string

	CNIBinDir string

	KubeletKubeconfig string

	CNIWaitAttempts int

	DiagnoseAttempts int

	LinkWaitAttempts int
}

func (o K3sJoinOptions) defaults() K3sJoinOptions {
	if o.NodeDir == "" {
		o.NodeDir = NodeSecretsDir
	}
	if o.K3sDataDir == "" {
		o.K3sDataDir = nodeagent.DefaultK3sDataDir
	}
	if o.KubeletKubeconfig == "" {
		o.KubeletKubeconfig = o.K3sDataDir + "/agent/kubelet.kubeconfig"
	}
	if o.DumbpipePath == "" {
		o.DumbpipePath = nodeagent.DefaultDumbpipePath
	}
	if o.K3sPath == "" {
		o.K3sPath = nodeagent.DefaultK3sPath
	}
	if o.NodeBinaryPath == "" {
		o.NodeBinaryPath = nodeagent.DefaultNodeBinaryPath
	}
	if o.K3sNodePasswordPath == "" {
		o.K3sNodePasswordPath = K3sNodePasswordPath
	}
	if o.DoneStamp == "" {
		o.DoneStamp = nodeagent.DefaultDoneStamp
	}
	if o.OpenBaoAddr == "" {
		o.OpenBaoAddr = NodeOpenBaoLinkAddress
	}
	if o.MeshdPath == "" {
		o.MeshdPath = MeshdPath
	}
	if o.CNIConfDir == "" {
		o.CNIConfDir = CNIConfDir
	}
	if o.CNIBinDir == "" {
		o.CNIBinDir = CNIBinDir
	}
	if o.CNIWaitAttempts <= 0 {
		o.CNIWaitAttempts = DefaultCNIWaitAttempts
	}
	if o.DiagnoseAttempts <= 0 {
		o.DiagnoseAttempts = DefaultDiagnoseAttempts
	}
	if o.LinkWaitAttempts <= 0 {
		o.LinkWaitAttempts = DefaultLinkWaitAttempts
	}
	return o
}

func (o K3sJoinOptions) config() (nodeagent.Config, error) {
	o = o.defaults()
	if err := nodeagent.ValidateNodeAgentPin(o.NodeAgent); err != nil {
		return nodeagent.Config{}, fmt.Errorf("cloudinit: %w", err)
	}
	if err := nodeagent.ValidateMeshdPin(o.Meshd); err != nil {
		return nodeagent.Config{}, fmt.Errorf("cloudinit: %w", err)
	}
	cfg := nodeagent.Config{
		ClaimDocPath:                  o.NodeDir + "/claim.json",
		ClaimCAPath:                   o.NodeDir + "/openbao-ca.pem",
		OpenBaoLinkAddress:            o.OpenBaoAddr,
		OpenBaoTicketPath:             o.NodeDir + "/openbao-ticket",
		GuestLoginHelperPath:          GuestLoginScriptPath,
		K3sDataDir:                    o.K3sDataDir,
		NodeDir:                       o.NodeDir,
		CNIConfDir:                    o.CNIConfDir,
		CNIBinDir:                     o.CNIBinDir,
		KubeletKubeconfig:             o.KubeletKubeconfig,
		APIServerEnvPath:              o.NodeDir + "/apiserver.env",
		TokenPath:                     o.NodeDir + "/token",
		NodePasswordPath:              o.NodeDir + "/node-password",
		ServerCAPath:                  o.NodeDir + "/server-ca.pem",
		K3sNodePasswordPath:           o.K3sNodePasswordPath,
		MeshdEnvPath:                  o.NodeDir + "/meshd.env",
		DoneStamp:                     o.DoneStamp,
		K3sPath:                       o.K3sPath,
		DumbpipePath:                  o.DumbpipePath,
		MeshdPath:                     o.MeshdPath,
		HostnamePath:                  HostnamePath,
		NodeBinaryPath:                o.NodeBinaryPath,
		ConfigPath:                    o.NodeDir + "/join.json",
		LinkWaitAttempts:              o.LinkWaitAttempts,
		CNIWaitAttempts:               o.CNIWaitAttempts,
		DiagnoseAttempts:              o.DiagnoseAttempts,
		GuestLoginWaitAttempts:        nodeagent.DefaultGuestLoginWaitAttempts,
		DiagnoseIntervalSeconds:       int(nodeagent.DefaultDiagnoseInterval.Seconds()),
		CNIWaitIntervalMS:             int(nodeagent.DefaultCNIWaitInterval.Milliseconds()),
		LinkWaitIntervalMS:            int(nodeagent.DefaultLinkWaitInterval.Milliseconds()),
		GuestLoginWaitIntervalSeconds: int(nodeagent.DefaultGuestLoginWaitInterval.Seconds()),
		Meshd:                         o.Meshd,
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nodeagent.Config{}, fmt.Errorf("cloudinit: %w", err)
	}
	return cfg, nil
}

func NodeAudience(clusterKey, nodeName string) string {
	return "api://socialweb-node?actx=" + clusterKey + "/" + nodeName
}

func NodeJWTMount(issuer string) string {
	sum := sha256sum.Hex([]byte(issuer))
	return NodeJWTMountPrefix + sum[:NodeJWTMountHashHex]
}

func renderResolver(nameservers []string) string {
	var b strings.Builder
	for _, name := range nameservers {
		b.WriteString("nameserver ")
		b.WriteString(name)
		b.WriteString("\n")
	}
	b.WriteString("options timeout:2 attempts:3\n")
	return b.String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func renderBootstrapScript(o K3sJoinOptions, cfg nodeagent.Config) string {
	pin := o.NodeAgent
	return `#!/bin/sh
set -eu
BIN=` + shellQuote(cfg.NodeBinaryPath) + `
CONFIG=` + shellQuote(cfg.ConfigPath) + `
BASE=` + shellQuote(pin.DownloadBase) + `
case "$(uname -m)" in
  x86_64|amd64) ARCH=x86_64; WANT=` + pin.Sha256["x86_64"] + ` ;;
  aarch64|arm64) ARCH=aarch64; WANT=` + pin.Sha256["aarch64"] + ` ;;
  *) echo "socialweb-node-bootstrap: no socialweb-node build for $(uname -m)" >&2; exit 1 ;;
esac
if [ -x "$BIN" ] && [ "$(sha256sum "$BIN" | cut -d' ' -f1)" = "$WANT" ]; then
  exec "$BIN" units start --config "$CONFIG"
fi
TMP="${BIN}.download.$$"
trap 'rm -f "$TMP"' EXIT
curl -fsSL --retry 5 --retry-delay 2 -o "$TMP" "$BASE/downloads/$WANT/socialweb-node-$ARCH"
GOT="$(sha256sum "$TMP" | cut -d' ' -f1)"
[ "$GOT" = "$WANT" ] || { echo "socialweb-node-bootstrap: the door served sha256 $GOT, not the pinned $WANT" >&2; exit 1; }
chmod 0755 "$TMP"
mv -f "$TMP" "$BIN"
trap - EXIT
exec "$BIN" units start --config "$CONFIG"
`
}

func bootstrapUnit(o K3sJoinOptions) systemdunit.Unit {
	return systemdunit.Unit{Name: NodeUnitBootstrap, Sections: []systemdunit.Section{
		{Name: "Unit", Entries: []systemdunit.KeyValue{
			systemdunit.KV("Description", "socialweb node: install the sha256-pinned socialweb-node and start the node units"),
			systemdunit.KV("After", "network-online.target"),
			systemdunit.KV("Wants", "network-online.target"),
			systemdunit.KV("StartLimitIntervalSec", "0"),
		}},
		{Name: "Service", Entries: []systemdunit.KeyValue{
			systemdunit.KV("Type", "oneshot"),
			systemdunit.KV("RemainAfterExit", "yes"),
			systemdunit.KV("ExecStart", o.NodeBinaryPath+"-bootstrap"),
			systemdunit.KV("Restart", "on-failure"),
			systemdunit.KV("RestartSec", "5"),
			systemdunit.KV("StandardOutput", "journal+console"),
			systemdunit.KV("StandardError", "journal+console"),
		}},
		{Name: "Install", Entries: []systemdunit.KeyValue{systemdunit.KV("WantedBy", "multi-user.target")}},
	}}
}

func scriptFile(path, content string) WriteFileEntry {
	return WriteFileEntry{Path: path, Owner: "root:root", Permissions: "0700", Content: content}
}

func unitFileEntry(file systemdunit.File) WriteFileEntry {
	return WriteFileEntry{Path: file.Path, Owner: "root:root", Permissions: fmt.Sprintf("%04o", file.Mode.Perm()), Content: string(file.Content)}
}

func K3sJoinModule(opts K3sJoinOptions) UserDataModule {
	return func(Context) (UserDataPatch, error) {
		opts = opts.defaults()
		cfg, err := opts.config()
		if err != nil {
			return UserDataPatch{}, err
		}
		raw, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return UserDataPatch{}, fmt.Errorf("cloudinit: encoding the node join config: %w", err)
		}
		raw = append(raw, '\n')
		nameservers := opts.DNSNameservers
		if len(nameservers) == 0 {
			nameservers = []string{DefaultDNSGateway}
		}
		resolver := renderResolver(nameservers)
		files := []WriteFileEntry{
			{Path: ResolvConfPath, Owner: "root:root", Permissions: "0644", Content: resolver},
			{Path: SysctlPath, Owner: "root:root", Permissions: "0644", Content: "net.ipv4.ip_forward = 1\n"},
			{Path: cfg.ConfigPath, Owner: "root:root", Permissions: "0600", Content: string(raw)},
			scriptFile(cfg.NodeBinaryPath+"-bootstrap", renderBootstrapScript(opts, cfg)),
		}
		bootstrap := bootstrapUnit(opts)
		files = append(files, unitFileEntry(systemdunit.Files("/etc/systemd/system", bootstrap)[0]))
		for _, file := range nodeagent.UnitFiles(cfg) {
			files = append(files, unitFileEntry(file))
		}
		writeResolver := "printf '%s' '" + strings.ReplaceAll(resolver, "'", `'\''`) + "' > " + ResolvConfPath
		return UserDataPatch{
			Packages:   []string{"curl", "ca-certificates", "iptables"},
			WriteFiles: files,
			Bootcmd:    []any{writeResolver},
			Runcmd: []any{
				writeResolver,
				"systemctl daemon-reload",
				"systemctl enable " + NodeUnitBootstrap,
				"systemctl start --no-block " + NodeUnitBootstrap,
			},
		}, nil
	}
}
