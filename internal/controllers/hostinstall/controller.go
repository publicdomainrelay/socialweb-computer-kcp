package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostlifecycle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "hostinstall"

	Export = "applications"

	ObjectName = "host-install"

	ConditionReady = "Ready"

	ConditionInstalled = "Installed"

	ConditionProgressing = "Progressing"

	authorityUnreadable = "the host authority could not be read; the controller retries"
)

type Options struct {
	Log *slog.Logger

	Authority hostlifecycle.Authority

	Scope func(workspace string) bool

	Changed <-chan struct{}
}

type Controller struct {
	log *slog.Logger

	authority hostlifecycle.Authority

	mu sync.Mutex

	known map[ctlruntime.Request]struct{}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Authority == nil {
		return nil, errors.New("hostinstall: an authority is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{log: log.With("component", Name+"-controller"), authority: opts.Authority, known: map[ctlruntime.Request]struct{}{}}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(c.gone)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if opts.Changed != nil {
		options = append(options, ctlruntime.Events(c.forward(opts.Changed)))
	}
	if err := ctlruntime.For[*v1alpha1.HostInstall](m, Name, c, options...); err != nil {
		return nil, err
	}
	return c, nil
}

func Desired(ctx context.Context, a hostlifecycle.Authority) (*v1alpha1.HostInstall, error) {
	obj := &v1alpha1.HostInstall{ObjectMeta: metav1.ObjectMeta{Name: ObjectName}}
	obs, err := a.ObserveHostInstall(ctx, v1alpha1.HostInstallSpec{})
	obj.Spec.Account, obj.Spec.Version = obs.Account, obs.InstalledVersion
	return obj, err
}

func (c *Controller) forward(changed <-chan struct{}) <-chan ctlruntime.Request {
	out := make(chan ctlruntime.Request)
	go func() {
		defer close(out)
		for range changed {
			for _, r := range c.requests() {
				out <- r
			}
		}
	}()
	return out
}

func (c *Controller) requests() []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.known))
	for r := range c.known {
		out = append(out, r)
	}
	return out
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.known, ctlruntime.Request{Cluster: req.Cluster, Name: req.Name})
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.HostInstall) (ctlruntime.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		c.gone(ctx, req)
		return ctlruntime.Result{}, nil
	}
	c.mu.Lock()
	c.known[ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}] = struct{}{}
	c.mu.Unlock()
	spec := obj.Spec
	generation := obj.Generation
	obs, err := c.authority.ObserveHostInstall(ctx, spec)
	status := v1alpha1.HostInstallStatus{ObservedGeneration: generation, InstalledVersion: obs.InstalledVersion, Steps: obs.Steps,
		Conditions: obj.Status.Conditions}
	if err != nil {
		c.log.Warn("reading the host install authority", "name", obj.Name, "err", err.Error())
		for _, kind := range []string{ConditionInstalled, ConditionProgressing} {
			conds.SetStatus(&status.Conditions, generation, kind, metav1.ConditionUnknown, "AuthorityUnreadable", authorityUnreadable)
		}
		conds.SetStatus(&status.Conditions, generation, ConditionReady, metav1.ConditionFalse, "AuthorityUnreadable", authorityUnreadable)
		return ctlruntime.Result{}, errors.Join(c.apply(ctx, req, obj, status), err)
	}
	found := obs.Found
	matches := spec.Version == "" || obs.InstalledVersion == spec.Version
	ready := found && obs.InstalledVersion != "" && matches
	progressing := found && !ready
	msg := message(obs, ready, matches)
	conds.SetStatus(&status.Conditions, generation, ConditionInstalled, conds.Status(found), boolReason(found, "RecordFound", "NoRecord"), msg)
	conds.SetStatus(&status.Conditions, generation, ConditionReady, conds.Status(ready), boolReason(ready, "Installed", "NotInstalled"), msg)
	conds.SetStatus(&status.Conditions, generation, ConditionProgressing, conds.Status(progressing), boolReason(progressing, "AwaitingVersion", "Settled"), msg)
	return ctlruntime.Result{}, c.apply(ctx, req, obj, status)
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.HostInstall, status v1alpha1.HostInstallStatus) error {
	next := obj.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func message(obs hostlifecycle.HostInstallObservation, ready, matches bool) string {
	switch {
	case !obs.Found:
		return "no host install record"
	case !matches:
		return "the requested version is not installed"
	case !ready:
		return "the installed version is not known"
	default:
		return fmt.Sprintf("version %s is installed", obs.InstalledVersion)
	}
}

func boolReason(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}
