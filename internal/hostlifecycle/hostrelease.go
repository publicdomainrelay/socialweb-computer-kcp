package hostlifecycle

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

func (c *Controller) ReconcileHostRelease(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	var spec v1alpha1.HostReleaseSpec
	if err := specInto(u, &spec); err != nil {
		return err
	}
	generation := u.GetGeneration()
	obs, err := c.authority.ObserveHostRelease(ctx, spec)
	status := v1alpha1.HostReleaseStatus{
		ObservedGeneration: generation,
		Current:            obs.Current,
		Previous:           obs.Previous,
		SelfCheck:          obs.SelfCheck,
		GC:                 obs.GC,
	}
	if err != nil {
		c.log.Warn("reading the host release authority", "name", u.GetName(), "err", err.Error())
		authorityFailure(&status.Conditions, generation, authorityUnreadable,
			ConditionCurrent,
			ConditionSelfCheck,
			ConditionGC)
		if werr := c.writeStatus(ctx, dyn, HostReleaseGVR, u.GetName(), &status); werr != nil {
			return werr
		}
		return err
	}
	matches := spec.Version == "" || obs.Current == spec.Version
	ready := obs.Current != "" && matches
	message := hostReleaseMessage(obs, ready, matches)
	conds.SetStatus(&status.Conditions, generation, "Ready", conds.Status(ready), boolReason(ready, "Current", "NotCurrent"), message)
	conds.SetStatus(&status.Conditions, generation, ConditionCurrent, conds.Status(obs.Current != "" && matches), boolReason(obs.Current != "" && matches, "MatchesSpec", "DiffersFromSpec"), message)
	value, reason, selfMessage := selfCheckCondition(obs.SelfCheck)
	conds.SetStatus(&status.Conditions, generation, ConditionSelfCheck, value, reason, selfMessage)
	gcValue, gcReason, gcMessage := gcCondition(obs.GC)
	conds.SetStatus(&status.Conditions, generation, ConditionGC, gcValue, gcReason, gcMessage)
	return c.writeStatus(ctx, dyn, HostReleaseGVR, u.GetName(), &status)
}

func hostReleaseMessage(obs HostReleaseObservation, ready, matches bool) string {
	switch {
	case obs.Current == "":
		return "the current release is not known"
	case !matches:
		return fmt.Sprintf("the requested version is not current; the current version is %s", obs.Current)
	case ready:
		return fmt.Sprintf("version %s is current", obs.Current)
	default:
		return "the host release is not ready"
	}
}

func selfCheckCondition(sc *v1alpha1.HostReleaseSelfCheck) (metav1.ConditionStatus, string, string) {
	if sc == nil {
		return metav1.ConditionUnknown, "NoSelfCheck", "no self-check report has been recorded"
	}
	phase := strings.ToLower(strings.TrimSpace(sc.Phase))
	switch phase {
	case "ok", "pass", "passed", "ready", "succeeded", "success", "healthy":
		return metav1.ConditionTrue, "SelfCheckPassed", "the self-check passed"
	case "", "pending", "running", "queued":
		return metav1.ConditionUnknown, "SelfCheckPending", "the self-check has not finished"
	default:
		return metav1.ConditionFalse, "SelfCheckFailed", "the self-check reported phase " + sc.Phase
	}
}

func gcCondition(gc *v1alpha1.HostReleaseGC) (metav1.ConditionStatus, string, string) {
	if gc == nil {
		return metav1.ConditionUnknown, "NoGC", "no GC report has been recorded"
	}
	return metav1.ConditionTrue, "Collected", fmt.Sprintf("GC kept %d and removed %d", gc.Kept, len(gc.Removed))
}
