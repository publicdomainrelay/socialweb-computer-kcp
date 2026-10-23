package leasecontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func (c *Controller) ReconcilePortLease(ctx context.Context, workspace string, u *unstructured.Unstructured) error {
	if u == nil {
		return errors.New("leasecontroller: a PortLease object is required")
	}
	dyn, err := c.dynamic(workspace)
	if err != nil {
		return fmt.Errorf("leasecontroller: opening the dynamic client for workspace %q: %w", workspace, err)
	}
	var lease v1alpha1.PortLease
	if err := decodeObject(u, &lease); err != nil {
		return fmt.Errorf("leasecontroller: decoding PortLease %s: %w", u.GetName(), err)
	}
	key := leaseKey(lease.Spec.ClusterRef.Workspace, lease.Spec.ClusterRef.Name, lease.Spec.Purpose, u.GetName())
	if u.GetDeletionTimestamp() != nil {
		for _, held := range uniqueKeys(lease.Status.HolderRef, key) {
			if err := c.ports.Release(held); err != nil {
				return fmt.Errorf("leasecontroller: releasing port lease %s of PortLease %s: %w", held, u.GetName(), err)
			}
		}
		return removeFinalizer(ctx, dyn, PortLeaseGVR, u, PortLeaseFinalizer)
	}
	u, err = addFinalizer(ctx, dyn, PortLeaseGVR, u, PortLeaseFinalizer)
	if err != nil {
		return err
	}
	if u == nil {
		return nil
	}

	var before v1alpha1.PortLeaseStatus
	lease.Status.DeepCopyInto(&before)
	var status v1alpha1.PortLeaseStatus
	lease.Status.DeepCopyInto(&status)
	generation := u.GetGeneration()
	status.ObservedGeneration = generation

	if strings.TrimSpace(key) == "" {
		status.Phase = v1alpha1.LeasePhasePending
		setReady(&status.Conditions, generation, false, "InvalidSpec", "the PortLease names no workspace, cluster or purpose")
		return writeStatus(ctx, dyn, PortLeaseGVR, u, before, status)
	}
	for _, held := range uniqueKeys(status.HolderRef) {
		if held == key {
			continue
		}
		if err := c.ports.Release(held); err != nil {
			return fmt.Errorf("leasecontroller: releasing the superseded port lease %s of PortLease %s: %w", held, u.GetName(), err)
		}
	}

	allocated, allocErr := c.ports.Allocate(key)
	if allocErr != nil {
		status.Phase = v1alpha1.LeasePhasePending
		setReady(&status.Conditions, generation, false, "AllocateFailed", allocErr.Error())
		if err := writeStatus(ctx, dyn, PortLeaseGVR, u, before, status); err != nil {
			return err
		}
		return fmt.Errorf("leasecontroller: allocating a port for PortLease %s: %w", u.GetName(), allocErr)
	}
	if lease.Spec.Port > 0 && int(lease.Spec.Port) != allocated.Port {
		if err := c.ports.Release(key); err != nil {
			return fmt.Errorf("leasecontroller: releasing the port granted instead of the requested port of PortLease %s: %w", u.GetName(), err)
		}
		status.Phase = v1alpha1.LeasePhasePending
		setReady(&status.Conditions, generation, false, "RequestedPortUnavailable",
			fmt.Sprintf("the port registry granted port %d for %s; the PortLease asks for port %d", allocated.Port, key, lease.Spec.Port))
		return writeStatus(ctx, dyn, PortLeaseGVR, u, before, status)
	}

	status.Phase = v1alpha1.LeasePhaseAllocated
	status.AllocatedAt = formatTime(allocated.AllocatedAt)
	status.HolderRef = key
	setReady(&status.Conditions, generation, true, "Allocated",
		fmt.Sprintf("the port registry holds port %d on %s for %s", allocated.Port, allocated.Host, key))
	return writeStatus(ctx, dyn, PortLeaseGVR, u, before, status)
}
