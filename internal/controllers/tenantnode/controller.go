package tenantnode

import (
	"context"
	"errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/cloudprovider"
)

type Options struct {
	Reconciler *cloudprovider.Reconciler

	InstanceChanges <-chan struct{}

	Queue workqueue.TypedRateLimitingInterface[string]

	Log func(event string, fields map[string]any)
}

type Controller struct {
	reconciler *cloudprovider.Reconciler
	cluster    string
	changes    <-chan struct{}
	queue      workqueue.TypedRateLimitingInterface[string]
	log        func(event string, fields map[string]any)
	informer   cache.SharedIndexInformer
}

func New(o Options, client kubernetes.Interface) (*Controller, error) {
	if o.Reconciler == nil {
		return nil, errors.New(
			"tenantnode: the controller has no reconciler, so every Node event would be observed " +
				"and dropped")
	}
	if o.InstanceChanges == nil {
		return nil, errors.New(
			"tenantnode: the controller has no instance change source, so a guest that is " +
				"provisioned, drained or reaped would never reach the Node that it backs")
	}
	if client == nil {
		return nil, errors.New(
			"tenantnode: the controller has no client, so it cannot watch the cluster's Nodes")
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

	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return client.CoreV1().Nodes().List(ctx, opts)
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			return client.CoreV1().Nodes().Watch(ctx, opts)
		},
	}

	c := &Controller{
		reconciler: o.Reconciler,
		cluster:    o.Reconciler.Cluster(),
		changes:    o.InstanceChanges,
		queue:      queue,
		log:        log,
		informer:   cache.NewSharedIndexInformer(lw, &corev1.Node{}, 0, cache.Indexers{}),
	}
	if _, err := c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, obj any) { c.enqueue(obj) },
		DeleteFunc: c.enqueue,
	}); err != nil {
		return nil, fmt.Errorf("tenantnode: attaching the Node event handler: %w", err)
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
			"tenantnode: the Node informer did not sync before shutdown, so no Node was ever " +
				"observed and nothing is written")
	}
	c.log("cloud controller manager: status", map[string]any{
		"phase":   "Ready",
		"cluster": c.cluster,
		"nodes":   len(c.informer.GetStore().List()),
	})
	var watchErr error
	wg.Go(func() {
		defer c.queue.ShutDown()
		for {
			select {
			case <-runCtx.Done():
				return
			case _, ok := <-c.changes:
				if !ok {
					if runCtx.Err() == nil {
						watchErr = errors.New(
							"tenantnode: the watch on the instance snapshot ended, so a change " +
								"to the instances behind the Nodes would no longer reach them")
					}
					return
				}
				c.enqueueAll()
			}
		}
	})
	for {
		nodeName, shutdown := c.queue.Get()
		if shutdown {
			cancel()
			wg.Wait()
			return watchErr
		}
		err := c.reconcile(ctx, nodeName)
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

func (c *Controller) reconcile(ctx context.Context, nodeName string) error {
	node, present, err := c.node(nodeName)
	if err != nil {
		return err
	}
	return c.reconciler.Reconcile(ctx, nodeName, node, present)
}

func (c *Controller) node(nodeName string) (corev1.Node, bool, error) {
	obj, present, err := c.informer.GetStore().GetByKey(nodeName)
	if err != nil {
		return corev1.Node{}, false, fmt.Errorf(
			"tenantnode: reading the Node %s out of the informer cache: %w", nodeName, err)
	}
	if !present {
		return corev1.Node{}, false, nil
	}
	node, ok := obj.(*corev1.Node)
	if !ok {
		return corev1.Node{}, false, fmt.Errorf(
			"tenantnode: the informer cache holds a %T under %s, not a Node", obj, nodeName)
	}
	return *node, true, nil
}

func (c *Controller) enqueueAll() {
	for _, key := range c.informer.GetStore().ListKeys() {
		c.queue.Add(key)
	}
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
