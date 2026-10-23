package eventstream

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsapi "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const AtprotoRepoKind = "AtprotoRepo"

type fallbackDirectory []identity.Directory

func (d fallbackDirectory) lookup(fn func(identity.Directory) (*identity.Identity, error)) (*identity.Identity, error) {
	var last error
	for _, dir := range d {
		ident, err := fn(dir)
		if err == nil {
			return ident, nil
		}
		last = err
	}
	if last == nil {
		last = identity.ErrDIDNotFound
	}
	return nil, last
}

func (d fallbackDirectory) LookupHandle(ctx context.Context, handle syntax.Handle) (*identity.Identity, error) {
	return d.lookup(func(dir identity.Directory) (*identity.Identity, error) {
		return dir.LookupHandle(ctx, handle)
	})
}

func (d fallbackDirectory) LookupDID(ctx context.Context, did syntax.DID) (*identity.Identity, error) {
	return d.lookup(func(dir identity.Directory) (*identity.Identity, error) {
		return dir.LookupDID(ctx, did)
	})
}

func (d fallbackDirectory) Lookup(ctx context.Context, atid syntax.AtIdentifier) (*identity.Identity, error) {
	return d.lookup(func(dir identity.Directory) (*identity.Identity, error) {
		return dir.Lookup(ctx, atid)
	})
}

func (d fallbackDirectory) Purge(ctx context.Context, atid syntax.AtIdentifier) error {
	var last error
	for _, dir := range d {
		if err := dir.Purge(ctx, atid); err != nil {
			last = err
		}
	}
	return last
}

type watchSourcePlan struct {
	Name string

	Key string

	SourceRef commonv1.ObjectRef

	RepoRef *commonv1.ObjectRef

	Filter eventsapi.EventStreamFilter
}

type watchRepo struct {
	Did string

	RepoRef *commonv1.ObjectRef
}

func EventStreamNameForWatchSource(watchName, sourceKey string) string {
	if strings.HasPrefix(sourceKey, "pds/") {
		return EventStreamNameForWatch(watchName)
	}
	sum := sha256.Sum256([]byte(sourceKey))
	suffix := hex.EncodeToString(sum[:6])
	candidate := EventStreamNamePrefix + watchName + "-" + suffix
	if len(candidate) <= maxEventStreamName {
		return candidate
	}
	keep := maxEventStreamName - len(suffix) - 1
	return candidate[:keep] + "-" + suffix
}

func (c *Controller) planWatchSources(ctx context.Context, dyn dynamic.Interface, watchName string, spec recordWatchSpec) ([]watchSourcePlan, error) {
	repo, err := c.resolveWatchRepo(ctx, dyn, spec)
	if err != nil {
		return nil, err
	}
	filter := eventsapi.EventStreamFilter{}
	if collection := strings.TrimSpace(spec.Collection); collection != "" {
		filter.Collections = []string{collection}
	}
	if repo.Did != "" {
		filter.DIDs = []string{repo.Did}
	}
	var plans []watchSourcePlan
	seen := map[string]bool{}
	for _, ref := range spec.Source.EventSourceRefs {
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			continue
		}
		if kind := strings.TrimSpace(ref.Kind); kind != "" && !strings.EqualFold(kind, eventsapi.EventSourceKind) {
			return nil, fmt.Errorf("eventstream: source.eventSourceRefs must name EventSources, not %s %s", kind, name)
		}
		source := commonv1.ObjectRef{Group: eventsapi.GroupName, Kind: eventsapi.EventSourceKind, Name: name, Workspace: ref.InWorkspace(c.opts.Workspace)}
		key := "source/" + source.Workspace + "/" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		plans = append(plans, watchSourcePlan{Name: EventStreamNameForWatchSource(watchName, key), Key: key, SourceRef: source, Filter: filter})
	}
	if len(plans) > 0 {
		return plans, nil
	}
	if repo.RepoRef == nil {
		return nil, errors.New("eventstream: the record watch names no source.eventSourceRefs and no AtprotoRepo repoRef")
	}
	key := "pds/" + repo.RepoRef.Name
	name := EventStreamNameForWatchSource(watchName, key)
	plans = append(plans, watchSourcePlan{Name: name, Key: key, RepoRef: repo.RepoRef, Filter: filter,
		SourceRef: commonv1.ObjectRef{Group: eventsapi.GroupName, Kind: eventsapi.EventSourceKind, Name: name, Workspace: c.opts.Workspace}})
	return plans, nil
}

func (c *Controller) resolveWatchRepo(ctx context.Context, dyn dynamic.Interface, spec recordWatchSpec) (watchRepo, error) {
	if spec.RepoRef != nil {
		return c.resolveWatchRepoRef(ctx, dyn, spec.RepoRef)
	}
	repo := strings.TrimSpace(spec.Repo)
	switch {
	case strings.HasPrefix(repo, "did:"):
		return watchRepo{Did: repo}, nil
	case repo == "" && len(spec.Source.EventSourceRefs) == 0:
		return watchRepo{}, errors.New("eventstream: spec.repo or spec.repoRef is required")
	}
	return watchRepo{}, nil
}

func (c *Controller) resolveWatchRepoRef(ctx context.Context, dyn dynamic.Interface, ref *recordRepoRef) (watchRepo, error) {
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return watchRepo{}, errors.New("eventstream: spec.repoRef needs a name")
	}
	if !strings.EqualFold(strings.TrimSpace(ref.Kind), AtprotoRepoKind) {
		return watchRepo{}, fmt.Errorf("eventstream: spec.repoRef must name an AtprotoRepo, not %q", ref.Kind)
	}
	workspace := cmp.Or(strings.TrimSpace(ref.Workspace), c.opts.Workspace)
	client := dyn
	if workspace != c.opts.Workspace {
		var err error
		if client, err = c.opts.Dynamic(workspace); err != nil {
			return watchRepo{}, err
		}
	}
	object, err := client.Resource(AtprotoRepoGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return watchRepo{}, fmt.Errorf("eventstream: reading AtprotoRepo %s: %w", name, err)
	}
	var repo v1alpha1.AtprotoRepo
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &repo); err != nil {
		return watchRepo{}, err
	}
	did := strings.TrimSpace(repo.Status.DID)
	if did == "" {
		backend, err := readRepoBackend(ctx, client, repo)
		if err != nil {
			return watchRepo{}, err
		}
		did = backend.DID
	}
	return watchRepo{Did: did, RepoRef: &commonv1.ObjectRef{Workspace: workspace, Group: v1alpha1.GroupName, Kind: AtprotoRepoKind, Name: name}}, nil
}

func (c *Controller) ensurePdsSource(ctx context.Context, dyn dynamic.Interface, watch *unstructured.Unstructured, name string, repo commonv1.ObjectRef) error {
	owner := []metav1.OwnerReference{{APIVersion: watch.GetAPIVersion(), Kind: watch.GetKind(), Name: watch.GetName(), UID: watch.GetUID()}}
	labels := map[string]string{RecordWatchLabel: watch.GetName()}
	if repo.Workspace == c.opts.Workspace {
		repo.Workspace = ""
	}
	backend := &eventsapi.PdsEventSource{TypeMeta: metav1.TypeMeta{APIVersion: eventsapi.GroupVersion.String(), Kind: eventsapi.PdsEventSourceKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: owner}, Spec: eventsapi.PdsEventSourceSpec{RepoRef: repo}}
	if err := applyObject(ctx, dyn.Resource(eventsapi.PdsEventSourceGVR), name, backend); err != nil {
		return err
	}
	abstract := &eventsapi.EventSource{TypeMeta: metav1.TypeMeta{APIVersion: eventsapi.GroupVersion.String(), Kind: eventsapi.EventSourceKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: owner},
		Spec:       eventsapi.EventSourceSpec{BackendRef: commonv1.ObjectRef{Group: eventsapi.GroupName, Kind: eventsapi.PdsEventSourceKind, Name: name}}}
	return applyObject(ctx, dyn.Resource(EventSourceGVR), name, abstract)
}

func applyObject(ctx context.Context, ri dynamic.ResourceInterface, name string, obj any) error {
	object, err := toUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: object}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	if _, err := ri.Patch(ctx, name, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: FieldManager, Force: &force}); err != nil {
		return fmt.Errorf("eventstream: applying %s %s: %w", u.GetKind(), name, err)
	}
	return nil
}
