package hostlifecycle

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

func (c *Controller) ReconcileKcpInstallation(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	var spec v1alpha1.KcpInstallationSpec
	if err := specInto(u, &spec); err != nil {
		return err
	}
	generation := u.GetGeneration()
	obs, err := c.authority.ObserveKcpInstallation(ctx, spec)
	status := v1alpha1.KcpInstallationStatus{
		ObservedGeneration: generation,
		ActiveSlot:         obs.ActiveSlot,
		FrontBackend:       obs.FrontBackend,
	}
	if err != nil {
		c.log.Warn("reading the kcp installation authority", "name", u.GetName(), "err", err.Error())
		status.Message = authorityUnreadable
		authorityFailure(&status.Conditions, generation, authorityUnreadable,
			v1alpha1.KcpInstallationConditionRolling,
			v1alpha1.KcpInstallationConditionDrifted,
			v1alpha1.KcpInstallationConditionLeaseStale)
		if werr := c.writeStatus(ctx, dyn, KcpInstallationGVR, u.GetName(), &status); werr != nil {
			return werr
		}
		return err
	}
	rolling := spec.SelectedSlot != "" && obs.ActiveSlot != spec.SelectedSlot
	ready := obs.ActiveSlot != "" && obs.FrontBackend != "" && !rolling && !obs.Drifted && !obs.LeaseStale
	status.Message = kcpMessage(spec, obs, rolling, ready)
	conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionReady, conds.Status(ready), kcpReason(ready, rolling, obs.Drifted, obs.LeaseStale), status.Message)
	conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionRolling, conds.Status(rolling), boolReason(rolling, "SlotMismatch", "Steady"), rollingMessage(spec, obs))
	conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionDrifted, conds.Status(obs.Drifted), boolReason(obs.Drifted, "SlotDrift", "NoDrift"), driftMessage(obs))
	conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionLeaseStale, conds.Status(obs.LeaseStale), boolReason(obs.LeaseStale, "LeaseNotRenewing", "LeaseFresh"), leaseMessage(obs))
	return c.writeStatus(ctx, dyn, KcpInstallationGVR, u.GetName(), &status)
}

func kcpMessage(spec v1alpha1.KcpInstallationSpec, obs KcpInstallationObservation, rolling, ready bool) string {
	switch {
	case rolling:
		return rollingMessage(spec, obs)
	case obs.Drifted:
		return driftMessage(obs)
	case obs.LeaseStale:
		return leaseMessage(obs)
	case obs.ActiveSlot == "":
		return "no kcp slot is active"
	case obs.FrontBackend == "":
		return "the front names no backend"
	case ready:
		return fmt.Sprintf("slot %s serves through the front", obs.ActiveSlot)
	default:
		return "the kcp installation is not ready"
	}
}

func kcpReason(ready, rolling, drifted, stale bool) string {
	switch {
	case ready:
		return "Serving"
	case rolling:
		return "Rolling"
	case drifted:
		return "Drifted"
	case stale:
		return "LeaseStale"
	default:
		return "NotReady"
	}
}

func rollingMessage(spec v1alpha1.KcpInstallationSpec, obs KcpInstallationObservation) string {
	if spec.SelectedSlot == "" || obs.ActiveSlot == spec.SelectedSlot {
		return "the selected slot is active"
	}
	return fmt.Sprintf("slot %s is selected and slot %s is active", spec.SelectedSlot, obs.ActiveSlot)
}

func driftMessage(obs KcpInstallationObservation) string {
	if !obs.Drifted {
		return "the active slot matches its unit"
	}
	if obs.DriftMessage != "" {
		return obs.DriftMessage
	}
	return "the active slot process differs from its unit"
}

func leaseMessage(obs KcpInstallationObservation) string {
	if !obs.LeaseStale {
		return "the controller lease is renewing"
	}
	if obs.LeaseMessage != "" {
		return obs.LeaseMessage
	}
	return "the controller lease has not renewed"
}

func boolReason(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}
