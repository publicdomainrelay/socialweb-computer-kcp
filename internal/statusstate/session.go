package statusstate

import (
	"context"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

func (c *Controller) reconcileSession(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	var obj v1alpha1.AtprotoSession
	if err := unstruct.Into(u, &obj); err != nil {
		c.log.Warn("decoding an AtprotoSession", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
		return
	}
	generation := u.GetGeneration()
	status := obj.Status
	status.ObservedGeneration = generation
	didText := strings.TrimSpace(obj.Spec.DID)
	sessionID := strings.TrimSpace(obj.Spec.SessionID)
	if didText == "" {
		setReady(&status.Conditions, generation, false, ReasonWaiting, "the AtprotoSession needs a did")
		c.write(ctx, dyn, AtprotoSessionGVR, u, &status)
		return
	}
	did, err := syntax.ParseDID(didText)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "the AtprotoSession did is invalid: "+err.Error())
		c.write(ctx, dyn, AtprotoSessionGVR, u, &status)
		return
	}
	observation, err := c.observeSession(ctx, c.self("AtprotoSession", obj.Name), pdsTarget(c.workspace, obj.Spec.PdsRef), did, sessionID)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "reading the session: "+err.Error())
		c.write(ctx, dyn, AtprotoSessionGVR, u, &status)
		return
	}
	status.Active = observation.Active
	status.ExpiresAt = observation.ExpiresAt
	status.Revoked = observation.Revoked
	setReady(&status.Conditions, generation, true, ReasonReady, "the session status is observed without its tokens")
	c.write(ctx, dyn, AtprotoSessionGVR, u, &status)
}

func (c *Controller) observeSession(ctx context.Context, from, to commonv1.ObjectRef, did syntax.DID, sessionID string) (SessionObservation, error) {
	if c.sessions == nil {
		return SessionObservation{}, fmt.Errorf("no session source is configured")
	}
	if strings.TrimSpace(to.Name) == "" {
		return SessionObservation{}, fmt.Errorf("the AtprotoSession names no pdsRef")
	}
	return c.sessions.SessionStatus(ctx, from, to, did.String(), sessionID)
}
