package statusstate

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func (c *Controller) reconcileAccount(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	var obj v1alpha1.PdsAccount
	if err := decode(u, &obj); err != nil {
		c.log.Warn("decoding a PdsAccount", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
		return
	}
	generation := u.GetGeneration()
	status := obj.Status
	status.ObservedGeneration = generation
	did := strings.TrimSpace(obj.Spec.DID)
	ref := refString(obj.Spec.PdsRef)
	if did == "" || ref == "" {
		setReady(&status.Conditions, generation, false, ReasonWaiting, "the PdsAccount needs a did and a pdsRef")
		c.write(ctx, dyn, PdsAccountGVR, u, &status)
		return
	}
	if c.accounts == nil {
		setReady(&status.Conditions, generation, false, ReasonSourceUnavailable, "no account source is configured")
		c.write(ctx, dyn, PdsAccountGVR, u, &status)
		return
	}
	observation, err := c.accounts.AccountStatus(ctx, ref, did)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "reading the account: "+err.Error())
		c.write(ctx, dyn, PdsAccountGVR, u, &status)
		return
	}
	status.Active = observation.Active
	status.HasPassword = observation.HasPassword
	status.AppPasswordCount = nonNegative(observation.AppPasswordCount)
	status.SessionCount = nonNegative(observation.SessionCount)
	setReady(&status.Conditions, generation, true, ReasonReady, "the account status is observed")
	c.write(ctx, dyn, PdsAccountGVR, u, &status)
}

func nonNegative(value int32) int32 {
	if value < 0 {
		return 0
	}
	return value
}
