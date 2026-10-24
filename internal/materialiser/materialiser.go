package materialiser

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identitybundle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
)

const ConditionReady = "Ready"

const PathGateGrace = 10 * time.Second

const (
	ReasonInitializing  = "Initializing"
	ReasonInitTimeout   = "InitTimeout"
	ReasonDraining      = "Draining"
	ReasonBundleFailed  = "IdentityBundleVerifyFailed"
	ReasonBundleHalted  = "IdentityVerificationFailed"
	ReasonAwaitPathShut = "AwaitTenantPathShut"
	ReasonStopped       = "Stopped"
	ReasonTeardownStuck = "TeardownGateTimeout"
	ReasonSessionCap    = "SessionCapRefuses"
	ReasonUp            = "ProcessGroupServing"
	ReasonSettling      = "AwaitRevocationSettled"
	ReasonUnhealthy     = "MembersUnhealthy"
)

const ProbeRetry = 2 * time.Second

const GateTimeout = 10 * time.Second

const DefaultInitTimeout = 3 * time.Minute

type Op string

const (
	OpReap           Op = "reap"
	OpAllocatePorts  Op = "allocate-ports"
	OpSpawn          Op = "spawn"
	OpIdentityHalted Op = "identity-halted"
	OpSpawnFailed    Op = "spawn-failed"
	OpParked         Op = "park-unavailable"
	OpShutPath       Op = "shut-tenant-path"
	OpAwaitPathShut  Op = "await-tenant-path-shut"
	OpPathShut       Op = "tenant-path-shut"
	OpReleasePorts   Op = "release-ports"
	OpReleaseSubnet  Op = "release-subnet"
	OpDropFinalizer  Op = "remove-finalizer"
	OpBlocked        Op = "teardown-blocked-nothing-destroyed"
	OpInstanceBumped Op = "instance-bumped"
)

type PathProbe struct {
	Answered bool
	Status   int
	Err      string
}

func (p PathProbe) String() string {
	if p.Answered {
		return fmt.Sprintf("the tenant front answered %d", p.Status)
	}
	if p.Err == "" {
		return "the tenant front was not probed"
	}
	return "the tenant front did not answer: " + p.Err
}

type Observed struct {
	Cluster v1alpha1.Cluster

	Path PathProbe

	Reachable bool

	IdentityState v1alpha1.IdentityState

	ServiceAccountPresent bool

	Now time.Time
}

type Result struct {
	Phase      v1alpha1.ClusterPhase
	URL        string
	Instance   string
	Conditions []metav1.Condition
	Alert      string
	Ops        []Op

	RequeueAfter time.Duration

	Again bool

	RemoveFinalizer bool

	Halted bool
}

type Leases map[portallocator.Purpose]portallocator.Lease

type StepBuilder func(o Observed, l Leases) ([]procgroup.Step, error)

type IdentityGuard func(ctx context.Context, o Observed) error

type Host interface {
	Live(ctx context.Context, cluster string) bool

	Spawn(ctx context.Context, cluster string, steps []procgroup.Step) error

	Reap(ctx context.Context, cluster string) error

	Unhealthy(ctx context.Context, cluster string) ([]string, error)
}

type Options struct {
	Host  Host
	Ports *portallocator.Allocator

	Networks *subnetallocator.Allocator

	Steps StepBuilder

	Front func(ctx context.Context, cluster string) error

	Identity IdentityGuard

	Alert func(cluster, message string)

	NewInstance func() string

	Now func() time.Time

	InitTimeout time.Duration

	GateTimeout time.Duration

	Purposes []portallocator.Purpose
}

type Reconciler struct {
	opts Options
}

var DefaultPurposes = []portallocator.Purpose{
	portallocator.PurposePostgres,
	portallocator.PurposeKine,
	portallocator.PurposeK3sSecure,
	portallocator.PurposeK3sSlotA,
	portallocator.PurposeK3sControllerManager,
	portallocator.PurposeK3sScheduler,
	portallocator.PurposeK3sCloudControllerManager,
}

func New(opts Options) (*Reconciler, error) {
	if opts.Host == nil {
		return nil, errors.New("materialiser: Host is required")
	}
	if opts.Ports == nil {
		return nil, errors.New("materialiser: Ports is required; the lease file is the authority")
	}
	if opts.Networks == nil {
		return nil, errors.New("materialiser: Networks is required. A cluster's range is leased " +
			"for as long as the cluster is, exactly as its ports are -- the range holds the " +
			"gateway its guests dial the apiserver through, which is baked into k3s's " +
			"advertise-address and its certificate -- so a teardown that dropped the ports and " +
			"kept the range would hold a range per deleted cluster until the pool ran out, and one " +
			"that dropped the range would renumber a cluster whose CR was recreated")
	}
	if opts.Identity == nil {
		return nil, errors.New("materialiser: Identity is required; the sha256 pre-check has no substitute")
	}
	if opts.NewInstance == nil {
		return nil, errors.New("materialiser: NewInstance is required; status.instance is the only discriminator there is")
	}
	if opts.Steps == nil {
		return nil, errors.New("materialiser: Steps is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.InitTimeout == 0 {
		opts.InitTimeout = DefaultInitTimeout
	}
	if opts.GateTimeout == 0 {
		opts.GateTimeout = GateTimeout
	}
	if opts.Purposes == nil {
		opts.Purposes = DefaultPurposes
	}
	return &Reconciler{opts: opts}, nil
}

func (r *Reconciler) Reconcile(ctx context.Context, o Observed) (Result, error) {
	if o.Cluster.Name == "" {
		return Result{}, errors.New("materialiser: observed Cluster has no name")
	}
	if o.Now.IsZero() {
		o.Now = r.opts.Now()
	}

	if o.Cluster.DeletionTimestamp != nil {
		return r.teardown(ctx, o)
	}

	if !o.Cluster.Spec.Running {
		return r.off(ctx, o)
	}

	if halted(o.Cluster.Status.Conditions) {
		return Result{
			Phase:      v1alpha1.ClusterUnavailable,
			URL:        o.Cluster.Status.URL,
			Instance:   o.Cluster.Status.Instance,
			Conditions: carry(o.Cluster.Status.Conditions),
			Ops:        []Op{OpIdentityHalted},
		}, nil
	}
	return r.on(ctx, o)
}

func StartupOwns(c v1alpha1.Cluster) bool {
	return c.Spec.Running && c.DeletionTimestamp == nil && !halted(c.Status.Conditions)
}

func (r *Reconciler) off(ctx context.Context, o Observed) (Result, error) {
	res := Result{
		Phase:      v1alpha1.ClusterUnavailable,
		URL:        o.Cluster.Status.URL,
		Instance:   o.Cluster.Status.Instance,
		Conditions: carry(o.Cluster.Status.Conditions),
	}
	if !shut(o) {
		res.Ops = []Op{OpShutPath, OpAwaitPathShut}
		meta.SetStatusCondition(&res.Conditions, metav1.Condition{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonAwaitPathShut,
			Message:            "phase is Unavailable; waiting for the front to stop admitting tenant requests before reaping",
			ObservedGeneration: o.Cluster.Generation,
		})
		res.RequeueAfter = shutIn(o)
		return res, nil
	}

	res.Ops = []Op{OpPathShut, OpReap}
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionTenantPathShut,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonAwaitPathShut,
		Message:            "the front admits no tenant request; " + o.Path.String(),
		ObservedGeneration: o.Cluster.Generation,
	})
	if err := r.opts.Host.Reap(ctx, o.Cluster.Name); err != nil {
		return res, err
	}
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonStopped,
		Message:            "spec.running is false; the front admits no tenant request and the members are reaped",
		ObservedGeneration: o.Cluster.Generation,
	})
	return res, nil
}

func (r *Reconciler) teardown(ctx context.Context, o Observed) (Result, error) {
	res := Result{
		Phase:      v1alpha1.ClusterDraining,
		URL:        o.Cluster.Status.URL,
		Instance:   o.Cluster.Status.Instance,
		Conditions: carry(o.Cluster.Status.Conditions),
	}
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonDraining,
		Message:            "teardown in progress; the tenant path is shut before anything is destroyed",
		ObservedGeneration: o.Cluster.Generation,
	})

	if refuse, why := r.sessionCapRefuses(o); refuse {
		res.Ops = append(res.Ops, OpBlocked)
		res.Alert = why
		meta.SetStatusCondition(&res.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionTeardownBlocked,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonSessionCap,
			Message:            why,
			ObservedGeneration: o.Cluster.Generation,
		})
		return res, nil
	}

	if !shut(o) {
		res.Ops = append(res.Ops, OpShutPath, OpAwaitPathShut)
		if o.Now.Sub(drainingSince(o, r.opts.Now())) >= r.opts.GateTimeout {
			res.Ops = append(res.Ops, OpBlocked)
			res.Alert = "teardown gate timed out; nothing is destroyed. Stuck-and-billing beats silent loss"
			res.RequeueAfter = shutIn(o)
			meta.SetStatusCondition(&res.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionTeardownBlocked,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonTeardownStuck,
				Message:            fmt.Sprintf("the tenant path gate is not shut after %s; %s", r.opts.GateTimeout, o.Path),
				ObservedGeneration: o.Cluster.Generation,
			})
			return res, nil
		}
		res.RequeueAfter = shutIn(o)
		return res, nil
	}

	res.Ops = append(res.Ops, OpPathShut, OpReap)
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionTenantPathShut,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonAwaitPathShut,
		Message:            "the front admits no tenant request; " + o.Path.String(),
		ObservedGeneration: o.Cluster.Generation,
	})
	if err := r.opts.Host.Reap(ctx, o.Cluster.Name); err != nil {
		return res, err
	}

	res.Ops = append(res.Ops, OpReleasePorts)
	if err := r.opts.Ports.Release(o.Cluster.Name); err != nil {
		return res, err
	}
	res.Ops = append(res.Ops, OpReleaseSubnet)
	if err := r.opts.Networks.Release(o.Cluster.Name); err != nil {
		return res, err
	}
	res.Ops = append(res.Ops, OpDropFinalizer)
	res.RemoveFinalizer = true
	return res, nil
}

func (r *Reconciler) on(ctx context.Context, o Observed) (Result, error) {
	live := r.opts.Host.Live(ctx, o.Cluster.Name)
	if live {
		sick, err := r.opts.Host.Unhealthy(ctx, o.Cluster.Name)
		if err != nil {
			return r.pending(o, "the members of this cluster could not be read: "+err.Error())
		}
		if len(sick) > 0 {
			if readyTrue(o) {
				return r.degraded(o, sick)
			}
			return r.bringUp(ctx, o, false)
		}
	}
	if live && o.Reachable {
		res := Result{
			Phase:      o.Cluster.Status.Phase,
			URL:        o.Cluster.Status.URL,
			Instance:   o.Cluster.Status.Instance,
			Conditions: carry(o.Cluster.Status.Conditions),
		}
		if o.Cluster.Status.Phase != settledReadyPhase {
			return r.claimReady(res, o)
		}
		meta.SetStatusCondition(&res.Conditions, metav1.Condition{
			Type:               ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonUp,
			Message:            "the apiserver answered",
			ObservedGeneration: o.Cluster.Generation,
		})
		return res, nil
	}
	if r.opts.Host.Live(ctx, o.Cluster.Name) {
		return r.pending(o, "the process group is up and the apiserver has not answered yet")
	}
	return r.bringUp(ctx, o, false)
}

func (r *Reconciler) claimReady(res Result, o Observed) (Result, error) {
	settled, err := revocation.WakeSettled(wakeEffect(o))
	if err != nil {
		res.Phase = v1alpha1.ClusterInitializing
		meta.SetStatusCondition(&res.Conditions, metav1.Condition{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonSettling,
			Message:            err.Error(),
			ObservedGeneration: o.Cluster.Generation,
		})
		return res, nil
	}
	phase, err := revocation.Ready(settled)
	if err != nil {
		res.Phase = v1alpha1.ClusterInitializing
		return res, nil
	}
	res.Phase = phase
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonUp,
		Message:            "the apiserver answered and the revocation reconcile is settled",
		ObservedGeneration: o.Cluster.Generation,
	})
	return res, nil
}

func wakeEffect(o Observed) revocation.Effect {
	state := o.IdentityState
	if state == "" {
		state = v1alpha1.IdentityRevoked
	}
	if state == v1alpha1.IdentityRevoked && o.ServiceAccountPresent {
		state = v1alpha1.IdentityRevocationPending
	}
	return revocation.Effect{
		Cluster:  o.Cluster.Name,
		State:    state,
		DeleteSA: state == v1alpha1.IdentityRevocationPending,
	}
}

var settledReadyPhase = func() v1alpha1.ClusterPhase {
	settled, err := revocation.WakeSettled(revocation.Effect{State: v1alpha1.IdentityRevoked})
	if err != nil {
		panic("materialiser: revocation.WakeSettled refused a terminal effect: " + err.Error())
	}
	phase, err := revocation.Ready(settled)
	if err != nil {
		panic("materialiser: revocation.Ready refused a Settled: " + err.Error())
	}
	return phase
}()

func (r *Reconciler) bringUp(ctx context.Context, o Observed, bump bool) (Result, error) {
	res := Result{
		Phase:      v1alpha1.ClusterInitializing,
		URL:        o.Cluster.Status.URL,
		Instance:   o.Cluster.Status.Instance,
		Conditions: carry(o.Cluster.Status.Conditions),
	}
	if c := meta.FindStatusCondition(res.Conditions, ConditionReady); c != nil && c.Reason == ReasonStopped {
		meta.RemoveStatusCondition(&res.Conditions, ConditionReady)
	}
	meta.RemoveStatusCondition(&res.Conditions, v1alpha1.ConditionTenantPathShut)
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonInitializing,
		Message:            "bringing up embedded-postgres -> openbao -> kine -> k3s",
		LastTransitionTime: metav1.NewTime(o.Now),
		ObservedGeneration: o.Cluster.Generation,
	})

	if o.Cluster.Status.Phase == v1alpha1.ClusterInitializing {
		if since := o.Now.Sub(readySince(o, o.Now)); since >= r.opts.InitTimeout {
			return r.park(res, o, fmt.Sprintf("still Initializing after %s; parking in Unavailable", since))
		}
	}

	instance := res.Instance
	if bump || instance == "" {
		instance = r.opts.NewInstance()
		res.Instance = instance
		if bump {
			res.Ops = append(res.Ops, OpInstanceBumped)
		}
	}

	if r.opts.Front != nil {
		if err := r.opts.Front(ctx, o.Cluster.Name); err != nil {
			return res, fmt.Errorf("materialiser: the apiserver front of %s holds the cluster's ports and did not come up: %w", o.Cluster.Name, err)
		}
	}
	leases := Leases{}
	for _, p := range r.opts.Purposes {
		l, err := r.opts.Ports.Allocate(ctx, portallocator.Request{
			Cluster:  o.Cluster.Name,
			Account:  o.Cluster.Spec.Account,
			Instance: instance,
			Purpose:  p,
		})
		if err != nil {
			return res, err
		}
		leases[p] = l
	}
	res.Ops = append(res.Ops, OpAllocatePorts)
	if api, ok := leases[portallocator.PurposeK3sSecure]; ok {
		res.URL = mountURL(api.Port)
	}

	steps, err := r.opts.Steps(o, leases)
	if err != nil {
		return res, err
	}
	steps = guardBeforeK3s(steps, r.opts.Identity, o)

	if err := r.opts.Host.Spawn(ctx, o.Cluster.Name, steps); err != nil {
		if isBundleFailure(err) {
			res.Phase = v1alpha1.ClusterUnavailable
			res.Halted = true
			res.Ops = append(res.Ops, OpIdentityHalted)
			res.Alert = fmt.Sprintf("identity bundle failed verification: %v. Start is halted, not retried", err)
			meta.SetStatusCondition(&res.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionIdentityVerificationFailed,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonBundleHalted,
				Message:            err.Error(),
				LastTransitionTime: metav1.NewTime(o.Now),
				ObservedGeneration: o.Cluster.Generation,
			})
			return res, nil
		}
		res.Ops = append(res.Ops, OpSpawnFailed)
		res.Alert = fmt.Sprintf("the process group did not come up: %v. A spawn failure that "+
			"reports only that it happened leaves the readiness gate's own reason -- and the "+
			"component's log under the cluster's data dir -- as the only account of it", err)
		return res, nil
	}
	res.Ops = append(res.Ops, OpSpawn)

	if !o.Reachable {
		res.RequeueAfter = r.probeIn(o)
		return res, nil
	}

	return r.claimReady(res, o)
}

func readyTrue(o Observed) bool {
	c := meta.FindStatusCondition(o.Cluster.Status.Conditions, ConditionReady)
	return c != nil && c.Status == metav1.ConditionTrue
}

func (r *Reconciler) degraded(o Observed, sick []string) (Result, error) {
	res := Result{
		Phase:      v1alpha1.ClusterInitializing,
		URL:        o.Cluster.Status.URL,
		Instance:   o.Cluster.Status.Instance,
		Conditions: carry(o.Cluster.Status.Conditions),
		Again:      true,
	}
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonUnhealthy,
		Message:            fmt.Sprintf("not every member of the cluster's slice is running and verified; the next pass re-places them: %v", sick),
		LastTransitionTime: metav1.NewTime(o.Now),
		ObservedGeneration: o.Cluster.Generation,
	})
	return res, nil
}

func (r *Reconciler) pending(o Observed, msg string) (Result, error) {
	res := Result{
		Phase:      v1alpha1.ClusterInitializing,
		URL:        o.Cluster.Status.URL,
		Instance:   o.Cluster.Status.Instance,
		Conditions: carry(o.Cluster.Status.Conditions),
	}
	if o.Cluster.Status.Phase == v1alpha1.ClusterInitializing {
		if since := o.Now.Sub(readySince(o, o.Now)); since >= r.opts.InitTimeout {
			return r.park(res, o, fmt.Sprintf("still Initializing after %s; parking in Unavailable", since))
		}
	}
	res.RequeueAfter = r.probeIn(o)
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonInitializing,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(o.Now),
		ObservedGeneration: o.Cluster.Generation,
	})
	return res, nil
}

func (r *Reconciler) park(res Result, o Observed, msg string) (Result, error) {
	res.Phase = v1alpha1.ClusterUnavailable
	res.Ops = append(res.Ops, OpParked)
	res.Alert = msg
	res.RequeueAfter = 0
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonInitTimeout,
		Message:            msg,
		ObservedGeneration: o.Cluster.Generation,
	})
	return res, nil
}

func (r *Reconciler) sessionCapRefuses(o Observed) (bool, string) {
	e := o.Cluster.Status.NodeExpiry
	if e == nil {
		if c := meta.FindStatusCondition(o.Cluster.Status.Conditions, v1alpha1.ConditionReachable); c == nil || c.Status != metav1.ConditionTrue {
			return false, ""
		}
		return true, "status.nodeExpiry is unknown and the apiserver has answered; refusing to tear down against a gate that cannot compare"
	}
	if *e == 0 {
		return false, ""
	}
	if o.Now.Add(r.opts.GateTimeout).Unix() >= *e {
		return true, fmt.Sprintf("the gate would outlive status.nodeExpiry (%d); refusing and re-provisioning", *e)
	}
	return false, ""
}

func shutIn(o Observed) time.Duration {
	since, closed := gateClosedSince(o)
	if !closed {
		return PathGateGrace
	}
	if left := since.Add(PathGateGrace).Sub(o.Now); left > 0 {
		return left
	}
	return PathGateGrace
}

func (r *Reconciler) probeIn(o Observed) time.Duration {
	left := readySince(o, o.Now).Add(r.opts.InitTimeout).Sub(o.Now)
	if left > 0 && left < ProbeRetry {
		return left
	}
	return ProbeRetry
}

func shut(o Observed) bool {
	since, closed := gateClosedSince(o)
	return closed && o.Now.Sub(since) >= PathGateGrace
}

func gateClosedSince(o Observed) (time.Time, bool) {
	var since time.Time
	closed := false
	if d := o.Cluster.DeletionTimestamp; d != nil {
		since, closed = d.Time, true
	}
	if o.Cluster.Status.Phase == v1alpha1.ClusterReady {
		return since, closed
	}
	c := meta.FindStatusCondition(o.Cluster.Status.Conditions, ConditionReady)
	switch {
	case c == nil:
		return time.Time{}, true
	case c.Status == metav1.ConditionTrue:
		return since, closed
	case c.LastTransitionTime.IsZero():
		return time.Time{}, true
	case !closed || c.LastTransitionTime.Time.Before(since):
		return c.LastTransitionTime.Time, true
	}
	return since, closed
}

func readySince(o Observed, fallback time.Time) time.Time {
	c := meta.FindStatusCondition(o.Cluster.Status.Conditions, ConditionReady)
	if c == nil || c.LastTransitionTime.IsZero() {
		return fallback
	}
	return c.LastTransitionTime.Time
}

func drainingSince(o Observed, fallback time.Time) time.Time {
	c := meta.FindStatusCondition(o.Cluster.Status.Conditions, ConditionReady)
	if c == nil || c.Reason != ReasonDraining || c.LastTransitionTime.IsZero() {
		return fallback
	}
	return c.LastTransitionTime.Time
}

func halted(conds []metav1.Condition) bool {
	c := meta.FindStatusCondition(conds, v1alpha1.ConditionIdentityVerificationFailed)
	return c != nil && c.Status == metav1.ConditionTrue
}

func carry(conds []metav1.Condition) []metav1.Condition {
	out := make([]metav1.Condition, len(conds))
	copy(out, conds)
	return out
}

func isBundleFailure(err error) bool {
	return errors.Is(err, identitybundle.ErrMismatch) || errors.Is(err, identitybundle.ErrIncomplete)
}

func mountURL(advertisePort int) string {
	return fmt.Sprintf("https://127.0.0.1:%d", advertisePort)
}

func guardBeforeK3s(steps []procgroup.Step, guard IdentityGuard, o Observed) []procgroup.Step {
	if guard == nil || len(steps) < 2 {
		return steps
	}
	idx := -1
	for i, s := range steps {
		if s.Name == procgroup.ComponentK3s {
			idx = i
			break
		}
	}
	if idx < 1 {
		return steps
	}
	prev := steps[idx-1]
	inner := prev.Ready
	prev.Ready = func(ctx context.Context, p procgroup.Member) error {
		if err := inner(ctx, p); err != nil {
			return err
		}
		return guard(ctx, o)
	}
	steps[idx-1] = prev
	return steps
}
