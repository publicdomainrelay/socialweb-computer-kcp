package vault

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const (
	Name = "vault"

	Export = "hostprocesses"

	Finalizer = "socialweb.computer/vault"

	KindOpenBaoServer = "OpenBaoServer"

	KindRemoteOpenBao = "RemoteOpenBao"
)

type Options struct {
	Log *slog.Logger

	Remote *openbao.Remote
}

type backendKey struct {
	cluster string

	kind string

	name string
}

type Controller struct {
	log *slog.Logger

	provider *openbao.Provider

	mu sync.Mutex

	bound map[backendKey]map[ctlruntime.Request]struct{}
}

func Register(m *ctlruntime.Manager, b *bus.Bus, opts Options) (*Controller, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctl := &Controller{log: log.With("component", "vault-controller"), provider: openbao.NewProvider(opts.Remote),
		bound: map[backendKey]map[ctlruntime.Request]struct{}{}}
	ctl.provider.Register(b)
	err := ctlruntime.For[*v1alpha1.Vault](m, Name, ctl, ctlruntime.Export(Export), ctlruntime.Finalizer(Finalizer),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.OpenBaoServer) []ctlruntime.Request {
			return ctl.vaultsOf(backendKey{cluster: src.Cluster, kind: KindOpenBaoServer, name: obj.Name})
		}),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.RemoteOpenBao) []ctlruntime.Request {
			return ctl.vaultsOf(backendKey{cluster: src.Cluster, kind: KindRemoteOpenBao, name: obj.Name})
		}))
	if err != nil {
		return nil, err
	}
	return ctl, nil
}

func (c *Controller) Provider() *openbao.Provider { return c.provider }

func (c *Controller) vaultsOf(k backendKey) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.bound[k]))
	for r := range c.bound[k] {
		out = append(out, r)
	}
	return out
}

func (c *Controller) index(req ctlruntime.Request, k *backendKey) {
	self := ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, set := range c.bound {
		if _, ok := set[self]; ok && (k == nil || key != *k) {
			delete(set, self)
			if len(set) == 0 {
				delete(c.bound, key)
			}
		}
	}
	if k == nil {
		return
	}
	if c.bound[*k] == nil {
		c.bound[*k] = map[ctlruntime.Request]struct{}{}
	}
	c.bound[*k][self] = struct{}{}
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.Vault) (ctlruntime.Result, error) {
	self := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Vault", Name: object.Name, Workspace: req.Workspace}
	if !object.DeletionTimestamp.IsZero() {
		c.index(req, nil)
		c.provider.Unbind(self)
		return ctlruntime.Result{}, nil
	}
	gen := object.Generation
	ref := object.Spec.BackendRef
	status := v1alpha1.VaultStatus{ObservedGeneration: gen, Conditions: object.Status.Conditions}
	unbound := func(reason, message string) (ctlruntime.Result, error) {
		c.provider.Unbind(self)
		conds.Set(&status.Conditions, gen, v1alpha1.VaultConditionBackendBound, false, reason, message)
		conds.Set(&status.Conditions, gen, v1alpha1.VaultConditionReady, false, "BackendUnbound", message)
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	if g := strings.TrimSpace(ref.Group); g != "" && g != v1alpha1.GroupName {
		c.index(req, nil)
		return unbound("UnsupportedGroup", fmt.Sprintf("spec.backendRef group %q carries no vault contract", ref.Group))
	}
	if ws := strings.TrimSpace(ref.Workspace); ws != "" && ws != req.Workspace {
		c.index(req, nil)
		return unbound("CrossWorkspace", fmt.Sprintf("spec.backendRef names workspace %s; a Vault binds a backend in its own workspace", ws))
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		c.index(req, nil)
		return unbound("NoBackendRef", "spec.backendRef names no backend")
	}
	key := backendKey{cluster: req.Cluster, kind: ref.Kind, name: name}
	backend := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: ref.Kind, Name: name, Workspace: req.Workspace}
	switch ref.Kind {
	case KindOpenBaoServer:
		c.index(req, &key)
		var server v1alpha1.OpenBaoServer
		if err := req.Get(ctx, "", name, &server); err != nil {
			if apierrors.IsNotFound(err) {
				return unbound("BackendNotFound", fmt.Sprintf("OpenBaoServer %s does not exist", name))
			}
			return ctlruntime.Result{}, err
		}
		st := server.Status
		status.Address, status.Initialized, status.Sealed, status.CABundle, status.Version = st.Address, st.Initialized, st.Sealed, st.CABundle, st.Version
		status.Ready = st.Ready && st.ObservedGeneration == server.Generation
		c.provider.Bind(openbao.Binding{Vault: self, Backend: backend, Server: st.DeepCopy()})
	case KindRemoteOpenBao:
		c.index(req, &key)
		var remote v1alpha1.RemoteOpenBao
		if err := req.Get(ctx, "", name, &remote); err != nil {
			if apierrors.IsNotFound(err) {
				return unbound("BackendNotFound", fmt.Sprintf("RemoteOpenBao %s does not exist", name))
			}
			return ctlruntime.Result{}, err
		}
		st := remote.Status
		status.Address, status.Initialized, status.Sealed, status.CABundle, status.Version = st.Address, st.Initialized, st.Sealed, st.CABundle, st.Version
		status.Ready = st.Ready && observedAt(st.Conditions, remote.Generation)
		c.provider.Bind(openbao.Binding{Vault: self, Backend: backend, Remote: remote.DeepCopy()})
	default:
		c.index(req, nil)
		return unbound("UnsupportedKind", fmt.Sprintf("spec.backendRef kind %q is not an OpenBaoServer or a RemoteOpenBao", ref.Kind))
	}
	conds.Set(&status.Conditions, gen, v1alpha1.VaultConditionBackendBound, true, "Bound", fmt.Sprintf("bound to %s %s", ref.Kind, name))
	if status.Ready {
		conds.Set(&status.Conditions, gen, v1alpha1.VaultConditionReady, true, "BackendReady", fmt.Sprintf("%s %s is ready", ref.Kind, name))
	} else {
		conds.Set(&status.Conditions, gen, v1alpha1.VaultConditionReady, false, "BackendNotReady", fmt.Sprintf("%s %s is not ready", ref.Kind, name))
	}
	return ctlruntime.Result{}, c.apply(ctx, req, object, status)
}

func observedAt(conditions []metav1.Condition, generation int64) bool {
	for _, c := range conditions {
		if c.ObservedGeneration < generation {
			return false
		}
	}
	return len(conditions) > 0
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.Vault, status v1alpha1.VaultStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}
