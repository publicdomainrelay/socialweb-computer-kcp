package leasecontroller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

func (c *Controller) ReconcileCluster(ctx context.Context, workspace string, u *unstructured.Unstructured) error {
	if u == nil {
		return errors.New("leasecontroller: a Cluster object is required")
	}
	dyn, err := c.dynamic(workspace)
	if err != nil {
		return fmt.Errorf("leasecontroller: opening the dynamic client for workspace %q: %w", workspace, err)
	}
	var cluster v1alpha1.Cluster
	if err := unstruct.Into(u, &cluster); err != nil {
		return fmt.Errorf("leasecontroller: decoding Cluster %s: %w", u.GetName(), err)
	}
	name := strings.TrimSpace(cluster.GetName())
	if name == "" {
		name = strings.TrimSpace(u.GetName())
	}

	var before v1alpha1.ClusterStatus
	cluster.Status.DeepCopyInto(&before)
	var status v1alpha1.ClusterStatus
	cluster.Status.DeepCopyInto(&status)
	generation := u.GetGeneration()

	front, frontErr := c.front(name)
	if frontErr != nil {
		status.HeldPorts = mergeHeldPorts(nil, name, status.HeldPorts, registryHeldPorts(c.ports.Leases(), workspace, name))
		setReady(&status.Conditions, generation, false, "FrontUnavailable", frontErr.Error())
		if err := writeStatus(ctx, dyn, ClusterGVR, u, before, status); err != nil {
			return err
		}
		return fmt.Errorf("leasecontroller: resolving the apiserver front of cluster %s: %w", name, frontErr)
	}

	state, stateErr := front.Status(ctx)
	if stateErr != nil {
		status.HeldPorts = mergeHeldPorts(nil, name, status.HeldPorts, registryHeldPorts(c.ports.Leases(), workspace, name))
		setReady(&status.Conditions, generation, false, "FrontUnreachable", stateErr.Error())
		if err := writeStatus(ctx, dyn, ClusterGVR, u, before, status); err != nil {
			return err
		}
		return fmt.Errorf("leasecontroller: reading the apiserver front of cluster %s: %w", name, stateErr)
	}

	status.FrontAddress = state.Address
	status.HeldPorts = mergeHeldPorts(state.Holds, name, registryHeldPorts(c.ports.Leases(), workspace, name))
	if strings.TrimSpace(state.Address) == "" {
		setReady(&status.Conditions, generation, false, "FrontNotListening",
			"the apiserver front of cluster "+name+" reports no listen address")
	} else {
		setReady(&status.Conditions, generation, true, "FrontReady",
			"the apiserver front of cluster "+name+" listens at "+state.Address)
	}
	return writeStatus(ctx, dyn, ClusterGVR, u, before, status)
}

func mergeHeldPorts(front map[string]int, cluster string, extra ...[]v1alpha1.ClusterHeldPort) []v1alpha1.ClusterHeldPort {
	byPort := make(map[int32]v1alpha1.ClusterHeldPort, len(front))
	for purpose, port := range front {
		if port <= 0 || port > 65535 {
			continue
		}
		byPort[int32(port)] = v1alpha1.ClusterHeldPort{
			Port:      int32(port),
			Purpose:   purpose,
			HolderRef: cluster + "/front",
		}
	}
	for _, group := range extra {
		for _, held := range group {
			if held.Port <= 0 || held.Port > 65535 {
				continue
			}
			if _, ok := byPort[held.Port]; ok {
				continue
			}
			byPort[held.Port] = held
		}
	}
	if len(byPort) == 0 {
		return nil
	}
	out := make([]v1alpha1.ClusterHeldPort, 0, len(byPort))
	for _, held := range byPort {
		out = append(out, held)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func registryHeldPorts(leases []component.PortLease, workspace, cluster string) []v1alpha1.ClusterHeldPort {
	out := make([]v1alpha1.ClusterHeldPort, 0, len(leases))
	for _, lease := range leases {
		if !heldByCluster(lease.Name, workspace, cluster) {
			continue
		}
		if lease.Port <= 0 || lease.Port > 65535 {
			continue
		}
		purpose := lease.Name
		if i := strings.LastIndex(lease.Name, "/"); i >= 0 && i+1 < len(lease.Name) {
			purpose = lease.Name[i+1:]
		}
		out = append(out, v1alpha1.ClusterHeldPort{
			Port:      int32(lease.Port),
			Purpose:   purpose,
			HolderRef: lease.Name,
		})
	}
	return out
}

func heldByCluster(name, workspace, cluster string) bool {
	name = strings.TrimSpace(name)
	workspace = strings.TrimSpace(workspace)
	cluster = strings.TrimSpace(cluster)
	if name == "" || cluster == "" {
		return false
	}
	if name == cluster || strings.HasPrefix(name, cluster+"/") {
		return true
	}
	if workspace == "" {
		return false
	}
	key := workspace + "/" + cluster
	return name == key || strings.HasPrefix(name, key+"/")
}
