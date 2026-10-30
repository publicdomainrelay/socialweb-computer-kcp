package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
)

const RepoKind = "AtprotoRepo"

var ErrRepoNoDID = errors.New("reports no did yet")

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
}

func RepoRef(workspace string, ref commonv1.ObjectRef) (commonv1.ObjectRef, error) {
	kind := strings.TrimSpace(ref.Kind)
	if kind != "" && !strings.EqualFold(kind, RepoKind) {
		return commonv1.ObjectRef{}, fmt.Errorf("infra: the repoRef must name an %s, not %s", RepoKind, kind)
	}
	if strings.TrimSpace(ref.Name) == "" {
		return commonv1.ObjectRef{}, errors.New("infra: the repoRef names no AtprotoRepo")
	}
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: RepoKind, Name: strings.TrimSpace(ref.Name), Workspace: ref.InWorkspace(workspace)}, nil
}

func Repo(ctx context.Context, b *bus.Bus, dynamic kube.Factory, self, ref commonv1.ObjectRef, nsid string) (string, atprotorepo.RecordReader, error) {
	dyn, err := dynamic(ref.Workspace)
	if err != nil {
		return "", nil, err
	}
	repo, err := kube.Get[v1alpha1.AtprotoRepo](ctx, dyn.Resource(v1alpha1.AtprotoRepoGVR), ref.Name)
	if err != nil {
		return "", nil, fmt.Errorf("infra: reading the AtprotoRepo %s: %w", ref.String(), err)
	}
	did := strings.TrimSpace(repo.Status.DID)
	if did == "" {
		return "", nil, fmt.Errorf("infra: the AtprotoRepo %s %w", ref.String(), ErrRepoNoDID)
	}
	reader, err := bus.Resolve[atprotorepo.RecordReader](ctx, b, self, ref, bus.Capability{Name: atprotorepo.CapabilityRead, Scope: []string{nsid}})
	if err != nil {
		return "", nil, fmt.Errorf("infra: the repo.read handle of %s: %w", ref.String(), err)
	}
	return did, reader, nil
}

func collectionOwned(obj metav1.Object) bool {
	for _, o := range obj.GetOwnerReferences() {
		if o.Controller != nil && *o.Controller && (o.Kind == CollectionKind || o.Kind == EphemeralCollectionKind) {
			return true
		}
	}
	return false
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
	rkey := v.object.GetName()
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
	}
	publish := Publishing(b)
	render := v.declaration
	if publish {
		render = render.Published()
	}
	out := &recordSync{rkey: rkey, repo: repo, uri: "at://" + did + "/" + nsid + "/" + rkey, createdAt: v.object.GetCreationTimestamp().Time,
		render: render, publish: publish}
	if !collectionOwned(v.object) {
		if err := r.ensureWatch(ctx, req, v, repo, b.EventSourceRefs); err != nil {
			return nil, err
		}
	}
	got, err := reader.GetRecord(ctx, did, nsid, rkey)
	present := err == nil
	if err != nil && !errors.Is(err, atprotorepo.ErrNotFound) {
		return nil, fmt.Errorf("infra: reading %s: %w", out.uri, err)
	}
	observed := infra.Record{Present: present, CID: got.CID}
	if present {
		decoded, derr := infra.Decode(v.kind, got.Value)
		observed.Hash, observed.Err = decoded.Declaration.Hash(), derr
		out.declaration = decoded.Declaration
		if !decoded.CreatedAt.IsZero() {
			out.createdAt = decoded.CreatedAt
		}
	}
	out.cid = got.CID
	r.c.index(string(v.kind), req, out.uri, got.CID)
	specHash := render.Hash()
	stored := v.status.Record
	out.action = infra.Decide(infra.SyncInput{Follow: follow, Publish: publish, Stored: stored, URI: out.uri, SpecHash: specHash, Record: observed})
	gen := v.object.GetGeneration()
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
	case infra.ActionInvalid:
		out.status = &keep
		out.conditions = []metav1.Condition{conds.New(v1alpha1.InfraConditionRecordValid, false, "Invalid", observed.Err.Error(), gen),
			conds.New(v1alpha1.InfraConditionSynced, false, "RecordInvalid", "the record is neither pulled nor overwritten", gen)}
	}
	return out, nil
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
	value, err := infra.Render(v.kind, rec.render, observed, rec.createdAt)
	if err != nil {
		return err
	}
	repo := rec.repo
	if repo.Workspace == req.Workspace {
		repo.Workspace = ""
	}
	ar := &v1alpha1.AtprotoRecord{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AtprotoRecord"},
		ObjectMeta: metav1.ObjectMeta{Name: infra.ChildName(v.kind, v.object.GetName()), Labels: childLabels(v),
			OwnerReferences: []metav1.OwnerReference{ownerRef(v)}},
		Spec: v1alpha1.AtprotoRecordSpec{RepoRef: &repo, Collection: v.kind.NSID(), Rkey: rec.rkey, Record: json.RawMessage(value), SwapCid: rec.swapCid}}
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return err
	}
	if _, err := kube.Apply[v1alpha1.AtprotoRecord](ctx, dyn.Resource(v1alpha1.AtprotoRecordGVR), ar, kube.ApplyOptions{FieldManager: r.manager, Force: true}); err != nil {
		return fmt.Errorf("infra: applying the AtprotoRecord %s: %w", ar.Name, err)
	}
	return nil
}

func (r *reconciler[T]) deleteRecord(ctx context.Context, req ctlruntime.Request, v view) (bool, error) {
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return false, err
	}
	name := infra.ChildName(v.kind, v.object.GetName())
	records := dyn.Resource(v1alpha1.AtprotoRecordGVR)
	ar, err := kube.Get[v1alpha1.AtprotoRecord](ctx, records, name)
	switch {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, err
	case !ar.DeletionTimestamp.IsZero():
		return false, nil
	}
	if err := records.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	r.c.log.Info("infra_record_deleting", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "atprotoRecord", name)
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
	if ar.Spec.DeletionPolicy != v1alpha1.RecordDeletionRetain {
		retained := &v1alpha1.AtprotoRecord{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AtprotoRecord"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: ar.Labels, OwnerReferences: ar.OwnerReferences}, Spec: ar.Spec}
		retained.Spec.DeletionPolicy = v1alpha1.RecordDeletionRetain
		if _, err := kube.Apply[v1alpha1.AtprotoRecord](ctx, records, retained, kube.ApplyOptions{FieldManager: r.manager, Force: true}); err != nil {
			return false, fmt.Errorf("infra: retaining the record of the AtprotoRecord %s: %w", name, err)
		}
	}
	if err := records.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	r.c.log.Info("infra_record_unbound", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "atprotoRecord", name, "uri", ar.Status.URI)
	return false, nil
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

func (r *reconciler[T]) ensureWatch(ctx context.Context, req ctlruntime.Request, v view, repo commonv1.ObjectRef, sources []commonv1.ObjectRef) error {
	return EnsureWatch(ctx, r.c.dynamic, r.manager, req.Workspace, infra.ChildName(v.kind, v.object.GetName()), childLabels(v), ownerRef(v),
		repo, v.kind.NSID(), sources)
}

func EnsureWatch(ctx context.Context, dynamic kube.Factory, manager, workspace, name string, labels map[string]string, owner metav1.OwnerReference,
	repo commonv1.ObjectRef, collection string, sources []commonv1.ObjectRef) error {
	dyn, err := dynamic(workspace)
	if err != nil {
		return err
	}
	if repo.Workspace == workspace {
		repo.Workspace = ""
	}
	watch := &v1alpha1.AtprotoRecordWatch{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: events.RecordWatchKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: v1alpha1.AtprotoRecordWatchSpec{RepoRef: &repo, Collection: collection,
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
	add := func(uri string, changed func(cid string) bool) {
		for key, cid := range c.bound[uri] {
			if key.kind != kind || key.cluster != cluster || !changed(cid) {
				continue
			}
			out = append(out, ctlruntime.Request{Cluster: key.cluster, Name: key.name})
		}
	}
	for _, rec := range watch.Status.Records {
		add(rec.URI, func(cid string) bool { return cid != rec.Cid })
	}
	for _, uri := range watch.Status.Deleted {
		add(uri, func(cid string) bool { return cid != "" })
	}
	return out
}
