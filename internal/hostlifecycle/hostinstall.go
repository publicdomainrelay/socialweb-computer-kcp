package hostlifecycle

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func (c *Controller) ReconcileHostInstall(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	var spec v1alpha1.HostInstallSpec
	if err := specInto(u, &spec); err != nil {
		return err
	}
	generation := u.GetGeneration()
	obs, err := c.authority.ObserveHostInstall(ctx, spec)
	status := v1alpha1.HostInstallStatus{
		ObservedGeneration: generation,
		InstalledVersion:   obs.InstalledVersion,
		Steps:              obs.Steps,
	}
	if err != nil {
		c.log.Warn("reading the host install authority", "name", u.GetName(), "err", err.Error())
		authorityFailure(&status.Conditions, generation, authorityUnreadable,
			ConditionInstalled,
			ConditionProgressing)
		if werr := c.writeStatus(ctx, dyn, HostInstallGVR, u.GetName(), &status); werr != nil {
			return werr
		}
		return err
	}
	found := obs.Found
	matches := spec.Version == "" || obs.InstalledVersion == spec.Version
	ready := found && obs.InstalledVersion != "" && matches
	progressing := found && !ready
	message := hostInstallMessage(obs, ready, matches)
	setCondition(&status.Conditions, generation, ConditionInstalled, condition(found), boolReason(found, "RecordFound", "NoRecord"), message)
	setCondition(&status.Conditions, generation, "Ready", condition(ready), boolReason(ready, "Installed", "NotInstalled"), message)
	setCondition(&status.Conditions, generation, ConditionProgressing, condition(progressing), boolReason(progressing, "AwaitingVersion", "Settled"), message)
	return c.writeStatus(ctx, dyn, HostInstallGVR, u.GetName(), &status)
}

func hostInstallMessage(obs HostInstallObservation, ready, matches bool) string {
	switch {
	case !obs.Found:
		return "no host install record"
	case !matches:
		return "the requested version is not installed"
	case !ready:
		return "the installed version is not known"
	default:
		return fmt.Sprintf("version %s is installed", obs.InstalledVersion)
	}
}
