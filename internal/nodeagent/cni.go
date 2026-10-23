package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
)

type cniRange struct {
	Subnet string `json:"subnet"`
}

type cniRoute struct {
	Dst string `json:"dst"`
}

type cniIPAM struct {
	Type string `json:"type"`

	Ranges [][]cniRange `json:"ranges"`

	Routes []cniRoute `json:"routes"`
}

type cniBridge struct {
	Type string `json:"type"`

	Bridge string `json:"bridge"`

	IsGateway bool `json:"isGateway"`

	IPMasq bool `json:"ipMasq"`

	HairpinMode bool `json:"hairpinMode"`

	MTU int `json:"mtu"`

	IPAM cniIPAM `json:"ipam"`
}

type cniPortmap struct {
	Type string `json:"type"`

	Capabilities map[string]bool `json:"capabilities"`
}

type cniConflist struct {
	CNIVersion string `json:"cniVersion"`

	Name string `json:"name"`

	Plugins []any `json:"plugins"`
}

func CNI(ctx context.Context, cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		env, err := readEnvFile(cfg.APIServerEnvPath)
		if err != nil {
			return fmt.Errorf("nodeagent: reading %s for NODE_NAME: %w", cfg.APIServerEnvPath, err)
		}
		nodeName = env["NODE_NAME"]
	}
	if !validName(nodeName) {
		return fmt.Errorf("nodeagent: NODE_NAME %q is not a node name", nodeName)
	}
	cidr := ""
	for attempt := 1; attempt <= cfg.CNIWaitAttempts; attempt++ {
		if fileNonEmpty(cfg.KubeletKubeconfig) {
			out, err := run(ctx, cfg.K3sPath, "kubectl", "--kubeconfig", cfg.KubeletKubeconfig,
				"get", "node", nodeName, "-o", "jsonpath={.spec.podCIDR}")
			if err == nil {
				cidr = strings.TrimSpace(string(out))
				if cidr != "" {
					break
				}
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if attempt < cfg.CNIWaitAttempts {
			if err := clock.SleepErr(ctx, cfg.CNIWaitInterval()); err != nil {
				return err
			}
		}
	}
	if cidr == "" {
		return fmt.Errorf("nodeagent: node %s has no podCIDR yet after %d attempts", nodeName, cfg.CNIWaitAttempts)
	}
	if err := validPodCIDR(cidr); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.CNIConfDir, 0o755); err != nil {
		return fmt.Errorf("nodeagent: creating %s: %w", cfg.CNIConfDir, err)
	}
	if err := os.MkdirAll(cfg.CNIBinDir, 0o755); err != nil {
		return fmt.Errorf("nodeagent: creating %s: %w", cfg.CNIBinDir, err)
	}
	for _, plugin := range []string{"bridge", "host-local", "portmap", "loopback"} {
		link := filepath.Join(cfg.CNIBinDir, plugin)
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("nodeagent: replacing the CNI plugin link %s: %w", link, err)
		}
		if err := os.Symlink(cfg.CNIPluginPath(), link); err != nil {
			return fmt.Errorf("nodeagent: linking %s to %s: %w", link, cfg.CNIPluginPath(), err)
		}
	}
	content, err := json.MarshalIndent(conflistFor(cidr), "", "  ")
	if err != nil {
		return fmt.Errorf("nodeagent: encoding the CNI conflist: %w", err)
	}
	content = append(content, '\n')
	conf := filepath.Join(cfg.CNIConfDir, CNIConflistName)
	if err := writeAtomic(conf, content, 0o644); err != nil {
		return err
	}
	log.Info("wrote the bridge CNI conflist", "path", conf, "podCIDR", cidr, "plugins", cfg.CNIPluginPath())
	return nil
}

func validPodCIDR(cidr string) error {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil || ip.To4() == nil || network.IP.To4() == nil {
		return fmt.Errorf("nodeagent: podCIDR %s is not an IPv4 CIDR", cidr)
	}
	return nil
}

func conflistFor(cidr string) cniConflist {
	return cniConflist{
		CNIVersion: "1.0.0",
		Name:       "socialweb-pods",
		Plugins: []any{
			cniBridge{
				Type:        "bridge",
				Bridge:      PodBridge,
				IsGateway:   true,
				IPMasq:      false,
				HairpinMode: true,
				MTU:         PodMTU,
				IPAM: cniIPAM{
					Type:   "host-local",
					Ranges: [][]cniRange{{{Subnet: cidr}}},
					Routes: []cniRoute{{Dst: "0.0.0.0/0"}},
				},
			},
			cniPortmap{Type: "portmap", Capabilities: map[string]bool{"portMappings": true}},
		},
	}
}
