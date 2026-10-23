package openbaoctl

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/baosync"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/rootvault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultcert"
)

const (
	DefaultInterval  = 5 * time.Second
	RetryAfter       = 2 * time.Second
	PassTimeout      = 30 * time.Second
	DefaultListen    = rootvault.LoopbackHost + ":0"
	DefaultShares    = 1
	DefaultThreshold = 1
	DirName          = "openbaoservers"
	ChildPrefix      = "bao-"
	OwnerLabel       = "socialweb.computer/openbaoserver"

	ConditionReady       = "Ready"
	ConditionInitialized = "Initialized"
	ConditionUnsealed    = "Unsealed"
	ConditionSynced      = "Synced"
	LogCapBytes          = 64 << 20
)

var GVR = v1alpha1.OpenBaoServerGVR

type Config struct {
	Kubeconfig string
	Workspace  string
	StateDir   string
	Prefix     string
	Interval   time.Duration
	Env        []v1alpha1.HostProcessEnv
	Resolve    func(v1alpha1.HostProcessAsset) (string, error)
	Log        *slog.Logger
}

var OutboundEnvNames = []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE"}

func OutboundEnv(getenv func(string) string) []v1alpha1.HostProcessEnv {
	var out []v1alpha1.HostProcessEnv
	for _, n := range OutboundEnvNames {
		if v := getenv(n); v != "" {
			out = append(out, v1alpha1.HostProcessEnv{Name: n, Value: v})
		}
	}
	return out
}

type Controller struct {
	c        Config
	servers  dynamic.ResourceInterface
	children dynamic.ResourceInterface
	grants   dynamic.ResourceInterface
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log.With("component", "openbaoserver")
}

func New(c Config) (*Controller, error) {
	if !filepath.IsAbs(c.StateDir) {
		return nil, fmt.Errorf("openbaoctl: StateDir %q must be absolute", c.StateDir)
	}
	if c.Prefix == "" {
		return nil, fmt.Errorf("openbaoctl: a unit prefix is required; a controller places only its own instance's servers")
	}
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	ws := c.Workspace
	ws = cmp.Or(ws, hostprocess.DefaultWorkspace)
	cfg := rest.CopyConfig(restCfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + ws
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Controller{c: c, servers: dc.Resource(GVR), children: dc.Resource(hostprocess.GVR), grants: dc.Resource(GrantGVR)}, nil
}

func ServerDir(stateDir, name string) string { return filepath.Join(stateDir, DirName, name) }

func ChildName(name string) string { return ChildPrefix + name }

func (c *Controller) unit(o v1alpha1.OpenBaoServer) string {
	if o.Spec.Unit != "" {
		return o.Spec.Unit
	}
	return c.c.Prefix + ChildPrefix + o.Name + ".service"
}

func refs(dir string, t *v1alpha1.OpenBaoServerTLS) *v1alpha1.OpenBaoServerRefs {
	ca := rootvault.TLSCAPath(dir)
	if t != nil {
		ca = t.CAFile
	}
	return &v1alpha1.OpenBaoServerRefs{UnsealKeys: "file:" + rootvault.UnsealKeysPath(dir), CA: "file:" + ca,
		ClientCert: "file:" + rootvault.ClientCertPath(dir), ClientKey: "file:" + rootvault.ClientKeyPath(dir),
		CertAuthMount: CertAuthMount, CertAuthRole: CertAuthRole, KVMount: KVMount}
}

var replaceable = map[v1alpha1.HostProcessLostReason]bool{v1alpha1.HostProcessLostUnitGone: true, v1alpha1.HostProcessLostHostRebooted: true,
	v1alpha1.HostProcessLostManagerRestarted: true, v1alpha1.HostProcessLostIdentityMismatch: true}

func restartKey(config []byte, files ...string) string {
	h := sha256.New()
	h.Write(config)
	for _, f := range files {
		if f == "" {
			continue
		}
		b, _ := os.ReadFile(f)
		h.Write([]byte(f))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func toServer(u *unstructured.Unstructured) (v1alpha1.OpenBaoServer, error) {
	var o v1alpha1.OpenBaoServer
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o)
	return o, err
}

const MemoryMax = 512 << 20

func (c *Controller) childSpec(o v1alpha1.OpenBaoServer, dir, key string) v1alpha1.HostProcessSpec {
	rp := o.Spec.RestartPolicy
	rp = cmp.Or(rp, v1alpha1.HostProcessRestartOnFailure)
	spec := v1alpha1.HostProcessSpec{Unit: c.unit(o), Exec: o.Spec.Exec, Argv: []string{"server", "-config", rootvault.ConfigPath(dir)},
		WorkDir: rootvault.Dir(dir), RestartPolicy: rp, Stop: &v1alpha1.HostProcessStop{Signal: "SIGTERM", GraceSeconds: 15},
		Placement: v1alpha1.HostProcessPlacementSupervisor, Slice: o.Spec.Slice, RestartKey: key,
		Limits: &v1alpha1.HostProcessLimits{MemoryMax: strconv.FormatUint(MemoryMax, 10)},
		After:  append([]string(nil), o.Spec.After...), Wants: append([]string(nil), o.Spec.Wants...)}
	if o.Spec.Asset != nil {
		a := *o.Spec.Asset
		spec.Asset = &a
	}
	if o.Spec.Log != "" {
		spec.Output = &v1alpha1.HostProcessOutput{Stdout: o.Spec.Log, Stderr: o.Spec.Log, CapBytes: LogCapBytes}
	}
	spec.Env = append(spec.Env, c.c.Env...)
	return spec
}

func sameChild(a, b v1alpha1.HostProcessSpec) bool {
	return a.Unit == b.Unit && a.Exec == b.Exec && slices.Equal(a.Argv, b.Argv) && a.WorkDir == b.WorkDir &&
		a.RestartPolicy == b.RestartPolicy && a.Placement == b.Placement && fmt.Sprint(a.Asset) == fmt.Sprint(b.Asset) &&
		a.Slice == b.Slice && slices.Equal(a.After, b.After) && slices.Equal(a.Wants, b.Wants) && a.RestartKey == b.RestartKey &&
		fmt.Sprint(a.Output) == fmt.Sprint(b.Output)
}

func (c *Controller) ensureChild(ctx context.Context, o v1alpha1.OpenBaoServer, want v1alpha1.HostProcessSpec) (v1alpha1.HostProcess, error) {
	name := ChildName(o.Name)
	cur, err := c.children.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		hp := v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{OwnerLabel: o.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "OpenBaoServer", Name: o.Name,
				UID: o.UID, Controller: new(true)}}}, Spec: want}
		u, err := unstruct.To(&hp, "HostProcess")
		if err != nil {
			return v1alpha1.HostProcess{}, err
		}
		got, err := c.children.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return v1alpha1.HostProcess{}, err
		}
		c.c.log().Info("placed the server's HostProcess", "openbaoserver", o.Name, "hostprocess", name, "unit", want.Unit)
		return unstruct.From[v1alpha1.HostProcess](got)
	}
	if err != nil {
		return v1alpha1.HostProcess{}, err
	}
	hp, err := unstruct.From[v1alpha1.HostProcess](cur)
	if err != nil {
		return hp, err
	}
	if hp.Labels[OwnerLabel] != o.Name {
		return hp, fmt.Errorf("HostProcess %s exists and is not owned by OpenBaoServer %s", name, o.Name)
	}
	if !sameChild(hp.Spec, want) {
		hp.Spec = want
		u, err := unstruct.To(&hp, "HostProcess")
		if err != nil {
			return hp, err
		}
		got, err := c.children.Update(ctx, u, metav1.UpdateOptions{})
		if err != nil {
			return hp, err
		}
		return unstruct.From[v1alpha1.HostProcess](got)
	}
	return hp, nil
}

func writeIfChanged(path string, data []byte, mode os.FileMode) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return os.Chmod(path, mode)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func unseal(ctx context.Context, client *openbao.Client, k UnsealKeys) error {
	for i := 0; i < k.Threshold; i++ {
		sealed, err := client.Unseal(ctx, k.Keys[i])
		if err != nil {
			return fmt.Errorf("unsealing with key %d of %d: %w", i+1, k.Threshold, err)
		}
		if !sealed {
			return nil
		}
	}
	return fmt.Errorf("still sealed after %d keys", k.Threshold)
}

type synced struct {
	report  baosync.Report
	pending []string
	how     string
	desired baosync.Desired
	running map[string]string
	tidied  bool
	tidyErr error
	roles   map[string]grantRole
	refused map[string]string
}

func tidyInterval(s v1alpha1.OpenBaoServerSpec) (time.Duration, error) {
	if s.Tidy == nil || s.Tidy.Interval == "" {
		return DefaultTidyInterval, nil
	}
	d, err := time.ParseDuration(s.Tidy.Interval)
	if err != nil || d < time.Minute {
		return 0, fmt.Errorf("spec.tidy.interval %q: a duration of at least 1m", s.Tidy.Interval)
	}
	return d, nil
}

func tidyDue(prev *v1alpha1.OpenBaoServerTidyStatus, interval time.Duration, now time.Time) bool {
	if prev == nil || prev.LastRunAt == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, prev.LastRunAt)
	return err != nil || now.Sub(at) >= interval
}

func tidyTokensAndLeases(ctx context.Context, admin *openbao.Client) (bool, error) {
	var errs []error
	for _, p := range []string{"auth/token/tidy", "sys/leases/tidy"} {
		if err := admin.Write(ctx, p, map[string]any{}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return len(errs) == 0, errors.Join(errs...)
}

func recordTidy(st *v1alpha1.OpenBaoServerStatus, s synced, interval time.Duration, log *slog.Logger) {
	next := &v1alpha1.OpenBaoServerTidyStatus{Interval: interval.String()}
	if st.Tidy != nil {
		next.LastRunAt, next.Message = st.Tidy.LastRunAt, st.Tidy.Message
	}
	switch {
	case s.tidyErr != nil:
		next.Message = "token and lease tidy failed: " + s.tidyErr.Error()
		log.Warn("token and lease tidy failed", "err", s.tidyErr.Error())
	case s.tidied:
		next.LastRunAt = time.Now().UTC().Format(time.RFC3339Nano)
		next.Message = "token and lease tidy ran"
		log.Info("token and lease tidy ran", "interval", interval.String())
	}
	st.Tidy = next
}

func syncWith(ctx context.Context, admin *openbao.Client, in layoutInput) (synced, error) {
	in.accessors = accessorsOf(ctx, admin)
	d, pending, roles, refused, err := desiredWithIdentity(ctx, in, pkiCA(admin))
	if err != nil {
		return synced{}, err
	}
	rep, err := baosync.Sync(ctx, admin, d)
	generatedRoot := false
	if err == nil && rep.Err == nil {
		generatedRoot, err = ensurePKIRoots(ctx, admin, in.declared)
		if err != nil {
			return synced{}, err
		}
	}
	if err == nil && rep.Err == nil && (generatedRoot || len(pending) > 0 && rep.Changed()) {
		in.accessors = accessorsOf(ctx, admin)
		if d2, pending2, roles2, refused2, derr := desiredWithIdentity(ctx, in, pkiCA(admin)); derr == nil {
			rep2, err2 := baosync.Sync(ctx, admin, d2)
			rep.Applied = append(rep.Applied, rep2.Applied...)
			rep.Conflicts, rep.Err = rep2.Conflicts, errors.Join(rep.Err, rep2.Err)
			d, pending, roles, refused, err = d2, pending2, roles2, refused2, err2
		}
	}
	s := synced{report: rep, pending: pending, desired: d, roles: roles, refused: refused}
	if err == nil && in.tidyDue {
		s.tidied, s.tidyErr = tidyTokensAndLeases(ctx, admin)
	}
	if err == nil && len(in.plugins) > 0 {
		s.running = map[string]string{}
		if auths, aerr := admin.AuthMounts(ctx); aerr == nil {
			for p, m := range auths {
				s.running[p] = m.RunningSHA256
			}
		}
	}
	return s, err
}

const (
	defaultPKIRootCommonName = "socialweb.local"
	defaultPKIRootTTL        = "8760h"
)

func ensurePKIRoots(ctx context.Context, c *openbao.Client, d *v1alpha1.OpenBaoServerDeclared) (bool, error) {
	if d == nil {
		return false, nil
	}
	generated := false
	for _, m := range d.SecretMounts {
		if m.Type != string(baosync.MountPKI) {
			continue
		}
		if _, err := c.CAPEM(ctx, m.Path); err == nil {
			continue
		} else if !pkiRootMissing(err) {
			return false, err
		}
		ttl := strings.TrimSpace(m.MaxLeaseTTL)
		ttl = cmp.Or(ttl, defaultPKIRootTTL)
		if _, err := c.GeneratePKIRoot(ctx, m.Path, defaultPKIRootCommonName, ttl); err != nil {
			return false, fmt.Errorf("generating the pki root under %s: %w", m.Path, err)
		}
		generated = true
	}
	return generated, nil
}

func pkiRootMissing(err error) bool {
	var re *openbao.ResponseError
	return errors.As(err, &re) && (re.Status == 404 || re.Status == 400) || strings.Contains(err.Error(), "holds no certificate")
}

func accessorsOf(ctx context.Context, admin *openbao.Client) map[string]string {
	out := map[string]string{}
	auths, err := admin.AuthMounts(ctx)
	if err != nil {
		return out
	}
	for p, m := range auths {
		if m.Accessor != "" {
			out[p] = m.Accessor
		}
	}
	return out
}

func (c *Controller) syncCore(ctx context.Context, client *openbao.Client, keysPath string, in layoutInput) (synced, error) {
	core, err := loginCore(ctx, client)
	if err == nil {
		s, serr := syncWith(ctx, core, in)
		core.RevokeSelf(context.WithoutCancel(ctx))
		if serr == nil || !permissionDenied(serr) {
			s.how = "the core's cert-auth token"
			return s, serr
		}
		err = serr
	} else if !loginRefused(err) {
		return synced{}, err
	}
	k, kerr := ReadUnsealKeys(keysPath)
	if kerr != nil {
		return synced{}, fmt.Errorf("the core cannot reconcile (%v) and %w", err, kerr)
	}
	tok, gerr := client.GenerateRoot(ctx, k.Keys, k.Threshold)
	if gerr != nil {
		return synced{}, fmt.Errorf("the core cannot reconcile (%v) and %w", err, gerr)
	}
	root := client.WithToken(tok)
	defer root.RevokeSelf(context.WithoutCancel(ctx))
	s, serr := syncWith(ctx, root, in)
	s.how = fmt.Sprintf("a root token generated from the unseal keys and revoked after the pass, because the core's own token could not reconcile (%v)", err)
	return s, serr
}

func recordSync(st *v1alpha1.OpenBaoServerStatus, gen int64, s synced, err error, log *slog.Logger) {
	prev := st.Sync
	next := &v1alpha1.OpenBaoServerSync{ObservedGeneration: gen}
	if prev != nil {
		next.SyncedAt, next.LastChange, next.LastChangeAt = prev.SyncedAt, prev.LastChange, prev.LastChangeAt
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err != nil {
		st.Sync = next
		if prev != nil {
			next.Declared, next.Pending, next.Conflicts = prev.Declared, prev.Pending, prev.Conflicts
		}
		conds.Set(&st.Conditions, gen, ConditionSynced, false, "SyncFailed", err.Error())
		log.Warn("the reconcile of the server against its declaration did not run", "err", err.Error())
		return
	}
	next.Declared, next.Pending, next.Conflicts = s.desired.String(), s.pending, s.report.Conflicts
	if s.report.Changed() {
		next.LastChange, next.LastChangeAt = s.report.Summary(), now
		ops := make([]string, 0, len(s.report.Applied))
		for _, o := range s.report.Applied {
			ops = append(ops, o.String()+" ("+o.Why+")")
		}
		log.Info("the server is brought back to its declaration", "at", now, "ops", ops, "by", s.how)
	}
	if prev == nil || prev.ObservedGeneration != gen || s.report.Changed() {
		next.SyncedAt = now
	}
	st.Sync = next
	switch {
	case s.report.Err != nil:
		conds.Set(&st.Conditions, gen, ConditionSynced, false, "ApplyFailed", s.report.Summary())
	case len(s.report.Conflicts) > 0:
		conds.Set(&st.Conditions, gen, ConditionSynced, false, "Conflict", strings.Join(s.report.Conflicts, "; "))
	default:
		conds.Set(&st.Conditions, gen, ConditionSynced, true, "InSync", "the server holds exactly its declaration: "+s.desired.String())
	}
}

func (c *Controller) reconcile(ctx context.Context, o v1alpha1.OpenBaoServer) v1alpha1.OpenBaoServerStatus {
	log := c.c.log().With("openbaoserver", o.Name)
	var st v1alpha1.OpenBaoServerStatus
	o.Status.DeepCopyInto(&st)
	gen := o.Generation
	st.ObservedGeneration = gen
	dir := ServerDir(c.c.StateDir, o.Name)
	st.HostProcess = ChildName(o.Name)
	st.Refs = refs(dir, o.Spec.TLS)
	fail := func(format string, args ...any) v1alpha1.OpenBaoServerStatus {
		st.Phase, st.Message = v1alpha1.OpenBaoServerPhaseFailed, fmt.Sprintf(format, args...)
		conds.Set(&st.Conditions, gen, ConditionReady, false, "Failed", st.Message)
		log.Warn("failed", "msg", st.Message)
		return st
	}
	starting := func(format string, args ...any) v1alpha1.OpenBaoServerStatus {
		st.Phase, st.Message, st.Address = v1alpha1.OpenBaoServerPhaseStarting, fmt.Sprintf(format, args...), ""
		conds.Set(&st.Conditions, gen, ConditionReady, false, "Starting", st.Message)
		return st
	}
	shares, threshold := int(o.Spec.Init.Shares), int(o.Spec.Init.Threshold)
	shares = cmp.Or(shares, DefaultShares)
	threshold = cmp.Or(threshold, DefaultThreshold)
	if threshold > shares {
		return fail("init.threshold %d is over init.shares %d", threshold, shares)
	}
	if o.Spec.Asset == nil && o.Spec.Exec == "" {
		return fail("name the bao executable: spec.asset (name bao) or spec.exec (an absolute path)")
	}
	listen := o.Spec.Listen
	listen = cmp.Or(listen, DefaultListen)
	if err := checkClients(o.Spec.Clients, o.Spec.Declared); err != nil {
		return fail("spec.clients: %v", err)
	}
	if o.Spec.Unit != "" && !strings.HasPrefix(o.Spec.Unit, c.c.Prefix) {
		return fail("spec.unit %s is not under this controller's prefix %s", o.Spec.Unit, c.c.Prefix)
	}
	tlsm, err := vaultcert.EnsureRootVaultTLS(dir, rootvault.LoopbackHost)
	if err != nil {
		return fail("mutual TLS material: %v", err)
	}
	clientsCA, err := vaultcert.EnsureOpenBaoClientsCA(dir, tlsm.CAPEM)
	if err != nil {
		return fail("the clients CA: %v", err)
	}
	st.Clients = nil
	for _, cl := range o.Spec.Clients {
		m, err := clientsCA.EnsureClient(dir, cl.Name)
		if err != nil {
			return fail("client %s: %v", cl.Name, err)
		}
		st.Clients = append(st.Clients, v1alpha1.OpenBaoServerClientRefs{Name: cl.Name, CertAuthMount: CertAuthMount,
			CertAuthRole: cl.Name, Policy: strings.Join(clientTokenPolicies(cl), ","), KVMount: cl.KVMount, ClientCert: "file:" + m.CertFile,
			ClientKey: "file:" + m.KeyFile})
	}
	plugins, err := c.placePlugins(o, dir)
	if err != nil {
		st.Plugins = nil
		return fail("spec.plugins: %v", err)
	}
	var grants []v1alpha1.GuestIdentityGrant
	if o.Spec.Declared != nil && o.Spec.Declared.IdentityOIDC != nil {
		if grants, err = c.listGrants(ctx); err != nil {
			return fail("listing the GuestIdentityGrants: %v", err)
		}
	}

	tidyEvery, err := tidyInterval(o.Spec)
	if err != nil {
		return fail("%v", err)
	}
	layout := layoutInput{caPEM: tlsm.CAPEM, clientsCAPEM: clientsCA.CAPEM, clients: o.Spec.Clients, declared: o.Spec.Declared, plugins: plugins, grants: grants,
		tidyDue: tidyDue(st.Tidy, tidyEvery, time.Now())}
	opts, err := serverOptions(o.Spec, clientsCA.BundlePath)
	if err != nil {
		return fail("%v", err)
	}
	if len(plugins) > 0 {
		opts.PluginDirectory = PluginDir(dir)
	}
	body, err := rootvault.ServerConfigWith(dir, listen, opts)
	if err != nil {
		return fail("%v", err)
	}
	if err := os.MkdirAll(rootvault.StorageDir(dir), 0o700); err != nil {
		return fail("storage dir: %v", err)
	}
	if err := writeIfChanged(rootvault.ConfigPath(dir), []byte(body), 0o600); err != nil {
		return fail("server config: %v", err)
	}
	certFile := opts.CertFile
	if certFile == "" {
		certFile = rootvault.ServerCertPath(dir)
	}
	key := restartKey([]byte(body), certFile, caFile(o.Spec, dir), clientsCA.BundlePath)
	child, err := c.ensureChild(ctx, o, c.childSpec(o, dir, key))
	if err != nil {
		return fail("HostProcess %s: %v", ChildName(o.Name), err)
	}
	if rt := child.Status.Runtime; rt != nil {
		st.Runtime = &v1alpha1.OpenBaoServerRuntime{Backend: rt.Backend, Ref: rt.Unit, Instance: rt.InvocationID, Pid: rt.Pid,
			StartTime: rt.ActiveSince, HostBootID: rt.HostBootID}
	}
	if child.Status.Phase == v1alpha1.HostProcessPhaseLost && replaceable[child.Status.LostReason] && child.DeletionTimestamp == nil {
		if err := c.children.Delete(ctx, child.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fail("HostProcess %s is Lost (%s) and deleting it failed: %v", child.Name, child.Status.LostReason, err)
		}
		log.Info("the server's HostProcess was Lost; it is placed again on the same storage", "hostprocess", child.Name,
			"reason", child.Status.LostReason)
		return starting("HostProcess %s was Lost (%s); placing it again from its storage", child.Name, child.Status.LostReason)
	}
	switch child.Status.Phase {
	case v1alpha1.HostProcessPhaseFailed, v1alpha1.HostProcessPhaseLost:
		return fail("HostProcess %s is %s %s: %s", child.Name, child.Status.Phase, child.Status.LostReason, child.Status.Message)
	case v1alpha1.HostProcessPhaseRunning:
	default:
		return starting("HostProcess %s is %q: %s", child.Name, child.Status.Phase, child.Status.Message)
	}
	if len(child.Status.Listeners) == 0 {
		return starting("HostProcess %s runs and reports no bound address yet", child.Name)
	}
	if child.Status.RestartKey != key {
		return starting("HostProcess %s restarts in place for its new config (restartKey %s)", child.Name, key)
	}
	addr := "https://" + child.Status.Listeners[0].Address
	tlsConf, err := tlsm.ClientTLSConfig()
	if err != nil {
		return fail("%v", err)
	}
	pool := tlsConf.RootCAs
	if o.Spec.TLS != nil {
		raw, err := os.ReadFile(o.Spec.TLS.CAFile)
		if err != nil {
			return fail("spec.tls.caFile: %v", err)
		}
		if pool, err = openbao.CertPoolFromPEM(raw); err != nil {
			return fail("spec.tls.caFile %s: %v", o.Spec.TLS.CAFile, err)
		}
	}
	client := openbao.New(addr, "", openbao.WithCACertPool(pool), openbao.WithClientCertificate(tlsConf.Certificates[0]))
	h, err := client.Health(ctx)
	if err != nil {
		return starting("the server at %s does not answer yet: %v", addr, err)
	}
	st.Address = addr
	keysPath := rootvault.UnsealKeysPath(dir)
	instance := ""
	if st.Runtime != nil {
		instance = st.Runtime.Instance
	}
	if !h.Initialized {
		if _, err := os.Lstat(keysPath); err == nil {
			return fail("the storage at %s is not initialized and unseal keys already exist at %s: refusing to initialize over them",
				rootvault.StorageDir(dir), keysPath)
		}
		res, err := client.Init(ctx, shares, threshold)
		if err != nil {
			return fail("init: %v", err)
		}
		k := UnsealKeys{Threshold: threshold, Shares: shares, Keys: res.KeysBase64}
		if err := WriteUnsealKeys(keysPath, k); err != nil {
			return fail("initialized, and writing the unseal keys to %s failed (%v): the storage at %s cannot be unsealed again",
				keysPath, err, rootvault.StorageDir(dir))
		}
		log.Info("initialized; unseal keys written to the state dir (0600), never to kcp", "shares", shares, "threshold", threshold,
			"keys", st.Refs.UnsealKeys)
		if err := unseal(ctx, client, k); err != nil {
			return fail("%v", err)
		}
		st.UnsealedInstance, st.UnsealedAt = instance, time.Now().UTC().Format(time.RFC3339Nano)
		root := client.WithToken(res.RootToken)
		s, serr := syncWith(ctx, root, layout)
		s.how = "the initial root token"
		if err := root.RevokeSelf(ctx); err != nil {
			log.Warn("revoking the initial root token", "err", err.Error())
		} else {
			log.Info("the initial root token is revoked; the core logs in with its client certificate")
		}
		recordSync(&st, gen, s, serr, log)
		recordTidy(&st, s, tidyEvery, log)
		layout.tidyDue = tidyDue(st.Tidy, tidyEvery, time.Now())
	} else if h.Sealed {
		k, err := ReadUnsealKeys(keysPath)
		if err != nil {
			return fail("sealed: %v", err)
		}
		if err := unseal(ctx, client, k); err != nil {
			return fail("%v", err)
		}
		st.UnsealedInstance, st.UnsealedAt = instance, time.Now().UTC().Format(time.RFC3339Nano)
		log.Info("unsealed", "instance", instance, "address", addr)
	}
	s, serr := c.syncCore(ctx, client, keysPath, layout)
	recordSync(&st, gen, s, serr, log)
	recordTidy(&st, s, tidyEvery, log)
	if len(grants) > 0 {
		reason := ""
		switch {
		case serr != nil:
			reason = serr.Error()
		case s.report.Err != nil:
			reason = s.report.Err.Error()
		case len(s.pending) > 0 || len(s.report.Conflicts) > 0:
			reason = "the server does not hold its declaration yet"
		}
		c.recordGrants(ctx, s.roles, s.refused, grants, reason == "", reason)
	}
	if len(plugins) > 0 {
		perr := serr
		if perr == nil {
			perr = s.report.Err
		}
		st.Plugins = pluginStatus(plugins, s.running, perr)
		if perr == nil {
			if removed := prunePlugins(dir, plugins); len(removed) > 0 {
				log.Info("removed plugin binaries no catalog entry runs", "files", removed)
			}
		}
	} else {
		st.Plugins = nil
	}
	h, err = client.Health(ctx)
	if err != nil {
		return starting("the server at %s stopped answering: %v", addr, err)
	}
	st.Initialized, st.Sealed = h.Initialized, h.Sealed
	conds.Set(&st.Conditions, gen, ConditionInitialized, h.Initialized, "Health", "")
	conds.Set(&st.Conditions, gen, ConditionUnsealed, !h.Sealed, "Health", "")
	if !h.Initialized || h.Sealed {
		st.Phase, st.Message = v1alpha1.OpenBaoServerPhaseSealed, "sealed"
		conds.Set(&st.Conditions, gen, ConditionReady, false, "Sealed", st.Message)
		return st
	}
	st.Phase, st.Message = v1alpha1.OpenBaoServerPhaseReady, "initialized and unsealed"
	conds.Set(&st.Conditions, gen, ConditionReady, true, "Ready", st.Message)
	return st
}

func (c *Controller) writeStatus(ctx context.Context, cur *unstructured.Unstructured, o v1alpha1.OpenBaoServer, st v1alpha1.OpenBaoServerStatus) error {
	o.Status = st
	u, err := unstruct.To(&o, "OpenBaoServer")
	if err != nil {
		return err
	}
	if fmt.Sprint(cur.Object["status"]) == fmt.Sprint(u.Object["status"]) {
		return nil
	}
	u.SetResourceVersion(cur.GetResourceVersion())
	_, err = c.servers.UpdateStatus(ctx, u, metav1.UpdateOptions{})
	return err
}

func (c *Controller) sweep(ctx context.Context, live map[string]bool) {
	list, err := c.children.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel})
	if err != nil {
		return
	}
	for _, it := range list.Items {
		owner := it.GetLabels()[OwnerLabel]
		if live[owner] {
			continue
		}
		if err := c.children.Delete(ctx, it.GetName(), metav1.DeleteOptions{}); err == nil {
			c.c.log().Info("the OpenBaoServer is gone; its HostProcess is deleted and the supervisor stops the unit; the storage stays",
				"openbaoserver", owner, "hostprocess", it.GetName())
		}
	}
}

func (c *Controller) Pass(ctx context.Context) error {
	list, err := c.servers.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for i := range list.Items {
		cur := &list.Items[i]
		o, err := toServer(cur)
		if err != nil {
			c.c.log().Warn("decoding", "name", cur.GetName(), "err", err.Error())
			continue
		}
		if o.DeletionTimestamp != nil {
			continue
		}
		live[o.Name] = true
		pctx, cancel := context.WithTimeout(ctx, PassTimeout)
		st := c.reconcile(pctx, o)
		cancel()
		if err := c.writeStatus(ctx, cur, o, st); err != nil && !apierrors.IsConflict(err) {
			c.c.log().Warn("writing status", "name", o.Name, "err", err.Error())
		}
	}
	c.sweep(ctx, live)
	return nil
}

func (c *Controller) Run(ctx context.Context) error {
	interval := c.c.Interval
	interval = cmp.Or(interval, DefaultInterval)
	c.c.log().Info("the OpenBaoServer controller runs in the core", "workspace", c.c.Workspace, "stateDir", c.c.StateDir)
	t := time.NewTicker(interval)
	defer t.Stop()
	kick := make(chan struct{}, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wg.Go(func() { c.watchSpecs(ctx, kick) })
	wg.Go(func() { c.watchGrants(ctx, kick) })
	for {
		if err := c.Pass(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-kick:
		}
	}
}

func (c *Controller) watchSpecs(ctx context.Context, kick chan<- struct{}) {
	seen := map[string]int64{}
	for ctx.Err() == nil {
		opened := time.Now()
		w, err := c.servers.Watch(ctx, metav1.ListOptions{})
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(RetryAfter):
			}
			continue
		}
		for ev := range w.ResultChan() {
			o, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			changed := ev.Type == watch.Deleted || seen[o.GetName()] != o.GetGeneration()
			seen[o.GetName()] = o.GetGeneration()
			if ev.Type == watch.Deleted {
				delete(seen, o.GetName())
			}
			if changed {
				select {
				case kick <- struct{}{}:
				default:
				}
			}
		}
		w.Stop()
		if time.Since(opened) < RetryAfter {
			select {
			case <-ctx.Done():
				return
			case <-time.After(RetryAfter):
			}
		}
	}
}

func Supervise(ctx context.Context, c Config) {
	log := c.log()
	supervise.Restart(ctx, "openbaoctl", supervise.Backoff{Min: RetryAfter, Max: RetryAfter, OnError: func(err error, after time.Duration) {
		log.Info("the OpenBaoServer controller waits; starting it again", "err", fmt.Sprint(err), "after", after.String())
	}}, func(ctx context.Context) error {
		ctl, err := New(c)
		if err != nil {
			return err
		}
		return ctl.Run(ctx)
	})
}
