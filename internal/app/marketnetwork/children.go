package marketnetwork

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/jsonx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcprest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/lease/local"
)

const (
	CoreAccount         = "app-core"
	CoreKubeconfig      = "parent.kubeconfig"
	CoreTokenTTL        = 30 * 24 * time.Hour
	CoreTokenRenewAfter = 7 * 24 * time.Hour
	ChildDeleteTimeout  = 2 * time.Minute
	ChildKillGrace      = 5 * time.Second
	ChildLockTimeout    = 2 * time.Minute

	StoppedMessage = "stopped: its core and its components are down; its workspace and state dir are kept and clearing spec.apps[].stopped starts them again from it"
)

var appName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type child struct {
	app    v1alpha1.NetworkApp
	id     string
	layout devinstance.Layout
}

func (ctl *Controller) layout() devinstance.Layout { return ctl.c.Config().Layout }

func (ctl *Controller) child(app v1alpha1.NetworkApp) (child, error) {
	if !appName.MatchString(app.Name) {
		return child{}, fmt.Errorf("apps[].name %q is not a DNS label", app.Name)
	}
	if err := checkAppName(app.Name); err != nil {
		return child{}, err
	}
	l := ctl.layout()
	id := l.ID + "-" + app.Name
	cl, err := devinstance.NewLayout(id, filepath.Join(l.StateDir, compose.AppsDir), filepath.Join(l.RuntimeDir, compose.AppsDir))
	if err != nil {
		return child{}, err
	}
	if _, err := devinstance.ID(id, "", nil); err != nil {
		return child{}, fmt.Errorf("the child instance id %s: %w", id, err)
	}
	return child{app: app, id: id, layout: cl}, nil
}

func (ctl *Controller) remote(ch child) (*hostprocess.Remote, error) {
	cfg := ctl.c.Config()
	return hostprocess.NewRemote(hostprocess.RemoteConfig{Kubeconfig: cfg.Kubeconfig, Workspace: devinstance.ChildWorkspace(ch.id),
		Reader: cfg.Manager, Log: ctl.log()})
}

func (ctl *Controller) coreCredential(ctx context.Context, r compose.Root, ch child) (string, error) {
	path := filepath.Join(ch.layout.StateDir, CoreKubeconfig)
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < CoreTokenRenewAfter {
		return path, nil
	}
	ws := devinstance.ChildWorkspace(ch.id)
	restCfg := ctl.c.RestConfig()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: CoreAccount, Namespace: "default"}}
	if err := ctl.applyRBAC(ctx, r, ws, serviceAccountGVR, sa, "ServiceAccount"); err != nil {
		return "", fmt.Errorf("the core's ServiceAccount in %s: %w", ws, err)
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: CoreAccount}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"hostprocesses", "hostprocesses/status"}, Verbs: []string{"*"}}}}
	if err := ctl.applyRBAC(ctx, r, ws, clusterRoleGVR, role, "ClusterRole"); err != nil {
		return "", fmt.Errorf("the core's ClusterRole in %s: %w", ws, err)
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: CoreAccount},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: CoreAccount},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: CoreAccount, Namespace: "default"}}}
	if err := ctl.applyRBAC(ctx, r, ws, clusterRoleBindingGVR, binding, "ClusterRoleBinding"); err != nil {
		return "", fmt.Errorf("the core's ClusterRoleBinding in %s: %w", ws, err)
	}
	cs, err := kubernetes.NewForConfig(kcprest.ForCluster(restCfg, ws))
	if err != nil {
		return "", err
	}
	secs := int64(CoreTokenTTL / time.Second)
	tr, err := cs.CoreV1().ServiceAccounts("default").CreateToken(ctx, CoreAccount,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &secs}}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("minting the core's token in %s: %w", ws, err)
	}
	kc := clientcmdapi.NewConfig()
	kc.Clusters["parent"] = &clientcmdapi.Cluster{Server: restCfg.Host, CertificateAuthorityData: restCfg.CAData, TLSServerName: "127.0.0.1"}
	kc.AuthInfos[CoreAccount] = &clientcmdapi.AuthInfo{Token: tr.Status.Token}
	kc.Contexts["parent"] = &clientcmdapi.Context{Cluster: "parent", AuthInfo: CoreAccount}
	kc.CurrentContext = "parent"
	if err := os.MkdirAll(ch.layout.StateDir, 0o700); err != nil {
		return "", err
	}
	if err := clientcmd.WriteToFile(*kc, path+".tmp"); err != nil {
		return "", err
	}
	os.Chmod(path+".tmp", 0o600)
	return path, os.Rename(path+".tmp", path)
}

func lockChild(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking); err == nil {
		return f, nil
	}
	got := make(chan error, 1)
	go func() { got <- hostos.Lock(f, hostos.LockExclusive) }()
	abandon := func() {
		go func() {
			<-got
			f.Close()
		}()
	}
	timer := time.NewTimer(ChildLockTimeout)
	defer timer.Stop()
	select {
	case err := <-got:
		if err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	case <-timer.C:
		abandon()
		return nil, fmt.Errorf("%s stayed locked", path)
	case <-ctx.Done():
		abandon()
		return nil, ctx.Err()
	}
}

func (ctl *Controller) coreLive(ctx context.Context, ch child) bool {
	var core devinstance.Core
	if fsx.ReadJSON(ch.layout.CoreRecord, &core) != nil || core.AdoptedAt.IsZero() {
		return false
	}
	st, err := ctl.c.Config().Manager.Inspect(ctx, ch.layout.CoreUnit())
	return err == nil && st.State == hostsvc.Active && st.InvocationID == core.InvocationID
}

func (ctl *Controller) placeChild(ctx context.Context, r compose.Root, ch child, gw *devinstance.GatewayChild) (devinstance.Record, error) {
	var rec devinstance.Record
	if fsx.ReadJSON(ch.layout.Record, &rec) == nil && rec.Ready && rec.SharedKcp != "" && ctl.coreLive(ctx, ch) && jsonx.Equal(rec.Gateway, gw) {
		return rec, nil
	}
	cfg := ctl.c.Config()
	if cfg.Record == nil {
		return rec, errors.New("marketnetwork: the core has no instance record to place children from")
	}
	parent, err := cfg.Record()
	if err != nil {
		return rec, err
	}
	for _, d := range []string{ch.layout.StateDir, ch.layout.RuntimeDir, ch.layout.Logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return rec, err
		}
	}
	if err := ctl.ensureAppWorkspace(ctx, r, ch.app.Name); err != nil {
		return rec, err
	}
	if err := devinstance.EnsureChildWorkspace(ctx, cfg.Kubeconfig, ch.id, devinstance.ChildWorkspaceReady); err != nil {
		return rec, err
	}
	kubeconfig, err := ctl.coreCredential(ctx, r, ch)
	if err != nil {
		return rec, err
	}
	lk, err := lockChild(ctx, ch.layout.Lock)
	if err != nil {
		return rec, err
	}
	defer lk.Close()
	if fsx.ReadJSON(ch.layout.Record, &rec) != nil {
		rec = devinstance.Record{Created: time.Now().UTC()}
	}
	rec.Layout, rec.Profile, rec.Backend, rec.Devsup = ch.layout, bootstrap.ProfileQuick, hostsvc.BackendProc, parent.Devsup
	rec.Executables, rec.SourceRoot, rec.BaseWorkspace = parent.Executables, parent.SourceRoot, ctl.appWorkspace(ch.app.Name)
	rec.SharedKcp, rec.AdminKubeconfig, rec.KcpURL, rec.KineURL, rec.Units = parent.Layout.StateDir, parent.AdminKubeconfig, parent.KcpURL, "", nil
	rec.OpenBao, rec.Host, rec.Worktree = nil, false, ""
	if rec.VM, err = ChildVM(ch.app, ch.layout, parent); err != nil {
		return rec, err
	}
	rec.Gateway = gw
	rec.Parent = &devinstance.Parent{ID: parent.Layout.ID, Socket: parent.Layout.Socket, Kubeconfig: kubeconfig,
		Workspace: devinstance.ChildWorkspace(ch.id)}
	remote, err := ctl.remote(ch)
	if err != nil {
		return rec, err
	}
	exe := parent.Executables["socialweb-computer"].Path
	bcfg := bootstrap.Config{Layout: ch.layout, Exe: exe, ReadyTimeout: cfg.ReadyTimeout, Log: ctl.log().With("child", ch.id)}
	started := time.Now()
	if err := bootstrap.Ensure(ctx, remote, bcfg, &rec); err != nil {
		return rec, fmt.Errorf("the bootstrap layer of %s: %w", ch.id, err)
	}
	if err := fsx.WriteJSON(ch.layout.Record, rec, 0o600); err != nil {
		return rec, err
	}
	spec, err := bcfg.CoreSpec(&rec)
	if err != nil {
		return rec, err
	}
	lk.Close()
	st, err := remote.Start(ctx, spec)
	if err != nil {
		return rec, fmt.Errorf("placing the core of %s: %w", ch.id, err)
	}
	if err := waitChildCore(ctx, ch.layout, st.InvocationID, cfg.ReadyTimeout); err != nil {
		return rec, err
	}
	fsx.ReadJSON(ch.layout.Record, &rec)
	ctl.log().Info("a child application runs in its workspace of the network's kcp", "child", ch.id, "kind", ch.app.Kind, "kcp", rec.KcpURL,
		"workspace", rec.BaseWorkspace, "seconds", time.Since(started).Seconds())
	return rec, nil
}

func waitChildCore(ctx context.Context, l devinstance.Layout, invocation string, timeout time.Duration) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	changed, err := local.Watch(wctx, l.CoreRecord)
	if err != nil {
		return fmt.Errorf("the core of %s: following its record %s: %w", l.ID, l.CoreRecord, err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		var core devinstance.Core
		if err := fsx.ReadJSON(l.CoreRecord, &core); err == nil && core.InvocationID == invocation && !core.AdoptedAt.IsZero() {
			return nil
		}
		select {
		case _, ok := <-changed:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("the core of %s: the watch of its record %s closed", l.ID, l.CoreRecord)
			}
		case <-timer.C:
			b, _ := os.ReadFile(filepath.Join(l.Logs, "core.log"))
			return fmt.Errorf("the core of %s (invocation %s) did not record its adoption within %s\n%s", l.ID, invocation, timeout, tail(b, 2048))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

func childWorkspace(rec devinstance.Record) string {
	return cmp.Or(rec.BaseWorkspace, compose.DefaultWorkspace)
}

func (ctl *Controller) applyRoot(ctx context.Context, r compose.Root, rec devinstance.Record, kind v1alpha1.ApplicationKind, obj any) error {
	gvr, err := compose.GVRFor(kind)
	if err != nil {
		return err
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := kube.Doc(m)
	u["apiVersion"], u["kind"] = v1alpha1.GroupVersion.String(), string(kind)
	meta := compose.DocMeta(u)
	labels := r.Labels(string(kind))
	if own, ok := meta["labels"].(map[string]any); ok {
		for k, v := range own {
			if s, ok := v.(string); ok {
				labels[k] = s
			}
		}
	}
	meta["labels"] = labels
	return ctl.c.Apply(ctx, r, childWorkspace(rec), gvr, u)
}

func (ctl *Controller) readRoot(ctx context.Context, rec devinstance.Record, kind v1alpha1.ApplicationKind) (v1alpha1.ApplicationStatus, bool, error) {
	gvr, err := compose.GVRFor(kind)
	if err != nil {
		return v1alpha1.ApplicationStatus{}, false, err
	}
	u, err := compose.Get[compose.Generic](ctx, ctl.c, childWorkspace(rec), gvr, compose.ChildRootName)
	if apierrors.IsNotFound(err) {
		return v1alpha1.ApplicationStatus{}, false, nil
	}
	if err != nil {
		return v1alpha1.ApplicationStatus{}, false, err
	}
	st, err := compose.ApplicationStatusOf(u)
	return st, true, err
}

func (ctl *Controller) liveUnits(ctx context.Context, ch child) ([]hostsvc.UnitStatus, error) {
	units, err := devinstance.ChildUnits(ctx, ctl.c.Config().Manager, ch.id)
	if err != nil {
		return nil, err
	}
	var live []hostsvc.UnitStatus
	for _, u := range units {
		if unitLive(u) {
			live = append(live, u)
		}
	}
	return live, nil
}

func unitLive(u hostsvc.UnitStatus) bool {
	return u.State == hostsvc.Active || u.State == hostsvc.Activating || u.State == hostsvc.Deactivating
}

func (ctl *Controller) awaitUnitsDown(ctx context.Context, ch child, timeout time.Duration) ([]hostsvc.UnitStatus, bool, error) {
	m := ctl.c.Config().Manager
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan hostsvc.UnitEvent)
	closed := make(chan struct{}, len(devinstance.Prefixes(ch.id)))
	for _, prefix := range devinstance.Prefixes(ch.id) {
		src, err := m.Watch(wctx, prefix)
		if err != nil {
			return nil, false, fmt.Errorf("following the units of %s: %w", ch.id, err)
		}
		go func() {
			for ev := range src {
				select {
				case events <- ev:
				case <-wctx.Done():
					return
				}
			}
			closed <- struct{}{}
		}()
	}
	units, err := devinstance.ChildUnits(wctx, m, ch.id)
	if err != nil {
		return nil, false, err
	}
	state := map[string]hostsvc.UnitStatus{}
	for _, u := range units {
		state[u.Name] = u
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		var live []hostsvc.UnitStatus
		for _, u := range state {
			if unitLive(u) {
				live = append(live, u)
			}
		}
		if len(live) == 0 {
			return nil, false, nil
		}
		slices.SortFunc(live, func(a, b hostsvc.UnitStatus) int { return strings.Compare(a.Name, b.Name) })
		select {
		case ev := <-events:
			if ev.Status.State == hostsvc.NotFound {
				delete(state, ev.Name)
				continue
			}
			st := ev.Status
			st.Name = cmp.Or(st.Name, ev.Name)
			state[ev.Name] = st
		case <-closed:
			if ctx.Err() != nil {
				return live, false, ctx.Err()
			}
			return live, false, fmt.Errorf("the watch of the units of %s closed with %d unit(s) still running", ch.id, len(live))
		case <-timer.C:
			return live, true, nil
		case <-ctx.Done():
			return live, false, ctx.Err()
		}
	}
}

func (ctl *Controller) stopChild(ctx context.Context, ch child) error {
	live, err := ctl.liveUnits(ctx, ch)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		KillUnder(ch.layout.StateDir, ChildKillGrace)
		return nil
	}
	log := ctl.log().With("child", ch.id)
	if err := ctl.stopAppComponents(ctx, ch.app.Name); err != nil {
		return err
	}
	if err := devinstance.DeleteChildWorkspace(ctx, ctl.c.Config().Kubeconfig, ch.id); err != nil {
		return err
	}
	live, timedOut, err := ctl.awaitUnitsDown(ctx, ch, ChildDeleteTimeout)
	if err != nil {
		return err
	}
	if timedOut {
		for _, u := range live {
			if err := ctl.c.Config().Manager.Stop(ctx, u.Name); err != nil {
				log.Warn("stopping a unit the supervisor left running", "unit", u.Name, "err", err.Error())
			}
		}
	}
	if pids := KillUnder(ch.layout.StateDir, ChildKillGrace); len(pids) > 0 {
		log.Warn("stopped processes of the child its units left behind", "pids", fmt.Sprint(pids))
	}
	if err := os.Remove(filepath.Join(ch.layout.StateDir, CoreKubeconfig)); err != nil && !os.IsNotExist(err) {
		return err
	}
	log.Info("a child application is stopped; its state dir is kept", "state", ch.layout.StateDir)
	return nil
}

func (ctl *Controller) teardownChild(ctx context.Context, ch child) error {
	log := ctl.log().With("child", ch.id)
	var rec devinstance.Record
	recErr := fsx.ReadJSON(ch.layout.Record, &rec)
	if rec.Gateway != nil {
		if err := ctl.deleteAccountWorkspaces(ctx); err != nil {
			log.Warn("the gateway's account workspaces did not go; its workspace is deleted anyway", "err", err.Error())
		}
	}
	if recErr == nil && rec.AdminKubeconfig != "" && ctl.coreLive(ctx, ch) {
		for _, kind := range []v1alpha1.ApplicationKind{v1alpha1.ApplicationKindMarketBidder, v1alpha1.ApplicationKindMarketRequester,
			v1alpha1.ApplicationKindGateway, v1alpha1.ApplicationKindSshGateway} {
			gvr, err := compose.GVRFor(kind)
			if err != nil {
				continue
			}
			ri, err := ctl.c.Client(childWorkspace(rec), gvr)
			if err != nil {
				continue
			}
			if err := ri.Delete(ctx, compose.ChildRootName, metav1.DeleteOptions{}); err != nil {
				continue
			}
			if err := ctl.waitGone(ctx, childWorkspace(rec), gvr, compose.ChildRootName, "the child's "+string(kind)); err != nil {
				log.Warn("the child's root object did not go within the timeout; its workspace is deleted anyway", "kind", kind, "err", err.Error())
			}
		}
	}
	if err := ctl.deleteAppWorkspace(ctx, ch.app.Name); err != nil {
		log.Warn("the child's workspace did not go; its core is stopped anyway", "workspace", ctl.appWorkspace(ch.app.Name), "err", err.Error())
	}
	if err := devinstance.DeleteChildWorkspace(ctx, ctl.c.Config().Kubeconfig, ch.id); err != nil {
		return err
	}
	live, timedOut, err := ctl.awaitUnitsDown(ctx, ch, ChildDeleteTimeout)
	if err != nil {
		return err
	}
	if timedOut {
		return fmt.Errorf("%d unit(s) of %s still run after its workspace was deleted", len(live), ch.id)
	}
	if pids := KillUnder(ch.layout.StateDir, ChildKillGrace); len(pids) > 0 {
		log.Warn("stopped processes of the child its units left behind", "pids", fmt.Sprint(pids))
	}
	err = errors.Join(assets.RemoveTree(ch.layout.StateDir), assets.RemoveTree(ch.layout.RuntimeDir))
	log.Info("a child application is gone: its root object, its workspace, its core and its state", "err", fmt.Sprint(err))
	return err
}

func (ctl *Controller) knownChildren() []string {
	entries, err := os.ReadDir(devinstance.DevRoot(filepath.Join(ctl.layout().StateDir, compose.AppsDir)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func (ctl *Controller) childByID(id string) child {
	l := ctl.layout()
	name := strings.TrimPrefix(id, l.ID+"-")
	ch, err := ctl.child(v1alpha1.NetworkApp{Name: name})
	if err != nil {
		cl, _ := devinstance.NewLayout(id, filepath.Join(l.StateDir, compose.AppsDir), filepath.Join(l.RuntimeDir, compose.AppsDir))
		return child{id: id, layout: cl}
	}
	return ch
}

func (ctl *Controller) teardownAll(ctx context.Context, _ compose.Root, keep []string) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	for _, id := range ctl.knownChildren() {
		if slices.Contains(keep, id) {
			continue
		}
		ch := ctl.childByID(id)
		wg.Go(func() {
			if err := ctl.teardownChild(ctx, ch); err != nil {
				mu.Lock()
				errs = append(errs, ch.id+": "+err.Error())
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("tearing children down: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (ctl *Controller) ensureApp(ctx context.Context, r compose.Root, app v1alpha1.NetworkApp, value v1alpha1.MarketNetworkValue, comms *v1alpha1.MarketComms,
	pdsURL, aliceURL string, bidderDids []string, suspend, placedOnly bool) v1alpha1.NetworkAppStatus {
	out := v1alpha1.NetworkAppStatus{Name: app.Name, Kind: app.Kind, Phase: v1alpha1.ApplicationPhaseProvisioning}
	fail := func(err error) v1alpha1.NetworkAppStatus {
		out.Message = err.Error()
		ctl.log().Warn("a child application is not ready yet", "app", app.Name, "err", err.Error())
		return out
	}
	ch, err := ctl.child(app)
	if err != nil {
		out.Phase = v1alpha1.ApplicationPhaseFailed
		return fail(err)
	}
	out.Instance = ch.id
	if app.Stopped {
		if err := ctl.stopChild(ctx, ch); err != nil {
			return fail(err)
		}
		out.Phase, out.Message = v1alpha1.ApplicationPhasePending, StoppedMessage
		return out
	}
	if _, err := compose.GVRFor(app.Kind); err != nil {
		out.Phase = v1alpha1.ApplicationPhaseFailed
		return fail(err)
	}
	if placedOnly {
		var rec devinstance.Record
		if fsx.ReadJSON(ch.layout.Record, &rec) != nil || rec.AdminKubeconfig == "" || !ctl.coreLive(ctx, ch) {
			out.Phase, out.Message = v1alpha1.ApplicationPhaseSuspended, compose.SuspendedMessage
			return out
		}
	}
	var gw *devinstance.GatewayChild
	if app.Kind == v1alpha1.ApplicationKindGateway {
		if gw, err = GatewayChildOf(ctx, ctl.c, r, ch.id, ch.layout.StateDir, app, value, aliceURL, bidderDids, suspend); err != nil {
			return fail(err)
		}
	}
	rec, err := ctl.placeChild(ctx, r, ch, gw)
	if err != nil {
		return fail(err)
	}
	out.KcpURL, out.Workspace = rec.KcpURL, rec.BaseWorkspace
	meta := metav1.ObjectMeta{Name: compose.ChildRootName}
	var root any
	switch app.Kind {
	case v1alpha1.ApplicationKindMarketBidder:
		var fc *v1alpha1.FirecrackerProvider
		if app.Firecracker != nil {
			f := *app.Firecracker
			fc = &f
		}
		var kcp *v1alpha1.KcpProvider
		if app.Kcp != nil {
			k := *app.Kcp
			k.Handle = KcpHandle(app)
			kcp = &k
			fc = nil
		}
		root = &v1alpha1.MarketBidder{ObjectMeta: meta, Spec: v1alpha1.MarketBidderSpec{Profile: app.Profile, Suspend: suspend, Network: &value,
			Identity: v1alpha1.ApplicationIdentity{Handle: app.Name + "." + BidderAccountDomain, PdsURL: pdsURL},
			Policy:   app.Policy, Firecracker: fc, Kcp: kcp, Options: app.Options, Quota: app.Quota, Comms: comms, Components: app.Components,
			BidderDids: append([]string(nil), bidderDids...)}}
	case v1alpha1.ApplicationKindMarketRequester:
		root = &v1alpha1.MarketRequester{ObjectMeta: meta, Spec: v1alpha1.MarketRequesterSpec{Profile: app.Profile, Suspend: suspend, Network: &value,
			Identity:   v1alpha1.ApplicationIdentity{Association: v1alpha1.AssociationSkip},
			BidderDids: bidderDids, Options: app.Options, Quota: app.Quota, Comms: comms, Components: app.Components}}
	case v1alpha1.ApplicationKindSshGateway:
		root = &v1alpha1.SshGateway{ObjectMeta: meta, Spec: v1alpha1.SshGatewaySpec{Profile: app.Profile, Suspend: suspend, Network: &value,
			BidderDids: bidderDids, Options: app.Options, Quota: app.Quota, OwnerSessionsFile: app.OwnerSessionsFile, Comms: comms, Components: app.Components}}
	}
	if root != nil {
		if err := ctl.applyRoot(ctx, r, rec, app.Kind, root); err != nil {
			return fail(fmt.Errorf("applying the root object into %s's workspace %s: %w", ch.id, rec.BaseWorkspace, err))
		}
	}
	rs, found, err := ctl.readRoot(ctx, rec, app.Kind)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(fmt.Errorf("the root object of %s is not readable yet", ch.id))
	}
	out.Phase, out.Message, out.Did, out.Components, out.Listeners = rs.Phase, rs.Message, rs.Did, rs.Components, rs.Listeners
	out.Phase = cmp.Or(out.Phase, v1alpha1.ApplicationPhasePending)
	return out
}
