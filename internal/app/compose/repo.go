package compose

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
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

func localAccountNames(acct LocalAccount) (repo, account, session string) {
	return atprotorepo.RepoName(acct.DID), ObjectName("account", acct.Handle), ObjectName("session", acct.Handle)
}

func LocalAccountLinks(ws string, acct LocalAccount) []Link {
	if strings.TrimSpace(acct.DID) == "" {
		return nil
	}
	pdsTarget := AppRef(ComponentKindPds, PdsComponent, ws)
	name, accountName, sessionName := localAccountNames(acct)
	localRef := AppRef(KindLocalRepo, name, ws)
	links := []Link{RepoBackendLink(AppRef(KindAtprotoRepo, name, ws), localRef), LocalRepoLink(localRef, pdsTarget, acct.DID),
		AccountStatusLink(AppRef("PdsAccount", accountName, ws), pdsTarget, acct.DID)}
	if acct.SessionID != "" {
		links = append(links, SessionStatusLink(AppRef("AtprotoSession", sessionName, ws), pdsTarget, acct.DID))
	}
	return links
}

func (c *Composer) ApplyLocalAccount(ctx context.Context, r Root, ws string, acct LocalAccount) error {
	if strings.TrimSpace(acct.DID) == "" {
		return nil
	}
	pdsRef := commonv1.ObjectRef{Kind: ComponentKindPds, Name: PdsComponent}
	name, accountName, sessionName := localAccountNames(acct)
	local := &v1alpha1.LocalRepo{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.LocalRepoSpec{DID: acct.DID, PdsRef: pdsRef}}
	if err := c.ApplyRepo(ctx, r, ws, name, KindLocalRepo, v1alpha1.LocalRepoGVR, local); err != nil {
		return err
	}
	account := &v1alpha1.PdsAccount{ObjectMeta: metav1.ObjectMeta{Name: accountName},
		Spec: v1alpha1.PdsAccountSpec{DID: acct.DID, Handle: acct.Handle, PdsRef: pdsRef}}
	if err := c.ApplyTyped(ctx, r, ws, v1alpha1.PdsAccountGVR, v1alpha1.GroupVersion.String(), "PdsAccount", account); err != nil {
		return err
	}
	if acct.PLC {
		plc := &v1alpha1.PlcIdentity{ObjectMeta: metav1.ObjectMeta{Name: ObjectName("plc", acct.DID)}, Spec: v1alpha1.PlcIdentitySpec{DID: acct.DID}}
		if err := c.ApplyTyped(ctx, r, ws, v1alpha1.PlcIdentityGVR, v1alpha1.GroupVersion.String(), "PlcIdentity", plc); err != nil {
			return err
		}
	}
	if acct.SessionID == "" {
		return nil
	}
	session := &v1alpha1.AtprotoSession{ObjectMeta: metav1.ObjectMeta{Name: sessionName},
		Spec: v1alpha1.AtprotoSessionSpec{DID: acct.DID, AccountRef: commonv1.LocalObjectRef{Name: accountName}, PdsRef: pdsRef, SessionID: acct.SessionID}}
	return c.ApplyTyped(ctx, r, ws, v1alpha1.AtprotoSessionGVR, v1alpha1.GroupVersion.String(), "AtprotoSession", session)
}

func (c *Composer) ApplyRelayStatus(ctx context.Context, r Root, relayName string) error {
	relayName = strings.TrimPrefix(strings.TrimSpace(relayName), ComponentKindRelay+"/")
	if relayName == "" {
		return nil
	}
	name := ObjectName("relay", relayName)
	obj := &v1alpha1.RelayStatus{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.RelayStatusSpec{RelayRef: commonv1.ObjectRef{Kind: ComponentKindRelay, Name: relayName}}}
	if err := c.ApplyTyped(ctx, r, r.Workspace, v1alpha1.RelayStatusGVR, v1alpha1.GroupVersion.String(), "RelayStatus", obj); err != nil {
		return err
	}
	return c.ApplyLinks(ctx, r, "relay-status", []Link{RefLink(AppRef("RelayStatus", name, r.Workspace),
		AppRef(ComponentKindRelay, relayName, r.Workspace), Cap(bus.CapabilityXRPC, "_health"))})
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
