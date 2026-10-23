package statusstate

import (
	"context"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

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
	ref := refString(obj.Spec.PdsRef)
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
	observation, err := c.observeSession(ctx, ref, did, sessionID)
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

func (c *Controller) observeSession(ctx context.Context, ref string, did syntax.DID, sessionID string) (SessionObservation, error) {
	if c.sessions != nil {
		return c.sessions.SessionStatus(ctx, ref, did.String(), sessionID)
	}
	if c.repo == nil && (c.pds == nil || ref == "") {
		return SessionObservation{}, fmt.Errorf("no session source and no pdsRef are configured")
	}
	pds, err := c.resolveRepoPDS(ctx, ref, did.String())
	if err != nil {
		return SessionObservation{}, fmt.Errorf("resolving the pdsRef: %w", err)
	}
	session, err := pds.GetSession(ctx)
	if err != nil {
		return SessionObservation{}, err
	}
	return SessionObservation{Active: session.Active}, nil
}
