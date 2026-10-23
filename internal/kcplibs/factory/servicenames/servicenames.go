package servicenames

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/denospec"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/listeners"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/store"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
)

const (
	ArgsKey = "SERVICE_ARGS"

	EnvKey = "SERVICE_ENV"

	TableKey = "KCP_DNS_TABLE"

	TableFileKey = "KCP_DNS_TABLE_FILE"

	TokensKey = "KCP_TOKENS"

	DomainKey = "KCP_SERVICE_DOMAIN"

	NamespaceKey = "KCP_NAMESPACE"

	ShimKey = "KCP_SHIM"
)

type Source interface {
	Pods() []*unstructured.Unstructured
}

type SourceFunc func() []*unstructured.Unstructured

func (f SourceFunc) Pods() []*unstructured.Unstructured {
	return f()
}

type PathResolver interface {
	Lookup(ctx context.Context, logicalCluster string) string
}

type PathResolverFunc func(ctx context.Context, logicalCluster string) string

func (f PathResolverFunc) Lookup(ctx context.Context, logicalCluster string) string {
	return f(ctx, logicalCluster)
}

type Options struct {
	Source Source

	Minter store.TokenMinter

	Paths PathResolver

	Domain string

	TokenTTL time.Duration

	ServiceAccountNamespace string

	Shim string

	TableFile string
}

type Resolver struct {
	opts Options

	mu sync.Mutex

	observed map[string]map[string]string

	written []byte
}

func New(opts Options) *Resolver {
	opts.Domain = cmp.Or(opts.Domain, kcp.DefaultServiceDomain)
	opts.ServiceAccountNamespace = cmp.Or(opts.ServiceAccountNamespace, "default")
	return &Resolver{opts: opts, observed: map[string]map[string]string{}}
}

func (d *Resolver) Observe(ctx context.Context, target ref.Ref, address string) {
	d.ObserveListeners(ctx, target, address, nil)
}

func (d *Resolver) ObserveListeners(ctx context.Context, target ref.Ref, address string, ls []listeners.Listener) {
	name := d.Name(ctx, target.Name, target.Namespace, target.LogicalCluster)
	entries := podEntries(name, address, ls)
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(entries) == 0 {
		delete(d.observed, name)
		return
	}
	d.observed[name] = entries
}

func podEntries(name, address string, ls []listeners.Listener) map[string]string {
	entries := listeners.TableEntries(name, ls)
	if address != "" {
		entries[name] = address
	}
	return entries
}

func (d *Resolver) WriteTableFile(ctx context.Context) (bool, error) {
	if d.opts.TableFile == "" {
		return false, nil
	}
	table, _ := d.Table(ctx)
	body, err := json.Marshal(table)
	if err != nil {
		return false, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if bytes.Equal(body, d.written) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(d.opts.TableFile), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(d.opts.TableFile), ".table-*.json")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), d.opts.TableFile); err != nil {
		return false, err
	}
	d.written = body
	return true, nil
}

func (d *Resolver) Name(ctx context.Context, name, namespace, logicalCluster string) string {
	cluster := logicalCluster
	if d.opts.Paths != nil {
		if path := d.opts.Paths.Lookup(ctx, logicalCluster); path != "" {
			cluster = path
		}
	}
	return kcp.ServiceFQDN(name, namespace, cluster, d.opts.Domain)
}

func (d *Resolver) Table(ctx context.Context) (map[string]string, []string) {
	table := map[string]string{}
	seen := map[string]bool{}
	var workspaces []string
	if d.opts.Source == nil {
		return table, workspaces
	}
	for _, pod := range d.opts.Source.Pods() {
		cluster := pod.GetAnnotations()[kcp.ClusterAnnotation]
		if cluster == "" {
			continue
		}
		if !seen[cluster] {
			seen[cluster] = true
			workspaces = append(workspaces, cluster)
		}
		address := statusAddress(pod)
		if address == "" {
			address = AdvertisedAddress(envOf(pod, ArgsKey), envOf(pod, EnvKey))
		}
		maps.Copy(table, podEntries(d.Name(ctx, pod.GetName(), pod.GetNamespace(), cluster), address, statusListeners(pod)))
	}
	d.mu.Lock()
	for name, entries := range d.observed {
		for key := range table {
			if key == name || strings.HasSuffix(key, "."+name) && strings.HasPrefix(key, listeners.NamePrefix) {
				delete(table, key)
			}
		}
		maps.Copy(table, entries)
	}
	d.mu.Unlock()
	return table, workspaces
}

func (d *Resolver) Tokens(ctx context.Context, workspaces []string, account *denospec.ServiceAccountRef) string {
	out := map[string]string{}
	if account != nil && d.opts.Minter != nil {
		namespace := account.Namespace
		namespace = cmp.Or(namespace, d.opts.ServiceAccountNamespace)
		for _, cluster := range workspaces {
			token, err := d.opts.Minter.MintServiceAccountToken(ctx, cluster, namespace, account.Name, d.opts.TokenTTL)
			if err != nil {
				continue
			}
			out[cluster] = token
		}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(body)
}

func (d *Resolver) Env(ctx context.Context, target ref.Ref, selfArgs, selfEnv string, account *denospec.ServiceAccountRef) map[string]string {
	env := map[string]string{
		DomainKey:    d.opts.Domain,
		NamespaceKey: target.Namespace,
	}
	if d.opts.Shim != "" {
		env[ShimKey] = d.opts.Shim
	}
	if d.opts.TableFile != "" {
		env[TableFileKey] = d.opts.TableFile
	}
	table, workspaces := d.Table(ctx)
	if !slices.Contains(workspaces, target.LogicalCluster) {
		workspaces = append(workspaces, target.LogicalCluster)
	}
	if self := AdvertisedAddress(selfArgs, selfEnv); self != "" {
		table[d.Name(ctx, target.Name, target.Namespace, target.LogicalCluster)] = self
	}
	if len(table) > 0 {
		if body, err := json.Marshal(table); err == nil {
			env[TableKey] = string(body)
		}
	}
	if account != nil {
		env[TokensKey] = d.Tokens(ctx, workspaces, account)
	}
	return env
}

func AdvertisedAddress(argsJSON, envJSON string) string {
	port, host := advertised(argsJSON, envJSON)
	if port == "" || port == "0" {
		return ""
	}
	return host + ":" + port
}

func BoundAddress(argsJSON, envJSON string, port int32) string {
	if port <= 0 {
		return ""
	}
	_, host := advertised(argsJSON, envJSON)
	return host + ":" + strconv.Itoa(int(port))
}

func BoundListeners(argsJSON, envJSON string, port int32, reported []listeners.Listener) []listeners.Listener {
	if len(reported) > 0 {
		return advertisedOnLoopback(argsJSON, envJSON, reported)
	}
	address := BoundAddress(argsJSON, envJSON, port)
	if address == "" {
		return nil
	}
	return []listeners.Listener{{Name: listeners.DefaultTCPName, Kind: listeners.KindTCP, Address: address}}
}

func advertisedOnLoopback(argsJSON, envJSON string, reported []listeners.Listener) []listeners.Listener {
	_, host := advertised(argsJSON, envJSON)
	name := strings.ToLower(host)
	if name != "localhost" && !strings.HasSuffix(name, ".localhost") {
		return reported
	}
	out := make([]listeners.Listener, 0, len(reported))
	for _, l := range reported {
		if l.Kind == listeners.KindTCP {
			if h, p, err := net.SplitHostPort(l.Address); err == nil && h == "127.0.0.1" {
				l.Address = net.JoinHostPort(host, p)
			}
		}
		out = append(out, l)
	}
	return out
}

var (
	bindFlags = []string{"--port", "--serve-port", "--tls-port"}

	bindEnv = []string{"PORT", "SERVE_PORT", "TLS_PORT"}
)

func FixedPorts(env map[string]string) []string {
	var found []string
	fixed := func(value string) bool {
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 16)
		return err == nil && n != 0
	}
	args := []string{}
	if raw := env[ArgsKey]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &args)
	}
	for i, arg := range args {
		if slices.Contains(bindFlags, arg) && i+1 < len(args) && fixed(args[i+1]) {
			found = append(found, ArgsKey+" "+arg+" "+args[i+1])
		}
	}
	serviceEnv := map[string]string{}
	if raw := env[EnvKey]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &serviceEnv)
	}
	for _, key := range bindEnv {
		if fixed(serviceEnv[key]) {
			found = append(found, EnvKey+" "+key+" "+serviceEnv[key])
		}
	}
	for _, key := range bindEnv {
		if fixed(env[key]) {
			found = append(found, "env "+key+" "+env[key])
		}
	}
	return found
}

func advertised(argsJSON, envJSON string) (string, string) {
	args := []string{}
	if argsJSON != "" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}
	serviceEnv := map[string]string{}
	if envJSON != "" {
		_ = json.Unmarshal([]byte(envJSON), &serviceEnv)
	}
	flag := func(name string) string {
		for i, arg := range args {
			if arg == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	port := flag("--port")
	if port == "" {
		port = serviceEnv["PORT"]
	}
	host := flag("--hostname")
	if host == "" {
		host = serviceEnv["HOSTNAME"]
	}
	return port, listeners.LoopbackFor(host)
}

func statusAddress(pod *unstructured.Unstructured) string {
	address, _, _ := unstructured.NestedString(pod.Object, "status", "address")
	return address
}

func statusListeners(pod *unstructured.Unstructured) []listeners.Listener {
	items, _, _ := unstructured.NestedSlice(pod.Object, "status", "listeners")
	var out []listeners.Listener
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := fields["name"].(string)
		kind, _ := fields["kind"].(string)
		address, _ := fields["address"].(string)
		out = append(out, listeners.Listener{Name: name, Kind: kind, Address: address})
	}
	return listeners.Clean(out)
}

func envOf(pod *unstructured.Unstructured, key string) string {
	env, _, _ := unstructured.NestedMap(pod.Object, "spec", "env")
	value, _ := env[key].(string)
	return value
}
