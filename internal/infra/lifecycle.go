package infra

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const maxName = 253

func ChildName(kind Kind, parts ...string) string {
	name := kind.Prefix()
	for _, p := range parts {
		name += "-" + p
	}
	if len(name) <= maxName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:6])
	return name[:maxName-len(suffix)-1] + "-" + suffix
}

func Finished(cc *v1alpha1.ComputeContract) bool {
	return cc != nil && (cc.Status.Phase == v1alpha1.ComputeContractReleased || cc.Status.Phase == v1alpha1.ComputeContractFailed)
}

func PhaseOf(cc *v1alpha1.ComputeContract) v1alpha1.InfraContractPhase {
	switch cc.Status.Phase {
	case v1alpha1.ComputeContractHeld:
		if cc.Spec.Release {
			return v1alpha1.InfraContractPhaseReleasing
		}
		return v1alpha1.InfraContractPhaseDeployed
	case v1alpha1.ComputeContractReleasing:
		return v1alpha1.InfraContractPhaseReleasing
	case v1alpha1.ComputeContractReleased:
		return v1alpha1.InfraContractPhaseReleased
	case v1alpha1.ComputeContractFailed:
		return v1alpha1.InfraContractPhaseFailed
	}
	if cc.Spec.Release {
		return v1alpha1.InfraContractPhaseReleasing
	}
	return v1alpha1.InfraContractPhaseRequesting
}

type Step struct {
	Create bool

	Release bool

	Delete bool

	Phase v1alpha1.InfraContractPhase
}

func Plan(state v1alpha1.InfraContractState, cc *v1alpha1.ComputeContract, previous v1alpha1.InfraContractPhase) Step {
	state = StateOrStaged(state)
	if cc == nil {
		switch state {
		case v1alpha1.InfraContractRequested:
			switch previous {
			case "", v1alpha1.InfraContractPhaseStaged, v1alpha1.InfraContractPhaseRequesting:
				return Step{Create: true, Phase: v1alpha1.InfraContractPhaseRequesting}
			}
			return Step{Phase: v1alpha1.InfraContractPhaseReleased}
		case v1alpha1.InfraContractReleased:
			if previous == "" || previous == v1alpha1.InfraContractPhaseStaged {
				return Step{Phase: v1alpha1.InfraContractPhaseStaged}
			}
			return Step{Phase: v1alpha1.InfraContractPhaseReleased}
		}
		return Step{Phase: v1alpha1.InfraContractPhaseStaged}
	}
	if state == v1alpha1.InfraContractRequested {
		return Step{Phase: PhaseOf(cc)}
	}
	if !Finished(cc) {
		return Step{Release: !cc.Spec.Release, Phase: PhaseOf(cc)}
	}
	if state == v1alpha1.InfraContractStaged {
		return Step{Delete: true, Phase: v1alpha1.InfraContractPhaseStaged}
	}
	return Step{Phase: PhaseOf(cc)}
}

func Orphan(cc *v1alpha1.ComputeContract) Step {
	if !Finished(cc) {
		return Step{Release: !cc.Spec.Release, Phase: PhaseOf(cc)}
	}
	return Step{Delete: true, Phase: PhaseOf(cc)}
}

func ContractSpec(c v1alpha1.InfraContract, secrets *commonv1.SecretRef, node *v1alpha1.InfraContractNode) v1alpha1.ComputeContractSpec {
	spec := v1alpha1.ComputeContractSpec{ComputeContractTemplate: *c.ComputeContractTemplate.DeepCopy()}
	if secrets != nil {
		ref := *secrets
		spec.SecretsRef = &ref
	}
	if node != nil {
		spec.OwnerDid, spec.Adopt = node.OwnerDid, node.Adopt
		if node.NodeJoin != nil {
			spec.NodeJoin = node.NodeJoin.DeepCopy()
		}
	}
	return spec
}

func Phase(contracts []v1alpha1.InfraContractStatus) v1alpha1.InfraPhase {
	count := map[v1alpha1.InfraContractPhase]int{}
	for _, c := range contracts {
		count[c.Phase]++
	}
	switch {
	case count[v1alpha1.InfraContractPhaseFailed] > 0:
		return v1alpha1.InfraPhaseDegraded
	case count[v1alpha1.InfraContractPhaseRequesting] > 0:
		return v1alpha1.InfraPhaseDeploying
	case count[v1alpha1.InfraContractPhaseReleasing] > 0:
		return v1alpha1.InfraPhaseReleasing
	case count[v1alpha1.InfraContractPhaseDeployed] > 0:
		return v1alpha1.InfraPhaseDeployed
	case count[v1alpha1.InfraContractPhaseReleased] > 0:
		return v1alpha1.InfraPhaseReleased
	}
	return v1alpha1.InfraPhaseStaged
}

func Complete(contracts []v1alpha1.InfraContractStatus) bool {
	requested := false
	for _, c := range contracts {
		switch c.Phase {
		case v1alpha1.InfraContractPhaseStaged:
		case v1alpha1.InfraContractPhaseReleased, v1alpha1.InfraContractPhaseFailed:
			requested = true
		default:
			return false
		}
	}
	return requested
}

type End struct {
	Ended bool

	Reason v1alpha1.EphemeralInfraEnd

	At time.Time

	ExpiresAt time.Time
}

func Ending(created time.Time, d Declaration, previous v1alpha1.EphemeralInfraStatus, now time.Time) End {
	ttl := d.TTLSeconds
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	out := End{ExpiresAt: created.Add(time.Duration(ttl) * time.Second).UTC()}
	if previous.EndedAt != "" {
		at, _ := time.Parse(time.RFC3339, previous.EndedAt)
		out.Ended, out.Reason, out.At = true, previous.EndReason, at
		return out
	}
	switch {
	case !now.Before(out.ExpiresAt):
		out.Ended, out.Reason, out.At = true, v1alpha1.EphemeralInfraEndDeadline, now.UTC()
	case d.EndWhen != v1alpha1.EphemeralInfraEndDeadline && Complete(previous.Contracts):
		out.Ended, out.Reason, out.At = true, v1alpha1.EphemeralInfraEndComplete, now.UTC()
	}
	return out
}
