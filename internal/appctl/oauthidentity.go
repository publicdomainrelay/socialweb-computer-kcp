package appctl

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func (ctl *Controller) ensureOAuthIdentityEndpoints(ctx context.Context, owner, ws, identity, exposure, role string) error {
	snap, err := ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return err
	}
	raw, _ := exposureURL(snap, exposure)
	base := strings.TrimRight(raw, "/")
	if base == "" {
		return nil
	}
	clientID := base + "/oauth-client-metadata.json"
	redirect := base + "/oauth/callback/" + role
	ri, err := ctl.client(ws, MarketIdentityGVR)
	if err != nil {
		return err
	}
	u, err := ri.Get(ctx, identity, metav1.GetOptions{})
	if err != nil {
		return err
	}
	mode, _, _ := unstructured.NestedString(u.Object, "spec", "mode")
	switch v1alpha1.MarketIdentityMode(mode) {
	case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
	default:
		return nil
	}
	haveClient, _, _ := unstructured.NestedString(u.Object, "spec", "oauthClientID")
	haveRedirect, _, _ := unstructured.NestedString(u.Object, "spec", "oauthRedirectURI")
	if haveClient == clientID && haveRedirect == redirect {
		return nil
	}
	u.Object["spec"].(map[string]any)["oauthClientID"] = clientID
	u.Object["spec"].(map[string]any)["oauthRedirectURI"] = redirect
	_, err = ri.Update(ctx, u, metav1.UpdateOptions{FieldManager: FieldManager})
	return err
}
