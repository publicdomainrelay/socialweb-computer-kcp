package cloudinit

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	"gopkg.in/yaml.v3"
)

type Context struct {
	VMName            string
	SSHAuthorizedKey  string
	TargetPort        int
	HostAliases       []string
	SecretsURL        string
	SecretsRoute      string
	SecretsAud        string
	SecretsAcceptPath string
	DNSGateway        string
	DNSFallbacks      []string
	DumbpipeVersion   string
	Tunnels           *TunnelsOptions
}

type WriteFileEntry struct {
	Path        string
	Owner       string
	Permissions string
	Content     string
	Extra       map[string]any
}

func (w WriteFileEntry) Map() map[string]any {
	out := make(map[string]any, len(w.Extra)+4)
	maps.Copy(out, w.Extra)
	out["path"] = w.Path
	if w.Owner != "" {
		out["owner"] = w.Owner
	}
	if w.Permissions != "" {
		out["permissions"] = w.Permissions
	}
	out["content"] = w.Content
	return out
}

type UserDataPatch struct {
	Apt            map[string]any
	Packages       []string
	PackageUpdate  *bool
	Users          []any
	WriteFiles     []WriteFileEntry
	Runcmd         []any
	RuncmdPrepend  []any
	Bootcmd        []any
	CACertsTrusted []string
	DisableRoot    *bool
	SSHPwAuth      *bool
}

type UserDataModule func(ctx Context) (UserDataPatch, error)

type LegacyTransportError struct {
	Transport string
}

func (e *LegacyTransportError) Error() string {
	return fmt.Sprintf("guest transport %q was removed (legacy-transport): %s is the guest transport, and an https tunnel is a ComputeContract's spec.tunnels", e.Transport, GuestTransportModule)
}

type RemovedTransportFlagError struct {
	Flag string
}

func (e *RemovedTransportFlagError) Error() string {
	return fmt.Sprintf("--%s was removed (legacy-transport): %s is the guest transport, and an https tunnel is a ComputeContract's spec.tunnels", e.Flag, GuestTransportModule)
}

type UnknownModuleError struct {
	ID string
}

func (e *UnknownModuleError) Error() string {
	return "unknown user-data module: " + e.ID
}

const (
	ResolvConfPath         = "/etc/resolv.conf"
	DefaultDNSGateway      = "172.30.0.1"
	GuestTransportModule   = "iroh"
	DumbpipeVersionDefault = "0.39.0"
	GuestLoginScriptPath   = "/usr/local/bin/guest-login.sh"
	GuestLoginUnit         = "guest-login.service"
	GuestLoginTokenPath    = "/root/secrets/openbao/token"
	GuestLoginNamespace    = "prove-sshd"
	GuestLoginDefaultMount = "sshsig"
	PoweroffEndsVMModule   = "poweroff-ends-vm"
	PoweroffEndsVMPath     = "/usr/lib/systemd/system-shutdown/socialweb-poweroff-ends-vm.shutdown"
)

var DefaultDNSFallbacks = []string{"1.1.1.1", "9.9.9.9"}

var RemovedTransportModules = []string{"tunnel", "fedproxy-ssh", "fedproxy-web"}

func GetUserDataModules(ids []any) ([]UserDataModule, error) {
	out := make([]UserDataModule, 0, len(ids))
	for _, idOrFn := range ids {
		switch v := idOrFn.(type) {
		case UserDataModule:
			out = append(out, v)
		case string:
			removed := slices.Contains(RemovedTransportModules, v)
			if removed {
				return nil, &LegacyTransportError{Transport: v}
			}
			module, ok := registry[v]
			if !ok {
				return nil, &UnknownModuleError{ID: v}
			}
			out = append(out, module)
		default:
			return nil, fmt.Errorf("unknown user-data module: %v", v)
		}
	}
	return out, nil
}

func ListUserDataModules() []string {
	out := make([]string, 0, len(registry))
	for id := range registry {
		out = append(out, id)
	}
	return out
}

func RemovedTransportFlagsIn(options map[string]any, flags []string) []error {
	out := []error{}
	for _, flag := range flags {
		camel := regexp.MustCompile(`-([a-z])`).ReplaceAllStringFunc(flag, func(s string) string {
			return strings.ToUpper(s[1:])
		})
		if _, ok := options[camel]; ok {
			out = append(out, &RemovedTransportFlagError{Flag: flag})
		}
	}
	return out
}

func RenderResolver(nameservers []string) string {
	unique := []string{}
	seen := map[string]bool{}
	for _, n := range nameservers {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		unique = append(unique, n)
	}
	lines := make([]string, 0, len(unique))
	for _, n := range unique {
		lines = append(lines, "nameserver "+n)
	}
	return strings.Join(lines, "\n") + "\noptions timeout:2 attempts:3\n"
}

func asArray(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{}
}

func asObj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func dedupeByKey(arr []any, key func(map[string]any) string) []any {
	indexOf := map[string]int{}
	out := []any{}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		k := key(m)
		if k != "" {
			if idx, ok := indexOf[k]; ok {
				out[idx] = m
				continue
			}
			indexOf[k] = len(out)
		}
		out = append(out, m)
	}
	return out
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func applyPatch(base map[string]any, p UserDataPatch) map[string]any {
	out := make(map[string]any, len(base)+8)
	maps.Copy(out, base)
	if p.Apt != nil {
		apt := map[string]any{}
		maps.Copy(apt, asObj(out["apt"]))
		maps.Copy(apt, p.Apt)
		out["apt"] = apt
	}
	if p.Packages != nil {
		existing := []any{}
		for _, v := range asArray(out["packages"]) {
			existing = append(existing, v)
		}
		seen := map[string]bool{}
		pkgs := []any{}
		for _, v := range existing {
			if s, ok := v.(string); ok {
				if seen[s] {
					continue
				}
				seen[s] = true
			}
			pkgs = append(pkgs, v)
		}
		for _, s := range p.Packages {
			if seen[s] {
				continue
			}
			seen[s] = true
			pkgs = append(pkgs, s)
		}
		out["packages"] = pkgs
	}
	if p.PackageUpdate != nil {
		out["package_update"] = *p.PackageUpdate
	}
	if p.Users != nil {
		users := append([]any{}, asArray(out["users"])...)
		users = append(users, p.Users...)
		out["users"] = dedupeByKey(users, func(m map[string]any) string {
			s, _ := m["name"].(string)
			return s
		})
	}
	if p.WriteFiles != nil {
		files := append([]any{}, asArray(out["write_files"])...)
		for _, entry := range p.WriteFiles {
			files = append(files, entry.Map())
		}
		out["write_files"] = dedupeByKey(files, func(m map[string]any) string {
			s, _ := m["path"].(string)
			return s
		})
	}
	if p.Bootcmd != nil {
		out["bootcmd"] = dedupeAny(append(append([]any{}, asArray(out["bootcmd"])...), p.Bootcmd...))
	}
	if p.CACertsTrusted != nil {
		ca := map[string]any{}
		maps.Copy(ca, asObj(out["ca_certs"]))
		existing := asArray(ca["trusted"])
		merged := append(append([]any{}, existing...), strs.AnySlice(p.CACertsTrusted)...)
		ca["trusted"] = dedupeAny(merged)
		out["ca_certs"] = ca
	}
	if p.Runcmd != nil || p.RuncmdPrepend != nil {
		runcmd := append([]any{}, p.RuncmdPrepend...)
		runcmd = append(runcmd, asArray(out["runcmd"])...)
		runcmd = append(runcmd, p.Runcmd...)
		out["runcmd"] = runcmd
	}
	if p.DisableRoot != nil {
		out["disable_root"] = *p.DisableRoot
	}
	if p.SSHPwAuth != nil {
		out["ssh_pwauth"] = *p.SSHPwAuth
	}
	return out
}

func dedupeAny(values []any) []any {
	out := []any{}
	for _, v := range values {
		duplicate := false
		for _, seen := range out {
			if reflect.DeepEqual(seen, v) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, v)
		}
	}
	return out
}

func marshalJSONIndent(value any) (string, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

var packageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)

func PackagesGuardCommand(packages []string) (string, error) {
	names := dedupeStrings(packages)
	for _, name := range names {
		if !packageNameRE.MatchString(name) {
			return "", fmt.Errorf("the composed user_data asks for the package %q, which is not a package name this will put in a shell command", name)
		}
	}
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, `"`+name+`"`)
	}
	list := strings.Join(quoted, " ")
	return `sh -c 'missing=""; for p in ` + list + `; do dpkg -s "$p" >/dev/null 2>&1 || ` +
		`missing="$missing $p"; done; [ -z "$missing" ] || { apt-get update && ` +
		`DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ` +
		`-o Dpkg::Options::=--force-confold -o Dpkg::Options::=--force-confdef $missing; }'`, nil
}

func guardPackages(base map[string]any) (map[string]any, error) {
	requested := []string{}
	for _, v := range asArray(base["packages"]) {
		if s, ok := v.(string); ok {
			requested = append(requested, s)
		}
	}
	if len(requested) == 0 {
		return base, nil
	}
	command, err := PackagesGuardCommand(requested)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(base))
	maps.Copy(out, base)
	delete(out, "packages")
	out["package_update"] = false
	out["runcmd"] = append([]any{command}, asArray(out["runcmd"])...)
	return out, nil
}

func parseBase(base string) map[string]any {
	if base == "" {
		return map[string]any{}
	}
	trimmed := regexp.MustCompile(`(?i)^#cloud-config\s*`).ReplaceAllString(base, "")
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(trimmed), &parsed); err != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

var nameserverRE = regexp.MustCompile(`^\s*nameserver\s+(\S+)`)

func nameserversIn(content string) []string {
	out := []string{}
	for line := range strings.SplitSeq(content, "\n") {
		if m := nameserverRE.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

func resolvWriteCommand(resolver string) string {
	return "printf '%s' '" + strings.ReplaceAll(resolver, "'", `'\''`) + "' > " + ResolvConfPath
}

func trailingOnce(arr []any, item string) []any {
	out := []any{}
	for _, v := range arr {
		if s, ok := v.(string); ok && s == item {
			continue
		}
		out = append(out, v)
	}
	return append(out, item)
}

func assertResolver(base map[string]any, gateway string, fallbacks []string) map[string]any {
	out := make(map[string]any, len(base))
	maps.Copy(out, base)
	files := append([]any{}, asArray(out["write_files"])...)
	idx := -1
	for i, raw := range files {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if p, _ := m["path"].(string); p == ResolvConfPath {
			idx = i
			break
		}
	}
	existing := ""
	if idx >= 0 {
		if s, ok := files[idx].(map[string]any)["content"].(string); ok {
			existing = s
		}
	}
	extras := []string{}
	for _, n := range nameserversIn(existing) {
		if n == gateway {
			continue
		}
		skip := slices.Contains(fallbacks, n)
		if !skip {
			extras = append(extras, n)
		}
	}
	resolver := RenderResolver(append([]string{gateway}, append(fallbacks, extras...)...))
	entry := map[string]any{
		"path":        ResolvConfPath,
		"owner":       "root:root",
		"permissions": "0644",
		"content":     resolver,
	}
	if idx >= 0 {
		merged := map[string]any{}
		maps.Copy(merged, files[idx].(map[string]any))
		maps.Copy(merged, entry)
		files[idx] = merged
	} else {
		files = append(files, entry)
	}
	out["write_files"] = files
	command := resolvWriteCommand(resolver)
	out["bootcmd"] = trailingOnce(asArray(out["bootcmd"]), command)
	out["runcmd"] = trailingOnce(asArray(out["runcmd"]), command)
	return out
}

type BuildOptions struct {
	Ctx          Context
	Base         string
	Modules      []any
	Overrides    *UserDataPatch
	DNSGateway   string
	DNSFallbacks []string
}

func BuildUserData(opts BuildOptions) (string, error) {
	ctx := opts.Ctx
	gateway := opts.DNSGateway
	gateway = cmp.Or(gateway, ctx.DNSGateway)
	gateway = cmp.Or(gateway, DefaultDNSGateway)
	fallbacks := opts.DNSFallbacks
	if fallbacks == nil {
		fallbacks = ctx.DNSFallbacks
	}
	if fallbacks == nil {
		fallbacks = DefaultDNSFallbacks
	}
	modules, err := GetUserDataModules(opts.Modules)
	if err != nil {
		return "", err
	}
	obj := parseBase(opts.Base)
	for _, module := range modules {
		patch, err := module(ctx)
		if err != nil {
			return "", err
		}
		obj = applyPatch(obj, patch)
	}
	if opts.Overrides != nil {
		obj = applyPatch(obj, *opts.Overrides)
	}
	obj, err = guardPackages(obj)
	if err != nil {
		return "", err
	}
	obj = assertResolver(obj, gateway, fallbacks)
	body, err := yaml.Marshal(obj)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(body), nil
}

func AcceptBundleModule(acceptPathVM string, bundle map[string]any) UserDataModule {
	parts := strings.Split(acceptPathVM, "/")
	parent := strings.Join(parts[:len(parts)-1], "/")
	return func(Context) (UserDataPatch, error) {
		content, err := marshalJSONIndent(bundle)
		if err != nil {
			return UserDataPatch{}, err
		}
		return UserDataPatch{
			WriteFiles: []WriteFileEntry{{
				Path:        acceptPathVM,
				Owner:       "root:root",
				Permissions: "0600",
				Content:     content,
			}},
			RuncmdPrepend: []any{[]any{"sh", "-c", "install -d -m 0700 -o root -g root " + parent}},
		}, nil
	}
}
