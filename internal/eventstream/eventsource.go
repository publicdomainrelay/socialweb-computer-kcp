package eventstream

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

var EventSourceGVR = eventsv1alpha1.EventSourceGVR

type StreamSource struct {
	Kind string

	RepoRef *commonv1.ObjectRef

	RelayRef *commonv1.ObjectRef

	URL string
}

type ResourceFor func(gvr schema.GroupVersionResource) (dynamic.ResourceInterface, error)

func DynamicResources(dyn dynamic.Interface) ResourceFor {
	return func(gvr schema.GroupVersionResource) (dynamic.ResourceInterface, error) {
		if dyn == nil {
			return nil, errors.New("eventstream: no dynamic client")
		}
		return dyn.Resource(gvr), nil
	}
}

func SourceRef(name string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: eventsv1alpha1.EventSourceKind, Name: name}
}

func backendGVR(kind string) (schema.GroupVersionResource, error) {
	switch kind {
	case eventsv1alpha1.PdsEventSourceKind:
		return eventsv1alpha1.PdsEventSourceGVR, nil
	case eventsv1alpha1.RelayEventSourceKind:
		return eventsv1alpha1.RelayEventSourceGVR, nil
	case eventsv1alpha1.JetstreamEventSourceKind:
		return eventsv1alpha1.JetstreamEventSourceGVR, nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("eventstream: %q is not an EventSource backend kind", kind)
	}
}

func backendSpec(src StreamSource) (any, error) {
	switch src.Kind {
	case eventsv1alpha1.PdsEventSourceKind:
		if src.RepoRef == nil {
			return nil, errors.New("eventstream: a PdsEventSource needs a repoRef")
		}
		return eventsv1alpha1.PdsEventSourceSpec{RepoRef: *src.RepoRef}, nil
	case eventsv1alpha1.RelayEventSourceKind:
		return eventsv1alpha1.RelayEventSourceSpec{RelayRef: src.RelayRef, URL: src.URL}, nil
	case eventsv1alpha1.JetstreamEventSourceKind:
		return eventsv1alpha1.JetstreamEventSourceSpec{URL: src.URL}, nil
	default:
		return nil, fmt.Errorf("eventstream: %q is not an EventSource backend kind", src.Kind)
	}
}

func EnsureEventSource(ctx context.Context, resources ResourceFor, fieldManager string, stream *unstructured.Unstructured, src StreamSource) error {
	name := stream.GetName()
	gvr, err := backendGVR(src.Kind)
	if err != nil {
		return err
	}
	spec, err := backendSpec(src)
	if err != nil {
		return err
	}
	owner := []metav1.OwnerReference{{APIVersion: stream.GetAPIVersion(), Kind: stream.GetKind(), Name: name, UID: stream.GetUID()}}
	backend, err := resources(gvr)
	if err != nil {
		return err
	}
	if err := ensureSourceObject(ctx, backend, fieldManager, src.Kind, name, owner, spec); err != nil {
		return err
	}
	abstract, err := resources(EventSourceGVR)
	if err != nil {
		return err
	}
	return ensureSourceObject(ctx, abstract, fieldManager, eventsv1alpha1.EventSourceKind, name, owner,
		eventsv1alpha1.EventSourceSpec{BackendRef: commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: src.Kind, Name: name}})
}

func ensureSourceObject(ctx context.Context, ri dynamic.ResourceInterface, fieldManager, kind, name string, owner []metav1.OwnerReference, spec any) error {
	specMap, err := toUnstructured(spec)
	if err != nil {
		return err
	}
	for range 5 {
		existing, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": eventsv1alpha1.GroupVersion.String(),
				"kind":       kind,
				"metadata":   map[string]any{"name": name},
				"spec":       specMap,
			}}
			obj.SetOwnerReferences(owner)
			if _, err := ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager}); err == nil || apierrors.IsAlreadyExists(err) {
				return nil
			} else {
				return fmt.Errorf("eventstream: creating %s %s: %w", kind, name, err)
			}
		}
		if err != nil {
			return err
		}
		current, _, _ := unstructured.NestedMap(existing.Object, "spec")
		if reflect.DeepEqual(current, specMap) && reflect.DeepEqual(existing.GetOwnerReferences(), owner) {
			return nil
		}
		existing.Object["spec"] = specMap
		existing.SetOwnerReferences(owner)
		if _, err := ri.Update(ctx, existing, metav1.UpdateOptions{FieldManager: fieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return fmt.Errorf("eventstream: updating %s %s: %w", kind, name, err)
		}
	}
	return fmt.Errorf("eventstream: %s %s stayed in conflict", kind, name)
}

func (c *Controller) loadSource(ctx context.Context, es eventsv1alpha1.EventStream) (StreamSource, error) {
	ref := es.Spec.SourceRef
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return StreamSource{}, errors.New("eventstream: spec.sourceRef needs a name")
	}
	dyn, err := c.opts.Dynamic(ref.InWorkspace(c.opts.Workspace))
	if err != nil {
		return StreamSource{}, err
	}
	u, err := dyn.Resource(EventSourceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return StreamSource{}, fmt.Errorf("eventstream: reading EventSource %s: %w", name, err)
	}
	var source eventsv1alpha1.EventSource
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &source); err != nil {
		return StreamSource{}, fmt.Errorf("eventstream: decoding EventSource %s: %w", name, err)
	}
	backendRef := source.Spec.BackendRef
	gvr, err := backendGVR(backendRef.Kind)
	if err != nil {
		return StreamSource{}, err
	}
	b, err := dyn.Resource(gvr).Get(ctx, backendRef.Name, metav1.GetOptions{})
	if err != nil {
		return StreamSource{}, fmt.Errorf("eventstream: reading %s %s: %w", backendRef.Kind, backendRef.Name, err)
	}
	spec, _, _ := unstructured.NestedMap(b.Object, "spec")
	out := StreamSource{Kind: backendRef.Kind}
	switch backendRef.Kind {
	case eventsv1alpha1.PdsEventSourceKind:
		var s eventsv1alpha1.PdsEventSourceSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &s); err != nil {
			return StreamSource{}, err
		}
		out.RepoRef = &s.RepoRef
	case eventsv1alpha1.RelayEventSourceKind:
		var s eventsv1alpha1.RelayEventSourceSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &s); err != nil {
			return StreamSource{}, err
		}
		out.RelayRef, out.URL = s.RelayRef, s.URL
	case eventsv1alpha1.JetstreamEventSourceKind:
		var s eventsv1alpha1.JetstreamEventSourceSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &s); err != nil {
			return StreamSource{}, err
		}
		out.URL = s.URL
	}
	return out, nil
}

func (c *Controller) sourcePDSRef(ctx context.Context, ref commonv1.ObjectRef) (*commonv1.ObjectRef, error) {
	if !strings.EqualFold(strings.TrimSpace(ref.Kind), "AtprotoRepo") {
		return &ref, nil
	}
	workspace := ref.InWorkspace(c.opts.Workspace)
	dyn, err := c.opts.Dynamic(workspace)
	if err != nil {
		return nil, err
	}
	u, err := dyn.Resource(AtprotoRepoGVR).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("eventstream: reading AtprotoRepo %s: %w", ref.Name, err)
	}
	var repo v1alpha1.AtprotoRepo
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &repo); err != nil {
		return nil, fmt.Errorf("eventstream: decoding AtprotoRepo %s: %w", ref.Name, err)
	}
	backend, err := pdsclient.ReadRepoBackend(ctx, dyn, workspace, repo)
	if err != nil {
		return nil, err
	}
	pdsRef := backend.PdsRef
	pdsRef.Workspace = pdsRef.InWorkspace(workspace)
	return &pdsRef, nil
}
