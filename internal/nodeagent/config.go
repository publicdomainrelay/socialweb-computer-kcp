package nodeagent

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"time"
)

const (
	DefaultNodeDir = "/etc/socialweb/node"

	DefaultK3sDataDir = "/var/lib/rancher/k3s"

	DefaultOpenBaoLinkAddress = "127.0.0.1:18200"

	DefaultGuestLoginHelper = "/usr/local/bin/guest-login.sh"

	DefaultK3sPath = "/usr/local/bin/k3s"

	DefaultDumbpipePath = "/usr/local/bin/dumbpipe"

	DefaultMeshdPath = "/usr/local/bin/socialweb-meshd"

	DefaultNodeBinaryPath = "/usr/local/bin/socialweb-node"

	DefaultCNIConfDir = "/etc/cni/net.d"

	DefaultCNIBinDir = "/opt/cni/bin"

	DefaultK3sNodePasswordPath = "/etc/rancher/node/password"

	DefaultDoneStamp = "/var/lib/socialweb-node-secrets.done"

	DefaultHostnamePath = "/etc/hostname"

	DefaultLinkWaitAttempts = 120

	DefaultLinkWaitInterval = 500 * time.Millisecond

	DefaultCNIWaitAttempts = 90

	DefaultCNIWaitInterval = 2 * time.Second

	DefaultDiagnoseAttempts = 6

	DefaultDiagnoseInterval = 20 * time.Second

	DefaultGuestLoginWaitAttempts = 60

	DefaultGuestLoginWaitInterval = 2 * time.Second

	DefaultCNIRetryInterval = 2 * time.Second

	DefaultUnitDir = "/etc/systemd/system"

	PodClusterCIDR = "10.42.0.0/16"

	PodMTU = 1160

	PodBridge = "cni0"

	PodEgressComment = "socialweb-pod-egress"

	CNIConflistName = "10-socialweb-bridge.conflist"

	NodeLocalDNSAddress = "169.254.20.10"

	NodeLocalDNSLink = "swdns0"
)

var (
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

	originPattern = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

	safeNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

	ipv4AddressPattern = regexp.MustCompile(`^([0-9]{1,3}\.){3}[0-9]{1,3}$`)

	portPattern = regexp.MustCompile(`^[0-9]{1,5}$`)

	ticketPattern = regexp.MustCompile(`^[a-z0-9]+$`)
)

type MeshdPin struct {
	DownloadBase string `json:"downloadBase"`

	Sha256 map[string]string `json:"sha256"`
}

type NodeAgentPin struct {
	DownloadBase string `json:"downloadBase"`

	Sha256 map[string]string `json:"sha256"`
}

func ValidateMeshdPin(pin *MeshdPin) error {
	if pin == nil {
		return nil
	}
	return ValidatePin("socialweb-meshd", pin.DownloadBase, pin.Sha256)
}

func ValidateNodeAgentPin(pin *NodeAgentPin) error {
	if pin == nil {
		return fmt.Errorf("nodeagent: the socialweb-node pin is missing")
	}
	return ValidatePin("socialweb-node", pin.DownloadBase, pin.Sha256)
}

func ValidatePin(kind, downloadBase string, sha map[string]string) error {
	if !originPattern.MatchString(downloadBase) {
		return fmt.Errorf("nodeagent: %s downloadBase %q is not an http(s) origin with no path", kind, downloadBase)
	}
	for _, arch := range []string{"x86_64", "aarch64"} {
		if !sha256Pattern.MatchString(sha[arch]) {
			return fmt.Errorf("nodeagent: %s sha256 for %s is not 64 lowercase hex characters", kind, arch)
		}
	}
	return nil
}

type Config struct {
	ClaimDocPath string `json:"claimDocPath"`

	ClaimCAPath string `json:"claimCAPath"`

	OpenBaoLinkAddress string `json:"openBaoLinkAddress"`

	OpenBaoTicketPath string `json:"openBaoTicketPath"`

	GuestLoginHelperPath string `json:"guestLoginHelperPath"`

	K3sDataDir string `json:"k3sDataDir"`

	NodeDir string `json:"nodeDir"`

	CNIConfDir string `json:"cniConfDir"`

	CNIBinDir string `json:"cniBinDir"`

	KubeletKubeconfig string `json:"kubeletKubeconfig"`

	APIServerEnvPath string `json:"apiserverEnvPath"`

	TokenPath string `json:"tokenPath"`

	NodePasswordPath string `json:"nodePasswordPath"`

	ServerCAPath string `json:"serverCAPath"`

	K3sNodePasswordPath string `json:"k3sNodePasswordPath"`

	MeshdEnvPath string `json:"meshdEnvPath"`

	DoneStamp string `json:"doneStamp"`

	K3sPath string `json:"k3sPath"`

	DumbpipePath string `json:"dumbpipePath"`

	MeshdPath string `json:"meshdPath"`

	HostnamePath string `json:"hostnamePath"`

	NodeBinaryPath string `json:"nodeBinaryPath"`

	ConfigPath string `json:"configPath"`

	LinkWaitAttempts int `json:"linkWaitAttempts"`

	LinkWaitIntervalMS int `json:"linkWaitIntervalMs"`

	CNIWaitAttempts int `json:"cniWaitAttempts"`

	CNIWaitIntervalMS int `json:"cniWaitIntervalMs"`

	DiagnoseAttempts int `json:"diagnoseAttempts"`

	DiagnoseIntervalSeconds int `json:"diagnoseIntervalSeconds"`

	GuestLoginWaitAttempts int `json:"guestLoginWaitAttempts"`

	GuestLoginWaitIntervalSeconds int `json:"guestLoginWaitIntervalSeconds"`

	Meshd *MeshdPin `json:"meshd,omitempty"`
}

func (c Config) WithDefaults() Config {
	if c.NodeDir == "" {
		c.NodeDir = DefaultNodeDir
	}
	inDir := func(name string) string { return filepath.Join(c.NodeDir, name) }
	if c.ClaimDocPath == "" {
		c.ClaimDocPath = inDir("claim.json")
	}
	if c.ClaimCAPath == "" {
		c.ClaimCAPath = inDir("openbao-ca.pem")
	}
	if c.OpenBaoTicketPath == "" {
		c.OpenBaoTicketPath = inDir("openbao-ticket")
	}
	if c.OpenBaoLinkAddress == "" {
		c.OpenBaoLinkAddress = DefaultOpenBaoLinkAddress
	}
	if c.GuestLoginHelperPath == "" {
		c.GuestLoginHelperPath = DefaultGuestLoginHelper
	}
	if c.K3sDataDir == "" {
		c.K3sDataDir = DefaultK3sDataDir
	}
	if c.CNIConfDir == "" {
		c.CNIConfDir = DefaultCNIConfDir
	}
	if c.CNIBinDir == "" {
		c.CNIBinDir = DefaultCNIBinDir
	}
	if c.KubeletKubeconfig == "" {
		c.KubeletKubeconfig = filepath.Join(c.K3sDataDir, "agent", "kubelet.kubeconfig")
	}
	if c.APIServerEnvPath == "" {
		c.APIServerEnvPath = inDir("apiserver.env")
	}
	if c.TokenPath == "" {
		c.TokenPath = inDir("token")
	}
	if c.NodePasswordPath == "" {
		c.NodePasswordPath = inDir("node-password")
	}
	if c.ServerCAPath == "" {
		c.ServerCAPath = inDir("server-ca.pem")
	}
	if c.K3sNodePasswordPath == "" {
		c.K3sNodePasswordPath = DefaultK3sNodePasswordPath
	}
	if c.MeshdEnvPath == "" {
		c.MeshdEnvPath = inDir("meshd.env")
	}
	if c.DoneStamp == "" {
		c.DoneStamp = DefaultDoneStamp
	}
	if c.K3sPath == "" {
		c.K3sPath = DefaultK3sPath
	}
	if c.DumbpipePath == "" {
		c.DumbpipePath = DefaultDumbpipePath
	}
	if c.MeshdPath == "" {
		c.MeshdPath = DefaultMeshdPath
	}
	if c.HostnamePath == "" {
		c.HostnamePath = DefaultHostnamePath
	}
	if c.NodeBinaryPath == "" {
		c.NodeBinaryPath = DefaultNodeBinaryPath
	}
	if c.ConfigPath == "" {
		c.ConfigPath = inDir("join.json")
	}
	if c.LinkWaitAttempts <= 0 {
		c.LinkWaitAttempts = DefaultLinkWaitAttempts
	}
	if c.LinkWaitIntervalMS <= 0 {
		c.LinkWaitIntervalMS = int(DefaultLinkWaitInterval / time.Millisecond)
	}
	if c.CNIWaitAttempts <= 0 {
		c.CNIWaitAttempts = DefaultCNIWaitAttempts
	}
	if c.CNIWaitIntervalMS <= 0 {
		c.CNIWaitIntervalMS = int(DefaultCNIWaitInterval / time.Millisecond)
	}
	if c.DiagnoseAttempts <= 0 {
		c.DiagnoseAttempts = DefaultDiagnoseAttempts
	}
	if c.DiagnoseIntervalSeconds <= 0 {
		c.DiagnoseIntervalSeconds = int(DefaultDiagnoseInterval / time.Second)
	}
	if c.GuestLoginWaitAttempts <= 0 {
		c.GuestLoginWaitAttempts = DefaultGuestLoginWaitAttempts
	}
	if c.GuestLoginWaitIntervalSeconds <= 0 {
		c.GuestLoginWaitIntervalSeconds = int(DefaultGuestLoginWaitInterval / time.Second)
	}
	return c
}

func (c Config) Validate() error {
	absolute := map[string]string{
		"claimDocPath": c.ClaimDocPath, "claimCAPath": c.ClaimCAPath, "k3sDataDir": c.K3sDataDir,
		"nodeDir": c.NodeDir, "cniConfDir": c.CNIConfDir, "cniBinDir": c.CNIBinDir,
		"kubeletKubeconfig": c.KubeletKubeconfig, "apiserverEnvPath": c.APIServerEnvPath,
		"tokenPath": c.TokenPath, "nodePasswordPath": c.NodePasswordPath, "serverCAPath": c.ServerCAPath,
		"k3sNodePasswordPath": c.K3sNodePasswordPath, "meshdEnvPath": c.MeshdEnvPath,
		"doneStamp": c.DoneStamp, "k3sPath": c.K3sPath, "dumbpipePath": c.DumbpipePath,
		"meshdPath": c.MeshdPath, "hostnamePath": c.HostnamePath, "nodeBinaryPath": c.NodeBinaryPath,
		"openBaoTicketPath": c.OpenBaoTicketPath, "configPath": c.ConfigPath,
	}
	for name, value := range absolute {
		if value == "" || !filepath.IsAbs(value) {
			return fmt.Errorf("nodeagent: %s %q is not an absolute path", name, value)
		}
	}
	if host, port, err := net.SplitHostPort(c.OpenBaoLinkAddress); err != nil || !validIPv4(host) || !validPort(port) {
		return fmt.Errorf("nodeagent: openBaoLinkAddress %q is not an IPv4 address and port", c.OpenBaoLinkAddress)
	}
	if err := ValidateMeshdPin(c.Meshd); err != nil {
		return err
	}
	return nil
}

func (c Config) LinkWaitInterval() time.Duration {
	return time.Duration(c.LinkWaitIntervalMS) * time.Millisecond
}

func (c Config) CNIWaitInterval() time.Duration {
	return time.Duration(c.CNIWaitIntervalMS) * time.Millisecond
}

func (c Config) DiagnoseInterval() time.Duration {
	return time.Duration(c.DiagnoseIntervalSeconds) * time.Second
}

func (c Config) GuestLoginWaitInterval() time.Duration {
	return time.Duration(c.GuestLoginWaitIntervalSeconds) * time.Second
}

func (c Config) CNIPluginPath() string {
	return filepath.Join(c.K3sDataDir, "data", "current", "bin", "cni")
}

func validName(name string) bool { return safeNamePattern.MatchString(name) }

func validIPv4(value string) bool { return ipv4AddressPattern.MatchString(value) }

func validPort(value string) bool { return portPattern.MatchString(value) }

func validTicket(value string) bool { return ticketPattern.MatchString(value) }

func valueString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		if v == "" {
			return "", false
		}
		return v, true
	case float64:
		return fmt.Sprintf("%.0f", v), true
	case float32:
		return fmt.Sprintf("%.0f", v), true
	case int:
		return fmt.Sprintf("%d", v), true
	case int64:
		return fmt.Sprintf("%d", v), true
	default:
		return "", false
	}
}
