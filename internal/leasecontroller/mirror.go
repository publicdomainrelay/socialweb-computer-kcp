package leasecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func (c *Controller) Mirror(ctx context.Context, workspace string) error {
	dyn, err := c.dynamic(workspace)
	if err != nil {
		return fmt.Errorf("leasecontroller: opening the dynamic client for workspace %q: %w", workspace, err)
	}
	return errors.Join(c.mirrorPortLeases(ctx, dyn, workspace), c.mirrorSubnetLeases(ctx, dyn, workspace))
}

func (c *Controller) mirrorPortLeases(ctx context.Context, dyn dynamic.Interface, workspace string) error {
	existing, err := portLeaseKeys(ctx, dyn)
	if err != nil {
		return err
	}
	for _, lease := range c.ports.Leases() {
		key := strings.TrimSpace(lease.Name)
		ws, cluster, purpose, ok := mirrorParts(key)
		if !ok {
			continue
		}
		if ws != "" && workspace != "" && ws != workspace {
			continue
		}
		if lease.Port <= 0 || lease.Port > 65535 {
			continue
		}
		identity := leaseKey(ws, cluster, purpose, "")
		if existing[identity] {
			continue
		}
		obj := &v1alpha1.PortLease{
			TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "PortLease"},
			ObjectMeta: metav1.ObjectMeta{
				Name:       mirrorName("portlease", key),
				Finalizers: []string{PortLeaseFinalizer},
			},
			Spec: v1alpha1.PortLeaseSpec{
				Workspace: ws,
				Cluster:   cluster,
				Purpose:   purpose,
				Port:      int32(lease.Port),
			},
		}
		if err := createMirror(ctx, dyn, PortLeaseGVR, obj, key); err != nil {
			return err
		}
		existing[identity] = true
	}
	return nil
}

func (c *Controller) mirrorSubnetLeases(ctx context.Context, dyn dynamic.Interface, workspace string) error {
	existing, err := subnetLeaseKeys(ctx, dyn)
	if err != nil {
		return err
	}
	for _, lease := range c.subnets.Leases() {
		key := strings.TrimSpace(lease.Cluster)
		ws, cluster, purpose, ok := mirrorParts(key)
		if !ok {
			continue
		}
		if ws != "" && workspace != "" && ws != workspace {
			continue
		}
		if !lease.Block.IsValid() {
			continue
		}
		identity := leaseKey(ws, cluster, purpose, "")
		if existing[identity] {
			continue
		}
		obj := &v1alpha1.SubnetLease{
			TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "SubnetLease"},
			ObjectMeta: metav1.ObjectMeta{
				Name:       mirrorName("subnetlease", key),
				Finalizers: []string{SubnetLeaseFinalizer},
			},
			Spec: v1alpha1.SubnetLeaseSpec{
				Workspace: ws,
				Cluster:   cluster,
				Purpose:   purpose,
				Prefix:    lease.Block.Masked().String(),
			},
		}
		if err := createMirror(ctx, dyn, SubnetLeaseGVR, obj, key); err != nil {
			return err
		}
		existing[identity] = true
	}
	return nil
}

func createMirror(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, obj any, key string) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("leasecontroller: encoding a mirrored %s for %s: %w", gvr.Resource, key, err)
	}
	u := &unstructured.Unstructured{Object: raw}
	if _, err := dyn.Resource(gvr).Create(ctx, u, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("leasecontroller: creating a mirrored %s for %s: %w", gvr.Resource, key, err)
	}
	return nil
}

func portLeaseKeys(ctx context.Context, dyn dynamic.Interface) (map[string]bool, error) {
	list, err := dyn.Resource(PortLeaseGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("leasecontroller: listing PortLease objects to mirror the port registry: %w", err)
	}
	keys := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		var lease v1alpha1.PortLease
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &lease); err != nil {
			continue
		}
		keys[leaseKey(lease.Spec.Workspace, lease.Spec.Cluster, lease.Spec.Purpose, list.Items[i].GetName())] = true
	}
	return keys, nil
}

func subnetLeaseKeys(ctx context.Context, dyn dynamic.Interface) (map[string]bool, error) {
	list, err := dyn.Resource(SubnetLeaseGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("leasecontroller: listing SubnetLease objects to mirror the subnet allocator: %w", err)
	}
	keys := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		var lease v1alpha1.SubnetLease
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &lease); err != nil {
			continue
		}
		keys[leaseKey(lease.Spec.Workspace, lease.Spec.Cluster, lease.Spec.Purpose, list.Items[i].GetName())] = true
	}
	return keys, nil
}

func mirrorParts(key string) (workspace, cluster, purpose string, ok bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", "", false
	}
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	switch {
	case len(parts) >= 3:
		workspace = strings.Join(parts[:len(parts)-2], "/")
		cluster = parts[len(parts)-2]
		purpose = parts[len(parts)-1]
	case len(parts) == 2:
		cluster, purpose = parts[0], parts[1]
	default:
		cluster = parts[0]
	}
	if leaseKey(workspace, cluster, purpose, "") != key {
		return "", "", "", false
	}
	return workspace, cluster, purpose, true
}

func mirrorName(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + "-" + hex.EncodeToString(sum[:])[:16]
}
