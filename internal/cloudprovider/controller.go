package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const DefaultResync = 30 * time.Second

type NodeWriter interface {
	PatchProviderID(ctx context.Context, nodeName, providerID string) error

	SetLabels(ctx context.Context, nodeName string, set map[string]string, remove []string) error

	DeleteNode(ctx context.Context, nodeName string) error
}

type Options struct {
	Cluster string

	Instances Instances

	Nodes NodeWriter

	Queue workqueue.TypedRateLimitingInterface[string]

	Log func(event string, fields map[string]any)

	Resync time.Duration
}

// ponytail: this is a hand-rolled subset of the Kubernetes cloud-provider node contracts -- node
// init (providerID and labels) and node lifecycle (delete on gone or draining) only. The ceiling is
// that it is not a cloudprovider.Interface, so it can serve no zone, route or load balancer. The
// upgrade path is k8s.io/cloud-provider/app with a cloudprovider.Interface once a load balancer or a
// zone concept exists here.
type Controller struct {
	cluster   string
	instances Instances
	nodes     NodeWriter
	queue     workqueue.TypedRateLimitingInterface[string]
	log       func(event string, fields map[string]any)
	informer  cache.SharedIndexInformer
}

func New(o Options, client kubernetes.Interface) (*Controller, error) {
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
	if client == nil {
		return nil, errors.New(
			"cloudprovider: the controller has no client, so it cannot watch the cluster's Nodes")
	}

	queue := o.Queue
	if queue == nil {
		queue = workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]())
	}
	log := o.Log
	if log == nil {
		log = func(string, map[string]any) {}
	}
	resync := o.Resync
	if resync <= 0 {
		resync = DefaultResync
	}

	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return client.CoreV1().Nodes().List(ctx, opts)
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			return client.CoreV1().Nodes().Watch(ctx, opts)
		},
	}

	c := &Controller{
		cluster:   o.Cluster,
		instances: o.Instances,
		nodes:     o.Nodes,
		queue:     queue,
		log:       log,
		informer:  cache.NewSharedIndexInformer(lw, &corev1.Node{}, resync, cache.Indexers{}),
	}
	if _, err := c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, obj any) { c.enqueue(obj) },
		DeleteFunc: c.enqueue,
	}); err != nil {
		return nil, fmt.Errorf("cloudprovider: attaching the Node event handler: %w", err)
	}
	return c, nil
}

func (c *Controller) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.queue.ShutDown()
	wg.Go(func() { c.informer.Run(runCtx.Done()) })
	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		return errors.New(
			"cloudprovider: the Node informer did not sync before shutdown, so no Node was ever " +
				"observed and nothing is written")
	}
	c.log("cloud controller manager: status", map[string]any{
		"phase":   "Ready",
		"cluster": c.cluster,
		"nodes":   len(c.informer.GetStore().List()),
	})
	wg.Go(func() {
		<-runCtx.Done()
		c.queue.ShutDown()
	})
	for {
		nodeName, shutdown := c.queue.Get()
		if shutdown {
			return nil
		}
		err := c.Reconcile(ctx, nodeName)
		if err != nil {
			c.log("cloud controller manager: reconcile failed", map[string]any{
				"phase":   "Degraded",
				"cluster": c.cluster,
				"node":    nodeName,
				"error":   err.Error(),
			})
			c.queue.AddRateLimited(nodeName)
		} else {
			c.queue.Forget(nodeName)
		}
		c.queue.Done(nodeName)
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeName string) error {
	if nodeName == "" {
		return errors.New("cloudprovider: reconcile was given an empty node name")
	}
	node, present, err := c.node(nodeName)
	if err != nil {
		return err
	}
	if !present {
		c.log("cloud controller manager: node gone from the cache", map[string]any{
			"cluster": c.cluster,
			"node":    nodeName,
		})
		return nil
	}

	inst, known, err := c.instances.Get(ctx, nodeName)
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
		if err := c.nodes.DeleteNode(ctx, nodeName); err != nil {
			return fmt.Errorf("cloudprovider: deleting the Node %s because %s: %w",
				nodeName, patch.Reason, err)
		}
		c.log("cloud controller manager: node deleted", map[string]any{
			"phase":   "Reconciled",
			"cluster": c.cluster,
			"node":    nodeName,
			"reason":  patch.Reason,
		})
		return nil
	}

	if patch.ProviderID != "" && node.Spec.ProviderID != patch.ProviderID {
		if err := c.nodes.PatchProviderID(ctx, nodeName, patch.ProviderID); err != nil {
			return fmt.Errorf("cloudprovider: stamping %s on the Node %s: %w",
				patch.ProviderID, nodeName, err)
		}
	}
	if len(patch.SetLabels)+len(patch.RemoveLabels) > 0 {
		if err := c.nodes.SetLabels(ctx, nodeName, patch.SetLabels, patch.RemoveLabels); err != nil {
			return fmt.Errorf("cloudprovider: labelling the Node %s: %w", nodeName, err)
		}
	}
	c.log("cloud controller manager: node stamped", map[string]any{
		"phase":      "Reconciled",
		"cluster":    c.cluster,
		"node":       nodeName,
		"providerID": patch.ProviderID,
		"reason":     patch.Reason,
	})
	return nil
}

func (c *Controller) node(nodeName string) (corev1.Node, bool, error) {
	obj, present, err := c.informer.GetStore().GetByKey(nodeName)
	if err != nil {
		return corev1.Node{}, false, fmt.Errorf(
			"cloudprovider: reading the Node %s out of the informer cache: %w", nodeName, err)
	}
	if !present {
		return corev1.Node{}, false, nil
	}
	node, ok := obj.(*corev1.Node)
	if !ok {
		return corev1.Node{}, false, fmt.Errorf(
			"cloudprovider: the informer cache holds a %T under %s, not a Node", obj, nodeName)
	}
	return *node, true, nil
}

func (c *Controller) enqueue(obj any) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		c.log("cloud controller manager: node event ignored", map[string]any{
			"cluster": c.cluster,
			"error":   err.Error(),
		})
		return
	}
	c.queue.Add(key)
}
