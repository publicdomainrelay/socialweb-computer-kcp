package computevm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

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

type GuestLoginRefs struct {
	Role string `json:"role"`

	Mount string `json:"mount"`

	Scopes []string `json:"scopes,omitempty"`
}

func GuestLoginRef(o v1alpha1.ComputeVM, scopes []string) (*GuestLoginRefs, error) {
	role, err := guestLoginRole(o)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	ref := &GuestLoginRefs{Role: role, Mount: o.Spec.GuestLogin.Mount}
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		ref.Scopes = append(ref.Scopes, scope)
	}
	sort.Strings(ref.Scopes)
	return ref, nil
}

func (ct *Controller) ensureGuestLogin(ctx context.Context, o v1alpha1.ComputeVM) error {
	path, err := guestLoginRole(o)
	if errors.Is(err, errNoLogin) {
		return nil
	}
	if err != nil {
		return err
	}
	client, err := ct.vaultFor(ctx, v1alpha1.VaultServer(o.Spec.UserData.Vault.Vault, ""))
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
	ct.c.log().Info("the guest's login role is in the bidder's OpenBao; the guest binds its ssh host key to it once",
		"computevm", o.Name, "role", path, "alias", GuestAliasPrefix+o.Name)
	return nil
}

func (ct *Controller) dropGuestLogin(ctx context.Context, o v1alpha1.ComputeVM) {
	path, err := guestLoginRole(o)
	if err != nil {
		return
	}
	server := v1alpha1.VaultServer(o.Spec.UserData.Vault.Vault, "")
	client, err := ct.vaultFor(ctx, server)
	if err == nil {
		err = client.Delete(ctx, path)
	}
	if err != nil {
		ct.c.log().Warn("the guest login role stays in the bidder's OpenBao", "computevm", o.Name, "role", path, "err", err.Error())
		return
	}
	ct.c.log().Info("the guest login role is gone with its ComputeVM", "computevm", o.Name, "role", path)
}

func (ct *Controller) dropGuestGrants(ctx context.Context, o v1alpha1.ComputeVM) {
	list, err := ct.grants.List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			ct.c.log().Warn("listing the GuestIdentityGrants of a released ComputeVM", "computevm", o.Name, "err", err.Error())
		}
		return
	}
	for _, it := range list.Items {
		spec, _ := it.Object["spec"].(map[string]any)
		if fmt.Sprint(spec["computevm"]) != o.Name {
			continue
		}
		if err := ct.grants.Delete(ctx, it.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			ct.c.log().Warn("deleting a GuestIdentityGrant of a released ComputeVM", "computevm", o.Name, "grant", it.GetName(), "err", err.Error())
			continue
		}
		ct.c.log().Info("a GuestIdentityGrant goes with its ComputeVM", "computevm", o.Name, "grant", it.GetName())
	}
}

func refusedLogin(err error) bool {
	var re *openbao.ResponseError
	if errors.As(err, &re) {
		return re.Status >= 400 && re.Status < 500 && re.Status != 403
	}
	return !errors.Is(err, errNoLogin) && errors.Is(err, errBadSpec)
}
