package computevm

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const (
	GuestLoginTokenTTL     = "5m"
	GuestLoginTokenNumUses = 50
	GuestAliasPrefix       = "guest-"
)

var (
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	mountName  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	errNoLogin = errors.New("no guest login")
	errBadSpec = errors.New("spec.guestLogin")
)

func guestLoginRole(o v1alpha1.ComputeVM) (string, error) {
	g := o.Spec.GuestLogin
	if g == nil {
		return "", errNoLogin
	}
	if !mountName.MatchString(g.Mount) {
		return "", fmt.Errorf("%w: mount %q is not an auth mount name", errBadSpec, g.Mount)
	}
	if !sha256Hex.MatchString(g.BindSecretSha256) {
		return "", fmt.Errorf("%w: bindSecretSha256 is not a hex sha256", errBadSpec)
	}
	return "auth/" + g.Mount + "/role/" + o.Name, nil
}

func (h *Host) EnsureGuestLogin(ctx context.Context, o v1alpha1.ComputeVM) error {
	path, err := guestLoginRole(o)
	if errors.Is(err, errNoLogin) {
		return nil
	}
	if err != nil {
		return err
	}
	client, err := h.vaultFor(ctx, v1alpha1.VaultServer(o.Spec.UserData.Vault.Vault, ""))
	if err != nil {
		return err
	}
	held, err := client.Read(ctx, path)
	switch {
	case errors.Is(err, openbao.ErrNotFound):
	case err != nil:
		return fmt.Errorf("reading the guest login role %s: %w", path, err)
	case held["bound"] == true:
		return nil
	}
	body := map[string]any{
		"bind_secret_hash": o.Spec.GuestLogin.BindSecretSha256,
		"alias_name":       GuestAliasPrefix + o.Name,
		"token_ttl":        GuestLoginTokenTTL,
		"token_num_uses":   GuestLoginTokenNumUses,
		"token_policies":   []string{GuestAliasPrefix + o.Name},
	}
	if err := client.Write(ctx, path, body); err != nil {
		return fmt.Errorf("writing the guest login role %s: %w", path, err)
	}
	h.c.log().Info("the guest's login role is in the bidder's OpenBao; the guest binds its ssh host key to it once",
		"computevm", o.Name, "role", path, "alias", GuestAliasPrefix+o.Name)
	return nil
}

func (h *Host) DropGuestLogin(ctx context.Context, o v1alpha1.ComputeVM) {
	path, err := guestLoginRole(o)
	if err != nil {
		return
	}
	server := v1alpha1.VaultServer(o.Spec.UserData.Vault.Vault, "")
	client, err := h.vaultFor(ctx, server)
	if err == nil {
		err = client.Delete(ctx, path)
	}
	if err != nil {
		h.c.log().Warn("the guest login role stays in the bidder's OpenBao", "computevm", o.Name, "role", path, "err", err.Error())
		return
	}
	h.c.log().Info("the guest login role is gone with its ComputeVM", "computevm", o.Name, "role", path)
}

func (h *Host) DropGuestGrants(ctx context.Context, name string) {
	list, err := h.grants.List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			h.c.log().Warn("listing the GuestIdentityGrants of a released ComputeVM", "computevm", name, "err", err.Error())
		}
		return
	}
	for _, it := range list.Items {
		spec, _ := it.Object["spec"].(map[string]any)
		if fmt.Sprint(spec["computevm"]) != name {
			continue
		}
		if err := h.grants.Delete(ctx, it.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			h.c.log().Warn("deleting a GuestIdentityGrant of a released ComputeVM", "computevm", name, "grant", it.GetName(), "err", err.Error())
			continue
		}
		h.c.log().Info("a GuestIdentityGrant goes with its ComputeVM", "computevm", name, "grant", it.GetName())
	}
}

func RefusedLogin(err error) bool {
	var re *openbao.ResponseError
	if errors.As(err, &re) {
		return re.Status >= 400 && re.Status < 500 && re.Status != 403
	}
	return !errors.Is(err, errNoLogin) && errors.Is(err, errBadSpec)
}
