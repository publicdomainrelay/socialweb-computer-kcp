package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
)

const (
	ProviderName = "clusterprovider"

	IdentityExport = "atprotoidentities"

	HostProcessExport = "hostprocesses"

	DefaultProviderWorkers = 32
)

type Pass interface {
	Pass(ctx context.Context, ref clusterprovider.Ref, cl *v1alpha1.Cluster) (time.Duration, error)

	Gone(ctx context.Context, ref clusterprovider.Ref)

	ClaimGone(ctx context.Context)
}

type ProviderOptions struct {
	Pass Pass

	Keep func(ref clusterprovider.Ref) bool

	Workers int

	Changes <-chan string
}

type ProviderController struct {
	pass Pass

	keep func(ref clusterprovider.Ref) bool

	mu sync.Mutex

	byKey map[string]ctlruntime.Request

	refs map[ctlruntime.Request]clusterprovider.Ref
}

func RegisterProvider(m *ctlruntime.Manager, opts ProviderOptions) (*ProviderController, error) {
	if opts.Pass == nil {
		return nil, errors.New("cluster: the provider controller needs a pass")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = DefaultProviderWorkers
	}
	ctl := &ProviderController{pass: opts.Pass, keep: opts.Keep, byKey: map[string]ctlruntime.Request{},
		refs: map[ctlruntime.Request]clusterprovider.Ref{}}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Workers(workers), ctlruntime.Gone(ctl.gone),
		ctlruntime.InWorkspaces(homeOrUnresolved),
		ctlruntime.Watches(ctl.nodeOf),
		ctlruntime.Watches(ctl.claimOf),
		ctlruntime.WatchesIn(IdentityExport, ctl.identityOf),
		ctlruntime.WatchesIn(HostProcessExport, hostOf[*v1alpha1.HostProcess](ctl)),
		ctlruntime.WatchesIn(HostProcessExport, hostOf[*v1alpha1.Postgres](ctl)),
		ctlruntime.WatchesIn(HostProcessExport, hostOf[*v1alpha1.OpenBaoServer](ctl))}
	if opts.Changes != nil {
		events, err := ctlruntime.Forward(m, ProviderName+": changes", opts.Changes, ctl.requestsOf)
		if err != nil {
			return nil, fmt.Errorf("cluster: forwarding provider changes: %w", err)
		}
		options = append(options, ctlruntime.Events(events))
	}
	if err := ctlruntime.For[*v1alpha1.Cluster](m, ProviderName, ctl, options...); err != nil {
		return nil, fmt.Errorf("cluster: registering the cluster provider controller: %w", err)
	}
	return ctl, nil
}

func (c *ProviderController) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.Cluster) (ctlruntime.Result, error) {
	ref, err := refOf(req, object)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	key, err := ref.Key()
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if c.keep != nil && !c.keep(ref) {
		return ctlruntime.Result{}, nil
	}
	c.remember(key, ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name}, ref)
	after, err := c.pass.Pass(ctx, ref, object)
	return ctlruntime.Result{RequeueAfter: after}, err
}

func refOf(req ctlruntime.Request, object *v1alpha1.Cluster) (clusterprovider.Ref, error) {
	home := strings.TrimSpace(req.Workspace)
	account, _, ok := strings.Cut(strings.TrimPrefix(home, kcpclient.RootWorkspace+":"), ":")
	if !isHome(home) || !ok {
		return clusterprovider.Ref{}, fmt.Errorf("cluster: the workspace path of cluster %s (logical cluster %s) is not resolved to a home %s:<account>:<cluster> yet: %q",
			req.Name, req.Cluster, kcpclient.RootWorkspace, home)
	}
	if object.Spec.Account == "" {
		return clusterprovider.Ref{}, fmt.Errorf("cluster: Cluster %s in %s has no spec.account, so no mount path can be derived for it", req.Name, home)
	}
	return clusterprovider.Ref{Account: kcpclient.RootWorkspace + ":" + account, DID: object.Spec.Account, Home: home, Name: req.Name}, nil
}

func isHome(workspace string) bool {
	parts := strings.Split(workspace, ":")
	return len(parts) == 3 && parts[0] == kcpclient.RootWorkspace && parts[1] != "" && parts[2] != ""
}

func homeOrUnresolved(workspace string) bool {
	return !strings.Contains(workspace, ":") || isHome(workspace)
}

func (c *ProviderController) remember(key string, r ctlruntime.Request, ref clusterprovider.Ref) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey[key] = r
	c.refs[r] = ref
}

func (c *ProviderController) gone(ctx context.Context, req ctlruntime.Request) {
	r := ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name}
	c.mu.Lock()
	ref, known := c.refs[r]
	if known {
		delete(c.refs, r)
		if key, err := ref.Key(); err == nil && c.byKey[key] == r {
			delete(c.byKey, key)
		}
	}
	c.mu.Unlock()
	if known {
		c.pass.Gone(ctx, ref)
	}
}

func (c *ProviderController) Known() []clusterprovider.Ref {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]clusterprovider.Ref, 0, len(c.refs))
	for _, ref := range c.refs {
		out = append(out, ref)
	}
	return out
}

func (c *ProviderController) request(key string) (ctlruntime.Request, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byKey[key]
	return r, ok
}

func (c *ProviderController) nodeOf(_ context.Context, src ctlruntime.Request, node *v1alpha1.Node) []ctlruntime.Request {
	if name := strings.TrimSpace(node.Spec.ClusterRef.Name); name != "" {
		return []ctlruntime.Request{{Cluster: src.Cluster, Name: name}}
	}
	return nil
}

func (c *ProviderController) claimOf(ctx context.Context, src ctlruntime.Request, claim *v1alpha1.NodeClaim) []ctlruntime.Request {
	if !claim.DeletionTimestamp.IsZero() {
		c.pass.ClaimGone(ctx)
	}
	for _, owner := range claim.OwnerReferences {
		if owner.Kind == "Cluster" && owner.Name != "" {
			return []ctlruntime.Request{{Cluster: src.Cluster, Name: owner.Name}}
		}
	}
	name := strings.TrimSpace(claim.Spec.ClusterRef.Name)
	if name == "" {
		return nil
	}
	if r, ok := c.request(name); ok {
		return []ctlruntime.Request{r}
	}
	return []ctlruntime.Request{{Cluster: src.Cluster, Name: name}}
}

func (c *ProviderController) identityOf(_ context.Context, src ctlruntime.Request, _ *v1alpha1.AtprotoIdentity) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ctlruntime.Request
	for r, ref := range c.refs {
		if ref.Account == src.Workspace {
			out = append(out, r)
		}
	}
	return out
}

func hostOf[T ctlruntime.Object](c *ProviderController) func(context.Context, ctlruntime.Request, T) []ctlruntime.Request {
	return func(_ context.Context, _ ctlruntime.Request, obj T) []ctlruntime.Request {
		if r, ok := c.request(obj.GetLabels()[clusterprovider.LabelCluster]); ok {
			return []ctlruntime.Request{r}
		}
		return nil
	}
}

func (c *ProviderController) requestsOf(key string) []ctlruntime.Request {
	if r, ok := c.request(key); ok {
		return []ctlruntime.Request{r}
	}
	return nil
}
