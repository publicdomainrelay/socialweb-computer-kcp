package leasecontroller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
)

const (
	FieldManager = "leasecontroller"

	ConditionReady = "Ready"

	ConditionRolling = "Rolling"

	PortLeaseFinalizer = "portlease.socialweb.computer/release"

	SubnetLeaseFinalizer = "subnetlease.socialweb.computer/release"
)

var (
	PortLeaseGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "portleases"}

	SubnetLeaseGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "subnetleases"}

	ClusterRollGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "clusterrolls"}

	ClusterGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "clusters"}
)

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type ReconcileFunc func(ctx context.Context, workspace string, obj *unstructured.Unstructured) error

type Registrar interface {
	Register(gvr schema.GroupVersionResource, reconcile ReconcileFunc) error
}

type PortAllocator interface {
	Allocate(name string) (component.PortLease, error)

	Release(name string) error

	Leases() []component.PortLease
}

type SubnetAllocator interface {
	Allocate(req subnetallocator.Request) (subnetallocator.Lease, error)

	Release(cluster string) error

	Leases() []subnetallocator.Lease
}

type FrontState interface {
	Status(ctx context.Context) (apiserverfront.Status, error)
}

type Options struct {
	Dynamic DynamicFactory

	Ports PortAllocator

	Subnets SubnetAllocator

	Rolls func(cluster string) (kcpserver.Slots, error)

	Front func(cluster string) (FrontState, error)
}

type Controller struct {
	dynamic DynamicFactory

	ports PortAllocator

	subnets SubnetAllocator

	rolls func(cluster string) (kcpserver.Slots, error)

	front func(cluster string) (FrontState, error)
}

func New(opts Options) (*Controller, error) {
	if opts.Dynamic == nil {
		return nil, errors.New("leasecontroller: a dynamic client factory is required")
	}
	if opts.Ports == nil {
		return nil, errors.New("leasecontroller: a port allocator is required")
	}
	if opts.Subnets == nil {
		return nil, errors.New("leasecontroller: a subnet allocator is required")
	}
	if opts.Rolls == nil {
		return nil, errors.New("leasecontroller: a kcpserver roll state reader is required")
	}
	if opts.Front == nil {
		return nil, errors.New("leasecontroller: an apiserver front resolver is required")
	}
	return &Controller{
		dynamic: opts.Dynamic,
		ports:   opts.Ports,
		subnets: opts.Subnets,
		rolls:   opts.Rolls,
		front:   opts.Front,
	}, nil
}

type registration struct {
	gvr       schema.GroupVersionResource
	reconcile ReconcileFunc
}

func Register(reg Registrar, opts Options) error {
	if reg == nil {
		return errors.New("leasecontroller: a registrar is required")
	}
	ctl, err := New(opts)
	if err != nil {
		return err
	}
	entries := []registration{
		{gvr: PortLeaseGVR, reconcile: ctl.ReconcilePortLease},
		{gvr: SubnetLeaseGVR, reconcile: ctl.ReconcileSubnetLease},
		{gvr: ClusterRollGVR, reconcile: ctl.ReconcileClusterRoll},
		{gvr: ClusterGVR, reconcile: ctl.ReconcileCluster},
	}
	for _, entry := range entries {
		if err := reg.Register(entry.gvr, entry.reconcile); err != nil {
			return fmt.Errorf("leasecontroller: registering %s: %w", entry.gvr.Resource, err)
		}
	}
	return nil
}

func decodeObject(u *unstructured.Unstructured, out any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}

func writeStatus(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, u *unstructured.Unstructured, before, after any) error {
	if reflect.DeepEqual(before, after) {
		return nil
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(after)
	if err != nil {
		return fmt.Errorf("leasecontroller: encoding the %s status of %s: %w", gvr.Resource, u.GetName(), err)
	}
	next := u.DeepCopy()
	next.Object["status"] = raw
	if _, err := dyn.Resource(gvr).UpdateStatus(ctx, next, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("leasecontroller: updating the %s status of %s: %w", gvr.Resource, u.GetName(), err)
	}
	return nil
}

func hasFinalizer(u *unstructured.Unstructured, finalizer string) bool {
	for _, held := range u.GetFinalizers() {
		if held == finalizer {
			return true
		}
	}
	return false
}

func addFinalizer(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, u *unstructured.Unstructured, finalizer string) (*unstructured.Unstructured, error) {
	if hasFinalizer(u, finalizer) {
		return u, nil
	}
	next := u.DeepCopy()
	next.SetFinalizers(append(next.GetFinalizers(), finalizer))
	out, err := dyn.Resource(gvr).Update(ctx, next, metav1.UpdateOptions{FieldManager: FieldManager})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return u, fmt.Errorf("leasecontroller: adding finalizer %s to %s %s: %w", finalizer, gvr.Resource, u.GetName(), err)
	}
	return out, nil
}

func removeFinalizer(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, u *unstructured.Unstructured, finalizer string) error {
	if !hasFinalizer(u, finalizer) {
		return nil
	}
	next := u.DeepCopy()
	held := next.GetFinalizers()
	kept := make([]string, 0, len(held))
	for _, name := range held {
		if name != finalizer {
			kept = append(kept, name)
		}
	}
	next.SetFinalizers(kept)
	if _, err := dyn.Resource(gvr).Update(ctx, next, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("leasecontroller: removing finalizer %s from %s %s: %w", finalizer, gvr.Resource, u.GetName(), err)
	}
	return nil
}

func leaseKey(workspace, cluster, purpose, name string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{workspace, cluster, purpose} {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(name)
	}
	return strings.Join(parts, "/")
}

func uniqueKeys(keys ...string) []string {
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func setCondition(conditions *[]metav1.Condition, generation int64, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

func setReady(conditions *[]metav1.Condition, generation int64, ready bool, reason, message string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	setCondition(conditions, generation, ConditionReady, status, reason, message)
}
