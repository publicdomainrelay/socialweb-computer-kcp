package vaultmain

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	envStateDir      = "SOCIALWEB_STATE_DIR"
	envKCPKubeconfig = "SOCIALWEB_KCP_KUBECONFIG"
	envKCPRootDir    = "SOCIALWEB_KCP_ROOT"
	envWorkspace     = "SOCIALWEB_ROOTVAULT_WORKSPACE"
	envServer        = "SOCIALWEB_ROOTVAULT_SERVER"
	envTLSCert       = "SOCIALWEB_TLS_CERT_FILE"
	envTLSKey        = "SOCIALWEB_TLS_KEY_FILE"
)

type Config struct {
	KCPKubeconfig string
	KCPRootDir    string
	Workspace     string
	Server        string

	TLSCert string
	TLSKey  string
}

func load(get func(string) string) (Config, error) {
	cfg := Config{
		KCPKubeconfig: strings.TrimSpace(get(envKCPKubeconfig)),
		KCPRootDir:    strings.TrimSpace(get(envKCPRootDir)),
		Workspace:     strings.TrimSpace(get(envWorkspace)),
		Server:        strings.TrimSpace(get(envServer)),
		TLSCert:       strings.TrimSpace(get(envTLSCert)),
		TLSKey:        strings.TrimSpace(get(envTLSKey)),
	}
	if cfg.KCPKubeconfig == "" {
		state := strings.TrimSpace(get(envStateDir))
		if state == "" {
			return Config{}, fmt.Errorf("neither %s nor %s is set: the root vault is an OpenBaoServer in kcp, read through the kcp front", envKCPKubeconfig, envStateDir)
		}
		cfg.KCPKubeconfig = filepath.Join(state, "kcp", "admin.kubeconfig")
	}
	cfg.Workspace = cmp.Or(cfg.Workspace, hostprocess.DefaultWorkspace)
	cfg.Server = cmp.Or(cfg.Server, vaultlayout.RootServerName)
	return cfg, nil
}
