package compose

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const NetworkCAFileName = "network-ca.pem"

func OptionInt(opts []v1alpha1.ApplicationOption, name string) int {
	raw := OptionValue(opts, name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

func Fragment(items []string) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, it := range items {
		q, err := json.Marshal(it)
		if err != nil {
			return "", err
		}
		b.WriteString(",")
		b.Write(q)
	}
	return b.String(), nil
}

func RefFragment(refs []commonv1.ObjectRef) (string, error) {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		q, err := json.Marshal(ref)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(q))
	}
	return strings.Join(parts, ", "), nil
}

func ListFragment(items ...string) (string, error) {
	var parts []string
	for _, it := range items {
		if it == "" {
			continue
		}
		q, err := json.Marshal(it)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(q))
	}
	return strings.Join(parts, ", "), nil
}

func ListJSON(items []string) (string, error) {
	if items == nil {
		items = []string{}
	}
	b, err := json.Marshal(items)
	return string(b), err
}

func PortOf(rawURL string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	_, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0, fmt.Errorf("%s names no port", rawURL)
	}
	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("%s names no port", rawURL)
	}
	return n, nil
}

func HostOf(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func NetworkHostsArg(hosts []v1alpha1.NetworkHost) string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name+"="+h.Address)
	}
	return strings.Join(out, ",")
}

func NetworkCAFile(dir string, n *v1alpha1.MarketNetworkValue) (string, error) {
	if n == nil || strings.TrimSpace(n.CABundle) == "" || strings.TrimSpace(dir) == "" {
		return "", nil
	}
	path := filepath.Join(dir, NetworkCAFileName)
	want := strings.TrimSpace(n.CABundle) + "\n"
	if have, err := os.ReadFile(path); err == nil && string(have) == want {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(want), 0o644)
}

func RequesterMode(mode v1alpha1.MarketIdentityMode) string {
	switch mode {
	case v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthDelegated:
		return "oauth"
	case v1alpha1.MarketIdentityModePdsAccount:
		return "callback"
	}
	return "local"
}

func RequesterComponentFields(opts []v1alpha1.ApplicationOption, mode v1alpha1.MarketIdentityMode) (string, string) {
	keys := OptionValue(opts, "guest-ssh-keys-file")
	if keys == "" {
		keys = OptionValue(opts, "ssh-keys-file")
	}
	return RequesterMode(mode), keys
}

func WriteSecret(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o600)
}

func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func ObjectName(prefix, subject string) string {
	raw := strings.ToLower(prefix + "-" + subject)
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.') {
			r = '-'
		}
		if r == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(r)
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		name = strings.Trim(prefix, "-.")
	}
	if len(name) > 253 {
		name = strings.TrimRight(name[:253], "-.")
	}
	return name
}
