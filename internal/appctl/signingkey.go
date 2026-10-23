package appctl

import (
	"context"
	"errors"
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const (
	BidderAccount     = "bidder"
	oidcRotationPath  = "oidc-rotation/"
	GuestOIDCKey      = "guest"
	guestOIDCRotation = "identity/oidc/key/" + GuestOIDCKey + "/rotate"
)

func rotationOf(data map[string]any) int64 {
	switch v := data["rotation"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func (ctl *Controller) rotateSigningKey(ctx context.Context, o v1alpha1.MarketBidder) error {
	want := int64(o.Spec.SigningKeyRotation)
	if want <= 0 || ctl.rotated[string(o.UID)] >= want {
		return nil
	}
	client, _, c, err := ctl.vaultClient(ctx, BidderVaultServer, BidderVaultClient)
	if err != nil {
		return err
	}
	defer client.RevokeSelf(context.WithoutCancel(ctx))
	path := c.KVMount + "/data/" + oidcRotationPath + GuestOIDCKey
	data, err := client.GetKV(ctx, path)
	var re *openbao.ResponseError
	if err != nil && !(errors.As(err, &re) && re.Status == http.StatusNotFound) {
		return err
	}
	held := rotationOf(data)
	if held >= want {
		ctl.rotated[string(o.UID)] = held
		return nil
	}
	if err := client.Write(ctx, guestOIDCRotation, nil); err != nil {
		if errors.As(err, &re) && (re.Status == http.StatusNotFound || re.Status == http.StatusBadRequest) {
			ctl.c.log().Info("no OIDC signing key to rotate yet; the server makes it from its declaration", "name", o.Name, "key", GuestOIDCKey)
			ctl.rotated[string(o.UID)] = want
			return nil
		}
		return err
	}
	if err := client.PutKV(ctx, path, map[string]any{"rotation": want}); err != nil {
		return err
	}
	ctl.rotated[string(o.UID)] = want
	ctl.c.log().Info("rotated the bidder's OIDC signing key in its OpenBao; the issuer URL stays and the old key stays published for the key's verification ttl",
		"name", o.Name, "key", GuestOIDCKey, "rotation", want)
	return nil
}
