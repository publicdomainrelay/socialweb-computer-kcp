package guestcontrolplane

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
)

func (c *Controller) Provide() {
	bus.Provide(c.bus, GroupKind, guestcp.CapabilityInputs, func(_ context.Context, to commonv1.ObjectRef, _ []string) (guestcp.Source, error) {
		workspace, name, err := target(to)
		if err != nil {
			return nil, err
		}
		return c.feedFor(workspace, name), nil
	})
	bus.Provide(c.bus, GroupKind, guestcp.CapabilityGuests, func(_ context.Context, to commonv1.ObjectRef, _ []string) (guestcp.Guests, error) {
		workspace, _, err := target(to)
		if err != nil {
			return nil, err
		}
		return guests{store: c.store, workspace: workspace}, nil
	})
	bus.Provide(c.bus, GroupKind, guestcp.CapabilityGrants, func(_ context.Context, to commonv1.ObjectRef, _ []string) (guestcp.Grants, error) {
		workspace, _, err := target(to)
		if err != nil {
			return nil, err
		}
		return grants{store: c.store, workspace: workspace}, nil
	})
}

func target(to commonv1.ObjectRef) (string, string, error) {
	workspace, name := strings.TrimSpace(to.Workspace), strings.TrimSpace(to.Name)
	if workspace == "" || name == "" {
		return "", "", fmt.Errorf("guestcontrolplane: the handle target %s names no workspace or name", to.String())
	}
	return workspace, name, nil
}

type feed struct {
	mu sync.Mutex

	inputs guestcp.Inputs

	held bool

	next int

	watchers map[int]func(guestcp.Inputs)
}

var _ guestcp.Source = (*feed)(nil)

func newFeed() *feed {
	return &feed{watchers: map[int]func(guestcp.Inputs){}}
}

func (f *feed) Inputs(context.Context) (guestcp.Inputs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs, nil
}

func (f *feed) Watch(fn func(guestcp.Inputs)) func() {
	if fn == nil {
		return func() {}
	}
	f.mu.Lock()
	id := f.next
	f.next++
	f.watchers[id] = fn
	in, held := f.inputs, f.held
	f.mu.Unlock()
	if held {
		fn(in)
	}
	return func() {
		f.mu.Lock()
		delete(f.watchers, id)
		f.mu.Unlock()
	}
}

func (f *feed) publish(in guestcp.Inputs) bool {
	f.mu.Lock()
	if f.held && reflect.DeepEqual(f.inputs, in) {
		f.mu.Unlock()
		return false
	}
	f.inputs, f.held = in, true
	watchers := make([]func(guestcp.Inputs), 0, len(f.watchers))
	for _, fn := range f.watchers {
		watchers = append(watchers, fn)
	}
	f.mu.Unlock()
	for _, fn := range watchers {
		fn(in)
	}
	return true
}

func (f *feed) close() {
	f.mu.Lock()
	f.watchers = map[int]func(guestcp.Inputs){}
	f.mu.Unlock()
}

type guests struct {
	store Store

	workspace string
}

var _ guestcp.Guests = guests{}

func (g guests) RecordGuest(ctx context.Context, name string, report v1alpha1.ComputeVMGuestReport) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("guestcontrolplane: the guest report names no ComputeVM")
	}
	guest, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&report)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "ComputeVM",
		"metadata":   map[string]any{"name": name},
		"status":     map[string]any{"market": map[string]any{"guest": guest}},
	}}
	return g.store.Apply(ctx, g.workspace, FieldManager, u, true)
}

type grants struct {
	store Store

	workspace string
}

var _ guestcp.Grants = grants{}

func (g grants) Apply(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error {
	if strings.TrimSpace(grant.Name) == "" {
		return errors.New("guestcontrolplane: applying a GuestIdentityGrant with no name")
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&grant)
	if err != nil {
		return err
	}
	metadata := map[string]any{"name": grant.Name}
	if owners, ok, _ := unstructured.NestedSlice(object, "metadata", "ownerReferences"); ok && len(owners) > 0 {
		metadata["ownerReferences"] = owners
	}
	if labels, ok, _ := unstructured.NestedMap(object, "metadata", "labels"); ok && len(labels) > 0 {
		metadata["labels"] = labels
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "GuestIdentityGrant",
		"metadata":   metadata,
		"spec":       object["spec"],
	}}
	return g.store.Apply(ctx, g.workspace, FieldManager, u, false)
}

func (g grants) Delete(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	grant := &v1alpha1.GuestIdentityGrant{}
	grant.Name = name
	return g.store.Delete(ctx, g.workspace, grant)
}
