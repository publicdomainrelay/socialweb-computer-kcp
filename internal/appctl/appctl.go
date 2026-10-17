package appctl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	ManifestSet        = "applications"
	FieldManager       = "appctl"
	Finalizer          = "apps.socialweb.computer/components"
	OwnerLabel         = "apps.socialweb.computer/root"
	ComponentWorkspace = "app"
	DefaultWorkspace   = "root"
	DefaultInterval    = 2 * time.Second
	RetryAfter         = 2 * time.Second
	ApplyTimeout       = time.Minute
	ComponentsDir      = "components"
)

var (
	MarketNetworkGVR   = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "marketnetworks"}
	MarketBidderGVR    = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "marketbidders"}
	MarketRequesterGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "marketrequesters"}
	GatewayGVR         = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "gateways"}
	DenoPodGVR         = schema.GroupVersionResource{Group: "deno.computer", Version: "v1alpha1", Resource: "denopods"}
	HostProcessGVR     = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "hostprocesses"}
)

func GVRFor(k v1alpha1.ApplicationKind) (schema.GroupVersionResource, error) {
	switch k {
	case v1alpha1.ApplicationKindMarketBidder:
		return MarketBidderGVR, nil
	case v1alpha1.ApplicationKindMarketRequester:
		return MarketRequesterGVR, nil
	case v1alpha1.ApplicationKindGateway:
		return GatewayGVR, nil
	case v1alpha1.ApplicationKindSshGateway:
		return SshGatewayGVR, nil
	}
	return schema.GroupVersionResource{}, fmt.Errorf("appctl: %q is no application kind (MarketBidder, MarketRequester, Gateway, SshGateway)", k)
}

type Config struct {
	Kubeconfig   string
	Workspace    string
	Layout       devinstance.Layout
	Manager      hostsvc.Manager
	Record       func() (devinstance.Record, error)
	ReadyTimeout time.Duration
	Interval     time.Duration
	Listeners    func(owner string) []v1alpha1.ApplicationListener
	WebApp       *WebApp
	OnComponents func(owner string, listeners []v1alpha1.ApplicationListener)
	SignInGuard  SignInGuard
	StoreUsage   func() *v1alpha1.StoreUsage
	TTLReport    func() *v1alpha1.TTLStatus
	Log          *slog.Logger
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log.With("component", "appctl")
}

func (c Config) workspace() string {
	if c.Workspace == "" {
		return DefaultWorkspace
	}
	return c.Workspace
}

func (c Config) interval() time.Duration {
	if c.Interval == 0 {
		return DefaultInterval
	}
	return c.Interval
}

func ComponentStateDir(instanceState, component string) string {
	return filepath.Join(instanceState, ComponentsDir, component)
}

type Controller struct {
	c        Config
	restCfg  *rest.Config
	applied  map[string]int64
	rotated  map[string]int64
	written  map[string]string
	placed   map[string]map[bool][]placedRef
	network  *networks
	stateDir atomic.Pointer[v1alpha1.StateDirUsage]
	gc       atomic.Pointer[v1alpha1.GCStatus]
	gcAt     time.Time
	ttl      atomic.Pointer[v1alpha1.TTLStatus]
	quotas   atomic.Pointer[[]v1alpha1.QuotaUsage]

	quotaApplied map[string]string
	quotaFailed  map[string]string
}

func clusterConfig(base *rest.Config, ws string) *rest.Config {
	cfg := rest.CopyConfig(base)
	host, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = host + "/clusters/" + ws
	return cfg
}

func (ctl *Controller) client(ws string, gvr schema.GroupVersionResource) (dynamic.NamespaceableResourceInterface, error) {
	dc, err := dynamic.NewForConfig(clusterConfig(ctl.restCfg, ws))
	if err != nil {
		return nil, err
	}
	return dc.Resource(gvr), nil
}

func applySet(ctx context.Context, restCfg *rest.Config, base string, log *slog.Logger) error {
	ap, err := kcpapply.New(restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, Base: base, Log: log})
	if err != nil {
		return err
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	set, ok := ix.Set(ManifestSet)
	if !ok {
		return fmt.Errorf("appctl: deploy/sets.yaml names no %s set", ManifestSet)
	}
	values, err := manifests.DecodeValues(set.Values, nil)
	if err != nil {
		return err
	}
	_, err = ap.Apply(ctx, set, values)
	return err
}

func New(c Config) (*Controller, error) {
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	ctl := &Controller{c: c, restCfg: restCfg, applied: map[string]int64{}, rotated: map[string]int64{}, written: map[string]string{},
		placed: map[string]map[bool][]placedRef{}, quotaApplied: map[string]string{}, quotaFailed: map[string]string{}}
	ctl.network = newNetworks(ctl)
	return ctl, nil
}

func Run(ctx context.Context, c Config) error {
	ctl, err := New(c)
	if err != nil {
		return err
	}
	log := c.log()
	for {
		err := applySet(ctx, ctl.restCfg, c.workspace(), log)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn("applying the applications set; retrying", "err", err.Error())
		if !sleep(ctx, RetryAfter) {
			return ctx.Err()
		}
	}
	log.Info("the application controller reconciles this instance's root objects", "workspace", c.workspace())
	go ctl.sampleStateDir(ctx)
	go ctl.sampleGC(ctx)
	for {
		ctl.Pass(ctx)
		if !sleep(ctx, c.interval()) {
			return ctx.Err()
		}
	}
}

func Supervise(ctx context.Context, c Config) {
	for {
		err := Run(ctx, c)
		if ctx.Err() != nil {
			return
		}
		c.log().Error("the application controller stopped; starting it again", "err", fmt.Sprint(err))
		if !sleep(ctx, RetryAfter) {
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (ctl *Controller) Pass(ctx context.Context) {
	log := ctl.c.log()
	for _, k := range []struct {
		gvr       schema.GroupVersionResource
		reconcile func(context.Context, *unstructured.Unstructured) error
	}{
		{MarketNetworkGVR, ctl.network.reconcile},
		{MarketBidderGVR, ctl.reconcileBidder},
		{MarketRequesterGVR, ctl.reconcileRequester},
		{GatewayGVR, ctl.reconcileGateway},
		{SshGatewayGVR, ctl.reconcileSshGateway},
	} {
		ri, err := ctl.client(ctl.c.workspace(), k.gvr)
		if err != nil {
			log.Warn("a client", "resource", k.gvr.Resource, "err", err.Error())
			continue
		}
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("listing root objects", "resource", k.gvr.Resource, "err", err.Error())
			}
			continue
		}
		for i := range list.Items {
			u := &list.Items[i]
			if err := k.reconcile(ctx, u); err != nil && ctx.Err() == nil {
				log.Warn("a reconcile pass failed; the next pass retries", "kind", u.GetKind(), "name", u.GetName(), "err", err.Error())
			}
			ctl.adoptChildren(ctx, u)
			ctl.ensureQuotas(ctx, u)
		}
	}
}

func fromUnstructured(u *unstructured.Unstructured, into any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, into)
}

func toUnstructured(o any, kind string) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(kind)
	return u, nil
}

func ownerOf(u *unstructured.Unstructured) string {
	return strings.ToLower(u.GetKind()) + "." + u.GetName()
}

func hasFinalizer(u *unstructured.Unstructured) bool {
	for _, f := range u.GetFinalizers() {
		if f == Finalizer {
			return true
		}
	}
	return false
}

func (ctl *Controller) setFinalizer(ctx context.Context, gvr schema.GroupVersionResource, u *unstructured.Unstructured, want bool) error {
	ri, err := ctl.client(ctl.c.workspace(), gvr)
	if err != nil {
		return err
	}
	for range 5 {
		cur, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if hasFinalizer(cur) == want {
			return nil
		}
		var fs []string
		for _, f := range cur.GetFinalizers() {
			if f != Finalizer {
				fs = append(fs, f)
			}
		}
		if want {
			fs = append(fs, Finalizer)
		}
		cur.SetFinalizers(fs)
		_, err = ri.Update(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("appctl: the finalizer of %s stayed in conflict", u.GetName())
}

var componentOwnedStatus = []string{"market"}

func withComponentOwnedStatus(next map[string]any, current any) map[string]any {
	cur, _ := current.(map[string]any)
	out := make(map[string]any, len(next)+len(componentOwnedStatus))
	for k, v := range next {
		out[k] = v
	}
	for _, k := range componentOwnedStatus {
		if v, ok := cur[k]; ok {
			out[k] = v
		}
	}
	return out
}

func (ctl *Controller) writeStatus(ctx context.Context, gvr schema.GroupVersionResource, name string, status any) error {
	ri, err := ctl.client(ctl.c.workspace(), gvr)
	if err != nil {
		return err
	}
	b, err := json.Marshal(status)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	extra := false
	if u := ctl.stateDir.Load(); u != nil {
		m["stateDir"], extra = u, true
	}
	if u := ctl.gc.Load(); u != nil {
		m["gc"], extra = u, true
	}
	if u := ctl.ttl.Load(); u != nil {
		m["ttl"], extra = u, true
	}
	if u := ctl.quotas.Load(); u != nil && len(*u) > 0 {
		m["quotas"], extra = *u, true
	}
	if ctl.c.StoreUsage != nil {
		if u := ctl.c.StoreUsage(); u != nil {
			m["store"], extra = u, true
		}
	}
	if extra {
		if b, err = json.Marshal(m); err != nil {
			return err
		}
		m = nil
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
	}
	key := gvr.Resource + "/" + name
	if ctl.written[key] == string(b) {
		return nil
	}
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		cur.Object["status"] = withComponentOwnedStatus(m, cur.Object["status"])
		_, err = ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
		if err == nil {
			ctl.written[key] = string(b)
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("appctl: the status of %s stayed in conflict", name)
}
