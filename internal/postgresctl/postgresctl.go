package postgresctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/logcap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
)

const (
	Finalizer       = "socialweb.computer/postgres"
	ChildPrefix     = "pg-"
	DefaultUser     = "socialweb"
	DefaultInterval = 3 * time.Second
	RetryAfter      = 3 * time.Second
	PendingInterval = 200 * time.Millisecond
	ProbeTimeout    = 5 * time.Second
	ConditionReady  = "Ready"
	OutputCap       = 64 << 20
)

var (
	GVR             = v1alpha1.PostgresGVR
	nameRE          = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	DefaultSettings = []string{"wal_level=logical", "autovacuum=on", "track_counts=on"}
)

type Config struct {
	Kubeconfig string
	Workspace  string
	Install    func() (pgunit.Install, error)
	DataRoot   string
	SocketRoot string
	LogDir     string
	Prefix     string
	Slice      string
	User       string
	Interval   time.Duration
	Log        *slog.Logger
}

type Controller struct {
	c   Config
	pgs dynamic.ResourceInterface
	hps dynamic.ResourceInterface
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log.With("component", "postgres")
}

func (c Config) user() string {
	if c.User == "" {
		return DefaultUser
	}
	return c.User
}

func New(c Config) (*Controller, error) {
	if c.Workspace == "" {
		c.Workspace = hostprocess.DefaultWorkspace
	}
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(restCfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + c.Workspace
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Controller{c: c, pgs: dc.Resource(GVR), hps: dc.Resource(hostprocess.GVR)}, nil
}

func ChildName(name string) string { return ChildPrefix + name }

func (ct *Controller) UnitName(name string) string {
	return ct.c.Prefix + ChildPrefix + name + ".service"
}

func (ct *Controller) SocketDir(name string) string {
	return filepath.Join(ct.c.SocketRoot, ChildPrefix+name)
}

func (ct *Controller) logFile(name string) string {
	return filepath.Join(ct.c.LogDir, ChildPrefix+name+".log")
}

func (ct *Controller) DataDir(o v1alpha1.Postgres) string {
	if o.Spec.DataDir != "" {
		return o.Spec.DataDir
	}
	return filepath.Join(ct.c.DataRoot, o.Name, "data")
}

func decode[T any](u *unstructured.Unstructured) (T, error) {
	var o T
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o)
	return o, err
}

func encode(o any, kind string) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(kind)
	return u, nil
}

func (ct *Controller) childSpec(o v1alpha1.Postgres, in pgunit.Install, s pgunit.Server) (v1alpha1.HostProcessSpec, error) {
	argv, err := in.Argv(s)
	if err != nil {
		return v1alpha1.HostProcessSpec{}, err
	}
	spec := v1alpha1.HostProcessSpec{Unit: ct.UnitName(o.Name), Exec: argv[0], Argv: argv[1:], WorkDir: s.Data,
		RestartPolicy: v1alpha1.HostProcessRestartOnFailure, Slice: ct.c.Slice,
		Stop:      &v1alpha1.HostProcessStop{Signal: "SIGINT", GraceSeconds: int32(pgunit.StopTimeout / time.Second)},
		Readiness: &v1alpha1.HostProcessReadiness{PgxSocketDir: s.SocketDir}, Placement: v1alpha1.HostProcessPlacementSupervisor,
		Limits: &v1alpha1.HostProcessLimits{MemoryMax: strconv.FormatUint(pgunit.MemoryMax, 10)}}
	for _, kv := range in.Env {
		k, v, _ := strings.Cut(kv, "=")
		spec.Env = append(spec.Env, v1alpha1.HostProcessEnv{Name: k, Value: v})
	}
	if ct.c.LogDir != "" {
		f := ct.logFile(o.Name)
		spec.Output = &v1alpha1.HostProcessOutput{Stdout: f, Stderr: f, CapBytes: OutputCap}
	}
	return spec, nil
}

func setReady(st *v1alpha1.PostgresStatus, gen int64, ok bool, reason, msg string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: ConditionReady, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen})
}

func (ct *Controller) child(ctx context.Context, name string) (*v1alpha1.HostProcess, *unstructured.Unstructured, error) {
	u, err := ct.hps.Get(ctx, ChildName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	hp, err := decode[v1alpha1.HostProcess](u)
	return &hp, u, err
}

func (ct *Controller) ensureChild(ctx context.Context, o v1alpha1.Postgres, spec v1alpha1.HostProcessSpec) (*v1alpha1.HostProcess, error) {
	hp, u, err := ct.child(ctx, o.Name)
	if err != nil {
		return nil, err
	}
	if hp == nil {
		ctrl := true
		want := v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: ChildName(o.Name),
			Labels: map[string]string{"socialweb.computer/postgres": o.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Postgres", Name: o.Name,
				UID: o.UID, Controller: &ctrl}}}, Spec: spec}
		nu, err := encode(&want, "HostProcess")
		if err != nil {
			return nil, err
		}
		if _, err := ct.hps.Create(ctx, nu, metav1.CreateOptions{}); err != nil {
			return nil, err
		}
		ct.c.log().Info("HostProcess child created", "postgres", o.Name, "hostprocess", want.Name, "unit", spec.Unit)
		return &want, nil
	}
	if !reflect.DeepEqual(hp.Spec, spec) {
		hp.Spec = spec
		nu, err := encode(hp, "HostProcess")
		if err != nil {
			return nil, err
		}
		nu.SetResourceVersion(u.GetResourceVersion())
		if _, err := ct.hps.Update(ctx, nu, metav1.UpdateOptions{}); err != nil {
			return nil, err
		}
		ct.c.log().Info("HostProcess child updated; the running unit is kept until it restarts", "postgres", o.Name)
	}
	return hp, nil
}

func (ct *Controller) finalize(ctx context.Context, cur *unstructured.Unstructured, o v1alpha1.Postgres) (v1alpha1.PostgresStatus, bool, error) {
	st := o.Status
	st.Phase = v1alpha1.PostgresPhaseDeleting
	hp, _, err := ct.child(ctx, o.Name)
	if err != nil {
		return st, false, err
	}
	if hp != nil {
		err := ct.hps.Delete(ctx, ChildName(o.Name), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return st, false, err
		}
		st.Message = "the HostProcess child is deleted; its supervisor stops the unit"
		return st, false, nil
	}
	data := ct.DataDir(o)
	if pgunit.Running(data) {
		if pgunit.Ready(ctx, ct.SocketDir(o.Name), ct.c.user()) == nil {
			st.Message = "waiting for the server to stop"
			return st, false, nil
		}
	}
	if o.Spec.DeleteData {
		if err := os.RemoveAll(data); err != nil {
			return st, false, err
		}
		if o.Spec.DataDir == "" {
			os.Remove(filepath.Dir(data))
		}
		if ct.c.LogDir != "" {
			f := ct.logFile(o.Name)
			os.Remove(f)
			os.Remove(f + logcap.OldSuffix)
		}
		ct.c.log().Info("data directory and log removed (spec.deleteData)", "postgres", o.Name, "dataDir", data)
	}
	os.Remove(ct.SocketDir(o.Name))
	fin := slices.DeleteFunc(cur.GetFinalizers(), func(f string) bool { return f == Finalizer })
	cur.SetFinalizers(fin)
	_, err = ct.pgs.Update(ctx, cur, metav1.UpdateOptions{})
	return st, true, err
}

func (ct *Controller) reconcile(ctx context.Context, cur *unstructured.Unstructured) error {
	o, err := decode[v1alpha1.Postgres](cur)
	if err != nil {
		return err
	}
	log := ct.c.log().With("postgres", o.Name)
	next := o.Status
	next.ObservedGeneration = o.Generation
	if o.DeletionTimestamp != nil {
		if !slices.Contains(o.Finalizers, Finalizer) {
			return nil
		}
		st, done, err := ct.finalize(ctx, cur, o)
		if done || err != nil {
			return err
		}
		return ct.writeStatus(ctx, cur, o, st)
	}
	if !slices.Contains(o.Finalizers, Finalizer) {
		cur.SetFinalizers(append(cur.GetFinalizers(), Finalizer))
		_, err := ct.pgs.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	}
	fail := func(msg string) error {
		next.Phase, next.Message = v1alpha1.PostgresPhaseFailed, msg
		setReady(&next, o.Generation, false, string(next.Phase), msg)
		return ct.writeStatus(ctx, cur, o, next)
	}
	if !nameRE.MatchString(o.Name) {
		return fail(fmt.Sprintf("name %q: want lower-case letters, digits and dashes, at most 32 (it names a unit and a socket directory)", o.Name))
	}
	in, err := ct.c.Install()
	if err != nil {
		return fail("no postgres asset: " + err.Error())
	}
	if v := o.Spec.Version; v != "" && v != in.Major() && v != in.Version {
		return fail(fmt.Sprintf("spec.version %s: this core runs the pinned postgres %s; another version is a new Postgres object (major) or a pin change (minor)", v, in.Version))
	}
	s := pgunit.Server{Data: ct.DataDir(o), SocketDir: ct.SocketDir(o.Name), User: ct.c.user(),
		Settings: append(slices.Clone(DefaultSettings), o.Spec.Settings...)}
	if !filepath.IsAbs(s.Data) {
		return fail(fmt.Sprintf("dataDir %q must be absolute", s.Data))
	}
	if err := pgunit.CheckSocketDir(s.SocketDir); err != nil {
		return fail(err.Error())
	}
	created, err := in.Initdb(ctx, s)
	if err != nil {
		return fail(err.Error())
	}
	if created {
		log.Info("initdb", "dataDir", s.Data, "user", s.User)
	}
	if err := os.MkdirAll(s.SocketDir, 0o700); err != nil {
		return fail(err.Error())
	}
	spec, err := ct.childSpec(o, in, s)
	if err != nil {
		return fail(err.Error())
	}
	hp, err := ct.ensureChild(ctx, o, spec)
	if err != nil {
		return err
	}
	next.DataDir, next.SocketDir, next.User, next.HostProcess = s.Data, s.SocketDir, s.User, hp.Name
	next.DSN = pgunit.DSN(s.SocketDir, s.User, "postgres")
	next.Runtime = hp.Status.Runtime
	switch hp.Status.Phase {
	case v1alpha1.HostProcessPhaseRunning:
	case v1alpha1.HostProcessPhaseLost:
		msg := fmt.Sprintf("its HostProcess %s is Lost %s (%s); placing a new one on the same data directory", hp.Name, hp.Status.LostReason, hp.Status.Message)
		log.Warn(msg)
		if err := ct.hps.Delete(ctx, hp.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		next.Phase, next.Message = v1alpha1.PostgresPhaseStarting, msg
		setReady(&next, o.Generation, false, string(next.Phase), msg)
		return ct.writeStatus(ctx, cur, o, next)
	case v1alpha1.HostProcessPhaseFailed:
		return fail(fmt.Sprintf("its HostProcess %s is Failed: %s", hp.Name, hp.Status.Message))
	default:
		next.Phase, next.Message = v1alpha1.PostgresPhaseStarting, "its HostProcess "+hp.Name+" is "+string(hp.Status.Phase)
		if hp.Status.Phase == "" {
			next.Message = "its HostProcess " + hp.Name + " waits for the supervisor"
		}
		setReady(&next, o.Generation, false, string(next.Phase), next.Message)
		return ct.writeStatus(ctx, cur, o, next)
	}
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	if err := pgunit.Ready(pctx, s.SocketDir, s.User); err != nil {
		next.Phase, next.Message = v1alpha1.PostgresPhaseStarting, "SELECT 1 over the socket: "+err.Error()
		setReady(&next, o.Generation, false, string(next.Phase), next.Message)
		return ct.writeStatus(ctx, cur, o, next)
	}
	for _, db := range o.Spec.Databases {
		made, err := pgunit.CreateDatabase(pctx, s.SocketDir, s.User, db)
		if err != nil {
			return fail("creating database " + db + ": " + err.Error())
		}
		if made {
			log.Info("database created", "database", db)
		}
	}
	if v, err := pgunit.ServerVersion(pctx, s.SocketDir, s.User); err == nil {
		next.Version = v
	}
	if o.Status.Phase != v1alpha1.PostgresPhaseReady || next.ReadyAt == "" ||
		(o.Status.Runtime != nil && hp.Status.Runtime != nil && o.Status.Runtime.InvocationID != hp.Status.Runtime.InvocationID) {
		next.ReadyAt = time.Now().UTC().Format(time.RFC3339Nano)
		log.Info("ready", "socketDir", s.SocketDir, "version", next.Version, "unit", spec.Unit)
	}
	next.Phase, next.Message = v1alpha1.PostgresPhaseReady, "SELECT 1 answered over "+filepath.Join(s.SocketDir, pgunit.SocketName)
	setReady(&next, o.Generation, true, string(next.Phase), next.Message)
	return ct.writeStatus(ctx, cur, o, next)
}

func (ct *Controller) writeStatus(ctx context.Context, cur *unstructured.Unstructured, o v1alpha1.Postgres, next v1alpha1.PostgresStatus) error {
	if reflect.DeepEqual(o.Status, next) {
		return nil
	}
	o.Status = next
	u, err := encode(&o, "Postgres")
	if err != nil {
		return err
	}
	u.SetResourceVersion(cur.GetResourceVersion())
	_, err = ct.pgs.UpdateStatus(ctx, u, metav1.UpdateOptions{})
	return err
}

func (ct *Controller) Pass(ctx context.Context) error {
	list, err := ct.pgs.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		if err := ct.reconcile(ctx, &list.Items[i]); err != nil && !apierrors.IsConflict(err) {
			errs = append(errs, fmt.Errorf("%s: %w", list.Items[i].GetName(), err))
		}
	}
	return errors.Join(errs...)
}

func (ct *Controller) pending(ctx context.Context) bool {
	list, err := ct.pgs.List(ctx, metav1.ListOptions{})
	if err != nil {
		return false
	}
	for i := range list.Items {
		if list.Items[i].GetDeletionTimestamp() != nil {
			continue
		}
		phase, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "phase")
		switch v1alpha1.PostgresPhase(phase) {
		case v1alpha1.PostgresPhaseReady, v1alpha1.PostgresPhaseFailed, v1alpha1.PostgresPhaseDeleting:
		default:
			return true
		}
	}
	return false
}

func (ct *Controller) watch(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		w, err := ct.pgs.Watch(ctx, metav1.ListOptions{})
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(RetryAfter):
			}
			continue
		}
		for ev := range w.ResultChan() {
			if ev.Type != watch.Added {
				continue
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		w.Stop()
	}
}

func (ct *Controller) Run(ctx context.Context) error {
	interval := ct.c.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	ct.c.log().Info("the Postgres controller runs in the core", "workspace", ct.c.Workspace, "dataRoot", ct.c.DataRoot,
		"socketRoot", ct.c.SocketRoot)
	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { ct.watch(ctx, wake) })
	for {
		if err := ct.Pass(ctx); err != nil {
			ct.c.log().Warn("a reconcile pass failed; servers keep running", "err", err.Error())
		}
		wait := interval
		if ct.pending(ctx) {
			wait = PendingInterval
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		case <-wake:
		}
	}
}

func Run(ctx context.Context, c Config) {
	log := c.log()
	for {
		ct, err := New(c)
		if err == nil {
			err = ct.Run(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		log.Error("the Postgres controller stopped; starting it again", "err", fmt.Sprint(err), "after", RetryAfter.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(RetryAfter):
		}
	}
}
