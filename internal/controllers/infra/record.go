package infra

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/netretry"
)

const RepoKind = "AtprotoRepo"

var ErrRepoNoDID = errors.New("reports no did yet")

var ErrRecordUnresolved = errors.New("infra: the record's repo cannot be resolved")

type unresolved struct {
	reason string

	err error
}

func (e unresolved) Error() string { return e.err.Error() }

func (e unresolved) Unwrap() []error { return []error{ErrRecordUnresolved, e.err} }

func Unresolved(err error) (string, bool) {
	var u unresolved
	if errors.As(err, &u) {
		return u.reason, true
	}
	return "", false
}

type readRetrying struct {
	err error
}

func (e readRetrying) Error() string { return e.err.Error() }

func (e readRetrying) Unwrap() error { return e.err }

func ReadFailed(err error) error {
	if netretry.Transient(err) {
		return readRetrying{err: err}
	}
	return err
}

func ReadRetrying(err error) bool {
	var r readRetrying
	return errors.As(err, &r)
}

type recordSync struct {
	action infra.Action

	uri string

	cid string

	rkey string

	repo commonv1.ObjectRef

	declaration infra.Declaration

	render infra.Declaration

	publish bool

	createdAt time.Time

	status *v1alpha1.InfraRecordStatus

	conditions []metav1.Condition

	write bool

	swapCid string

	recorded infra.Decoded
}

func recordURI(did, nsid, rkey string) string {
	return "at://" + did + "/" + nsid + "/" + rkey
}

func RepoRef(workspace string, ref commonv1.ObjectRef) (commonv1.ObjectRef, error) {
	kind := strings.TrimSpace(ref.Kind)
	if kind != "" && !strings.EqualFold(kind, RepoKind) {
		return commonv1.ObjectRef{}, unresolved{reason: "RepoRefInvalid", err: fmt.Errorf("infra: the repoRef must name an %s, not %s", RepoKind, kind)}
	}
	if strings.TrimSpace(ref.Name) == "" {
		return commonv1.ObjectRef{}, unresolved{reason: "RepoRefInvalid", err: errors.New("infra: the repoRef names no AtprotoRepo")}
	}
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: RepoKind, Name: strings.TrimSpace(ref.Name), Workspace: ref.InWorkspace(workspace)}, nil
}

func Repo(ctx context.Context, b *bus.Bus, dynamic kube.Factory, self, ref commonv1.ObjectRef, nsids ...string) (string, atprotorepo.RecordReader, error) {
	dyn, err := dynamic(ref.Workspace)
	if err != nil {
		return "", nil, err
	}
	repo, err := kube.Get[v1alpha1.AtprotoRepo](ctx, dyn.Resource(v1alpha1.AtprotoRepoGVR), ref.Name)
	if apierrors.IsNotFound(err) {
		return "", nil, unresolved{reason: "RepoNotFound", err: fmt.Errorf("infra: the AtprotoRepo %s does not exist", ref.String())}
	}
	if err != nil {
		return "", nil, fmt.Errorf("infra: reading the AtprotoRepo %s: %w", ref.String(), err)
	}
	did := strings.TrimSpace(repo.Status.DID)
	if did == "" {
		if backendGone(ctx, dynamic, ref.Workspace, repo.Spec.BackendRef) {
			return "", nil, unresolved{reason: "RepoBackendGone", err: fmt.Errorf("infra: the AtprotoRepo %s has no DID and its backend %s %s does not exist",
				ref.String(), repo.Spec.BackendRef.Kind, repo.Spec.BackendRef.Name)}
		}
		return "", nil, fmt.Errorf("infra: the AtprotoRepo %s %w", ref.String(), ErrRepoNoDID)
	}
	reader, err := bus.Resolve[atprotorepo.RecordReader](ctx, b, self, ref, bus.Capability{Name: atprotorepo.CapabilityRead, Scope: nsids})
	if err != nil {
		return "", nil, unresolved{reason: "RepoUnreadable", err: fmt.Errorf("infra: the repo.read handle of %s: %w", ref.String(), err)}
	}
	return did, reader, nil
}

func backendGone(ctx context.Context, dynamic kube.Factory, workspace string, ref commonv1.ObjectRef) bool {
	var gvr schema.GroupVersionResource
	switch strings.TrimSpace(ref.Kind) {
	case "LocalRepo":
		gvr = v1alpha1.LocalRepoGVR
	case "RemoteRepo":
		gvr = v1alpha1.RemoteRepoGVR
	default:
		return false
	}
	dyn, err := dynamic(ref.InWorkspace(workspace))
	if err != nil {
		return false
	}
	_, err = kube.Get[metav1.PartialObjectMetadata](ctx, dyn.Resource(gvr), strings.TrimSpace(ref.Name))
	return apierrors.IsNotFound(err)
}

func (r *reconciler[T]) syncRecord(ctx context.Context, req ctlruntime.Request, v view, now time.Time) (*recordSync, error) {
	b := v.spec.Record
	nsid := v.kind.NSID()
	repo, err := RepoRef(req.Workspace, b.RepoRef)
	if err != nil {
		return nil, err
	}
	r.c.bindRepo(string(v.kind), req, repo)
	did, reader, err := Repo(ctx, r.c.bus, r.c.dynamic, r.self(req), repo, nsid)
	if err != nil {
		return nil, err
	}
	name := v.object.GetName()
	uid := string(v.object.GetUID())
	rkey := name
	stored := v.status.Record
	fresh := stored == nil || strings.TrimSpace(stored.URI) == "" || stored.Direction == v1alpha1.InfraSyncRetained
	follow := strings.TrimSpace(b.URI) != ""
	if follow {
		uri, err := syntax.ParseATURI(strings.TrimSpace(b.URI))
		if err != nil {
			return nil, fmt.Errorf("infra: spec.record.uri: %w", err)
		}
		if uri.Authority().String() != did || uri.Collection().String() != nsid {
			return nil, fmt.Errorf("infra: spec.record.uri %s is not a %s record of the repo %s", b.URI, nsid, did)
		}
		rkey = uri.RecordKey().String()
	} else if !fresh {
		if uri, err := syntax.ParseATURI(strings.TrimSpace(stored.URI)); err == nil && uri.Authority().String() == did &&
			uri.Collection().String() == nsid && infra.InSuccession(name, uid, uri.RecordKey().String()) {
			rkey = uri.RecordKey().String()
		}
	}
	publish := Publishing(b)
	render := v.declaration
	if publish {
		render = render.Published()
	}
	out := &recordSync{rkey: rkey, repo: repo, uri: recordURI(did, nsid, rkey), createdAt: v.object.GetCreationTimestamp().Time,
		render: render, publish: publish}
	gen := v.object.GetGeneration()
	if v.kind == infra.KindInfra && rkey == infra.ReservedRkey {
		out.action = infra.ActionReserved
		out.status = &v1alpha1.InfraRecordStatus{URI: out.uri}
		out.conditions = []metav1.Condition{
			conds.New(v1alpha1.InfraConditionRecordReserved, true, "Reserved", "rkey self holds the account's index of live deployments; nothing is written", gen),
			conds.New(v1alpha1.InfraConditionSynced, false, "Reserved", "rkey self is reserved", gen)}
		return out, nil
	}
	var got atprotorepo.Record
	var present bool
	var decoded infra.Decoded
	var derr error
	for n := 0; ; n++ {
		got, err = reader.GetRecord(ctx, did, nsid, rkey)
		present = err == nil
		if err != nil && !errors.Is(err, atprotorepo.ErrNotFound) {
			return nil, ReadFailed(fmt.Errorf("infra: reading %s: %w", out.uri, err))
		}
		decoded, derr = infra.Decoded{}, nil
		if present {
			decoded, derr = infra.Decode(v.kind, got.Value)
		}
		if follow || !fresh || !present || !(decoded.Over(v.kind, now) || errors.Is(derr, infra.ErrReserved)) {
			break
		}
		if n+1 > infra.MaxSuccessors {
			return nil, fmt.Errorf("infra: %d successor rkeys of %s all hold ended records", infra.MaxSuccessors, name)
		}
		next := infra.Successor(name, uid, n+1)
		r.c.log.Info("infra_record_successor", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "ended", out.uri,
			"endedAt", decoded.EndedAt, "successor", next)
		rkey = next
		out.rkey, out.uri = rkey, recordURI(did, nsid, rkey)
	}
	if err := r.ensureWatch(ctx, req, v, repo, rkey, b.EventSourceRefs); err != nil {
		return nil, err
	}
	observed := infra.Record{Present: present, CID: got.CID}
	if present {
		observed.Hash, observed.Err = decoded.Declaration.Hash(), derr
		observed.Ended, observed.Reserved = decoded.Ended(), errors.Is(derr, infra.ErrReserved)
		out.declaration, out.recorded = decoded.Declaration, decoded
		if !decoded.CreatedAt.IsZero() {
			out.createdAt = decoded.CreatedAt
		}
	}
	out.cid = got.CID
	r.c.index(string(v.kind), req, out.uri, got.CID)
	specHash := render.Hash()
	out.action = infra.Decide(infra.SyncInput{Follow: follow, Publish: publish, Stored: stored, URI: out.uri, SpecHash: specHash, Record: observed})
	endHeld := stored != nil && stored.URI == out.uri && stored.Direction != v1alpha1.InfraSyncRetained &&
		meta.IsStatusConditionTrue(v.status.Conditions, v1alpha1.InfraConditionRecordEnded)
	endLost := false
	switch out.action {
	case infra.ActionEnded, infra.ActionDelete, infra.ActionReserved:
	default:
		if endHeld {
			r.c.log.Warn("infra_record_end_lost", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", out.uri, "cid", got.CID,
				"action", out.action)
			out.action, endLost = infra.ActionEnded, true
		}
	}
	keep := v1alpha1.InfraRecordStatus{URI: out.uri}
	if stored != nil && stored.URI == out.uri {
		keep = *stored
	}
	synced := func(direction v1alpha1.InfraSyncDirection) *v1alpha1.InfraRecordStatus {
		return &v1alpha1.InfraRecordStatus{URI: out.uri, CID: got.CID, DeclarationHash: observed.Hash, SyncedGeneration: gen,
			SyncedAt: now.Format(time.RFC3339), Direction: direction}
	}
	valid := conds.New(v1alpha1.InfraConditionRecordValid, true, "Decoded", "the record decodes into a declaration", gen)
	switch out.action {
	case infra.ActionInSync:
		out.status = &keep
		out.conditions = []metav1.Condition{valid, conds.New(v1alpha1.InfraConditionSynced, true, "InSync", "the record holds the declaration", gen)}
		note, pulled := r.c.takePull(string(v.kind), req, observed.Hash)
		rebound := keep.Direction == v1alpha1.InfraSyncRetained
		if keep.CID != got.CID || keep.DeclarationHash != observed.Hash || rebound {
			direction := keep.Direction
			if pulled {
				direction = v1alpha1.InfraSyncPulled
			}
			out.status = synced(direction)
			if out.status.Direction == "" || rebound {
				out.status.Direction = v1alpha1.InfraSyncInSync
			}
		}
		out.conditions = append(out.conditions, note...)
		out.write, out.swapCid = true, got.CID
	case infra.ActionCreate:
		if publish && keep.CID != "" {
			r.c.log.Warn("infra_record_rewritten", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", out.uri, "goneCid", keep.CID)
		}
		out.status = &v1alpha1.InfraRecordStatus{URI: out.uri, Direction: v1alpha1.InfraSyncCreated}
		out.write = true
		out.conditions = []metav1.Condition{conds.New(v1alpha1.InfraConditionSynced, false, "Creating", "writing the record", gen)}
	case infra.ActionPush:
		keep.Direction = v1alpha1.InfraSyncPushed
		out.status = &keep
		out.write, out.swapCid = true, got.CID
		out.conditions = []metav1.Condition{valid, conds.New(v1alpha1.InfraConditionSynced, false, "Pushing", "writing the kcp declaration to the record", gen)}
		if publish && (observed.Err != nil || observed.Hash != keep.DeclarationHash) {
			if observed.Err != nil {
				out.conditions[0] = conds.New(v1alpha1.InfraConditionRecordValid, false, "Invalid", observed.Err.Error(), gen)
			}
			out.conditions[1] = conds.New(v1alpha1.InfraConditionSynced, false, "Overwriting",
				"the record is published from kcp only; a change made elsewhere is written over", gen)
			r.c.log.Warn("infra_record_overwritten", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", out.uri, "cid", got.CID,
				"invalid", observed.Err != nil)
		}
	case infra.ActionPull:
		out.status = synced(v1alpha1.InfraSyncPulled)
		out.conditions = []metav1.Condition{valid, conds.New(v1alpha1.InfraConditionSynced, true, "Pulled", "the record's declaration is applied to the spec", gen)}
	case infra.ActionConflict:
		out.status = synced(v1alpha1.InfraSyncPulled)
		message := fmt.Sprintf("generation %d of the spec and record cid %s both changed since the last sync; the record won", gen, got.CID)
		out.conditions = []metav1.Condition{valid,
			conds.New(v1alpha1.InfraConditionSynced, true, "Pulled", "the record's declaration is applied to the spec", gen),
			conds.New(v1alpha1.InfraConditionSyncConflict, true, "RecordWon", message, gen)}
		r.c.log.Warn("infra_record_conflict", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", out.uri, "cid", got.CID,
			"generation", gen)
	case infra.ActionEnded:
		direction := keep.Direction
		if direction == "" || direction == v1alpha1.InfraSyncRetained {
			direction = v1alpha1.InfraSyncInSync
		}
		if endLost {
			out.status = &keep
			out.conditions = []metav1.Condition{conds.New(v1alpha1.InfraConditionSynced, false, "EndLost",
				"the record no longer shows the end this object recorded; the object stays ended and writes nothing", gen)}
			break
		}
		out.status = synced(direction)
		message := fmt.Sprintf("the record ended at %s (%s); it is kept and never written again", decoded.EndedAt, strings.ToLower(string(decoded.EndReason)))
		out.conditions = []metav1.Condition{
			conds.New(v1alpha1.InfraConditionSynced, true, "Ended", message, gen),
			conds.New(v1alpha1.InfraConditionRecordEnded, true, string(decoded.EndReason), message, gen)}
	case infra.ActionReserved:
		out.status = &keep
		out.conditions = []metav1.Condition{
			conds.New(v1alpha1.InfraConditionRecordReserved, true, "Reserved", "the record holds the account's index of live deployments; nothing is written", gen),
			conds.New(v1alpha1.InfraConditionSynced, false, "Reserved", "the record is the index, not a deployment", gen)}
	case infra.ActionInvalid:
		out.status = &keep
		out.conditions = []metav1.Condition{conds.New(v1alpha1.InfraConditionRecordValid, false, "Invalid", observed.Err.Error(), gen),
			conds.New(v1alpha1.InfraConditionSynced, false, "RecordInvalid", "the record is neither pulled nor overwritten", gen)}
	}
	return out, nil
}

func (r *reconciler[T]) settled(ctx context.Context, req ctlruntime.Request, v view, finished bool) bool {
	stored := v.status.Record
	if !finished || stored == nil || strings.TrimSpace(stored.URI) == "" || stored.CID == "" {
		return false
	}
	synced := meta.FindStatusCondition(v.status.Conditions, v1alpha1.InfraConditionSynced)
	if synced == nil || synced.Status != metav1.ConditionTrue || synced.ObservedGeneration != v.object.GetGeneration() {
		return false
	}
	k := keyOf(req, v)
	r.c.mu.Lock()
	cid, indexed := r.c.bound[stored.URI][k]
	_, moved := r.c.moved[k]
	unwritten := r.c.unwritten[k]
	r.c.mu.Unlock()
	switch {
	case moved || unwritten:
		return false
	case indexed:
		return cid == stored.CID
	}
	repo, err := RepoRef(req.Workspace, v.spec.Record.RepoRef)
	if err != nil {
		return false
	}
	r.c.bindRepo(string(v.kind), req, repo)
	uri, err := syntax.ParseATURI(strings.TrimSpace(stored.URI))
	if err != nil {
		return false
	}
	if err := r.ensureWatch(ctx, req, v, repo, uri.RecordKey().String(), v.spec.Record.EventSourceRefs); err != nil {
		return false
	}
	if changed, err := r.watchedChange(ctx, req, v, stored); err != nil || changed {
		return false
	}
	r.c.index(string(v.kind), req, stored.URI, stored.CID)
	r.c.log.Info("infra_record_settled", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", stored.URI, "cid", stored.CID)
	return true
}

func (r *reconciler[T]) watchedChange(ctx context.Context, req ctlruntime.Request, v view, stored *v1alpha1.InfraRecordStatus) (bool, error) {
	name := infra.ChildName(v.kind, v.object.GetName())
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return false, err
	}
	watch, err := kube.Get[v1alpha1.AtprotoRecordWatch](ctx, dyn.Resource(v1alpha1.AtprotoRecordWatchGVR), name)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(watch.Status.Records, func(rec v1alpha1.WatchedRecord) bool { return rec.URI == stored.URI })
	if i >= 0 {
		return watch.Status.Records[i].Cid != stored.CID, nil
	}
	return slices.Contains(watch.Status.Deleted, stored.URI), nil
}

func (r *reconciler[T]) pull(ctx context.Context, req ctlruntime.Request, v view, d infra.Declaration) error {
	contracts := d.Contracts
	if contracts == nil {
		contracts = []v1alpha1.InfraContract{}
	}
	spec := map[string]any{"description": d.Description, "contracts": contracts}
	gvr := v1alpha1.InfraGVR
	if v.kind == infra.KindEphemeralInfra {
		gvr = v1alpha1.EphemeralInfraGVR
		spec["ttlSeconds"], spec["endWhen"] = d.TTLSeconds, d.EndWhen
	}
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return err
	}
	doc := kube.Doc{"apiVersion": v1alpha1.GroupVersion.String(), "kind": string(v.kind), "metadata": map[string]any{"name": req.Name}, "spec": spec}
	if _, err := kube.Apply[kube.Doc](ctx, dyn.Resource(gvr), doc, kube.ApplyOptions{FieldManager: r.pullManager, Force: true}); err != nil {
		return fmt.Errorf("infra: applying the record's declaration to %s %s: %w", v.kind, req.Name, err)
	}
	return nil
}

func Publishing(b *v1alpha1.InfraRecordBinding) bool {
	return b != nil && b.Sync == v1alpha1.InfraRecordSyncPublish
}

func (r *reconciler[T]) writeRecord(ctx context.Context, req ctlruntime.Request, v view, rec *recordSync, observed infra.Observed) error {
	if rec.publish {
		observed = observed.Published()
	}
	observed.EndedAt, observed.EndReason = "", ""
	value, err := infra.Render(v.kind, rec.render, observed, rec.createdAt)
	if err != nil {
		return err
	}
	return r.applyRecord(ctx, req, v, rec.repo, rec.rkey, value, rec.swapCid)
}

func (r *reconciler[T]) applyRecord(ctx context.Context, req ctlruntime.Request, v view, repo commonv1.ObjectRef, rkey string, value json.RawMessage, swapCid string) error {
	if repo.Workspace == req.Workspace {
		repo.Workspace = ""
	}
	ar := &v1alpha1.AtprotoRecord{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AtprotoRecord"},
		ObjectMeta: metav1.ObjectMeta{Name: infra.ChildName(v.kind, v.object.GetName()), Labels: childLabels(v),
			OwnerReferences: []metav1.OwnerReference{ownerRef(v)}},
		Spec: v1alpha1.AtprotoRecordSpec{RepoRef: &repo, Collection: v.kind.NSID(), Rkey: rkey, Record: value, SwapCid: swapCid,
			DeletionPolicy: v1alpha1.RecordDeletionRetain}}
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return err
	}
	if _, err := kube.Apply[v1alpha1.AtprotoRecord](ctx, dyn.Resource(v1alpha1.AtprotoRecordGVR), ar, kube.ApplyOptions{FieldManager: r.manager, Force: true}); err != nil {
		return fmt.Errorf("infra: applying the AtprotoRecord %s: %w", ar.Name, err)
	}
	return nil
}

func RecordOrphaned(err error) bool {
	reason, ok := Unresolved(err)
	return ok && (reason == "RepoNotFound" || reason == "RepoBackendGone")
}

func (r *reconciler[T]) finalObserved(v view) infra.Observed {
	at := v.object.GetDeletionTimestamp()
	out := infra.Observed{Phase: v1alpha1.InfraPhaseEnded, EndReason: v1alpha1.EphemeralInfraEndDeleted}
	for _, c := range v.status.Contracts {
		switch c.Phase {
		case v1alpha1.InfraContractPhaseStaged, v1alpha1.InfraContractPhaseReleased, v1alpha1.InfraContractPhaseFailed:
		default:
			if v.object.GetDeletionTimestamp() != nil {
				c.Phase = v1alpha1.InfraContractPhaseReleased
			}
		}
		out.Contracts = append(out.Contracts, c)
	}
	if at != nil {
		out.EndedAt = at.UTC().Format(time.RFC3339)
	}
	if v.ephemeral != nil {
		out.ExpiresAt = v.ephemeral.ExpiresAt
		if v.ephemeral.EndedAt != "" {
			out.EndedAt, out.EndReason = v.ephemeral.EndedAt, cmp.Or(v.ephemeral.EndReason, v1alpha1.EphemeralInfraEndDeleted)
		}
	}
	if out.EndedAt == "" {
		out.EndedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if v.spec.Record == nil || Publishing(v.spec.Record) {
		out = out.Published()
	}
	return out
}

func (r *reconciler[T]) owns(v view, rkey string) bool {
	if v.spec.Record != nil && strings.TrimSpace(v.spec.Record.URI) != "" {
		return false
	}
	for _, o := range v.object.GetOwnerReferences() {
		if o.Controller != nil && *o.Controller && (o.Kind == CollectionKind || o.Kind == EphemeralCollectionKind) {
			return false
		}
	}
	return infra.InSuccession(v.object.GetName(), string(v.object.GetUID()), rkey)
}

func arInSync(ar *v1alpha1.AtprotoRecord) bool {
	c := meta.FindStatusCondition(ar.Status.Conditions, v1alpha1.ConditionRecordInSync)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == ar.Generation && ar.Status.ObservedGeneration == ar.Generation
}

func pendingEnd(ar *v1alpha1.AtprotoRecord) string {
	var value struct {
		Status struct {
			EndedAt string `json:"endedAt"`
		} `json:"status"`
	}
	if json.Unmarshal(ar.Spec.Record, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value.Status.EndedAt)
}

func (r *reconciler[T]) finalWrite(ctx context.Context, req ctlruntime.Request, v view) (bool, error) {
	stored := v.status.Record
	if stored == nil || stored.Direction == v1alpha1.InfraSyncRetained || strings.TrimSpace(stored.URI) == "" {
		return true, nil
	}
	uri, err := syntax.ParseATURI(strings.TrimSpace(stored.URI))
	if err != nil || uri.Collection().String() != v.kind.NSID() {
		return true, nil
	}
	rkey := uri.RecordKey().String()
	if v.kind == infra.KindInfra && rkey == infra.ReservedRkey {
		return true, nil
	}
	attrs := []any{"kind", v.kind, "workspace", req.Workspace, "name", req.Name, "uri", stored.URI}
	if !r.owns(v, rkey) {
		r.c.log.Info("infra_record_not_owned", attrs...)
		return true, nil
	}
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return false, err
	}
	ar, err := kube.Get[v1alpha1.AtprotoRecord](ctx, dyn.Resource(v1alpha1.AtprotoRecordGVR), infra.ChildName(v.kind, v.object.GetName()))
	switch {
	case apierrors.IsNotFound(err):
		ar = nil
	case err != nil:
		return false, err
	}
	var ref commonv1.ObjectRef
	switch {
	case v.spec.Record != nil:
		ref = v.spec.Record.RepoRef
	case ar != nil && ar.Spec.RepoRef != nil:
		ref = *ar.Spec.RepoRef
	default:
		r.c.log.Info("infra_record_end_skipped", append(attrs, "reason", "no repoRef")...)
		return true, nil
	}
	repo, err := RepoRef(req.Workspace, ref)
	if err != nil {
		return false, err
	}
	r.c.bindRepo(string(v.kind), req, repo)
	_, reader, err := Repo(ctx, r.c.bus, r.c.dynamic, r.self(req), repo, v.kind.NSID())
	if err != nil {
		return false, err
	}
	got, err := reader.GetRecord(ctx, uri.Authority().String(), uri.Collection().String(), rkey)
	switch {
	case errors.Is(err, atprotorepo.ErrNotFound):
		r.c.log.Info("infra_record_absent_at_end", attrs...)
		return true, nil
	case err != nil:
		return false, ReadFailed(fmt.Errorf("infra: reading %s for its final status: %w", stored.URI, err))
	}
	attrs = append(attrs, "cid", got.CID)
	decoded, derr := infra.Decode(v.kind, got.Value)
	switch {
	case errors.Is(derr, infra.ErrReserved):
		return true, nil
	case decoded.Ended():
		r.c.log.Info("infra_record_ended", append(attrs, "endedAt", decoded.EndedAt, "endReason", decoded.EndReason)...)
		return true, nil
	}
	own := []string{strings.TrimSpace(stored.CID)}
	if ar != nil && arInSync(ar) {
		own = append(own, strings.TrimSpace(ar.Status.Cid))
	}
	if !slices.Contains(own, got.CID) {
		r.c.log.Info("infra_record_replaced", append(attrs, "storedCid", stored.CID)...)
		return true, nil
	}
	observed := r.finalObserved(v)
	if ar != nil {
		if at := pendingEnd(ar); at != "" {
			observed.EndedAt = at
			if ar.Status.Phase == v1alpha1.AtprotoRecordPhaseFailed && ar.Status.ObservedGeneration == ar.Generation {
				return false, fmt.Errorf("infra: the final status write of %s failed: %s", stored.URI, ar.Status.Message)
			}
		}
	}
	value, err := infra.Final(v.kind, got.Value, observed)
	if err != nil {
		return false, err
	}
	if err := r.applyRecord(ctx, req, v, repo, rkey, value, got.CID); err != nil {
		return false, err
	}
	r.c.log.Info("infra_record_ending", attrs...)
	return false, nil
}

func (r *reconciler[T]) unbindRecord(ctx context.Context, req ctlruntime.Request, v view) (bool, error) {
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return false, err
	}
	name := infra.ChildName(v.kind, v.object.GetName())
	records := dyn.Resource(v1alpha1.AtprotoRecordGVR)
	ar, err := kube.Get[v1alpha1.AtprotoRecord](ctx, records, name)
	switch {
	case apierrors.IsNotFound(err):
		return true, r.dropWatch(ctx, dyn, name)
	case err != nil:
		return false, err
	case !ar.DeletionTimestamp.IsZero():
		return false, nil
	}
	if err := r.retain(ctx, records, ar); err != nil {
		return false, err
	}
	if err := records.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	r.c.log.Info("infra_record_unbound", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "atprotoRecord", name, "uri", ar.Status.URI)
	return false, nil
}

func (r *reconciler[T]) retain(ctx context.Context, records kube.Resource, ar *v1alpha1.AtprotoRecord) error {
	if ar.Spec.DeletionPolicy == v1alpha1.RecordDeletionRetain {
		return nil
	}
	retained := &v1alpha1.AtprotoRecord{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AtprotoRecord"},
		ObjectMeta: metav1.ObjectMeta{Name: ar.Name, Labels: ar.Labels, OwnerReferences: ar.OwnerReferences}, Spec: ar.Spec}
	retained.Spec.DeletionPolicy = v1alpha1.RecordDeletionRetain
	if _, err := kube.Apply[v1alpha1.AtprotoRecord](ctx, records, retained, kube.ApplyOptions{FieldManager: r.manager, Force: true}); err != nil {
		return fmt.Errorf("infra: retaining the record of the AtprotoRecord %s: %w", ar.Name, err)
	}
	return nil
}

func (r *reconciler[T]) retainChild(ctx context.Context, req ctlruntime.Request, v view) error {
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return err
	}
	records := dyn.Resource(v1alpha1.AtprotoRecordGVR)
	ar, err := kube.Get[v1alpha1.AtprotoRecord](ctx, records, infra.ChildName(v.kind, v.object.GetName()))
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !ar.DeletionTimestamp.IsZero():
		return nil
	}
	return r.retain(ctx, records, ar)
}

func (r *reconciler[T]) retainOnce(ctx context.Context, req ctlruntime.Request, v view) error {
	if v.spec.Record == nil && (v.status.Record == nil || v.status.Record.Direction == v1alpha1.InfraSyncRetained) {
		return nil
	}
	k := keyOf(req, v)
	r.c.mu.Lock()
	done := r.c.retained[k]
	r.c.mu.Unlock()
	if done {
		return nil
	}
	if err := r.retainChild(ctx, req, v); err != nil {
		return err
	}
	r.c.mu.Lock()
	r.c.retained[k] = true
	r.c.mu.Unlock()
	return nil
}

func (r *reconciler[T]) dropWatch(ctx context.Context, dyn *kube.Client, name string) error {
	for _, child := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{{v1alpha1.AtprotoRecordWatchGVR, name}, {exposurev1alpha1.ComponentLinkGVR, name + "-deliver"}} {
		if err := dyn.Resource(child.gvr).Delete(ctx, child.name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *reconciler[T]) ensureWatch(ctx context.Context, req ctlruntime.Request, v view, repo commonv1.ObjectRef, rkey string, sources []commonv1.ObjectRef) error {
	return EnsureWatch(ctx, r.c.dynamic, r.manager, req.Workspace, infra.ChildName(v.kind, v.object.GetName()), childLabels(v), ownerRef(v),
		repo, v.kind.NSID(), []string{rkey}, sources)
}

func EnsureWatch(ctx context.Context, dynamic kube.Factory, manager, workspace, name string, labels map[string]string, owner metav1.OwnerReference,
	repo commonv1.ObjectRef, collection string, rkeys []string, sources []commonv1.ObjectRef) error {
	dyn, err := dynamic(workspace)
	if err != nil {
		return err
	}
	if repo.Workspace == workspace {
		repo.Workspace = ""
	}
	watch := &v1alpha1.AtprotoRecordWatch{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: events.RecordWatchKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: v1alpha1.AtprotoRecordWatchSpec{RepoRef: &repo, Collection: collection, Rkeys: rkeys,
			Source: v1alpha1.AtprotoRecordWatchSource{EventSourceRefs: sources}}}
	if _, err := kube.Apply[v1alpha1.AtprotoRecordWatch](ctx, dyn.Resource(v1alpha1.AtprotoRecordWatchGVR), watch,
		kube.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
		return fmt.Errorf("infra: applying the AtprotoRecordWatch %s: %w", name, err)
	}
	link := &exposurev1alpha1.ComponentLink{TypeMeta: metav1.TypeMeta{APIVersion: exposurev1alpha1.GroupVersion.String(), Kind: "ComponentLink"},
		ObjectMeta: metav1.ObjectMeta{Name: name + "-deliver", Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: exposurev1alpha1.ComponentLinkSpec{
			From: exposurev1alpha1.ComponentLinkFrom{Selector: &exposurev1alpha1.ComponentLinkSelector{Group: eventsv1alpha1.GroupName,
				Kind: "EventStream", Workspace: workspace, MatchLabels: map[string]string{eventstream.RecordWatchLabel: name}}},
			To:           commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: events.RecordWatchKind, Name: name, Workspace: workspace},
			Capabilities: []exposurev1alpha1.LinkCapability{{Name: events.CapabilityDeliver, Scope: []string{name}}}}}
	if _, err := kube.Apply[exposurev1alpha1.ComponentLink](ctx, dyn.Resource(exposurev1alpha1.ComponentLinkGVR), link,
		kube.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
		return fmt.Errorf("infra: applying the ComponentLink %s: %w", link.Name, err)
	}
	return nil
}

type pullNote struct {
	hash string

	conditions []metav1.Condition
}

func (c *Controller) notePull(kind string, req ctlruntime.Request, hash string, conditions []metav1.Condition) {
	var keep []metav1.Condition
	for _, cond := range conditions {
		if cond.Type == v1alpha1.InfraConditionSyncConflict {
			keep = append(keep, cond)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pulls[indexKey{kind: kind, cluster: req.Cluster, name: req.Name}] = pullNote{hash: hash, conditions: keep}
}

func (c *Controller) takePull(kind string, req ctlruntime.Request, hash string) ([]metav1.Condition, bool) {
	k := indexKey{kind: kind, cluster: req.Cluster, name: req.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	note, ok := c.pulls[k]
	if !ok || note.hash != hash {
		return nil, false
	}
	delete(c.pulls, k)
	return note.conditions, true
}

type indexKey struct {
	kind string

	cluster string

	name string
}

func (c *Controller) index(kind string, req ctlruntime.Request, uri, cid string) {
	k := indexKey{kind: kind, cluster: req.Cluster, name: req.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(k)
	if moved, ok := c.moved[k]; ok && moved == cid {
		delete(c.moved, k)
	}
	users := c.bound[uri]
	if users == nil {
		users = map[indexKey]string{}
		c.bound[uri] = users
	}
	users[k] = cid
}

func (c *Controller) forget(kind string, req ctlruntime.Request) {
	c.repos[kind].Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
	c.mu.Lock()
	defer c.mu.Unlock()
	k := indexKey{kind: kind, cluster: req.Cluster, name: req.Name}
	c.dropLocked(k)
	delete(c.pulls, k)
	delete(c.moved, k)
	delete(c.unwritten, k)
	delete(c.retained, k)
	delete(c.wants, k)
	delete(c.indexFailed, k)
	c.retries.Reset(k)
	c.indexRetries.Reset(k)
}

func (c *Controller) bindRepo(kind string, req ctlruntime.Request, repo commonv1.ObjectRef) {
	c.repos[kind].Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, &struct{}{},
		binding.Key(repo.Workspace, RepoKind, repo.Name))
}

func (c *Controller) repoUsers(kind, workspace, name string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.repos[kind].Users(binding.Key(workspace, RepoKind, name)) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) dropLocked(k indexKey) {
	for u, users := range c.bound {
		delete(users, k)
		if len(users) == 0 {
			delete(c.bound, u)
		}
	}
}

func (c *Controller) watched(cluster, kind string, watch *v1alpha1.AtprotoRecordWatch) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ctlruntime.Request
	present := map[string]bool{}
	add := func(uri, now string) {
		for key, cid := range c.bound[uri] {
			if key.kind != kind || key.cluster != cluster {
				continue
			}
			if cid == now {
				delete(c.moved, key)
				continue
			}
			c.moved[key] = now
			out = append(out, ctlruntime.Request{Cluster: key.cluster, Name: key.name})
		}
	}
	for _, rec := range watch.Status.Records {
		present[rec.URI] = true
		add(rec.URI, rec.Cid)
	}
	for _, uri := range watch.Status.Deleted {
		if !present[uri] {
			add(uri, "")
		}
	}
	return out
}

func (c *Controller) setUnwritten(k indexKey, pending bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pending {
		c.unwritten[k] = true
		return
	}
	delete(c.unwritten, k)
}
