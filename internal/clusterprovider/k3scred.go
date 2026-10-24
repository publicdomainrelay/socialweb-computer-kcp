package clusterprovider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

func repointApiserverClients(spec clusterstack.Spec) func(context.Context, procgroup.Spec) (procgroup.Spec, error) {
	return func(_ context.Context, ps procgroup.Spec) (procgroup.Spec, error) {
		port := strconv.Itoa(spec.K3sApiserverPort)
		for _, path := range spec.ApiserverClientKubeconfigs() {
			if err := repointKubeconfig(path, port); err != nil {
				return ps, fmt.Errorf("clusterprovider: pointing %s at the apiserver port %s of %s: %w", path, port, spec.Cluster, err)
			}
		}
		return ps, nil
	}
}

func repointKubeconfig(path, port string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	out, changed, err := withServerPort(string(raw), port)
	if err != nil || !changed {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func withServerPort(raw, port string) (string, bool, error) {
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		value, ok := strings.CutPrefix(trimmed, "server: ")
		if !ok {
			continue
		}
		u, err := url.Parse(value)
		if err != nil {
			return raw, false, err
		}
		if u.Port() == port {
			return raw, false, nil
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
		lines[i] = line[:len(line)-len(trimmed)] + "server: " + u.String()
		return strings.Join(lines, "\n"), true, nil
	}
	return raw, false, nil
}
