package marketintents

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	market "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const clusterAnnotation = "kcp.io/cluster"

var kindGVKs = map[string]schema.GroupVersionKind{
	market.KindBid:             v1alpha1.GroupVersion.WithKind(market.KindBid),
	market.KindComputeVM:       v1alpha1.GroupVersion.WithKind(market.KindComputeVM),
	market.KindStateSync:       v1alpha1.GroupVersion.WithKind(market.KindStateSync),
	market.KindComputeContract: v1alpha1.GroupVersion.WithKind(market.KindComputeContract),
	market.KindExposure:        exposurev1alpha1.GroupVersion.WithKind(market.KindExposure),
	market.KindMarketIdentity:  v1alpha1.GroupVersion.WithKind(market.KindMarketIdentity),
}

func (s *subscriber) matches(workspace, kind string) bool {
	return s.workspaces[workspace] && (len(s.kinds) == 0 || s.kinds[kind])
}

func (c *Controller) listed(ctx context.Context, s *subscriber) (map[key]string, error) {
	out := map[key]string{}
	for workspace := range s.workspaces {
		if c.scope != nil && !c.scope(workspace) {
			continue
		}
		cl, err := c.clients(workspace)
		if err != nil {
			return nil, fmt.Errorf("marketintents: the client of %s: %w", workspace, err)
		}
		for kind, gvk := range kindGVKs {
			if !s.matches(workspace, kind) {
				continue
			}
			list := &metav1.PartialObjectMetadataList{}
			list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
			if err := cl.List(ctx, list); err != nil {
				if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
					continue
				}
				return nil, fmt.Errorf("marketintents: listing the %s objects of %s: %w", kind, workspace, err)
			}
			for _, item := range list.Items {
				out[key{workspace: workspace, kind: kind, name: item.GetName()}] = item.GetAnnotations()[clusterAnnotation]
			}
		}
	}
	return out, nil
}

func (c *Controller) clusterOfLocked(k key, annotated string) string {
	if cluster, ok := c.clusters[k]; ok {
		return cluster
	}
	if annotated != "" {
		return annotated
	}
	for other, cluster := range c.clusters {
		if other.workspace == k.workspace {
			return cluster
		}
	}
	return ""
}

func (c *Controller) observed(s *subscriber, k key) {
	c.mu.Lock()
	if s.pending == nil || s.fired {
		c.mu.Unlock()
		return
	}
	delete(s.pending, k)
	c.mu.Unlock()
	c.settle(s)
}

func (c *Controller) settle(s *subscriber) {
	c.mu.Lock()
	if s.pending == nil || s.fired || len(s.pending) > 0 {
		c.mu.Unlock()
		return
	}
	s.fired = true
	replay := c.replayLocked(s)
	c.mu.Unlock()
	s.synced()
	c.enqueue(replay)
}

func (c *Controller) replayLocked(s *subscriber) map[string][]ctlruntime.Request {
	replay := map[string][]ctlruntime.Request{}
	for k, cluster := range c.clusters {
		if s.matches(k.workspace, k.kind) {
			replay[k.kind] = append(replay[k.kind], ctlruntime.Request{Cluster: cluster, Name: k.name})
		}
	}
	return replay
}

func (c *Controller) enqueue(replay map[string][]ctlruntime.Request) {
	c.mu.Lock()
	queues := map[string]*ctlruntime.Queue{}
	for kind := range replay {
		queues[kind] = c.events[kind]
	}
	c.mu.Unlock()
	for kind, reqs := range replay {
		if q := queues[kind]; q != nil {
			q.Add(reqs...)
		}
	}
}
