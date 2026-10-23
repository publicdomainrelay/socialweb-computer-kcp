package compose

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

func (c *Composer) ApplyRepo(ctx context.Context, r Root, ws, name, backendKind string, backendGVR schema.GroupVersionResource, backend any) error {
	if err := c.ApplyTyped(ctx, r, ws, backendGVR, v1alpha1.GroupVersion.String(), backendKind, backend); err != nil {
		return err
	}
	repo := &v1alpha1.AtprotoRepo{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AtprotoRepoSpec{BackendRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: backendKind, Name: name}}}
	return c.ApplyTyped(ctx, r, ws, v1alpha1.AtprotoRepoGVR, v1alpha1.GroupVersion.String(), KindAtprotoRepo, repo)
}

type LocalAccount struct {
	DID string

	Handle string

	SessionID string

	PLC bool
}

func (c *Composer) ApplyLocalAccount(ctx context.Context, r Root, ws string, acct LocalAccount) ([]Link, error) {
	if strings.TrimSpace(acct.DID) == "" {
		return nil, nil
	}
	pdsRef := commonv1.ObjectRef{Kind: ComponentKindPds, Name: PdsComponent}
	pdsTarget := AppRef(ComponentKindPds, PdsComponent, ws)
	name := atprotorepo.RepoName(acct.DID)
	local := &v1alpha1.LocalRepo{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.LocalRepoSpec{DID: acct.DID, PdsRef: pdsRef}}
	if err := c.ApplyRepo(ctx, r, ws, name, KindLocalRepo, v1alpha1.LocalRepoGVR, local); err != nil {
		return nil, err
	}
	repo := AppRef(KindAtprotoRepo, name, ws)
	localRef := AppRef(KindLocalRepo, name, ws)
	links := []Link{RepoBackendLink(repo, localRef), LocalRepoLink(localRef, pdsTarget, acct.DID)}
	accountName := ObjectName("account", acct.Handle)
	account := &v1alpha1.PdsAccount{ObjectMeta: metav1.ObjectMeta{Name: accountName},
		Spec: v1alpha1.PdsAccountSpec{DID: acct.DID, Handle: acct.Handle, PdsRef: pdsRef}}
	if err := c.ApplyTyped(ctx, r, ws, v1alpha1.PdsAccountGVR, v1alpha1.GroupVersion.String(), "PdsAccount", account); err != nil {
		return nil, err
	}
	links = append(links, AccountStatusLink(AppRef("PdsAccount", accountName, ws), pdsTarget, acct.DID))
	if acct.PLC {
		plc := &v1alpha1.PlcIdentity{ObjectMeta: metav1.ObjectMeta{Name: ObjectName("plc", acct.DID)}, Spec: v1alpha1.PlcIdentitySpec{DID: acct.DID}}
		if err := c.ApplyTyped(ctx, r, ws, v1alpha1.PlcIdentityGVR, v1alpha1.GroupVersion.String(), "PlcIdentity", plc); err != nil {
			return nil, err
		}
	}
	if acct.SessionID != "" {
		sessionName := ObjectName("session", acct.Handle)
		session := &v1alpha1.AtprotoSession{ObjectMeta: metav1.ObjectMeta{Name: sessionName},
			Spec: v1alpha1.AtprotoSessionSpec{DID: acct.DID, AccountRef: commonv1.LocalObjectRef{Name: accountName}, PdsRef: pdsRef, SessionID: acct.SessionID}}
		if err := c.ApplyTyped(ctx, r, ws, v1alpha1.AtprotoSessionGVR, v1alpha1.GroupVersion.String(), "AtprotoSession", session); err != nil {
			return nil, err
		}
		links = append(links, SessionStatusLink(AppRef("AtprotoSession", sessionName, ws), pdsTarget, acct.DID))
	}
	return links, nil
}

func (c *Composer) ApplyRelayStatus(ctx context.Context, r Root, relayName string) error {
	relayName = strings.TrimPrefix(strings.TrimSpace(relayName), ComponentKindRelay+"/")
	if relayName == "" {
		return nil
	}
	obj := &v1alpha1.RelayStatus{ObjectMeta: metav1.ObjectMeta{Name: ObjectName("relay", relayName)},
		Spec: v1alpha1.RelayStatusSpec{RelayRef: commonv1.ObjectRef{Kind: ComponentKindRelay, Name: relayName}}}
	return c.ApplyTyped(ctx, r, r.Workspace, v1alpha1.RelayStatusGVR, v1alpha1.GroupVersion.String(), "RelayStatus", obj)
}

func (c *Composer) ApplyVaultCredential(ctx context.Context, r Root, server, mount, path string) error {
	server = strings.TrimSpace(server)
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if server == "" || mount == "" || path == "" {
		return nil
	}
	obj := &v1alpha1.VaultCredential{ObjectMeta: metav1.ObjectMeta{Name: ObjectName("credential", server+"-"+path)},
		Spec: v1alpha1.VaultCredentialSpec{SecretRef: commonv1.SecretRef{Vault: v1alpha1.VaultServerRef(server), Mount: mount, Path: path}}}
	return c.ApplyTyped(ctx, r, r.Workspace, v1alpha1.VaultCredentialGVR, v1alpha1.GroupVersion.String(), "VaultCredential", obj)
}
