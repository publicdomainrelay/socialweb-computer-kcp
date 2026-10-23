package hostprocess

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const RemoteTimeout = 2 * time.Minute

type RemoteConfig struct {
	Kubeconfig string
	Workspace  string
	Reader     hostsvc.Manager
	Timeout    time.Duration
	Log        *slog.Logger
}

var _ host.Units = (*Remote)(nil)

type Remote struct {
	hostsvc.Manager
	c      RemoteConfig
	client kube.Resource
}

func NewRemote(c RemoteConfig) (*Remote, error) {
	if c.Reader == nil {
		return nil, fmt.Errorf("hostprocess: a remote placement needs the backend it reads units from")
	}
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(restCfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + c.Workspace
	kc, err := kube.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Remote{Manager: c.Reader, c: c, client: kc.Resource(GVR)}, nil
}

func (r *Remote) log() *slog.Logger {
	if r.c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.c.Log.With("component", "hostprocess-remote", "workspace", r.c.Workspace)
}

func (r *Remote) timeout() time.Duration {
	if r.c.Timeout == 0 {
		return RemoteTimeout
	}
	return r.c.Timeout
}

func ObjectName(unit string) string {
	n := strings.TrimSuffix(strings.TrimPrefix(unit, hostsvc.NamePrefix), hostsvc.ServiceSuffix)
	return strings.ToLower(strings.NewReplacer("@", "-", "_", "-", ".", "-").Replace(n))
}

func restartPolicy(r hostsvc.Restart) v1alpha1.HostProcessRestartPolicy {
	switch r {
	case hostsvc.RestartAlways:
		return v1alpha1.HostProcessRestartAlways
	case hostsvc.RestartOnFailure:
		return v1alpha1.HostProcessRestartOnFailure
	}
	return v1alpha1.HostProcessRestartNever
}

func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	}
	return "SIGTERM"
}

func SpecOf(u hostsvc.UnitSpec) v1alpha1.HostProcessSpec {
	s := v1alpha1.HostProcessSpec{Unit: u.Name, WorkDir: u.WorkDir, RestartPolicy: restartPolicy(u.Restart), After: u.After,
		Wants: u.Wants, Slice: u.Slice, PrivateHostname: u.PrivateHostname, FdStore: int32(u.FDStore), EnvFiles: u.EnvFiles,
		Placement: v1alpha1.HostProcessPlacementSupervisor}
	if len(u.Exec) > 0 {
		s.Exec, s.Argv = u.Exec[0], u.Exec[1:]
	}
	for _, e := range u.Env {
		k, v, _ := strings.Cut(e, "=")
		s.Env = append(s.Env, v1alpha1.HostProcessEnv{Name: k, Value: v})
	}
	if u.Output.StdoutFile != "" || u.Output.StderrFile != "" {
		s.Output = &v1alpha1.HostProcessOutput{Stdout: u.Output.StdoutFile, Stderr: u.Output.StderrFile}
	}
	if u.KillSignal != 0 || u.StopTimeout > 0 {
		s.Stop = &v1alpha1.HostProcessStop{Signal: signalName(u.KillSignal), GraceSeconds: int32((u.StopTimeout + time.Second - 1) / time.Second)}
	}
	l := u.Limits
	if l.MemoryMax > 0 || l.CPUWeight > 0 || l.CPUQuotaPercent > 0 || l.TasksMax > 0 {
		s.Limits = &v1alpha1.HostProcessLimits{CpuWeight: int64(l.CPUWeight), CpuQuotaPercent: int32(l.CPUQuotaPercent), TasksMax: int64(l.TasksMax)}
		if l.MemoryMax > 0 {
			s.Limits.MemoryMax = strconv.FormatUint(l.MemoryMax, 10)
		}
	}
	return s
}

func (r *Remote) Start(ctx context.Context, spec hostsvc.UnitSpec) (hostsvc.UnitStatus, error) {
	if err := hostsvc.ValidateSpec(spec); err != nil {
		return hostsvc.UnitStatus{}, err
	}
	name := ObjectName(spec.Name)
	_, err := kube.Get[v1alpha1.HostProcess](ctx, r.client, name)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return hostsvc.UnitStatus{}, fmt.Errorf("hostprocess: reading %s in %s: %w", name, r.c.Workspace, err)
	default:
		if st, err := r.Manager.Inspect(ctx, spec.Name); err == nil && (st.State == hostsvc.Active || st.State == hostsvc.Activating) {
			st.Adopted = true
			return st, nil
		}
		if err := r.Stop(ctx, spec.Name); err != nil {
			return hostsvc.UnitStatus{}, err
		}
	}
	o := v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		MirrorLabel: string(v1alpha1.HostProcessPlacementSupervisor)}},
		Spec: SpecOf(spec)}
	if _, err := kube.Create[v1alpha1.HostProcess](ctx, r.client, &o, metav1.CreateOptions{}); err != nil {
		return hostsvc.UnitStatus{}, fmt.Errorf("hostprocess: writing %s into %s: %w", name, r.c.Workspace, err)
	}
	r.log().Info("declared", "hostprocess", name, "unit", spec.Name)
	return r.waitPlaced(ctx, name, spec.Name)
}

var errWatchEnded = errors.New("hostprocess: a watch ended")

func (r *Remote) follow(ctx context.Context, name, unit string, done func(o *v1alpha1.HostProcess) (bool, error)) error {
	for {
		err := r.followOnce(ctx, name, unit, done)
		if !errors.Is(err, errWatchEnded) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(RetryAfter):
		}
	}
}

func (r *Remote) followOnce(ctx context.Context, name, unit string, done func(o *v1alpha1.HostProcess) (bool, error)) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	units, err := r.Manager.Watch(wctx, unit)
	if errors.Is(err, hostsvc.ErrUnreachable) {
		return fmt.Errorf("%w: %w", errWatchEnded, err)
	}
	if err != nil {
		return err
	}
	events, stop, err := kube.Watch[v1alpha1.HostProcess](wctx, r.client, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		return fmt.Errorf("%w: %w", errWatchEnded, err)
	}
	defer stop()
	var o *v1alpha1.HostProcess
	if cur, err := kube.Get[v1alpha1.HostProcess](ctx, r.client, name); err == nil {
		o = cur
	}
	for {
		if ok, err := done(o); ok || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok || ev.Type == watch.Error {
				return errWatchEnded
			}
			if ev.Type == watch.Deleted {
				o = nil
				continue
			}
			if ev.Object != nil {
				o = ev.Object
			}
		case _, ok := <-units:
			if !ok {
				return errWatchEnded
			}
		}
	}
}

func (r *Remote) waitPlaced(ctx context.Context, name, unit string) (hostsvc.UnitStatus, error) {
	tctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	var st hostsvc.UnitStatus
	err := r.follow(tctx, name, unit, func(o *v1alpha1.HostProcess) (bool, error) {
		if o == nil {
			return false, nil
		}
		if o.Status.Phase == v1alpha1.HostProcessPhaseFailed && o.Status.Runtime == nil {
			return true, fmt.Errorf("hostprocess: %s in %s failed to place: %s", name, r.c.Workspace, o.Status.Message)
		}
		rt := o.Status.Runtime
		if rt == nil || rt.InvocationID == "" {
			return false, nil
		}
		cur, err := r.Manager.Inspect(tctx, unit)
		if err != nil {
			return false, nil
		}
		if cur.InvocationID == rt.InvocationID && cur.State == hostsvc.Active {
			st = cur
			return true, nil
		}
		if cur.State == hostsvc.Failed {
			st = cur
			return true, fmt.Errorf("hostprocess: %s failed after placement (result %s, status %d)", unit, cur.Result, cur.ExecMainStatus)
		}
		return false, nil
	})
	switch {
	case err == nil:
		return st, nil
	case ctx.Err() != nil:
		return hostsvc.UnitStatus{}, ctx.Err()
	case tctx.Err() != nil:
		return hostsvc.UnitStatus{}, fmt.Errorf("hostprocess: the supervisor of %s did not place %s within %s", r.c.Workspace, unit, r.timeout())
	}
	return st, err
}

func (r *Remote) Stop(ctx context.Context, unit string) error {
	name := ObjectName(unit)
	err := r.client.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("hostprocess: deleting %s in %s: %w", name, r.c.Workspace, err)
	}
	tctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	var st hostsvc.UnitStatus
	err = r.follow(tctx, name, unit, func(*v1alpha1.HostProcess) (bool, error) {
		cur, err := r.Manager.Inspect(tctx, unit)
		st = cur
		return err == nil && !hostsvc.Live(cur), nil
	})
	switch {
	case err == nil:
		r.log().Info("stopped", "hostprocess", name, "unit", unit, "state", string(st.State))
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case tctx.Err() != nil:
		return fmt.Errorf("hostprocess: the supervisor of %s did not stop %s within %s", r.c.Workspace, unit, r.timeout())
	}
	return err
}

func (r *Remote) ResetFailed(ctx context.Context, unit string) error { return nil }

func (r *Remote) StopSlice(ctx context.Context, slice string) error {
	if err := hostsvc.ValidateSlice(slice); err != nil {
		return err
	}
	list, err := kube.List[v1alpha1.HostProcess](ctx, r.client, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := len(list) - 1; i >= 0; i-- {
		o := list[i]
		if !hostsvc.SliceContains(slice, o.Spec.Slice) {
			continue
		}
		if err := r.Stop(ctx, o.Spec.Unit); err != nil {
			return err
		}
	}
	return nil
}
