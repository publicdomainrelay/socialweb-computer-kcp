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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/netretry"
)

const (
	Name = "infra"

	EphemeralName = "ephemeralinfra"

	Export = "computecontracts"

	RecordExport = "marketidentities"

	RepoExport = "applications"

	LinkExport = "exposures"

	repoBindQueue = 64

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

	pulls map[indexKey]pullNote

	moved map[indexKey]string

	unwritten map[indexKey]bool

	retained map[indexKey]bool

	repos map[string]*binding.Table[struct{}]

	retries *netretry.Tracker[indexKey]
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, dynamic: opts.Dynamic, log: log.With("controller", Name), bound: map[string]map[indexKey]string{},
		pulls: map[indexKey]pullNote{}, moved: map[indexKey]string{}, unwritten: map[indexKey]bool{}, retained: map[indexKey]bool{}, retries: netretry.NewTracker[indexKey](), repos: map[string]*binding.Table[struct{}]{
			string(infra.KindInfra): binding.New[struct{}](), string(infra.KindEphemeralInfra): binding.New[struct{}]()}}
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
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Finalizer(v1alpha1.FinalizerInfra),
		ctlruntime.WatchesFiltered("", ctlruntime.WatchFilter[*v1alpha1.ComputeContract]{Changed: contractChanged},
			func(_ context.Context, _ ctlruntime.Request, obj *v1alpha1.ComputeContract) []ctlruntime.Request {
				return labelled(obj, kind)
			}),
		ctlruntime.WatchesIn(RecordExport, func(_ context.Context, _ ctlruntime.Request, obj *v1alpha1.AtprotoRecord) []ctlruntime.Request {
			return labelled(obj, kind)
		}),
		ctlruntime.WatchesFiltered(RecordExport, ctlruntime.WatchFilter[*v1alpha1.AtprotoRecordWatch]{Changed: recordWatchChanged},
			func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch) []ctlruntime.Request {
				return append(labelled(obj, kind), c.watched(src.Cluster, kind, obj)...)
			}),
		ctlruntime.WatchesFiltered(RepoExport, ctlruntime.WatchFilter[*v1alpha1.AtprotoRepo]{Changed: func(old, new *v1alpha1.AtprotoRepo) bool {
			return old.Status.DID != new.Status.DID
		}}, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRepo) []ctlruntime.Request {
			return c.repoUsers(kind, src.Workspace, obj.Name)
		}),
		RepoLinkWatch(func(workspace, name string) []ctlruntime.Request { return c.repoUsers(kind, workspace, name) }),
		ctlruntime.Events(RepoBindEvents(c.bus, func(workspace, name string) []ctlruntime.Request { return c.repoUsers(kind, workspace, name) })),
		ctlruntime.Gone(func(_ context.Context, req ctlruntime.Request) { c.forget(kind, req) })}
	if scope != nil {
		options = append(options, ctlruntime.InWorkspaces(scope))
	}
	if err := ctlruntime.For[T](m, name, r, options...); err != nil {
		return fmt.Errorf("infra: registering the %s controller: %w", kind, err)
	}
	return nil
}

func RepoBindEvents(b *bus.Bus, users func(workspace, name string) []ctlruntime.Request) <-chan ctlruntime.Request {
	ch := make(chan ctlruntime.Request, repoBindQueue)
	b.Subscribe(func(key bus.Key) {
		if key.Kind != RepoKind {
			return
		}
		for _, req := range users(key.Workspace, key.Name) {
			select {
			case ch <- req:
			default:
			}
		}
	})
	return ch
}

func RepoLinkWatch(users func(workspace, name string) []ctlruntime.Request) ctlruntime.Option {
	return ctlruntime.WatchesFiltered(LinkExport, ctlruntime.WatchFilter[*exposurev1alpha1.ComponentLink]{Changed: linkChanged}, func(_ context.Context, src ctlruntime.Request, obj *exposurev1alpha1.ComponentLink) []ctlruntime.Request {
		to := obj.Spec.To
		if !strings.EqualFold(strings.TrimSpace(to.Kind), RepoKind) {
			return nil
		}
		return users(to.InWorkspace(src.Workspace), to.Name)
	})
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
	if err := r.retainOnce(ctx, req, v); err != nil {
		return ctlruntime.Result{}, err
	}
	now := time.Now().UTC()
	status := v1alpha1.InfraStatus{ObservedGeneration: v.object.GetGeneration(), Record: v.status.Record,
		Conditions: slices.Clone(v.status.Conditions)}
	var ephemeral *v1alpha1.EphemeralInfraStatus
	var end infra.End
	ending := func(recorded infra.Decoded) {
		if v.ephemeral == nil {
			return
		}
		end = infra.Ending(v.object.GetCreationTimestamp().Time, v.declaration, *v.ephemeral, recorded, now)
		ephemeral = &v1alpha1.EphemeralInfraStatus{ExpiresAt: end.ExpiresAt.Format(time.RFC3339)}
		if end.Ended {
			ephemeral.EndedAt, ephemeral.EndReason = end.At.Format(time.RFC3339), end.Reason
		}
	}
	ending(infra.Decoded{})
	var rec *recordSync
	var recordErr error
	backoff := false
	var retry time.Duration
	finished := contractsFinished(v, contracts)
	if v.spec.Record != nil && !r.settled(ctx, req, v, finished) {
		rec, err = r.syncRecord(ctx, req, v, now)
		reason, isUnresolved := Unresolved(err)
		r.noteUnresolved(req, v, &status, isUnresolved, reason, err)
		if finished {
			r.c.retries.Reset(keyOf(req, v))
			if ReadRetrying(err) {
				r.c.log.Info("infra_record_read_failed", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "finished", true, "err", err.Error())
			}
		} else {
			retry = r.readRetry(req, v, err)
		}
		if err != nil {
			syncedReason, message := "RecordUnavailable", err.Error()
			if errors.Is(err, ErrRepoNoDID) {
				syncedReason = "RepoPending"
			}
			if isUnresolved {
				syncedReason = reason
			}
			if retry > 0 {
				syncedReason, message = v1alpha1.InfraReasonRecordReadRetrying, v1alpha1.InfraMessageRecordReadRetrying
			}
			conds.Set(&status.Conditions, status.ObservedGeneration, v1alpha1.InfraConditionSynced, false, syncedReason, message)
			recordErr = err
			if errors.Is(err, ErrRepoNoDID) || isUnresolved || retry > 0 || finished {
				recordErr = nil
			}
			backoff = isUnresolved && !finished
			if !Publishing(v.spec.Record) && end.Ended {
				recordErr = nil
			} else if !Publishing(v.spec.Record) {
				if err := r.releaseDeclared(ctx, req, v, contracts); err != nil {
					return ctlruntime.Result{}, errors.Join(recordErr, err)
				}
				status.Phase, status.Contracts, status.Message = v.status.Phase, v.status.Contracts, message
				return requeueAt(v, end, backoff, retry), errors.Join(recordErr, req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status, ephemeral)))
			}
		}
	}
	frozen := v.spec.Record != nil && meta.IsStatusConditionTrue(v.status.Conditions, v1alpha1.InfraConditionRecordEnded)
	if rec != nil {
		recorded := rec.recorded
		if slices.ContainsFunc(contracts, func(cc v1alpha1.ComputeContract) bool { return !infra.Finished(&cc) }) {
			recorded.CreatedAt = time.Time{}
		}
		ending(recorded)
		frozen = rec.action == infra.ActionEnded
		if !frozen {
			meta.RemoveStatusCondition(&status.Conditions, v1alpha1.InfraConditionRecordEnded)
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
			r.c.notePull(string(v.kind), req, rec.status.DeclarationHash, status.Conditions)
			r.c.log.Info("infra_record_pulled", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", rec.uri, "cid", rec.cid,
				"conflict", rec.action == infra.ActionConflict, "generation", v.object.GetGeneration())
			status.Phase, status.Contracts, status.Message = v.status.Phase, v.status.Contracts, "pulled the record's declaration"
			return ctlruntime.Result{}, req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status, v.ephemeral))
		}
	} else if v.spec.Record == nil {
		ended := true
		if v.status.Record != nil && v.status.Record.Direction != v1alpha1.InfraSyncRetained {
			done, err := r.finalWrite(ctx, req, v)
			switch {
			case err != nil && ReadRetrying(err):
				ended, retry = false, r.readRetry(req, v, err)
			case err != nil:
				r.c.log.Warn("infra_record_end_skipped", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", v.status.Record.URI,
					"err", err.Error())
			default:
				ended = done
			}
		}
		if ended {
			r.c.forget(string(v.kind), req)
		}
		if ended && v.status.Record != nil && v.status.Record.Direction != v1alpha1.InfraSyncRetained {
			gone, err := r.unbindRecord(ctx, req, v)
			if err != nil {
				return ctlruntime.Result{}, err
			}
			if gone {
				kept := *v.status.Record
				kept.Direction = v1alpha1.InfraSyncRetained
				status.Record = &kept
				conds.Set(&status.Conditions, status.ObservedGeneration, v1alpha1.InfraConditionSynced, false, "Unbound",
					"spec.record is not set; the record stays in the repo")
			}
		}
	}
	statuses, left, err := r.contracts(ctx, req, v, contracts, end.Ended || frozen)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	status.Contracts = statuses
	status.Phase = infra.Phase(statuses)
	status.Message = summary(statuses)
	switch {
	case end.Ended:
		status.Phase = v1alpha1.InfraPhaseEnding
		status.Message = fmt.Sprintf("ended (%s); %d contract(s) releasing", end.Reason, left)
	case frozen && left > 0:
		status.Phase = v1alpha1.InfraPhaseEnding
		status.Message = fmt.Sprintf("the record ended; %d contract(s) releasing", left)
	case frozen:
		status.Phase = v1alpha1.InfraPhaseEnded
		status.Message = "the record ended; it is kept and this object drives nothing"
	}
	if rec != nil && rec.write {
		observed := infra.Observed{Phase: status.Phase, Contracts: statuses}
		if ephemeral != nil {
			observed.ExpiresAt, observed.EndedAt, observed.EndReason = ephemeral.ExpiresAt, ephemeral.EndedAt, ephemeral.EndReason
		}
		r.c.setUnwritten(keyOf(req, v), true)
		if err := r.writeRecord(ctx, req, v, rec, observed); err != nil {
			return ctlruntime.Result{}, err
		}
		r.c.setUnwritten(keyOf(req, v), false)
	}
	if err := req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status, ephemeral)); err != nil {
		return ctlruntime.Result{}, err
	}
	if recordErr != nil {
		return ctlruntime.Result{}, recordErr
	}
	if end.Ended {
		if left == 0 {
			r.c.log.Info("infra_ephemeral_ended", "workspace", req.Workspace, "name", req.Name, "reason", end.Reason)
			return ctlruntime.Result{}, req.Delete(ctx, obj)
		}
		return requeueAt(v, end, backoff, retry), nil
	}
	return requeueAt(v, end, backoff, retry), nil
}

func requeueAt(v view, end infra.End, backoff bool, retry time.Duration) ctlruntime.Result {
	after := retry
	if v.ephemeral != nil && !end.Ended {
		if deadline := max(time.Until(end.ExpiresAt), time.Second); after <= 0 || deadline < after {
			after = deadline
		}
	}
	return ctlruntime.Result{RequeueAfter: after, Backoff: backoff}
}

func keyOf(req ctlruntime.Request, v view) indexKey {
	return indexKey{kind: string(v.kind), cluster: req.Cluster, name: req.Name}
}

func (r *reconciler[T]) readRetry(req ctlruntime.Request, v view, err error) time.Duration {
	return ReadRetry(r.c.log, r.c.retries, keyOf(req, v), err, "kind", v.kind, "workspace", req.Workspace, "name", req.Name)
}

func contractsFinished(v view, contracts []v1alpha1.ComputeContract) bool {
	if len(v.declaration.Contracts) == 0 || len(v.status.Contracts) != len(v.declaration.Contracts) {
		return false
	}
	for _, s := range v.status.Contracts {
		if s.Phase != v1alpha1.InfraContractPhaseReleased && s.Phase != v1alpha1.InfraContractPhaseFailed {
			return false
		}
	}
	for i := range contracts {
		if !infra.Finished(&contracts[i]) {
			return false
		}
	}
	return true
}

func ReadRetry[K comparable](log *slog.Logger, retries *netretry.Tracker[K], key K, err error, attrs ...any) time.Duration {
	if !ReadRetrying(err) {
		retries.Reset(key)
		return 0
	}
	attempt, delay := retries.Fail(key)
	log.Info("infra_record_read_retrying", append(attrs, "attempt", attempt, "retryIn", delay.String(), "err", err.Error())...)
	return delay
}

func (r *reconciler[T]) noteUnresolved(req ctlruntime.Request, v view, status *v1alpha1.InfraStatus, isUnresolved bool, reason string, err error) {
	if !isUnresolved {
		err = nil
	}
	NoteUnresolved(r.c.log, v.status.Conditions, &status.Conditions, status.ObservedGeneration, reason, err,
		"kind", v.kind, "workspace", req.Workspace, "name", req.Name)
}

func NoteUnresolved(log *slog.Logger, prior []metav1.Condition, conditions *[]metav1.Condition, generation int64, reason string, err error, attrs ...any) {
	held := meta.FindStatusCondition(prior, v1alpha1.InfraConditionRecordUnresolved)
	if err == nil {
		if held == nil {
			return
		}
		meta.RemoveStatusCondition(conditions, v1alpha1.InfraConditionRecordUnresolved)
		if held.Status == metav1.ConditionTrue {
			log.Info("infra_record_resolved", attrs...)
		}
		return
	}
	conds.Set(conditions, generation, v1alpha1.InfraConditionRecordUnresolved, true, reason, err.Error())
	if held == nil || held.Status != metav1.ConditionTrue || held.Reason != reason || held.Message != err.Error() {
		log.Info("infra_record_unresolved", append(attrs, "reason", reason, "err", err.Error())...)
	}
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
		Record: v.status.Record, Conditions: slices.Clone(v.status.Conditions),
		Message: fmt.Sprintf("%d ComputeContract(s) releasing", left)}
	if left == 0 && len(v.status.Contracts) > 0 {
		status.Contracts = v.status.Contracts
	}
	backoff := false
	var retry, giveUp, deadline time.Duration
	if left == 0 && (v.spec.Record != nil || (v.status.Record != nil && v.status.Record.Direction != v1alpha1.InfraSyncRetained)) {
		done, err := r.finalWrite(ctx, req, v)
		reason, isUnresolved := Unresolved(err)
		r.noteUnresolved(req, v, &status, isUnresolved, reason, err)
		retry = r.readRetry(req, v, err)
		orphaned := RecordOrphaned(err)
		giveUp = atprotorepo.RecordDeleteGiveUp - time.Since(v.object.GetDeletionTimestamp().Time)
		switch {
		case (err != nil || !done) && (orphaned || giveUp <= 0):
			if !orphaned {
				reason = "FinalWriteGaveUp"
			}
			message := "the final record write gave up"
			if err != nil {
				message = err.Error()
			}
			conds.Set(&status.Conditions, status.ObservedGeneration, v1alpha1.InfraConditionRecordOrphaned, true, reason, message)
			status.Message = "the record is kept with its last status: " + message
			r.c.log.Warn("infra_record_orphaned", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "reason", reason, "err", message)
		case isUnresolved || errors.Is(err, ErrRepoNoDID):
			status.Message = "the final record write waits: " + err.Error()
			left, backoff = 1, true
		case retry > 0:
			status.Message = "the final record write waits: " + v1alpha1.InfraMessageRecordReadRetrying
			left = 1
		case err != nil:
			return ctlruntime.Result{Waiting: true}, err
		case !done:
			status.Message = "writing the record's final status; the record is kept"
			left, deadline = 1, giveUp
		}
	}
	if left == 0 && v.status.Record != nil {
		gone, err := r.unbindRecord(ctx, req, v)
		if err != nil {
			return ctlruntime.Result{Waiting: true}, err
		}
		if !gone {
			status.Message = "releasing the AtprotoRecord; the record is kept"
			left = 1
		}
	}
	if err := req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status, v.ephemeral)); err != nil {
		return ctlruntime.Result{Waiting: true}, err
	}
	if left > 0 {
		after := retry
		if after <= 0 {
			after = deadline
		}
		return ctlruntime.Result{Waiting: true, Backoff: backoff, RequeueAfter: after}, nil
	}
	r.c.forget(string(v.kind), req)
	r.c.log.Info("infra_released", "kind", v.kind, "workspace", req.Workspace, "name", req.Name)
	return ctlruntime.Result{}, nil
}
