package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/postgresctl"
)

const (
	Name = "postgres"

	Export = "hostprocesses"

	Finalizer = "socialweb.computer/postgres"

	LabelPostgres = "socialweb.computer/postgres"

	ConditionReady = "Ready"

	ProbeTimeout = 5 * time.Second

	eventBuffer = 64
)

type Options struct {
	Log *slog.Logger

	Host *postgresctl.Host

	Scope func(workspace string) bool
}

type Controller struct {
	log *slog.Logger

	host *postgresctl.Host

	events chan ctlruntime.Request

	mu sync.Mutex

	follows map[ctlruntime.Request]*follow
}

type follow struct {
	cancel context.CancelFunc
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Host == nil {
		return nil, errors.New("postgres: a postgres host is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{log: log.With("component", "postgres-controller"), host: opts.Host, events: make(chan ctlruntime.Request, eventBuffer),
		follows: map[ctlruntime.Request]*follow{}}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Finalizer(Finalizer), ctlruntime.Owns[*v1alpha1.HostProcess](),
		ctlruntime.Events(c.events), ctlruntime.Gone(c.gone)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.Postgres](m, Name, c, options...); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, o *v1alpha1.Postgres) (ctlruntime.Result, error) {
	if !o.DeletionTimestamp.IsZero() {
		return c.finalize(ctx, req, o)
	}
	log := c.log.With("workspace", req.Workspace, "postgres", o.Name)
	next := o.Status.DeepCopy()
	next.ObservedGeneration = o.Generation
	set := func(phase v1alpha1.PostgresPhase, msg string) {
		next.Phase, next.Message = phase, msg
		conds.Set(&next.Conditions, o.Generation, ConditionReady, phase == v1alpha1.PostgresPhaseReady, string(phase), msg)
	}
	fail := func(err error) (ctlruntime.Result, error) {
		set(v1alpha1.PostgresPhaseFailed, err.Error())
		if werr := c.apply(ctx, req, o, next); werr != nil {
			return ctlruntime.Result{}, werr
		}
		if postgresctl.IsInvalid(err) {
			return ctlruntime.Result{}, nil
		}
		return ctlruntime.Result{}, err
	}
	s, err := c.host.Prepare(ctx, *o)
	if err != nil {
		return fail(err)
	}
	if s.Initialized {
		log.Info("initdb", "dataDir", s.Data, "user", s.User)
	}
	hp, err := c.child(ctx, req, o, s.Child)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	next.DataDir, next.SocketDir, next.User, next.HostProcess = s.Data, s.SocketDir, s.User, hp.Name
	next.DSN = s.DSN()
	next.Runtime = hp.Status.Runtime
	switch hp.Status.Phase {
	case v1alpha1.HostProcessPhaseRunning:
	case v1alpha1.HostProcessPhaseLost:
		msg := fmt.Sprintf("its HostProcess %s is Lost %s (%s); placing a new one on the same data directory", hp.Name, hp.Status.LostReason, hp.Status.Message)
		log.Warn(msg)
		if err := req.Delete(ctx, hp); err != nil {
			return ctlruntime.Result{}, err
		}
		set(v1alpha1.PostgresPhaseStarting, msg)
		return ctlruntime.Result{}, c.apply(ctx, req, o, next)
	case v1alpha1.HostProcessPhaseFailed:
		return fail(fmt.Errorf("its HostProcess %s is Failed: %s", hp.Name, hp.Status.Message))
	default:
		msg := "its HostProcess " + hp.Name + " is " + string(hp.Status.Phase)
		if hp.Status.Phase == "" {
			msg = "its HostProcess " + hp.Name + " waits for the supervisor"
		}
		set(v1alpha1.PostgresPhaseStarting, msg)
		return ctlruntime.Result{}, c.apply(ctx, req, o, next)
	}
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	if err := s.Ready(pctx); err != nil {
		set(v1alpha1.PostgresPhaseStarting, "SELECT 1 over the socket: "+err.Error())
		if werr := c.apply(ctx, req, o, next); werr != nil {
			return ctlruntime.Result{}, werr
		}
		if ferr := c.follow(ctx, req, o); ferr != nil {
			return ctlruntime.Result{}, ferr
		}
		if c.host.Accepting(*o) {
			return ctlruntime.Result{}, fmt.Errorf("postgres: %s reports ready and SELECT 1 fails: %w", o.Name, err)
		}
		return ctlruntime.Result{}, nil
	}
	c.unfollow(req)
	created, err := s.Databases(pctx, o.Spec.Databases)
	for _, db := range created {
		log.Info("database created", "database", db)
	}
	if err != nil {
		return fail(err)
	}
	if v, err := s.Version(pctx); err == nil {
		next.Version = v
	}
	if o.Status.Phase != v1alpha1.PostgresPhaseReady || next.ReadyAt == "" ||
		(o.Status.Runtime != nil && hp.Status.Runtime != nil && o.Status.Runtime.InvocationID != hp.Status.Runtime.InvocationID) {
		next.ReadyAt = time.Now().UTC().Format(time.RFC3339Nano)
		log.Info("ready", "socketDir", s.SocketDir, "version", next.Version, "unit", s.Unit)
	}
	set(v1alpha1.PostgresPhaseReady, "SELECT 1 answered over "+filepath.Join(s.SocketDir, pgunit.SocketName))
	return ctlruntime.Result{}, c.apply(ctx, req, o, next)
}

func (c *Controller) child(ctx context.Context, req ctlruntime.Request, o *v1alpha1.Postgres, spec v1alpha1.HostProcessSpec) (*v1alpha1.HostProcess, error) {
	name := postgresctl.ChildName(o.Name)
	var cur v1alpha1.HostProcess
	err := req.Get(ctx, "", name, &cur)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	found := err == nil
	if found && reflect.DeepEqual(cur.Spec, spec) {
		return &cur, nil
	}
	controller := true
	want := &v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelPostgres: o.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Postgres", Name: o.Name,
			UID: o.UID, Controller: &controller}}}, Spec: spec}
	if err := req.Apply(ctx, want); err != nil {
		return nil, err
	}
	if !found {
		c.log.Info("HostProcess child created", "workspace", req.Workspace, "postgres", o.Name, "hostprocess", name, "unit", spec.Unit)
		return want, nil
	}
	c.log.Info("HostProcess child updated; the running unit is kept until it restarts", "workspace", req.Workspace, "postgres", o.Name)
	cur.Spec = spec
	return &cur, nil
}

func (c *Controller) finalize(ctx context.Context, req ctlruntime.Request, o *v1alpha1.Postgres) (ctlruntime.Result, error) {
	next := o.Status.DeepCopy()
	next.Phase = v1alpha1.PostgresPhaseDeleting
	waiting := ctlruntime.Result{Waiting: true}
	var hp v1alpha1.HostProcess
	err := req.Get(ctx, "", postgresctl.ChildName(o.Name), &hp)
	if err != nil && !apierrors.IsNotFound(err) {
		return waiting, err
	}
	if err == nil {
		if hp.DeletionTimestamp.IsZero() {
			if err := req.Delete(ctx, &hp); err != nil {
				return waiting, err
			}
		}
		next.Message = "the HostProcess child is deleted; its supervisor stops the unit"
		return waiting, c.apply(ctx, req, o, next)
	}
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	if c.host.Serving(pctx, *o) {
		if err := c.follow(ctx, req, o); err != nil {
			return waiting, err
		}
		if c.host.Serving(pctx, *o) {
			next.Message = "waiting for the server to stop"
			return waiting, c.apply(ctx, req, o, next)
		}
	}
	c.unfollow(req)
	removed, err := c.host.Remove(*o)
	if err != nil {
		return waiting, err
	}
	if removed {
		c.log.Info("data directory and log removed (spec.deleteData)", "workspace", req.Workspace, "postgres", o.Name, "dataDir", c.host.DataDir(*o))
	}
	return ctlruntime.Result{}, nil
}

func (c *Controller) follow(ctx context.Context, req ctlruntime.Request, o *v1alpha1.Postgres) error {
	key := ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.follows[key]; ok {
		return nil
	}
	fctx, cancel := context.WithCancel(ctx)
	changes, err := c.host.PidChanges(fctx, *o)
	if err != nil {
		cancel()
		return fmt.Errorf("postgres: following %s: %w", c.host.PidFile(*o), err)
	}
	f := &follow{cancel: cancel}
	c.follows[key] = f
	go func() {
		defer c.drop(key, f)
		for range changes {
			select {
			case c.events <- key:
			case <-fctx.Done():
				return
			}
		}
	}()
	return nil
}

func (c *Controller) drop(key ctlruntime.Request, f *follow) {
	f.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.follows[key] == f {
		delete(c.follows, key)
	}
}

func (c *Controller) unfollow(req ctlruntime.Request) {
	key := ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}
	c.mu.Lock()
	f := c.follows[key]
	delete(c.follows, key)
	c.mu.Unlock()
	if f != nil {
		f.cancel()
	}
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.unfollow(req)
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, o *v1alpha1.Postgres, next *v1alpha1.PostgresStatus) error {
	if reflect.DeepEqual(o.Status, *next) {
		return nil
	}
	obj := o.DeepCopy()
	obj.Status = *next
	return req.ApplyStatus(ctx, obj)
}
