package queue

import (
	"fmt"
	"sort"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

const (
	ReasonAtCapacity = "AtCapacity"

	ReasonSuperseded = "Superseded"
)

type Lifecycle struct {
	Running func(phase string) bool

	Terminal func(phase string) bool
}

type Policy string

func EffectivePolicy(policy Policy) Policy {
	if policy == "" {
		return PolicyForbid
	}
	return policy
}

const (
	PolicyAllow Policy = "Allow"

	PolicyForbid Policy = "Forbid"

	PolicyReplace Policy = "Replace"
)

type Run struct {
	Ref ref.Ref

	Phase string

	Created time.Time
}

type Capacity struct {
	Policy Policy

	MaxConcurrent *int32
}

type Blocker struct {
	Reason string

	Message string
}

type Admission struct {
	Gated bool

	Allowed bool

	Position int32

	Active int32

	Limit int32

	Unlimited bool

	Reason string

	Message string

	Preempt []ref.Ref
}

func (a Admission) Waiting() bool {
	return a.Gated && !a.Allowed
}

func Limit(policy Policy, maxConcurrent *int32) (int32, bool) {
	if EffectivePolicy(policy) == PolicyAllow {
		if maxConcurrent == nil || *maxConcurrent <= 0 {
			return 0, true
		}
		return *maxConcurrent, false
	}
	return 1, false
}

func Decision(policy Policy, maxConcurrent *int32, active, ahead int32) (bool, string, string) {
	limit, unlimited := Limit(policy, maxConcurrent)
	if unlimited || active+ahead < limit {
		return true, "", ""
	}
	display := policy
	if display == "" {
		display = PolicyForbid
	}
	return false, ReasonAtCapacity,
		fmt.Sprintf("waiting for a free slot (concurrencyPolicy=%s, maxConcurrent=%d)", display, limit)
}

func Order(runs []Run) {
	sort.SliceStable(runs, func(a, b int) bool {
		if !runs[a].Created.Equal(runs[b].Created) {
			return runs[a].Created.Before(runs[b].Created)
		}
		return runs[a].Ref.Name < runs[b].Ref.Name
	})
}

func Plan(runs []Run, capacity Capacity, blocker *Blocker, reserved int32, lifecycle Lifecycle) []Admission {
	ordered := append([]Run(nil), runs...)
	Order(ordered)

	active := reserved
	var pending []int
	for i := range ordered {
		switch {
		case lifecycle.Terminal(ordered[i].Phase):
		case lifecycle.Running(ordered[i].Phase):
			active++
		default:
			pending = append(pending, i)
		}
	}

	limit, unlimited := Limit(capacity.Policy, capacity.MaxConcurrent)

	newestPending := -1
	if capacity.Policy == PolicyReplace && len(pending) > 0 {
		newestPending = pending[len(pending)-1]
	}

	out := make([]Admission, len(ordered))
	for i := range ordered {
		out[i] = Admission{Gated: false, Allowed: true, Active: active, Limit: limit, Unlimited: unlimited}
	}

	for position, i := range pending {
		admission := Admission{
			Gated:     true,
			Position:  int32(position),
			Active:    active,
			Limit:     limit,
			Unlimited: unlimited,
		}
		switch {
		case blocker != nil:
			admission.Reason = blocker.Reason
			admission.Message = blocker.Message
		case capacity.Policy == PolicyReplace:
			if i == newestPending {
				admission.Allowed = true
				for j := range ordered {
					if j == i || lifecycle.Terminal(ordered[j].Phase) {
						continue
					}
					admission.Preempt = append(admission.Preempt, ordered[j].Ref)
				}
			} else {
				admission.Reason = ReasonSuperseded
				admission.Message = "a newer run supersedes this one under concurrencyPolicy=Replace"
			}
		default:
			admission.Allowed, admission.Reason, admission.Message = Decision(capacity.Policy, capacity.MaxConcurrent, active, int32(position))
		}
		out[i] = admission
	}
	return out
}

func PlanIndex(runs []Run, capacity Capacity, blocker *Blocker, reserved int32, lifecycle Lifecycle) map[ref.Ref]Admission {
	ordered := append([]Run(nil), runs...)
	Order(ordered)
	planned := Plan(ordered, capacity, blocker, reserved, lifecycle)
	out := make(map[ref.Ref]Admission, len(ordered))
	for i := range ordered {
		out[ordered[i].Ref] = planned[i]
	}
	return out
}

func WakeList(runs []Run, lifecycle Lifecycle, limit int32, unlimited bool) []ref.Ref {
	var pending []Run
	for _, run := range runs {
		if lifecycle.Running(run.Phase) || lifecycle.Terminal(run.Phase) {
			continue
		}
		pending = append(pending, run)
	}
	Order(pending)
	count := len(pending)
	if !unlimited && int(limit) < count {
		count = int(limit)
	}
	out := make([]ref.Ref, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, pending[i].Ref)
	}
	return out
}
