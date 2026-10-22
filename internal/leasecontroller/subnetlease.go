package leasecontroller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
)

func (c *Controller) ReconcileSubnetLease(ctx context.Context, workspace string, u *unstructured.Unstructured) error {
	if u == nil {
		return errors.New("leasecontroller: a SubnetLease object is required")
	}
	dyn, err := c.dynamic(workspace)
	if err != nil {
		return fmt.Errorf("leasecontroller: opening the dynamic client for workspace %q: %w", workspace, err)
	}
	var lease v1alpha1.SubnetLease
	if err := decodeObject(u, &lease); err != nil {
		return fmt.Errorf("leasecontroller: decoding SubnetLease %s: %w", u.GetName(), err)
	}
	key := leaseKey(lease.Spec.Workspace, lease.Spec.Cluster, lease.Spec.Purpose, u.GetName())
	if u.GetDeletionTimestamp() != nil {
		for _, held := range uniqueKeys(lease.Status.HolderRef, key) {
			if err := c.subnets.Release(held); err != nil {
				return fmt.Errorf("leasecontroller: releasing subnet lease %s of SubnetLease %s: %w", held, u.GetName(), err)
			}
		}
		return removeFinalizer(ctx, dyn, SubnetLeaseGVR, u, SubnetLeaseFinalizer)
	}
	u, err = addFinalizer(ctx, dyn, SubnetLeaseGVR, u, SubnetLeaseFinalizer)
	if err != nil {
		return err
	}
	if u == nil {
		return nil
	}

	var before v1alpha1.SubnetLeaseStatus
	lease.Status.DeepCopyInto(&before)
	var status v1alpha1.SubnetLeaseStatus
	lease.Status.DeepCopyInto(&status)
	generation := u.GetGeneration()
	status.ObservedGeneration = generation

	if strings.TrimSpace(key) == "" {
		status.Phase = v1alpha1.LeasePhasePending
		setReady(&status.Conditions, generation, false, "InvalidSpec", "the SubnetLease names no workspace, cluster or purpose")
		return writeStatus(ctx, dyn, SubnetLeaseGVR, u, before, status)
	}
	for _, held := range uniqueKeys(status.HolderRef) {
		if held == key {
			continue
		}
		if err := c.subnets.Release(held); err != nil {
			return fmt.Errorf("leasecontroller: releasing the superseded subnet lease %s of SubnetLease %s: %w", held, u.GetName(), err)
		}
	}

	granted, allocErr := c.subnets.Allocate(subnetallocator.Request{Cluster: key})
	if allocErr != nil {
		status.Phase = v1alpha1.LeasePhasePending
		setReady(&status.Conditions, generation, false, "AllocateFailed", allocErr.Error())
		if err := writeStatus(ctx, dyn, SubnetLeaseGVR, u, before, status); err != nil {
			return err
		}
		return fmt.Errorf("leasecontroller: allocating a subnet for SubnetLease %s: %w", u.GetName(), allocErr)
	}
	if requested := strings.TrimSpace(lease.Spec.Prefix); requested != "" {
		want, parseErr := netip.ParsePrefix(requested)
		if parseErr != nil {
			status.Phase = v1alpha1.LeasePhasePending
			setReady(&status.Conditions, generation, false, "InvalidPrefix", parseErr.Error())
			return writeStatus(ctx, dyn, SubnetLeaseGVR, u, before, status)
		}
		if granted.Block.Masked() != want.Masked() {
			if err := c.subnets.Release(key); err != nil {
				return fmt.Errorf("leasecontroller: releasing the subnet granted instead of the requested prefix of SubnetLease %s: %w", u.GetName(), err)
			}
			status.Phase = v1alpha1.LeasePhasePending
			setReady(&status.Conditions, generation, false, "RequestedPrefixUnavailable",
				fmt.Sprintf("the subnet allocator granted %s for %s; the SubnetLease asks for %s", granted.Block.Masked(), key, want.Masked()))
			return writeStatus(ctx, dyn, SubnetLeaseGVR, u, before, status)
		}
	}

	status.Phase = v1alpha1.LeasePhaseAllocated
	status.AllocatedAt = formatTime(granted.AllocatedAt)
	status.HolderRef = key
	setReady(&status.Conditions, generation, true, "Allocated",
		fmt.Sprintf("the subnet allocator holds %s for %s", granted.Block.Masked(), key))
	return writeStatus(ctx, dyn, SubnetLeaseGVR, u, before, status)
}
