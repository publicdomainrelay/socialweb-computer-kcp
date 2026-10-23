package cloudprovider

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

type NodeWriter interface {
	PatchProviderID(ctx context.Context, nodeName, providerID string) error

	SetLabels(ctx context.Context, nodeName string, set map[string]string, remove []string) error

	DeleteNode(ctx context.Context, nodeName string) error
}

type Options struct {
	Cluster string

	Instances Instances

	Nodes NodeWriter

	Log func(event string, fields map[string]any)
}

// ponytail: this is a hand-rolled subset of the Kubernetes cloud-provider node contracts -- node
// init (providerID and labels) and node lifecycle (delete on gone or draining) only. The ceiling is
// that it is not a cloudprovider.Interface, so it can serve no zone, route or load balancer. The
// upgrade path is k8s.io/cloud-provider/app with a cloudprovider.Interface once a load balancer or a
// zone concept exists here.
type Reconciler struct {
	cluster   string
	instances Instances
	nodes     NodeWriter
	log       func(event string, fields map[string]any)
}

func NewReconciler(o Options) (*Reconciler, error) {
	if o.Cluster == "" {
		return nil, errors.New(
			"cloudprovider: the controller needs the name of the cluster whose Nodes it owns; an " +
				"empty one would stamp provider IDs and labels that name no cluster")
	}
	if o.Instances == nil {
		return nil, errors.New(
			"cloudprovider: the controller has no instance source, so every Node would read as " +
				"unknown and be deleted")
	}
	if o.Nodes == nil {
		return nil, errors.New(
			"cloudprovider: the controller has no Node writer, so every decision would be made " +
				"and dropped")
	}
	log := o.Log
	if log == nil {
		log = func(string, map[string]any) {}
	}
	return &Reconciler{
		cluster:   o.Cluster,
		instances: o.Instances,
		nodes:     o.Nodes,
		log:       log,
	}, nil
}

func (r *Reconciler) Cluster() string {
	return r.cluster
}

func (r *Reconciler) Reconcile(ctx context.Context, nodeName string, node corev1.Node, present bool) error {
	if nodeName == "" {
		return errors.New("cloudprovider: reconcile was given an empty node name")
	}
	if !present {
		r.log("cloud controller manager: node gone from the cache", map[string]any{
			"cluster": r.cluster,
			"node":    nodeName,
		})
		return nil
	}

	inst, known, err := r.instances.Get(ctx, nodeName)
	if err != nil {
		return fmt.Errorf(
			"cloudprovider: the instance source for %s could not be read: %w. An unreadable "+
				"source is NOT an absent instance, and acting on it would delete the Node of a "+
				"guest that is still running its work, so nothing is written and the Node is "+
				"requeued", nodeName, err)
	}
	if known && inst.Cluster == "" {
		return fmt.Errorf(
			"cloudprovider: the instance record for %s names no cluster. A record with no "+
				"cluster is MALFORMED, not absent: it yields no provider ID, and stamping the "+
				"Node anyway would leave whatever identity it carries -- one from a previous "+
				"life -- in place while its cluster label was overwritten. Nothing is written "+
				"and the Node is requeued", nodeName)
	}
	if known && inst.NodeName == "" {
		return fmt.Errorf(
			"cloudprovider: the instance record for %s names no node. A record with no node is "+
				"MALFORMED, not absent: it yields no provider ID, and nothing may be written "+
				"against it, so the Node is requeued", nodeName)
	}

	patch, changed := Desired(node, inst, known)
	if !changed {
		return nil
	}

	if patch.Delete {
		if err := r.nodes.DeleteNode(ctx, nodeName); err != nil {
			return fmt.Errorf("cloudprovider: deleting the Node %s because %s: %w",
				nodeName, patch.Reason, err)
		}
		r.log("cloud controller manager: node deleted", map[string]any{
			"phase":   "Reconciled",
			"cluster": r.cluster,
			"node":    nodeName,
			"reason":  patch.Reason,
		})
		return nil
	}

	if patch.ProviderID != "" && node.Spec.ProviderID != patch.ProviderID {
		if err := r.nodes.PatchProviderID(ctx, nodeName, patch.ProviderID); err != nil {
			return fmt.Errorf("cloudprovider: stamping %s on the Node %s: %w",
				patch.ProviderID, nodeName, err)
		}
	}
	if len(patch.SetLabels)+len(patch.RemoveLabels) > 0 {
		if err := r.nodes.SetLabels(ctx, nodeName, patch.SetLabels, patch.RemoveLabels); err != nil {
			return fmt.Errorf("cloudprovider: labelling the Node %s: %w", nodeName, err)
		}
	}
	r.log("cloud controller manager: node stamped", map[string]any{
		"phase":      "Reconciled",
		"cluster":    r.cluster,
		"node":       nodeName,
		"providerID": patch.ProviderID,
		"reason":     patch.Reason,
	})
	return nil
}
