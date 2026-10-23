package nodeagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	cidr, err := awaitPodCIDR(ctx, cfg, nodeName, log)
	if err != nil {
		return err
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

func awaitPodCIDR(ctx context.Context, cfg Config, nodeName string, log *slog.Logger) (string, error) {
	limit := time.Duration(cfg.CNIWaitAttempts) * cfg.CNIWaitInterval()
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	last := "the kubelet kubeconfig " + cfg.KubeletKubeconfig + " is not written yet"
	for {
		if fileNonEmpty(cfg.KubeletKubeconfig) {
			cidr, err := followPodCIDR(wctx, cfg, nodeName)
			if cidr != "" {
				return cidr, nil
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if err != nil {
				last = err.Error()
				log.Info("the node watch ended before a podCIDR; it restarts", "node", nodeName, "err", last)
			}
		}
		if err := clock.SleepErr(wctx, cfg.CNIWaitInterval()); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("nodeagent: node %s has no podCIDR yet after %s: %s", nodeName, limit, last)
		}
	}
}

func followPodCIDR(ctx context.Context, cfg Config, nodeName string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.K3sPath, "kubectl", "--kubeconfig", cfg.KubeletKubeconfig,
		"get", "node", nodeName, "--watch", "-o", `jsonpath={.spec.podCIDR}{"\n"}`)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("nodeagent: watching node %s: %w", nodeName, err)
	}
	lines := bufio.NewScanner(out)
	for lines.Scan() {
		if cidr := strings.TrimSpace(lines.Text()); cidr != "" {
			cancel()
			_ = cmd.Wait()
			return cidr, nil
		}
	}
	err = cmd.Wait()
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return "", fmt.Errorf("nodeagent: watching node %s: %s", nodeName, msg)
	}
	if err != nil {
		return "", fmt.Errorf("nodeagent: watching node %s: %w", nodeName, err)
	}
	return "", fmt.Errorf("nodeagent: the watch of node %s closed", nodeName)
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
