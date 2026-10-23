package statusstate

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

func (c *Controller) reconcileRelay(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	var obj v1alpha1.RelayStatus
	if err := unstruct.Into(u, &obj); err != nil {
		c.log.Warn("decoding a RelayStatus", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
		return
	}
	generation := u.GetGeneration()
	status := obj.Status
	status.ObservedGeneration = generation
	ref := refString(obj.Spec.RelayRef)
	if ref == "" {
		setReady(&status.Conditions, generation, false, ReasonWaiting, "the RelayStatus needs a relayRef")
		c.write(ctx, dyn, RelayStatusGVR, u, &status)
		return
	}
	if c.relay == nil {
		setReady(&status.Conditions, generation, false, ReasonSourceUnavailable, "no relay source is configured")
		c.write(ctx, dyn, RelayStatusGVR, u, &status)
		return
	}
	observation, err := c.relay.RelayStatus(ctx, ref)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "reading the relay status: "+err.Error())
		c.write(ctx, dyn, RelayStatusGVR, u, &status)
		return
	}
	status.Phase = observation.Phase
	if status.Phase == "" {
		status.Phase = v1alpha1.RelayStatusPhasePending
	}
	status.Cursor = nonNegativeInt64(observation.Cursor)
	status.LastEventAt = observation.LastEventAt
	status.Events = nonNegativeInt64(observation.Events)
	setReady(&status.Conditions, generation, true, ReasonReady, "the relay status is observed")
	c.write(ctx, dyn, RelayStatusGVR, u, &status)
}

func nonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
