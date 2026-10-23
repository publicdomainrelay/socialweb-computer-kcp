package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
)

func Hostname(ctx context.Context, cfg Config, log *slog.Logger) error {
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
	if len(nodeName) > 64 {
		return fmt.Errorf("nodeagent: node name %s is longer than the 64 bytes a hostname holds", nodeName)
	}
	current, err := os.Hostname()
	if err == nil && current == nodeName {
		return nil
	}
	if err := writeAtomic(cfg.HostnamePath, []byte(nodeName+"\n"), 0o644); err != nil {
		return err
	}
	if err := setHostname(nodeName); err != nil {
		return err
	}
	log.Info("hostname set to the node name", "node", nodeName)
	return nil
}

func DNSLink(ctx context.Context, cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	_, _ = run(ctx, "modprobe", "dummy")
	if _, err := run(ctx, "ip", "link", "show", NodeLocalDNSLink); err != nil {
		if _, err := run(ctx, "ip", "link", "add", NodeLocalDNSLink, "type", "dummy"); err != nil {
			return err
		}
	}
	out, err := run(ctx, "ip", "-4", "addr", "show", "dev", NodeLocalDNSLink)
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), " "+NodeLocalDNSAddress+"/32 ") {
		if _, err := run(ctx, "ip", "addr", "add", NodeLocalDNSAddress+"/32", "dev", NodeLocalDNSLink); err != nil {
			return err
		}
	}
	if _, err := run(ctx, "ip", "link", "set", NodeLocalDNSLink, "up"); err != nil {
		return err
	}
	log.Info("the per-node DNS address is on the dummy interface", "address", NodeLocalDNSAddress, "link", NodeLocalDNSLink)
	return nil
}

func Masquerade(ctx context.Context, cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if _, err := run(ctx, "sysctl", "-qw", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	rule := []string{"-w", "-t", "nat", "-C", "POSTROUTING", "-s", PodClusterCIDR, "!", "-d", PodClusterCIDR,
		"-m", "comment", "--comment", PodEgressComment, "-j", "MASQUERADE"}
	if _, err := run(ctx, "iptables", rule...); err != nil {
		rule[3] = "-A"
		if _, err := run(ctx, "iptables", rule...); err != nil {
			return err
		}
	}
	log.Info("pod egress is masqueraded", "clusterCIDR", PodClusterCIDR, "comment", PodEgressComment)
	return nil
}

func InstallMeshd(ctx context.Context, cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Meshd == nil {
		return fmt.Errorf("nodeagent: no socialweb-meshd pin is configured")
	}
	arch := ""
	switch runtime.GOARCH {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	default:
		return fmt.Errorf("nodeagent: no socialweb-meshd build for %s", runtime.GOARCH)
	}
	pin := cfg.Meshd.Sha256[arch]
	if executable(cfg.MeshdPath) {
		if got, err := sha256sum.File(cfg.MeshdPath); err == nil && got == pin {
			log.Info("socialweb-meshd is already installed", "sha256", pin)
			return nil
		}
	}
	base := strings.TrimRight(cfg.Meshd.DownloadBase, "/")
	url := base + "/downloads/" + pin + "/socialweb-meshd-" + arch
	dir := filepath.Dir(cfg.MeshdPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("nodeagent: creating %s: %w", dir, err)
	}
	var last error
	for attempt := 1; attempt <= 5; attempt++ {
		tmp, err := os.CreateTemp(dir, "."+filepath.Base(cfg.MeshdPath)+".download-*")
		if err != nil {
			return fmt.Errorf("nodeagent: creating a temporary file beside %s: %w", cfg.MeshdPath, err)
		}
		name := tmp.Name()
		got, err := fetchSHA256(ctx, url, tmp)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil && got != pin {
			err = fmt.Errorf("nodeagent: the door served sha256 %s, not the pinned %s", got, pin)
		}
		if err == nil {
			if err := os.Chmod(name, 0o755); err != nil {
				os.Remove(name)
				return fmt.Errorf("nodeagent: setting the mode of %s: %w", name, err)
			}
			if err := os.Rename(name, cfg.MeshdPath); err != nil {
				os.Remove(name)
				return fmt.Errorf("nodeagent: renaming %s into place: %w", cfg.MeshdPath, err)
			}
			log.Info("installed socialweb-meshd", "sha256", pin, "arch", arch, "url", url)
			return nil
		}
		os.Remove(name)
		last = err
		if attempt < 5 {
			log.Info("the socialweb-meshd download failed; retrying", "url", url, "attempt", attempt,
				"of", 5, "err", err.Error())
			if err := clock.SleepErr(ctx, 2*time.Second); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("nodeagent: downloading %s: %w", url, last)
}

func fetchSHA256(ctx context.Context, url string, out io.Writer) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", response.StatusCode, url)
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hash), response.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
