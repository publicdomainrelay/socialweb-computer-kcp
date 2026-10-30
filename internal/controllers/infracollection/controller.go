package infracollection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	infractl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/infra"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "infracollection"

	EphemeralName = "ephemeralinfracollection"

	listPage = 100
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

	repos map[string]*binding.Table[struct{}]
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Bus == nil {
		return nil, errors.New("infracollection: the bus is required")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("infracollection: the dynamic client factory is required")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme, exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{bus: opts.Bus, dynamic: opts.Dynamic, log: log.With("controller", Name), repos: map[string]*binding.Table[struct{}]{
		infractl.CollectionKind: binding.New[struct{}](), infractl.EphemeralCollectionKind: binding.New[struct{}]()}}
	if err := register[*v1alpha1.InfraCollection, *v1alpha1.Infra](m, c, Name, infraOps, opts.Scope); err != nil {
		return nil, err
	}
	if err := register[*v1alpha1.EphemeralInfraCollection, *v1alpha1.EphemeralInfra](m, c, EphemeralName, ephemeralOps, opts.Scope); err != nil {
		return nil, err
	}
	return c, nil
}

func register[T, C ctlruntime.Object](m *ctlruntime.Manager, c *Controller, name string, ops collectionOps[T], scope func(string) bool) error {
	r := &reconciler[T]{c: c, ops: ops, manager: ctlruntime.FieldManagerPrefix + name}
	options := []ctlruntime.Option{ctlruntime.Export(infractl.Export), ctlruntime.Owns[C](),
		ctlruntime.WatchesIn(infractl.RecordExport, func(_ context.Context, _ ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch) []ctlruntime.Request {
			labels := obj.GetLabels()
			if labels[v1alpha1.InfraKindLabel] != ops.kind || labels[v1alpha1.InfraNameLabel] == "" {
				return nil
			}
			return []ctlruntime.Request{{Name: labels[v1alpha1.InfraNameLabel]}}
		}),
		ctlruntime.WatchesFiltered(infractl.RepoExport, ctlruntime.WatchFilter[*v1alpha1.AtprotoRepo]{Changed: func(old, new *v1alpha1.AtprotoRepo) bool {
			return old.Status.DID != new.Status.DID
		}}, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRepo) []ctlruntime.Request {
			return c.repoUsers(ops.kind, src.Workspace, obj.Name)
		}),
		ctlruntime.Gone(func(_ context.Context, req ctlruntime.Request) {
			c.repos[ops.kind].Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
		})}
	if scope != nil {
		options = append(options, ctlruntime.InWorkspaces(scope))
	}
	if err := ctlruntime.For[T](m, name, r, options...); err != nil {
		return fmt.Errorf("infracollection: registering the %s controller: %w", ops.kind, err)
	}
	return nil
}

func (c *Controller) bindRepo(kind string, req ctlruntime.Request, repo commonv1.ObjectRef) {
	c.repos[kind].Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, &struct{}{},
		binding.Key(repo.Workspace, infractl.RepoKind, repo.Name))
}

func (c *Controller) repoUsers(kind, workspace, name string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.repos[kind].Users(binding.Key(workspace, infractl.RepoKind, name)) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

type child struct {
	name string

	owners []metav1.OwnerReference

	record *v1alpha1.InfraRecordBinding

	uri string
}

type collectionOps[T ctlruntime.Object] struct {
	kind string

	child infra.Kind

	view func(T) (v1alpha1.InfraCollectionSpec, v1alpha1.InfraCollectionStatus)

	statusObject func(name string, status v1alpha1.InfraCollectionStatus) T

	children func(ctx context.Context, dyn *kube.Client) ([]child, error)

	newChild func(meta metav1.ObjectMeta, binding v1alpha1.InfraRecordBinding) ctlruntime.Object
}

func childOf(meta metav1.ObjectMeta, spec v1alpha1.InfraSpec, status v1alpha1.InfraStatus) child {
	out := child{name: meta.Name, owners: meta.OwnerReferences, record: spec.Record}
	if status.Record != nil {
		out.uri = status.Record.URI
	}
	return out
}

var infraOps = collectionOps[*v1alpha1.InfraCollection]{
	kind:  infractl.CollectionKind,
	child: infra.KindInfra,
	view: func(o *v1alpha1.InfraCollection) (v1alpha1.InfraCollectionSpec, v1alpha1.InfraCollectionStatus) {
		return o.Spec, o.Status
	},
	statusObject: func(name string, status v1alpha1.InfraCollectionStatus) *v1alpha1.InfraCollection {
		return &v1alpha1.InfraCollection{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: status}
	},
	children: func(ctx context.Context, dyn *kube.Client) ([]child, error) {
		items, err := kube.List[v1alpha1.Infra](ctx, dyn.Resource(v1alpha1.InfraGVR), metav1.ListOptions{})
		var out []child
		for _, o := range items {
			out = append(out, childOf(o.ObjectMeta, o.Spec, o.Status))
		}
		return out, err
	},
	newChild: func(meta metav1.ObjectMeta, binding v1alpha1.InfraRecordBinding) ctlruntime.Object {
		return &v1alpha1.Infra{ObjectMeta: meta, Spec: v1alpha1.InfraSpec{Record: &binding}}
	},
}

var ephemeralOps = collectionOps[*v1alpha1.EphemeralInfraCollection]{
	kind:  infractl.EphemeralCollectionKind,
	child: infra.KindEphemeralInfra,
	view: func(o *v1alpha1.EphemeralInfraCollection) (v1alpha1.InfraCollectionSpec, v1alpha1.InfraCollectionStatus) {
		return o.Spec, o.Status
	},
	statusObject: func(name string, status v1alpha1.InfraCollectionStatus) *v1alpha1.EphemeralInfraCollection {
		return &v1alpha1.EphemeralInfraCollection{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: status}
	},
	children: func(ctx context.Context, dyn *kube.Client) ([]child, error) {
		items, err := kube.List[v1alpha1.EphemeralInfra](ctx, dyn.Resource(v1alpha1.EphemeralInfraGVR), metav1.ListOptions{})
		var out []child
		for _, o := range items {
			out = append(out, childOf(o.ObjectMeta, o.Spec.InfraSpec, o.Status.InfraStatus))
		}
		return out, err
	},
	newChild: func(meta metav1.ObjectMeta, binding v1alpha1.InfraRecordBinding) ctlruntime.Object {
		return &v1alpha1.EphemeralInfra{ObjectMeta: meta, Spec: v1alpha1.EphemeralInfraSpec{InfraSpec: v1alpha1.InfraSpec{Record: &binding}}}
	},
}

type reconciler[T ctlruntime.Object] struct {
	c *Controller

	ops collectionOps[T]

	manager string
}

func ChildName(rkey string) string {
	if len(validation.IsDNS1123Subdomain(rkey)) == 0 {
		return rkey
	}
	sum := sha256.Sum256([]byte(rkey))
	return "r-" + hex.EncodeToString(sum[:6])
}

func (r *reconciler[T]) Reconcile(ctx context.Context, req ctlruntime.Request, obj T) (ctlruntime.Result, error) {
	if !obj.GetDeletionTimestamp().IsZero() {
		return ctlruntime.Result{}, nil
	}
	spec, previous := r.ops.view(obj)
	nsid := r.ops.child.NSID()
	gen := obj.GetGeneration()
	status := v1alpha1.InfraCollectionStatus{ObservedGeneration: gen, Collection: nsid, Did: previous.Did, Records: previous.Records,
		Children: previous.Children, Conditions: slices.Clone(previous.Conditions)}
	fail := func(err error) (ctlruntime.Result, error) {
		status.Message = err.Error()
		reason, retry := "ListFailed", err
		if errors.Is(err, infractl.ErrRepoNoDID) {
			reason, retry = "RepoPending", nil
		}
		conds.Set(&status.Conditions, gen, v1alpha1.InfraCollectionConditionListed, false, reason, err.Error())
		return ctlruntime.Result{}, errors.Join(retry, req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status)))
	}
	repo, err := infractl.RepoRef(req.Workspace, spec.RepoRef)
	if err != nil {
		return fail(err)
	}
	r.c.bindRepo(r.ops.kind, req, repo)
	self := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: r.ops.kind, Name: req.Name, Workspace: req.Workspace}
	did, reader, err := infractl.Repo(ctx, r.c.bus, r.c.dynamic, self, repo, nsid)
	if err != nil {
		return fail(err)
	}
	owner := metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: r.ops.kind, Name: req.Name, UID: obj.GetUID(), Controller: ptr.To(true)}
	labels := map[string]string{v1alpha1.InfraKindLabel: r.ops.kind, v1alpha1.InfraNameLabel: req.Name}
	if err := infractl.EnsureWatch(ctx, r.c.dynamic, r.manager, req.Workspace, infra.ChildName(r.ops.child, "coll", req.Name), labels, owner,
		repo, nsid, spec.EventSourceRefs); err != nil {
		return fail(err)
	}
	records, err := listAll(ctx, reader, did, nsid)
	if err != nil {
		return fail(err)
	}
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return fail(err)
	}
	kids, err := r.ops.children(ctx, dyn)
	if err != nil {
		return fail(err)
	}
	byURI := map[string]child{}
	byName := map[string]child{}
	for _, k := range kids {
		byName[k.name] = k
		if uri := boundURI(k, req.Workspace, repo, did, nsid); uri != "" {
			byURI[uri] = k
		}
	}
	var children []v1alpha1.InfraCollectionChild
	created := 0
	for _, rec := range records {
		rkey := rec.RKey
		if rkey == "" {
			if uri, err := syntax.ParseATURI(rec.URI); err == nil {
				rkey = uri.RecordKey().String()
			}
		}
		entry := v1alpha1.InfraCollectionChild{Rkey: rkey, URI: rec.URI, CID: rec.CID}
		if k, ok := byURI[rec.URI]; ok {
			if ownedBy(k, obj.GetUID()) {
				entry.Name = k.name
			} else {
				entry.BoundBy = k.name
			}
			children = append(children, entry)
			continue
		}
		name := ChildName(rkey)
		if k, ok := byName[name]; ok {
			entry.Message = fmt.Sprintf("NameConflict: %s %s exists and binds no record of this collection", r.ops.child, k.name)
			children = append(children, entry)
			continue
		}
		meta := metav1.ObjectMeta{Name: name, Labels: map[string]string{v1alpha1.InfraCollectionLabel: req.Name},
			OwnerReferences: []metav1.OwnerReference{owner}}
		binding := v1alpha1.InfraRecordBinding{RepoRef: spec.RepoRef, URI: rec.URI, EventSourceRefs: spec.EventSourceRefs}
		if err := req.Apply(ctx, r.ops.newChild(meta, binding)); err != nil {
			return fail(fmt.Errorf("infracollection: creating %s %s for %s: %w", r.ops.child, name, rec.URI, err))
		}
		r.c.log.Info("infra_collection_child_created", "kind", r.ops.kind, "workspace", req.Workspace, "name", req.Name, "child", name,
			"uri", rec.URI)
		entry.Name = name
		created++
		children = append(children, entry)
	}
	status.Did, status.Records, status.Children = did, len(records), children
	status.Message = fmt.Sprintf("%d %s record(s), %d child object(s) created this pass", len(records), nsid, created)
	conds.Set(&status.Conditions, gen, v1alpha1.InfraCollectionConditionListed, true, "Listed", status.Message)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, r.ops.statusObject(req.Name, status))
}

func listAll(ctx context.Context, reader atprotorepo.RecordReader, did, nsid string) ([]atprotorepo.Record, error) {
	var out []atprotorepo.Record
	cursor := ""
	for {
		page, err := reader.ListRecords(ctx, did, nsid, atprotorepo.ListOptions{Limit: listPage, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("infracollection: listRecords %s of %s: %w", nsid, did, err)
		}
		out = append(out, page.Records...)
		if page.Cursor == "" || page.Cursor == cursor || len(page.Records) == 0 {
			return out, nil
		}
		cursor = page.Cursor
	}
}

func ownedBy(k child, uid types.UID) bool {
	for _, o := range k.owners {
		if o.UID == uid {
			return true
		}
	}
	return false
}

func boundURI(k child, workspace string, repo commonv1.ObjectRef, did, nsid string) string {
	if k.uri != "" {
		return k.uri
	}
	if k.record == nil {
		return ""
	}
	if uri := strings.TrimSpace(k.record.URI); uri != "" {
		return uri
	}
	ref, err := infractl.RepoRef(workspace, k.record.RepoRef)
	if err != nil || ref.Name != repo.Name || ref.Workspace != repo.Workspace {
		return ""
	}
	return "at://" + did + "/" + nsid + "/" + k.name
}
