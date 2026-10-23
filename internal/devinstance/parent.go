package devinstance

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
)

const (
	HostID              = "dev-host"
	HostSet             = "dev-host"
	DevWorkspace        = "root:dev"
	ChildWorkspaceType  = "dev-instance"
	HostInterval        = time.Second
	HostApplyTimeout    = time.Minute
	ChildWorkspaceReady = 30 * time.Second
)

const hostProcessExport = "hostprocesses"

var sweptKinds = []string{hostsvc.NamePrefix + "kcp-", hostsvc.NamePrefix + "core-", hostsvc.NamePrefix + "hp-"}

type Parent struct {
	ID         string `json:"id"`
	Socket     string `json:"socket"`
	Kubeconfig string `json:"kubeconfig"`
	Workspace  string `json:"workspace"`
}

func ChildWorkspace(id string) string { return DevWorkspace + ":" + id }

func Prefixes(id string) []string {
	n := table.DevNames(Short(id))
	return []string{n.UnitPrefix(), n.CorePrefix(), n.HostProcessPrefix()}
}

func clusterClient(kubeconfig, workspace string) (*kube.Client, error) {
	restCfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(restCfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + workspace
	return kube.New(cfg)
}

func workspacePhase(w *tenancyv1alpha1.Workspace) string {
	return string(w.Status.Phase)
}

func WaitHost(ctx context.Context, kubeconfig string, timeout time.Duration) error {
	parent, name, _ := strings.Cut(DevWorkspace, ":")
	root, err := clusterClient(kubeconfig, parent)
	if err != nil {
		return err
	}
	dc, err := clusterClient(kubeconfig, DevWorkspace)
	if err != nil {
		return err
	}
	_, err = kube.Await(ctx, root.Resource(kcpgvr.Workspaces), name, timeout, func(w *tenancyv1alpha1.Workspace) (bool, error) {
		return w != nil && workspacePhase(w) == "Ready", nil
	})
	if err == nil {
		_, err = kube.List[tenancyv1alpha1.Workspace](ctx, dc.Resource(kcpgvr.Workspaces), metav1.ListOptions{Limit: 1})
	}
	if err != nil {
		return fmt.Errorf("devinstance: the dev host's %s did not serve workspaces within %s: %w", DevWorkspace, timeout, err)
	}
	return nil
}

func EnsureChildWorkspace(ctx context.Context, kubeconfig, id string, timeout time.Duration) error {
	if err := WaitHost(ctx, kubeconfig, timeout); err != nil {
		return err
	}
	dc, err := clusterClient(kubeconfig, DevWorkspace)
	if err != nil {
		return err
	}
	ws := dc.Resource(kcpgvr.Workspaces)
	o := kube.Doc{
		"apiVersion": "tenancy.kcp.io/v1alpha1", "kind": "Workspace",
		"metadata": map[string]any{"name": id, "annotations": map[string]any{kcpserver.WorkspaceOwnerAnnotation: kcpserver.ShardAdminOwner}},
		"spec":     map[string]any{"type": map[string]any{"name": ChildWorkspaceType, "path": "root"}},
	}
	if _, err := kube.Create[kube.Doc](ctx, ws, o, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("devinstance: creating workspace %s in the parent: %w", ChildWorkspace(id), err)
	}
	child, err := clusterClient(kubeconfig, ChildWorkspace(id))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cur, err := kube.Await(ctx, ws, id, 0, func(w *tenancyv1alpha1.Workspace) (bool, error) {
		if w != nil && w.DeletionTimestamp != nil {
			return false, fmt.Errorf("devinstance: workspace %s is being deleted in the parent; wait for it to go, then dev up again", ChildWorkspace(id))
		}
		return w != nil && workspacePhase(w) == "Ready", nil
	})
	phase := ""
	if cur != nil {
		phase = workspacePhase(cur)
	}
	if err == nil {
		err = awaitBound(ctx, child.Resource(kcpgvr.APIBindings), hostProcessExport)
	}
	if err == nil {
		_, err = kube.List[v1alpha1.HostProcess](ctx, child.Resource(hostprocess.GVR), metav1.ListOptions{Limit: 1})
	}
	if err != nil {
		return fmt.Errorf("devinstance: workspace %s did not become Ready with the HostProcess API bound within %s (phase %q): %w", ChildWorkspace(id), timeout, phase, err)
	}
	return nil
}

func awaitBound(ctx context.Context, bindings kube.Resource, export string) error {
	_, err := kube.AwaitList(ctx, bindings, metav1.ListOptions{}, 0, func(items []apisv1alpha2.APIBinding) (bool, error) {
		for _, b := range items {
			ref := b.Spec.Reference.Export
			if ref != nil && ref.Name == export && b.Status.Phase == apisv1alpha2.APIBindingPhaseBound {
				return true, nil
			}
		}
		return false, nil
	})
	return err
}

func DeleteChildWorkspace(ctx context.Context, kubeconfig, id string) error {
	dc, err := clusterClient(kubeconfig, DevWorkspace)
	if err != nil {
		return err
	}
	err = dc.Resource(kcpgvr.Workspaces).Delete(ctx, id, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("devinstance: deleting workspace %s in the parent: %w", ChildWorkspace(id), err)
	}
	return nil
}

func ChildUnits(ctx context.Context, m hostsvc.Manager, id string) ([]hostsvc.UnitStatus, error) {
	var out []hostsvc.UnitStatus
	for _, p := range Prefixes(id) {
		units, err := m.List(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, units...)
	}
	return out, nil
}

func OpenRemote(ctx context.Context, p Parent, log *slog.Logger) (*hostprocess.Remote, error) {
	reader, err := hostsvc.Open(ctx, hostsvc.Config{Backend: hostsvc.BackendProc, ProcSocket: p.Socket})
	if err != nil {
		return nil, fmt.Errorf("devinstance: the parent %s devsup on %s: %w", p.ID, p.Socket, err)
	}
	r, err := hostprocess.NewRemote(hostprocess.RemoteConfig{Kubeconfig: p.Kubeconfig, Workspace: p.Workspace, Reader: reader, Log: log})
	if err != nil {
		reader.Close()
		return nil, err
	}
	return r, nil
}

type HostConfig struct {
	Kubeconfig string
	Manager    hostsvc.Manager
	Own        []string
	Resolve    func(v1alpha1.HostProcessAsset) (string, error)
	Log        *slog.Logger
}

type host struct {
	c        HostConfig
	store    *kube.Cache[tenancyv1alpha1.Workspace]
	children map[string]context.CancelFunc
	wg       sync.WaitGroup
}

func applyHostSet(ctx context.Context, c HostConfig) error {
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return err
	}
	ap, err := kcpapply.New(restCfg, kcpapply.Options{WaitTimeout: HostApplyTimeout, Log: c.Log})
	if err != nil {
		return err
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	set, ok := ix.Set(HostSet)
	if !ok {
		return fmt.Errorf("devinstance: deploy/sets.yaml names no %s set", HostSet)
	}
	values, err := manifests.DecodeValues(set.Values, nil)
	if err != nil {
		return err
	}
	_, err = ap.Apply(ctx, set, values)
	return err
}

func RunHost(ctx context.Context, c HostConfig) {
	log := c.Log.With("component", "dev-host")
	for {
		err := applyHostSet(ctx, c)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("applying the dev-host set; retrying", "err", err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(hostprocess.RetryAfter):
		}
	}
	dc, err := clusterClient(c.Kubeconfig, DevWorkspace)
	if err != nil {
		log.Error("the root:dev client", "err", err.Error())
		return
	}
	kick := make(chan struct{}, 1)
	nudge := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	informer, err := kube.NewCache[tenancyv1alpha1.Workspace](dc.Resource(kcpgvr.Workspaces), metav1.ListOptions{}, nudge)
	if err != nil {
		log.Error("the root:dev workspace watch", "err", err.Error())
		return
	}
	h := &host{c: c, store: informer, children: map[string]context.CancelFunc{}}
	var watches sync.WaitGroup
	wctx, stopWatches := context.WithCancel(ctx)
	watches.Go(func() { informer.Run(wctx) })
	for _, k := range sweptKinds {
		watches.Go(func() { followUnits(wctx, c.Manager, k, nudge, log) })
	}
	defer func() {
		stopWatches()
		watches.Wait()
	}()
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return
	}
	log.Info("the dev host places its children's HostProcess objects", "workspace", DevWorkspace)
	retry := time.NewTimer(0)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, cancel := range h.children {
				cancel()
			}
			h.wg.Wait()
			return
		case <-kick:
		case <-retry.C:
		}
		if err := h.pass(ctx, log); err != nil {
			log.Warn("a dev-host pass failed; children keep running", "err", err.Error())
			retry.Reset(HostInterval)
		}
	}
}

func followUnits(ctx context.Context, m hostsvc.Manager, prefix string, changed func(), log *slog.Logger) {
	for ctx.Err() == nil {
		events, err := m.Watch(ctx, prefix)
		if err != nil {
			log.Warn("watching the units a dev host sweeps", "prefix", prefix, "err", err.Error())
		} else {
			changed()
			for range events {
				changed()
			}
		}
		if !clock.Sleep(ctx, hostprocess.RetryAfter) {
			return
		}
	}
}

func (h *host) pass(ctx context.Context, log *slog.Logger) error {
	keep := append([]string(nil), h.c.Own...)
	live := map[string]bool{}
	for _, w := range h.store.List() {
		id := w.Name
		if w.DeletionTimestamp != nil {
			continue
		}
		live[id] = true
		keep = append(keep, Prefixes(id)...)
		if _, ok := h.children[id]; ok || workspacePhase(&w) != "Ready" {
			continue
		}
		cctx, cancel := context.WithCancel(ctx)
		h.children[id] = cancel
		childCfg := hostprocess.Config{Kubeconfig: h.c.Kubeconfig, Workspace: ChildWorkspace(id), Manager: h.c.Manager,
			Prefixes: Prefixes(id), Bound: true, Resolve: h.c.Resolve, Log: log.With("child", id)}
		h.wg.Go(func() { hostprocess.Supervise(cctx, childCfg) })
		log.Info("a child instance's workspace is supervised", "child", id, "workspace", ChildWorkspace(id))
	}
	for id, cancel := range h.children {
		if !live[id] {
			cancel()
			delete(h.children, id)
			log.Info("a child instance's workspace is gone; stopping its processes", "child", id)
		}
	}
	return h.sweep(ctx, keep, log)
}

func kept(name string, keep []string) bool {
	for _, p := range keep {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (h *host) sweep(ctx context.Context, keep []string, log *slog.Logger) error {
	var gone []hostsvc.UnitStatus
	for _, k := range sweptKinds {
		units, err := h.c.Manager.List(ctx, k)
		if err != nil {
			return err
		}
		for _, u := range units {
			if !kept(u.Name, keep) && (u.State == hostsvc.Active || u.State == hostsvc.Activating || u.State == hostsvc.Failed) {
				gone = append(gone, u)
			}
		}
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i].ActiveSince.After(gone[j].ActiveSince) })
	for _, u := range gone {
		if err := h.c.Manager.Stop(ctx, u.Name); err != nil {
			log.Warn("stopping a gone child's unit", "unit", u.Name, "err", err.Error())
			continue
		}
		h.c.Manager.ResetFailed(ctx, u.Name)
		log.Info("stopped a unit whose child workspace is gone", "unit", u.Name, "pid", u.MainPID)
	}
	return nil
}
