package gateway

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

type publishers struct {
	mu sync.Mutex

	clusters map[string]string

	roots map[string]ctlruntime.Request
}

func newPublishers() *publishers {
	return &publishers{clusters: map[string]string{}, roots: map[string]ctlruntime.Request{}}
}

func rootKey(req ctlruntime.Request) string {
	return req.Cluster + "|" + req.Workspace + "|" + req.Name
}

func (s *publishers) root(req ctlruntime.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roots[rootKey(req)] = req
}

func (s *publishers) forget(req ctlruntime.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roots, rootKey(req))
}

func (s *publishers) requests() []ctlruntime.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(s.roots))
	for _, req := range s.roots {
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool { return rootKey(out[i]) < rootKey(out[j]) })
	return out
}

func (s *publishers) observe(workspace string, cl *v1alpha1.Cluster) bool {
	key := workspace + "|" + cl.Name
	did := strings.TrimSpace(cl.Spec.Account)
	want := cl.Spec.PublishInfra && cl.DeletionTimestamp == nil && did != ""
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.clusters[key]
	switch {
	case want && (!had || prev != did):
		s.clusters[key] = did
		return true
	case !want && had:
		delete(s.clusters, key)
		return true
	}
	return false
}

func (s *publishers) owners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, did := range s.clusters {
		if !slices.Contains(out, did) {
			out = append(out, did)
		}
	}
	sort.Strings(out)
	return out
}

func (ctl *Controller) publishWatches() []ctlruntime.Option {
	return []ctlruntime.Option{
		ctlruntime.WatchesIn(compose.ExportClusters, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.Cluster) []ctlruntime.Request {
			if ctl.publishers.observe(src.Workspace, obj) {
				return ctl.publishers.requests()
			}
			return nil
		}),
		ctlruntime.WatchesIn(compose.ExportMarketIdentities, func(_ context.Context, _ ctlruntime.Request, obj *v1alpha1.AtprotoRecord) []ctlruntime.Request {
			if _, infra := obj.Labels[v1alpha1.InfraKindLabel]; infra && obj.DeletionTimestamp != nil {
				return ctl.publishers.requests()
			}
			return nil
		}),
	}
}
