package infra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "infra"

	EphemeralName = "ephemeralinfra"

	Export = "computecontracts"

	RecordExport = "marketidentities"

	CollectionKind = "InfraCollection"

	EphemeralCollectionKind = "EphemeralInfraCollection"
)

type Options struct {
	Bus *bus.Bus

	Dynamic kube.Factory

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	bus *bus.Bus

	dynamic kube.Factory

	log *slog.Logger

	mu sync.Mutex

	bound map[string]map[indexKey]string
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, dynamic: opts.Dynamic, log: log.With("controller", Name), bound: map[string]map[indexKey]string{}}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Bus == nil {
		return nil, errors.New("infra: the bus is required")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("infra: the dynamic client factory is required")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme, exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	c := New(opts)
	if err := register(m, c, Name, infraOps, opts.Scope); err != nil {
		return nil, err
	}
	if err := register(m, c, EphemeralName, ephemeralOps, opts.Scope); err != nil {
		return nil, err
	}
	return c, nil
}

func register[T ctlruntime.Object](m *ctlruntime.Manager, c *Controller, name string, ops kindOps[T], scope func(string) bool) error {
	r := &reconciler[T]{c: c, ops: ops, manager: ctlruntime.FieldManagerPrefix + name, pullManager: ctlruntime.FieldManagerPrefix + name + "-record"}
	kind := string(ops.kind)
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Finalizer(v1alpha1.FinalizerInfra), ctlruntime.Owns[*v1alpha1.ComputeContract](),
		ctlruntime.WatchesIn(RecordExport, func(_ context.Context, _ ctlruntime.Request, obj *v1alpha1.AtprotoRecord) []ctlruntime.Request {
			return labelled(obj, kind)
		}),
		ctlruntime.WatchesIn(RecordExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch) []ctlruntime.Request {
			return append(labelled(obj, kind), c.watched(src.Cluster, kind, obj)...)
		}),
		ctlruntime.Gone(func(_ context.Context, req ctlruntime.Request) { c.forget(kind, req) })}
	if scope != nil {
		options = append(options, ctlruntime.InWorkspaces(scope))
	}
	if err := ctlruntime.For[T](m, name, r, options...); err != nil {
		return fmt.Errorf("infra: registering the %s controller: %w", kind, err)
	}
	return nil
}

func labelled(obj metav1.Object, kind string) []ctlruntime.Request {
	labels := obj.GetLabels()
	if labels[v1alpha1.InfraKindLabel] != kind || labels[v1alpha1.InfraNameLabel] == "" {
		return nil
	}
	return []ctlruntime.Request{{Name: labels[v1alpha1.InfraNameLabel]}}
}

type view struct {
	kind infra.Kind

	object metav1.Object

	spec v1alpha1.InfraSpec

	declaration infra.Declaration

	status v1alpha1.InfraStatus

	ephemeral *v1alpha1.EphemeralInfraStatus
}

type kindOps[T ctlruntime.Object] struct {
	kind infra.Kind

	view func(T) view

	statusObject func(name string, status v1alpha1.InfraStatus, ephemeral *v1alpha1.EphemeralInfraStatus) T
}

var infraOps = kindOps[*v1alpha1.Infra]{
	kind: infra.KindInfra,
	view: func(o *v1alpha1.Infra) view {
		return view{kind: infra.KindInfra, object: o, spec: o.Spec, declaration: infra.FromSpec(o.Spec), status: o.Status}
	},
	statusObject: func(name string, status v1alpha1.InfraStatus, _ *v1alpha1.EphemeralInfraStatus) *v1alpha1.Infra {
		return &v1alpha1.Infra{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: status}
	},
}

var ephemeralOps = kindOps[*v1alpha1.EphemeralInfra]{
	kind: infra.KindEphemeralInfra,
	view: func(o *v1alpha1.EphemeralInfra) view {
		st := o.Status
		return view{kind: infra.KindEphemeralInfra, object: o, spec: o.Spec.InfraSpec, declaration: infra.FromEphemeralSpec(o.Spec),
			status: o.Status.InfraStatus, ephemeral: &st}
	},
	statusObject: func(name string, status v1alpha1.InfraStatus, ephemeral *v1alpha1.EphemeralInfraStatus) *v1alpha1.EphemeralInfra {
		out := &v1alpha1.EphemeralInfra{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if ephemeral != nil {
			out.Status = *ephemeral
		}
		out.Status.InfraStatus = status
		return out
	},
}

type reconciler[T ctlruntime.Object] struct {
	c *Controller

	ops kindOps[T]

	manager string

	pullManager string
}

func (r *reconciler[T]) self(req ctlruntime.Request) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: string(r.ops.kind), Name: req.Name, Workspace: req.Workspace}
}

func (r *reconciler[T]) Reconcile(ctx context.Context, req ctlruntime.Request, obj T) (ctlruntime.Result, error) {
	v := r.ops.view(obj)
	contracts, err := r.c.owned(ctx, req, v.kind)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if !v.object.GetDeletionTimestamp().IsZero() {
		return r.finalize(ctx, req, obj, v, contracts)
	}
	now := time.Now().UTC()
	status := v1alpha1.InfraStatus{ObservedGeneration: v.object.GetGeneration(), Record: v.status.Record,
		Conditions: slices.Clone(v.status.Conditions)}
	var rec *recordSync
	if v.spec.Record != nil {
		rec, err = r.syncRecord(ctx, req, v, now)
		if err != nil {
			conds.Set(&status.Conditions, status.ObservedGeneration, v1alpha1.InfraConditionSynced, false, "RecordUnavailable", err.Error())
			status.Phase, status.Contracts, status.Message = v.status.Phase, v.status.Contracts, err.Error()
			return ctlruntime.Result{}, errors.Join(err, req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status, v.ephemeral)))
		}
		for _, cond := range rec.conditions {
			conds.Set(&status.Conditions, status.ObservedGeneration, cond.Type, cond.Status == metav1.ConditionTrue, cond.Reason, cond.Message)
		}
		status.Record = rec.status
		switch rec.action {
		case infra.ActionDelete:
			r.c.log.Info("infra_record_gone", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", rec.uri)
			return ctlruntime.Result{}, req.Delete(ctx, obj)
		case infra.ActionPull, infra.ActionConflict:
			if err := r.pull(ctx, req, v, rec.declaration); err != nil {
				return ctlruntime.Result{}, err
			}
			r.c.log.Info("infra_record_pulled", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", rec.uri, "cid", rec.cid,
				"conflict", rec.action == infra.ActionConflict, "generation", v.object.GetGeneration())
			status.Phase, status.Contracts, status.Message = v.status.Phase, v.status.Contracts, "pulled the record's declaration"
			return ctlruntime.Result{}, req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status, v.ephemeral))
		}
	} else {
		r.c.forget(string(v.kind), req)
		if v.status.Record != nil {
			gone, err := r.deleteRecord(ctx, req, v)
			if err != nil {
				return ctlruntime.Result{}, err
			}
			if gone {
				status.Record = nil
				conds.Set(&status.Conditions, status.ObservedGeneration, v1alpha1.InfraConditionSynced, false, "Unbound", "spec.record is not set")
			}
		}
	}
	var ephemeral *v1alpha1.EphemeralInfraStatus
	var end infra.End
	if v.ephemeral != nil {
		end = infra.Ending(v.object.GetCreationTimestamp().Time, v.declaration, *v.ephemeral, now)
		ephemeral = &v1alpha1.EphemeralInfraStatus{ExpiresAt: end.ExpiresAt.Format(time.RFC3339)}
		if end.Ended {
			ephemeral.EndedAt, ephemeral.EndReason = end.At.Format(time.RFC3339), end.Reason
		}
	}
	statuses, left, err := r.contracts(ctx, req, v, contracts, end.Ended)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	status.Contracts = statuses
	status.Phase = infra.Phase(statuses)
	status.Message = summary(statuses)
	if end.Ended {
		status.Phase = v1alpha1.InfraPhaseEnding
		status.Message = fmt.Sprintf("ended (%s); %d contract(s) releasing", end.Reason, left)
	}
	if err := req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status, ephemeral)); err != nil {
		return ctlruntime.Result{}, err
	}
	if rec != nil && rec.write {
		observed := infra.Observed{Phase: status.Phase, Contracts: statuses}
		if ephemeral != nil {
			observed.ExpiresAt, observed.EndedAt, observed.EndReason = ephemeral.ExpiresAt, ephemeral.EndedAt, ephemeral.EndReason
		}
		if err := r.writeRecord(ctx, req, v, rec, observed); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	if end.Ended {
		if left == 0 {
			r.c.log.Info("infra_ephemeral_ended", "workspace", req.Workspace, "name", req.Name, "reason", end.Reason)
			return ctlruntime.Result{}, req.Delete(ctx, obj)
		}
		return ctlruntime.Result{}, nil
	}
	if v.ephemeral != nil {
		return ctlruntime.Result{RequeueAfter: max(time.Until(end.ExpiresAt), time.Second)}, nil
	}
	return ctlruntime.Result{}, nil
}

func summary(statuses []v1alpha1.InfraContractStatus) string {
	count := map[v1alpha1.InfraContractPhase]int{}
	for _, s := range statuses {
		count[s.Phase]++
	}
	var parts []string
	for _, p := range []v1alpha1.InfraContractPhase{v1alpha1.InfraContractPhaseStaged, v1alpha1.InfraContractPhaseRequesting,
		v1alpha1.InfraContractPhaseDeployed, v1alpha1.InfraContractPhaseReleasing, v1alpha1.InfraContractPhaseReleased,
		v1alpha1.InfraContractPhaseFailed} {
		if count[p] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count[p], strings.ToLower(string(p))))
		}
	}
	if len(parts) == 0 {
		return "no contracts declared"
	}
	return strings.Join(parts, ", ")
}

func (r *reconciler[T]) finalize(ctx context.Context, req ctlruntime.Request, obj T, v view, contracts []v1alpha1.ComputeContract) (ctlruntime.Result, error) {
	left := 0
	statuses := make([]v1alpha1.InfraContractStatus, 0, len(contracts))
	for i := range contracts {
		cc := &contracts[i]
		step := infra.Orphan(cc)
		if err := r.apply(ctx, req, v, cc, step); err != nil {
			return ctlruntime.Result{Waiting: true}, err
		}
		statuses = append(statuses, contractStatus(cc.Labels[v1alpha1.InfraContractLabel], cc, step.Phase))
		left++
	}
	status := v1alpha1.InfraStatus{ObservedGeneration: v.object.GetGeneration(), Phase: v1alpha1.InfraPhaseDeleting, Contracts: statuses,
		Record: v.status.Record, Conditions: v.status.Conditions,
		Message: fmt.Sprintf("%d ComputeContract(s) releasing", left)}
	if left == 0 && v.spec.Record != nil {
		gone, err := r.deleteRecord(ctx, req, v)
		if err != nil {
			return ctlruntime.Result{Waiting: true}, err
		}
		if !gone {
			status.Message = "the record is being deleted"
			left = 1
		}
	}
	if err := req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status, v.ephemeral)); err != nil {
		return ctlruntime.Result{Waiting: true}, err
	}
	if left > 0 {
		return ctlruntime.Result{Waiting: true}, nil
	}
	r.c.forget(string(v.kind), req)
	r.c.log.Info("infra_released", "kind", v.kind, "workspace", req.Workspace, "name", req.Name)
	return ctlruntime.Result{}, nil
}
