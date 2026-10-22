package appctl

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/remotepds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
)

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

func (p accountPlan) repoRef() *v1alpha1.RepoRef {
	if p.Local {
		return &v1alpha1.RepoRef{Kind: ComponentKindPds, Name: PdsComponent}
	}
	if p.RemotePDS != nil && p.RemotePDS.Name != "" {
		return &v1alpha1.RepoRef{Kind: "RemotePDS", Name: p.RemotePDS.Name}
	}
	if p.PdsRef != nil && p.PdsRef.Name != "" {
		return &v1alpha1.RepoRef{Kind: p.PdsRef.Kind, Name: p.PdsRef.Name}
	}
	return nil
}

func (ctl *Controller) planAccount(ctx context.Context, ws string, id v1alpha1.ApplicationIdentity,
	suffix string, defaultAssociation v1alpha1.AssociationMode) (accountPlan, error) {
	mode := id.Mode
	if mode == "" {
		mode = v1alpha1.MarketIdentityModeLocalRepo
	}
	sessionPath := "identity/" + suffix + "/pds-session"
	keysPath := "identity/" + suffix + "/keys"
	plan := accountPlan{Mode: mode, SessionPath: sessionPath, KeysPath: keysPath}
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
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsUrl: id.PdsUrl, Association: association}
		plan.SessionPath = ""
		plan.KeysPath = ""
		return plan, nil
	case v1alpha1.MarketIdentityModePdsAccount:
		plan.ComponentMode = "callback"
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsUrl: id.PdsUrl,
			KeysRef:    &v1alpha1.VaultRef{Mount: suffix, Path: keysPath},
			SessionRef: &v1alpha1.VaultRef{Mount: suffix, Path: sessionPath}}
		url, remote, err := ctl.resolveAccountPds(ctx, ws, id)
		if err != nil {
			return accountPlan{}, err
		}
		plan.PdsURL = url
		plan.RemotePDS = remote
		plan.Identity.PdsUrl = url
		if remote != nil {
			plan.PdsRef = &v1alpha1.ComponentRef{Kind: "RemotePDS", Name: remote.Name, Did: remote.Spec.DID}
		}
		return plan, nil
	case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
		plan.ComponentMode = "oauth"
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsUrl: id.PdsUrl,
			SessionRef: &v1alpha1.VaultRef{Mount: suffix, Path: sessionPath}}
		if remote, err := ctl.remotePDSRef(ctx, ws, id.PdsRef); err != nil {
			return accountPlan{}, err
		} else if remote != nil {
			plan.RemotePDS = remote
			plan.PdsURL = remote.Spec.URL
			plan.PdsRef = &v1alpha1.ComponentRef{Kind: "RemotePDS", Name: remote.Name, Did: remote.Spec.DID}
			if plan.Identity.PdsUrl == "" {
				plan.Identity.PdsUrl = remote.Spec.URL
			}
		}
		return plan, nil
	}
	return accountPlan{}, fmt.Errorf("appctl: MarketIdentity mode %q: want one of localRepo, pdsAccount, oauthBrowser, oauthQr, oauthDelegated", mode)
}

func (ctl *Controller) resolveAccountPds(ctx context.Context, ws string, id v1alpha1.ApplicationIdentity) (string, *pdsv1alpha1.RemotePDS, error) {
	if url := strings.TrimSpace(id.PdsUrl); url != "" {
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

func (ctl *Controller) ensureAccountRepo(ctx context.Context, ws, identityName string, plan accountPlan, did string) {
	if plan.Local || strings.TrimSpace(did) == "" {
		return
	}
	pdsRef := plan.repoRef()
	if pdsRef == nil {
		ctl.c.log().Warn("the existing-account repo has no PDS ref", "identity", identityName)
		return
	}
	spec := v1alpha1.AtprotoRepoSpec{DID: did, PdsRef: pdsRef,
		IdentityRef: &v1alpha1.RepoRef{Kind: "MarketIdentity", Name: identityName},
		AuthRef:     &v1alpha1.RepoRef{Kind: "Vault", Name: plan.accessTokenPath()}}
	ctl.ensureStatusObject(ctx, ws, statusstate.AtprotoRepoGVR, "AtprotoRepo", statusObjectName("repo", did), spec)
}
