package openbaoctl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/baosync"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	PluginDirName      = "plugins"
	PluginCommandBytes = 16
)

var (
	PluginAssets = []string{"openbao-plugin-auth-atproto", "openbao-plugin-auth-sshsig"}

	pluginNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

var PluginCorePolicyRules = []v1alpha1.OpenBaoPolicyRule{
	{Path: "sys/plugins/catalog", Capabilities: []string{"read", "list", "sudo"}},
	{Path: "sys/plugins/catalog/*", Capabilities: []string{"create", "read", "update", "delete", "sudo"}},
	{Path: "sys/plugins/reload/backend", Capabilities: []string{"update", "sudo"}},
}

func PluginDir(dir string) string { return filepath.Join(dir, PluginDirName) }

type placedPlugin struct {
	spec   v1alpha1.OpenBaoPlugin
	plugin baosync.Plugin
}

func checkPlugins(ps []v1alpha1.OpenBaoPlugin) error {
	names, mounts := map[string]bool{}, map[string]bool{}
	for _, p := range ps {
		if !pluginNamePattern.MatchString(p.Name) {
			return fmt.Errorf("plugin name %q: lower case letters, digits and dashes", p.Name)
		}
		if p.Type != baosync.PluginAuth {
			return fmt.Errorf("plugin %s: type %q; auth plugins are supported", p.Name, p.Type)
		}
		if !slices.Contains(PluginAssets, p.Asset.Name) {
			return fmt.Errorf("plugin %s: asset %q is none of the OpenBao plugin assets (%s)", p.Name, p.Asset.Name, strings.Join(PluginAssets, ", "))
		}
		mount := strings.Trim(p.MountPath, "/")
		if mount == "" || strings.Contains(mount, "/") || reservedMountNames[mount] || mount == AppRoleMount {
			return fmt.Errorf("plugin %s: mountPath %q is not a mount of its own", p.Name, p.MountPath)
		}
		if names[p.Name] || mounts[mount] {
			return fmt.Errorf("plugin %s: names and mount paths are each used once", p.Name)
		}
		if p.Atproto != nil {
			if !atprotoPlugin(p) {
				return fmt.Errorf("plugin %s: atproto settings need the %s asset", p.Name, AtprotoPluginAsset)
			}
			if c := p.Atproto.Config; (c.ClientID == "") != (len(c.RedirectURIs) == 0) {
				return fmt.Errorf("plugin %s: atproto config names a clientID and redirectURIs together or neither", p.Name)
			}
			for _, r := range p.Atproto.Roles {
				if !pluginNamePattern.MatchString(r.Name) || (len(r.BoundDIDs) == 0 && len(r.BoundHandles) == 0) {
					return fmt.Errorf("plugin %s: atproto role %q needs a lower case name and boundDIDs or boundHandles", p.Name, r.Name)
				}
			}
		}
		names[p.Name], mounts[mount] = true, true
	}
	return nil
}

func (s *Servers) placePlugins(o v1alpha1.OpenBaoServer, dir string) ([]placedPlugin, error) {
	if len(o.Spec.Plugins) == 0 {
		return nil, nil
	}
	if err := checkPlugins(o.Spec.Plugins); err != nil {
		return nil, err
	}
	if s.c.Resolve == nil {
		return nil, fmt.Errorf("this core resolves no assets, so it cannot place plugin %s", o.Spec.Plugins[0].Name)
	}
	pdir := PluginDir(dir)
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		return nil, err
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	var out []placedPlugin
	for _, p := range o.Spec.Plugins {
		src, err := s.c.Resolve(p.Asset)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: asset %s: %w", p.Name, p.Asset.Name, err)
		}
		sum, err := sha256sum.File(src)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		pin, err := versions.FetchSHA256(p.Asset.Name, platform)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		if sum != pin || (p.Asset.Sha256 != "" && sum != p.Asset.Sha256) {
			return nil, fmt.Errorf("plugin %s: %s has sha256 %s; internal/versions/versions.json pins %s for %s", p.Name, src, sum, pin, platform)
		}
		command := p.Asset.Name + "-" + sum[:PluginCommandBytes]
		dst := filepath.Join(pdir, command)
		if have, err := sha256sum.File(dst); err != nil || have != sum {
			body, err := os.ReadFile(src)
			if err != nil {
				return nil, fmt.Errorf("plugin %s: %w", p.Name, err)
			}
			if err := writeIfChanged(dst, body, 0o700); err != nil {
				return nil, fmt.Errorf("plugin %s: %w", p.Name, err)
			}
			s.log.Info("placed an OpenBao plugin in the server's plugin directory", "openbaoserver", o.Name, "plugin", p.Name,
				"path", dst, "sha256", sum)
		}
		out = append(out, placedPlugin{spec: p, plugin: baosync.Plugin{Type: p.Type, Name: p.Name, Command: command, SHA256: sum}})
	}
	return out, nil
}

func prunePlugins(dir string, keep []placedPlugin) []string {
	entries, err := os.ReadDir(PluginDir(dir))
	if err != nil {
		return nil
	}
	want := map[string]bool{}
	for _, p := range keep {
		want[p.plugin.Command] = true
	}
	var removed []string
	for _, e := range entries {
		if want[e.Name()] {
			continue
		}
		if os.Remove(filepath.Join(PluginDir(dir), e.Name())) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}

func pluginStatus(placed []placedPlugin, running map[string]string, err error) []v1alpha1.OpenBaoServerPluginStatus {
	var out []v1alpha1.OpenBaoServerPluginStatus
	for _, p := range placed {
		mount := strings.Trim(p.spec.MountPath, "/")
		st := v1alpha1.OpenBaoServerPluginStatus{Name: p.spec.Name, Type: p.spec.Type, Sha256: p.plugin.SHA256, Command: p.plugin.Command,
			MountPath: mount, RunningSha256: running[mount]}
		switch {
		case err != nil:
			st.Message = err.Error()
		case st.RunningSha256 != st.Sha256:
			st.Message = "the mount does not run the declared sha256 yet"
		}
		out = append(out, st)
	}
	return out
}
