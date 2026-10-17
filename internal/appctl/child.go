package appctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const (
	AppsDir             = "apps"
	CoreAccount         = "app-core"
	CoreKubeconfig      = "parent.kubeconfig"
	CoreTokenTTL        = 30 * 24 * time.Hour
	CoreTokenRenewAfter = 7 * 24 * time.Hour
	ChildRootName       = "app"
	ChildDeleteTimeout  = 2 * time.Minute
	ChildPoll           = 200 * time.Millisecond
)

var appName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type child struct {
	app    v1alpha1.NetworkApp
	id     string
	layout devinstance.Layout
}

func (n *networks) child(app v1alpha1.NetworkApp) (child, error) {
	if !appName.MatchString(app.Name) {
		return child{}, fmt.Errorf("apps[].name %q is not a DNS label", app.Name)
	}
	if err := checkAppName(app.Name); err != nil {
		return child{}, err
	}
	id := n.ctl.c.Layout.ID + "-" + app.Name
	l, err := devinstance.NewLayout(id, filepath.Join(n.ctl.c.Layout.StateDir, AppsDir), filepath.Join(n.ctl.c.Layout.RuntimeDir, AppsDir))
	if err != nil {
		return child{}, err
	}
	if _, err := devinstance.ID(id, "", nil); err != nil {
		return child{}, fmt.Errorf("the child instance id %s: %w", id, err)
	}
	return child{app: app, id: id, layout: l}, nil
}

func (n *networks) remote(c child) (*hostprocess.Remote, error) {
	return hostprocess.NewRemote(hostprocess.RemoteConfig{Kubeconfig: n.ctl.c.Kubeconfig, Workspace: devinstance.ChildWorkspace(c.id),
		Reader: n.ctl.c.Manager, Log: n.ctl.c.log()})
}

func (n *networks) coreCredential(ctx context.Context, c child) (string, error) {
	path := filepath.Join(c.layout.StateDir, CoreKubeconfig)
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < CoreTokenRenewAfter {
		return path, nil
	}
	ws := devinstance.ChildWorkspace(c.id)
	cs, err := kubernetes.NewForConfig(clusterConfig(n.ctl.restCfg, ws))
	if err != nil {
		return "", err
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: CoreAccount, Namespace: "default"}}
	if _, err := cs.CoreV1().ServiceAccounts("default").Create(ctx, sa, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("the core's ServiceAccount in %s: %w", ws, err)
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: CoreAccount}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"hostprocesses", "hostprocesses/status"}, Verbs: []string{"*"}}}}
	if _, err := cs.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("the core's ClusterRole in %s: %w", ws, err)
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: CoreAccount},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: CoreAccount},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: CoreAccount, Namespace: "default"}}}
	if _, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("the core's ClusterRoleBinding in %s: %w", ws, err)
	}
	secs := int64(CoreTokenTTL / time.Second)
	tr, err := cs.CoreV1().ServiceAccounts("default").CreateToken(ctx, CoreAccount,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &secs}}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("minting the core's token in %s: %w", ws, err)
	}
	kc := clientcmdapi.NewConfig()
	kc.Clusters["parent"] = &clientcmdapi.Cluster{Server: n.ctl.restCfg.Host, CertificateAuthorityData: n.ctl.restCfg.CAData, TLSServerName: "127.0.0.1"}
	kc.AuthInfos[CoreAccount] = &clientcmdapi.AuthInfo{Token: tr.Status.Token}
	kc.Contexts["parent"] = &clientcmdapi.Context{Cluster: "parent", AuthInfo: CoreAccount}
	kc.CurrentContext = "parent"
	if err := os.MkdirAll(c.layout.StateDir, 0o700); err != nil {
		return "", err
	}
	if err := clientcmd.WriteToFile(*kc, path+".tmp"); err != nil {
		return "", err
	}
	os.Chmod(path+".tmp", 0o600)
	return path, os.Rename(path+".tmp", path)
}

func lockChild(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking); err == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%s stayed locked", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (n *networks) coreLive(ctx context.Context, c child) bool {
	var core devinstance.Core
	if devinstance.ReadJSON(c.layout.CoreRecord, &core) != nil || core.AdoptedAt.IsZero() {
		return false
	}
	st, err := n.ctl.c.Manager.Inspect(ctx, c.layout.CoreUnit())
	return err == nil && st.State == hostsvc.Active && st.InvocationID == core.InvocationID
}

func (n *networks) placeChild(ctx context.Context, c child, gw *devinstance.GatewayChild) (devinstance.Record, error) {
	var rec devinstance.Record
	if devinstance.ReadJSON(c.layout.Record, &rec) == nil && rec.Ready && rec.SharedKcp != "" && n.coreLive(ctx, c) && sameSpec(rec.Gateway, gw) {
		return rec, nil
	}
	parent, err := n.ctl.c.Record()
	if err != nil {
		return rec, err
	}
	for _, d := range []string{c.layout.StateDir, c.layout.RuntimeDir, c.layout.Logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return rec, err
		}
	}
	if err := n.ensureAppWorkspace(ctx, c.app.Name); err != nil {
		return rec, err
	}
	if err := devinstance.EnsureChildWorkspace(ctx, n.ctl.c.Kubeconfig, c.id, devinstance.ChildWorkspaceReady); err != nil {
		return rec, err
	}
	kubeconfig, err := n.coreCredential(ctx, c)
	if err != nil {
		return rec, err
	}
	lk, err := lockChild(c.layout.Lock)
	if err != nil {
		return rec, err
	}
	defer lk.Close()
	if devinstance.ReadJSON(c.layout.Record, &rec) != nil {
		rec = devinstance.Record{Created: time.Now().UTC()}
	}
	rec.Layout, rec.Profile, rec.Backend, rec.Devsup = c.layout, bootstrap.ProfileQuick, hostsvc.BackendProc, parent.Devsup
	rec.Executables, rec.SourceRoot, rec.BaseWorkspace = parent.Executables, parent.SourceRoot, n.appWorkspace(c.app.Name)
	rec.SharedKcp, rec.AdminKubeconfig, rec.KcpURL, rec.KineURL, rec.Units = parent.Layout.StateDir, parent.AdminKubeconfig, parent.KcpURL, "", nil
	rec.OpenBao, rec.Host, rec.Worktree = nil, false, ""
	if rec.VM, err = ChildVM(c.app, c.layout, parent); err != nil {
		return rec, err
	}
	rec.Gateway = gw
	rec.Parent = &devinstance.Parent{ID: parent.Layout.ID, Socket: parent.Layout.Socket, Kubeconfig: kubeconfig,
		Workspace: devinstance.ChildWorkspace(c.id)}
	remote, err := n.remote(c)
	if err != nil {
		return rec, err
	}
	exe := parent.Executables["socialweb-computer"].Path
	bcfg := bootstrap.Config{Layout: c.layout, Exe: exe, ReadyTimeout: n.ctl.c.ReadyTimeout, Log: n.ctl.c.log().With("child", c.id)}
	started := time.Now()
	if err := bootstrap.Ensure(ctx, remote, bcfg, &rec); err != nil {
		return rec, fmt.Errorf("the bootstrap layer of %s: %w", c.id, err)
	}
	if err := devinstance.WriteJSON(c.layout.Record, rec); err != nil {
		return rec, err
	}
	spec, err := bcfg.CoreSpec(&rec)
	if err != nil {
		return rec, err
	}
	lk.Close()
	st, err := remote.Start(ctx, spec)
	if err != nil {
		return rec, fmt.Errorf("placing the core of %s: %w", c.id, err)
	}
	if err := waitChildCore(ctx, c.layout, st.InvocationID, n.ctl.c.ReadyTimeout); err != nil {
		return rec, err
	}
	devinstance.ReadJSON(c.layout.Record, &rec)
	n.ctl.c.log().Info("a child application runs in its workspace of the network's kcp", "child", c.id, "kind", c.app.Kind, "kcp", rec.KcpURL,
		"workspace", rec.BaseWorkspace, "seconds", time.Since(started).Seconds())
	return rec, nil
}

func waitChildCore(ctx context.Context, l devinstance.Layout, invocation string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var c devinstance.Core
		if err := devinstance.ReadJSON(l.CoreRecord, &c); err == nil && c.InvocationID == invocation && !c.AdoptedAt.IsZero() {
			return nil
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(l.Logs, "core.log"))
			return fmt.Errorf("the core of %s (invocation %s) did not record its adoption within %s\n%s", l.ID, invocation, timeout, tail(b, 2048))
		}
		if !sleep(ctx, ChildPoll) {
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

func childClient(rec devinstance.Record, kind v1alpha1.ApplicationKind) (dynamic.ResourceInterface, error) {
	gvr, err := GVRFor(kind)
	if err != nil {
		return nil, err
	}
	restCfg, err := kcpserver.FrontRestConfig(rec.AdminKubeconfig)
	if err != nil {
		return nil, err
	}
	ws := rec.BaseWorkspace
	if ws == "" {
		ws = DefaultWorkspace
	}
	dc, err := dynamic.NewForConfig(clusterConfig(restCfg, ws))
	if err != nil {
		return nil, err
	}
	return dc.Resource(gvr), nil
}

func applyRoot(ctx context.Context, rec devinstance.Record, kind v1alpha1.ApplicationKind, obj any) error {
	ri, err := childClient(rec, kind)
	if err != nil {
		return err
	}
	u, err := toUnstructured(obj, string(kind))
	if err != nil {
		return err
	}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = ri.Patch(ctx, u.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

func readRoot(ctx context.Context, rec devinstance.Record, kind v1alpha1.ApplicationKind) (v1alpha1.ApplicationStatus, bool, error) {
	ri, err := childClient(rec, kind)
	if err != nil {
		return v1alpha1.ApplicationStatus{}, false, err
	}
	u, err := ri.Get(ctx, ChildRootName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return v1alpha1.ApplicationStatus{}, false, nil
	}
	if err != nil {
		return v1alpha1.ApplicationStatus{}, false, err
	}
	m, _, _ := unstructured.NestedMap(u.Object, "status")
	var st v1alpha1.ApplicationStatus
	if err := fromUnstructured(&unstructured.Unstructured{Object: m}, &st); err != nil {
		return st, true, err
	}
	return st, true, nil
}

const StoppedMessage = "stopped: its core and its DenoPods are down; its workspace and state dir are kept and clearing spec.apps[].stopped starts them again from it"

func (n *networks) liveUnits(ctx context.Context, c child) ([]hostsvc.UnitStatus, error) {
	units, err := devinstance.ChildUnits(ctx, n.ctl.c.Manager, c.id)
	if err != nil {
		return nil, err
	}
	var live []hostsvc.UnitStatus
	for _, u := range units {
		if u.State == hostsvc.Active || u.State == hostsvc.Activating || u.State == hostsvc.Deactivating {
			live = append(live, u)
		}
	}
	return live, nil
}

func (n *networks) stopChild(ctx context.Context, c child) error {
	live, err := n.liveUnits(ctx, c)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		KillUnder(c.layout.StateDir, ChildPoll*25)
		return nil
	}
	log := n.ctl.c.log().With("child", c.id)
	if err := n.stopAppPods(ctx, c.app.Name); err != nil {
		return err
	}
	if err := devinstance.DeleteChildWorkspace(ctx, n.ctl.c.Kubeconfig, c.id); err != nil {
		return err
	}
	deadline := time.Now().Add(ChildDeleteTimeout)
	for len(live) > 0 {
		if time.Now().After(deadline) {
			for _, u := range live {
				if err := n.ctl.c.Manager.Stop(ctx, u.Name); err != nil {
					log.Warn("stopping a unit the supervisor left running", "unit", u.Name, "err", err.Error())
				}
			}
			break
		}
		if !sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
		if live, err = n.liveUnits(ctx, c); err != nil {
			return err
		}
	}
	if pids := KillUnder(c.layout.StateDir, ChildPoll*25); len(pids) > 0 {
		log.Warn("stopped processes of the child its units left behind", "pids", fmt.Sprint(pids))
	}
	if err := os.Remove(filepath.Join(c.layout.StateDir, CoreKubeconfig)); err != nil && !os.IsNotExist(err) {
		return err
	}
	log.Info("a child application is stopped; its state dir is kept", "state", c.layout.StateDir)
	return nil
}

func (n *networks) teardownChild(ctx context.Context, c child) error {
	log := n.ctl.c.log().With("child", c.id)
	var rec devinstance.Record
	if devinstance.ReadJSON(c.layout.Record, &rec) == nil && rec.AdminKubeconfig != "" && n.coreLive(ctx, c) {
		for _, kind := range []v1alpha1.ApplicationKind{v1alpha1.ApplicationKindMarketBidder, v1alpha1.ApplicationKindMarketRequester, v1alpha1.ApplicationKindGateway,
			v1alpha1.ApplicationKindSshGateway} {
			ri, err := childClient(rec, kind)
			if err != nil {
				continue
			}
			if err := ri.Delete(ctx, ChildRootName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				continue
			}
			deadline := time.Now().Add(ChildDeleteTimeout)
			for {
				_, err := ri.Get(ctx, ChildRootName, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					break
				}
				if time.Now().After(deadline) {
					log.Warn("the child's root object did not go within the timeout; its workspace is deleted anyway", "kind", kind)
					break
				}
				if !sleep(ctx, ChildPoll) {
					return ctx.Err()
				}
			}
		}
	}
	if err := n.deleteAppWorkspace(ctx, c.app.Name); err != nil {
		log.Warn("the child's workspace did not go; its core is stopped anyway", "workspace", n.appWorkspace(c.app.Name), "err", err.Error())
	}
	if err := devinstance.DeleteChildWorkspace(ctx, n.ctl.c.Kubeconfig, c.id); err != nil {
		return err
	}
	deadline := time.Now().Add(ChildDeleteTimeout)
	for {
		units, err := devinstance.ChildUnits(ctx, n.ctl.c.Manager, c.id)
		live := 0
		for _, u := range units {
			if u.State == hostsvc.Active || u.State == hostsvc.Activating || u.State == hostsvc.Deactivating {
				live++
			}
		}
		if err == nil && live == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d unit(s) of %s still run after its workspace was deleted", live, c.id)
		}
		if !sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
	}
	if pids := KillUnder(c.layout.StateDir, ChildPoll*25); len(pids) > 0 {
		log.Warn("stopped processes of the child its units left behind", "pids", fmt.Sprint(pids))
	}
	err := errors.Join(assets.RemoveTree(c.layout.StateDir), assets.RemoveTree(c.layout.RuntimeDir))
	log.Info("a child application is gone: its root object, its workspace, its core and its state", "err", fmt.Sprint(err))
	return err
}
