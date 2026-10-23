package queue

import (
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const defaultLeaseTTL = 2 * time.Minute

type Lease struct {
	Parent ref.Ref
}

type Leases struct {
	entries *ttlcache.Cache[ref.Ref, Lease]
}

func NewLeases(ttl time.Duration, now func() time.Time) *Leases {
	ttl = opt.Positive(ttl, defaultLeaseTTL)
	return &Leases{entries: ttlcache.New[ref.Ref, Lease](ttlcache.Options{TTL: ttl, Now: now})}
}

func (l *Leases) Grant(run, parent ref.Ref) {
	l.entries.Set(run, Lease{Parent: parent})
}

func (l *Leases) Forget(run ref.Ref) {
	l.entries.Delete(run)
}

func (l *Leases) Count(parent ref.Ref, observed map[ref.Ref]string, lifecycle Lifecycle) int32 {
	l.entries.Prune()
	l.entries.DeleteFunc(func(run ref.Ref, lease Lease) bool {
		if lease.Parent != parent {
			return false
		}
		phase, seen := observed[run]
		return seen && (lifecycle.Running(phase) || lifecycle.Terminal(phase))
	})
	var held int32
	l.entries.Range(func(_ ref.Ref, lease Lease) bool {
		if lease.Parent == parent {
			held++
		}
		return true
	})
	return held
}

func (l *Leases) Len() int {
	return l.entries.Len()
}
