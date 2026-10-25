package compose

import (
	"cmp"
	"context"
	"sync"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

type parkState struct {
	generation int64

	reason string
}

type parking struct {
	mu sync.Mutex

	roots map[string]parkState
}

func ParkReason(phase v1alpha1.ApplicationPhase, identity v1alpha1.MarketIdentityPhase) string {
	switch identity {
	case v1alpha1.MarketIdentityPhaseSessionExpired, v1alpha1.MarketIdentityPhaseAwaitingScan, v1alpha1.MarketIdentityPhaseFailed:
		return "identity " + string(identity)
	}
	if phase == v1alpha1.ApplicationPhaseFailed {
		return "root Failed"
	}
	return ""
}

func (c *Composer) Park(ctx context.Context, r Root, streamRole, reason string) {
	key := adoptKey(r.Cluster, r.Kind, r.Name)
	c.parking.mu.Lock()
	if c.parking.roots == nil {
		c.parking.roots = map[string]parkState{}
	}
	prior, was := c.parking.roots[key]
	c.parking.roots[key] = parkState{generation: r.Generation, reason: reason}
	c.parking.mu.Unlock()
	if was && prior.generation == r.Generation && prior.reason == reason {
		return
	}
	c.log.Info("root_parked", "root", r.Owner(), "reason", reason, "generation", r.Generation, "streamsStopped", streamRole != "")
	if streamRole == "" {
		return
	}
	if err := c.Prune(ctx, r.Workspace, eventsv1alpha1.EventStreamGVR, marketStreamLabel+"="+streamRole, nil); err != nil && ctx.Err() == nil {
		c.log.Warn("stopping the market streams of a parked root", "root", r.Owner(), "err", err.Error())
	}
}

func (c *Composer) Parked(r Root) bool {
	key := adoptKey(r.Cluster, r.Kind, r.Name)
	c.parking.mu.Lock()
	defer c.parking.mu.Unlock()
	st, ok := c.parking.roots[key]
	if !ok {
		return false
	}
	if r.Deleting || st.generation != r.Generation {
		delete(c.parking.roots, key)
		return false
	}
	return true
}

func (c *Composer) unpark(rootKind string, reqs []ctlruntime.Request, src ctlruntime.Request) {
	c.parking.mu.Lock()
	defer c.parking.mu.Unlock()
	for _, req := range reqs {
		key := adoptKey(cmp.Or(req.Cluster, src.Cluster), rootKind, req.Name)
		if st, ok := c.parking.roots[key]; ok {
			delete(c.parking.roots, key)
			c.log.Info("root_unparked", "root", OwnerName(rootKind, req.Name), "reason", st.reason)
		}
	}
}

func (c *Composer) unparked(rootKind string, reqs []ctlruntime.Request, src ctlruntime.Request) []ctlruntime.Request {
	c.parking.mu.Lock()
	defer c.parking.mu.Unlock()
	if len(c.parking.roots) == 0 {
		return reqs
	}
	out := reqs[:0:0]
	for _, req := range reqs {
		if _, ok := c.parking.roots[adoptKey(cmp.Or(req.Cluster, src.Cluster), rootKind, req.Name)]; !ok {
			out = append(out, req)
		}
	}
	return out
}
