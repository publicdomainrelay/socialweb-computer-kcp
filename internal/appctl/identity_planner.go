package appctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/remotepds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
)

const EnvPdsAccountPassword = "ATPROTO_PASSWORD"

var ErrMissingPdsAccountPassword = errors.New("appctl: mode pdsAccount needs the account password in " +
	EnvPdsAccountPassword + " or --atproto-password; no password is available")

type accountVault struct {
	Server string

	Client string

	Mount string
}

func (v accountVault) mount(fallback string) string {
	mount := strings.Trim(strings.TrimSpace(v.Mount), "/")
	if mount == "" {
		mount = fallback
	}
	return mount
}

type accountPlan struct {
	Mode v1alpha1.MarketIdentityMode

	Local bool

	Identity v1alpha1.MarketIdentitySpec

	PdsRef *v1alpha1.ComponentRef

	RemotePDS *pdsv1alpha1.RemotePDS

	PdsURL string

	SessionPath string

	KeysPath string

	ComponentMode string

	MarketURL string

	Vault accountVault
}

func (p accountPlan) accessTokenPath() string {
	if p.SessionPath == "" {
		return ""
	}
	return "openbao:" + p.SessionPath
}

func (p accountPlan) componentPdsRef() *v1alpha1.ComponentRef {
	if p.Local {
		return ComponentRefFor(ComponentKindPds, PdsComponent)
	}
	return p.PdsRef
}

type accountBinding struct {
	DID string

	PDSRef *v1alpha1.RepoRef

	AuthRef *v1alpha1.RepoRef

	RemotePDS *pdsv1alpha1.RemotePDS
}

func (p accountPlan) usesAssociation() bool {
	return p.Local
}

func (p accountPlan) binding(did, pdsURL string) (accountBinding, error) {
	binding := accountBinding{DID: strings.TrimSpace(did)}
	if p.Local {
		binding.PDSRef = &v1alpha1.RepoRef{Kind: ComponentKindPds, Name: PdsComponent}
		return binding, nil
	}
	if token := p.accessTokenPath(); token != "" {
		binding.AuthRef = &v1alpha1.RepoRef{Kind: "Vault", Name: token}
	}
	if p.RemotePDS != nil && p.RemotePDS.Name != "" {
		binding.RemotePDS = p.RemotePDS
	} else if p.PdsRef != nil && strings.EqualFold(p.PdsRef.Kind, "RemotePDS") && p.PdsRef.Name != "" {
		binding.RemotePDS = &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: p.PdsRef.Name}}
	}
	if url := firstNonEmptyString(p.PdsURL, p.Identity.PdsURL, pdsURL); binding.RemotePDS == nil && url != "" {
		binding.RemotePDS = &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: deterministicPdsName(url)},
			Spec: pdsv1alpha1.RemotePDSSpec{URL: url, DID: pdsServiceDID(url)}}
	}
	if binding.RemotePDS != nil {
		if binding.AuthRef != nil && binding.RemotePDS.Spec.Auth == (pdsv1alpha1.RemotePDSAuth{}) {
			binding.RemotePDS.Spec.Auth.DPoPKeyRef = binding.AuthRef.Name
		}
		binding.PDSRef = &v1alpha1.RepoRef{Kind: "RemotePDS", Name: binding.RemotePDS.Name}
		return binding, nil
	}
	return accountBinding{}, errors.New("appctl: the existing-account repo names no PDS")
}

func deterministicPdsName(url string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(url)))
	return "pds-" + hex.EncodeToString(sum[:8])
}

func (ctl *Controller) planAccount(ctx context.Context, ws string, id v1alpha1.ApplicationIdentity,
	suffix string, vault accountVault, defaultAssociation v1alpha1.AssociationMode) (accountPlan, error) {
	mode := id.Mode
	if mode == "" {
		mode = v1alpha1.MarketIdentityModeLocalRepo
	}
	mount := vault.mount(suffix)
	sessionPath := "identity/" + suffix + "/pds-session"
	keysPath := "identity/" + suffix + "/keys"
	plan := accountPlan{Mode: mode, SessionPath: sessionPath, KeysPath: keysPath, Vault: vault}
	switch mode {
	case v1alpha1.MarketIdentityModeLocalRepo:
		association := id.Association
		if association == "" {
			association = defaultAssociation
		}
		if association == "" {
			association = v1alpha1.AssociationRequireApproval
		}
		plan.Local = true
		plan.ComponentMode = "local"
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsURL: id.PdsURL, Association: association}
		plan.SessionPath = ""
		plan.KeysPath = ""
		return plan, nil
	case v1alpha1.MarketIdentityModePdsAccount:
		plan.ComponentMode = "callback"
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsURL: id.PdsURL,
			KeysRef:    &v1alpha1.VaultRef{Mount: mount, Path: keysPath},
			SessionRef: &v1alpha1.VaultRef{Mount: mount, Path: sessionPath}}
		url, remote, err := ctl.resolveAccountPds(ctx, ws, id)
		if err != nil {
			return accountPlan{}, err
		}
		plan.PdsURL = url
		plan.RemotePDS = remote
		plan.Identity.PdsURL = url
		if remote != nil {
			plan.PdsRef = &v1alpha1.ComponentRef{Kind: "RemotePDS", Name: remote.Name, Did: remote.Spec.DID}
		}
		return plan, nil
	case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
		plan.ComponentMode = "oauth"
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsURL: id.PdsURL,
			SessionRef: &v1alpha1.VaultRef{Mount: mount, Path: sessionPath}}
		plan.PdsURL = strings.TrimSpace(id.PdsURL)
		if remote, err := ctl.remotePDSRef(ctx, ws, id.PdsRef); err != nil {
			return accountPlan{}, err
		} else if remote != nil {
			plan.RemotePDS = remote
			plan.PdsRef = &v1alpha1.ComponentRef{Kind: "RemotePDS", Name: remote.Name, Did: remote.Spec.DID}
			if plan.PdsURL == "" {
				plan.PdsURL = remote.Spec.URL
			}
			if plan.Identity.PdsURL == "" {
				plan.Identity.PdsURL = remote.Spec.URL
			}
		}
		return plan, nil
	}
	return accountPlan{}, fmt.Errorf("appctl: MarketIdentity mode %q: want one of localRepo, pdsAccount, oauthBrowser, oauthQr, oauthDelegated", mode)
}

func (ctl *Controller) seedPdsAccountPassword(ctx context.Context, plan accountPlan) error {
	if plan.Mode != v1alpha1.MarketIdentityModePdsAccount {
		return nil
	}
	password := strings.TrimSpace(os.Getenv(EnvPdsAccountPassword))
	if password == "" {
		return ErrMissingPdsAccountPassword
	}
	if plan.Identity.KeysRef == nil || strings.TrimSpace(plan.KeysPath) == "" {
		return errors.New("appctl: mode pdsAccount names no keysRef for its password")
	}
	if plan.Vault.Server == "" || plan.Vault.Client == "" {
		return errors.New("appctl: mode pdsAccount names no vault for its password")
	}
	client, _, c, err := ctl.vaultClient(ctx, plan.Vault.Server, plan.Vault.Client)
	if err != nil {
		return err
	}
	defer client.RevokeSelf(context.Background())
	path := c.KVMount + "/data/" + plan.KeysPath
	data, err := client.GetKV(ctx, path)
	var re *openbao.ResponseError
	if err != nil && !(errors.As(err, &re) && re.Status == http.StatusNotFound) {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	if existing, _ := data["password"].(string); existing == password {
		return nil
	}
	data["password"] = password
	if err := client.PutKV(ctx, path, data); err != nil {
		return err
	}
	ctl.c.log().Info("the pdsAccount password is in its OpenBao keysRef", "server", plan.Vault.Server, "path", plan.KeysPath)
	return nil
}

func (ctl *Controller) resolveAccountPds(ctx context.Context, ws string, id v1alpha1.ApplicationIdentity) (string, *pdsv1alpha1.RemotePDS, error) {
	if url := strings.TrimSpace(id.PdsURL); url != "" {
		remote, err := ctl.remotePDSRef(ctx, ws, id.PdsRef)
		if err != nil {
			return "", nil, err
		}
		return url, remote, nil
	}
	remote, err := ctl.remotePDSRef(ctx, ws, id.PdsRef)
	if err != nil {
		return "", nil, err
	}
	if remote == nil {
		return "", nil, fmt.Errorf("appctl: mode pdsAccount names no pdsUrl and no RemotePDS ref; it fails closed instead of using a local repo")
	}
	if strings.TrimSpace(remote.Spec.URL) == "" {
		return "", nil, fmt.Errorf("appctl: RemotePDS %s names no url; mode pdsAccount fails closed", remote.Name)
	}
	return strings.TrimSpace(remote.Spec.URL), remote, nil
}

func (ctl *Controller) remotePDSRef(ctx context.Context, ws string, ref *v1alpha1.RepoRef) (*pdsv1alpha1.RemotePDS, error) {
	if ref == nil || !strings.EqualFold(strings.TrimSpace(ref.Kind), "RemotePDS") || strings.TrimSpace(ref.Name) == "" {
		return nil, nil
	}
	target := strings.TrimSpace(ref.Workspace)
	if target == "" {
		target = ws
	}
	ri, err := ctl.client(target, remotepds.RemotePDSGVR)
	if err != nil {
		return nil, err
	}
	u, err := ri.Get(ctx, strings.TrimSpace(ref.Name), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("appctl: RemotePDS %s is absent in workspace %s; the existing-account mode fails closed", ref.Name, target)
		}
		return nil, err
	}
	var object pdsv1alpha1.RemotePDS
	if err := fromUnstructured(u, &object); err != nil {
		return nil, err
	}
	return &object, nil
}

func (ctl *Controller) deleteAssociationObjects(ctx context.Context, ws string) {
	for _, gvr := range []schema.GroupVersionResource{AtprotoRecordGVR,
		{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "atprotorecordwatches"}} {
		ri, err := ctl.client(ws, gvr)
		if err != nil {
			continue
		}
		if err := ri.Delete(ctx, AssociationWatch, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			ctl.c.log().Warn("removing an association object", "resource", gvr.Resource, "err", err.Error())
			continue
		}
		obj, err := ri.Get(ctx, AssociationWatch, metav1.GetOptions{})
		if err != nil || len(obj.GetFinalizers()) == 0 {
			continue
		}
		obj.SetFinalizers(nil)
		if _, err := ri.Update(ctx, obj, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsNotFound(err) {
			ctl.c.log().Warn("dropping an association finalizer", "resource", gvr.Resource, "err", err.Error())
		}
	}
}

func pdsServiceDID(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return "did:web:" + strings.ReplaceAll(strings.ToLower(parsed.Host), ":", "%3A")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (ctl *Controller) ensureRemotePDS(ctx context.Context, ws string, desired *pdsv1alpha1.RemotePDS) *pdsv1alpha1.RemotePDS {
	if desired == nil || strings.TrimSpace(desired.Name) == "" {
		return nil
	}
	name := desired.Name
	ri, err := ctl.client(ws, pdsv1alpha1.RemotePDSGVR)
	if err != nil {
		ctl.c.log().Warn("creating a RemotePDS", "name", name, "err", err.Error())
		return nil
	}
	existing, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		var current pdsv1alpha1.RemotePDS
		if fromUnstructured(existing, &current) != nil {
			return nil
		}
		if strings.TrimRight(current.Spec.URL, "/") == strings.TrimRight(desired.Spec.URL, "/") && current.Spec.DID == desired.Spec.DID &&
			current.Spec.Auth == desired.Spec.Auth {
			return &current
		}
		current.Spec.URL, current.Spec.DID, current.Spec.Auth = desired.Spec.URL, desired.Spec.DID, desired.Spec.Auth
		u, err := toUnstructured(&current, "RemotePDS")
		if err != nil {
			return nil
		}
		u.SetAPIVersion(pdsv1alpha1.GroupVersion.String())
		u.SetResourceVersion(existing.GetResourceVersion())
		updated, err := ri.Update(ctx, u, metav1.UpdateOptions{FieldManager: FieldManager})
		if err != nil {
			ctl.c.log().Warn("updating a RemotePDS", "name", name, "err", err.Error())
			return nil
		}
		var result pdsv1alpha1.RemotePDS
		if fromUnstructured(updated, &result) == nil {
			return &result
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		ctl.c.log().Warn("reading a RemotePDS", "name", name, "err", err.Error())
		return nil
	}
	u, err := toUnstructured(desired, "RemotePDS")
	if err != nil {
		ctl.c.log().Warn("encoding a RemotePDS", "name", name, "err", err.Error())
		return nil
	}
	u.SetAPIVersion(pdsv1alpha1.GroupVersion.String())
	created, err := ri.Create(ctx, u, metav1.CreateOptions{FieldManager: FieldManager})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		ctl.c.log().Warn("creating a RemotePDS", "name", name, "err", err.Error())
		return nil
	}
	if created != nil {
		var out pdsv1alpha1.RemotePDS
		if fromUnstructured(created, &out) == nil {
			return &out
		}
	}
	return desired
}

func (ctl *Controller) ensureAccountRepo(ctx context.Context, ws, identityName string, plan accountPlan, did, pdsURL string) {
	if plan.Local || strings.TrimSpace(did) == "" {
		return
	}
	binding, err := plan.binding(did, pdsURL)
	if err != nil {
		ctl.c.log().Warn("the existing-account repo has no PDS ref", "identity", identityName, "err", err.Error())
		return
	}
	if binding.RemotePDS != nil {
		ensured := ctl.ensureRemotePDS(ctx, ws, binding.RemotePDS)
		if ensured == nil {
			return
		}
		binding.RemotePDS = ensured
		binding.PDSRef = &v1alpha1.RepoRef{Kind: "RemotePDS", Name: ensured.Name}
	}
	if !plan.usesAssociation() {
		ctl.deleteAssociationObjects(ctx, ws)
	}
	spec := v1alpha1.AtprotoRepoSpec{DID: binding.DID, PdsRef: binding.PDSRef,
		IdentityRef: &v1alpha1.RepoRef{Kind: "MarketIdentity", Name: identityName},
		AuthRef:     binding.AuthRef}
	ctl.ensureStatusObject(ctx, ws, statusstate.AtprotoRepoGVR, "AtprotoRepo", statusObjectName("repo", binding.DID), spec)
}
