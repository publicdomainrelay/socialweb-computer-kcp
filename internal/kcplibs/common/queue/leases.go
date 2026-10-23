package queue

import (
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/expiringmap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

type Lease struct {
	Parent ref.Ref
}

type Leases struct {
	entries *expiringmap.Map[ref.Ref, Lease]
}

func NewLeases(ttl time.Duration) *Leases {
	return &Leases{entries: expiringmap.NewMap[ref.Ref, Lease](ttl)}
}

func (l *Leases) Grant(run, parent ref.Ref, now time.Time) {
	l.entries.Set(run, Lease{Parent: parent}, now)
}

func (l *Leases) Forget(run ref.Ref) {
	l.entries.Delete(run)
}

func (l *Leases) Count(parent ref.Ref, observed map[ref.Ref]string, now time.Time, lifecycle Lifecycle) int32 {
	l.entries.Expire(now)
	l.entries.DeleteIf(func(run ref.Ref, lease Lease) bool {
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
