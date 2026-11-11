package compose

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

const OperatorKeysPath = "identity/operator/keys"

type Operator struct {
	PdsURL string

	CARef *commonv1.ObjectRef

	Identifier string

	Password string

	Vault string

	Mount string

	Create bool
}

func (o Operator) remoteName() string {
	return "operator-" + Stamp(strings.TrimRight(strings.TrimSpace(o.PdsURL), "/"))
}

func (o Operator) ref(ws string) commonv1.ObjectRef {
	return RemotePDSRefIn(o.remoteName(), ws)
}

func OperatorLinks(r Root, o Operator) []Link {
	links := []Link{
		RefLink(r.Ref(), o.ref(r.Workspace), Cap(pds.CapabilitySession, ScopeAll), Cap(pds.CapabilityAccount, ScopeAll)),
		RootVaultLink(r, o.Vault, o.Mount+"/identity/*"),
	}
	if o.CARef != nil && o.tls() {
		links = append(links, TrustLink(o.ref(r.Workspace), *o.CARef))
	}
	creds := o.credentials()
	return append(links, SecretLinks(o.ref(r.Workspace), SecretUse{Ref: &creds})...)
}

func (o Operator) credentials() commonv1.SecretRef {
	vaultRef := AppRef(KindVault, o.Vault, "")
	return commonv1.SecretRef{Vault: &vaultRef, Mount: o.Mount, Path: OperatorKeysPath, Key: "password"}
}

func (o Operator) tls() bool {
	return strings.HasPrefix(strings.TrimRight(strings.TrimSpace(o.PdsURL), "/"), "https://")
}

func (c *Composer) applyOperatorPDS(ctx context.Context, r Root, o Operator) error {
	spec := pdsv1alpha1.RemotePDSSpec{URL: strings.TrimRight(strings.TrimSpace(o.PdsURL), "/"), DID: pdsServiceDID(o.PdsURL)}
	if o.CARef != nil && o.tls() {
		ca := *o.CARef
		spec.TLS = pdsv1alpha1.RemotePDSTLS{Enabled: true, CARef: &ca}
	}
	name := o.remoteName()
	remote := &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{OperatorRecordLabel: OperatorRepoName}}, Spec: spec}
	if err := c.ApplyTyped(ctx, r, r.Workspace, RemotePDSGVR, pdsv1alpha1.GroupVersion.String(), KindRemotePDS, remote); err != nil {
		return err
	}
	selector := OwnerLabel + "=" + r.Owner() + "," + OperatorRecordLabel + "=" + OperatorRepoName
	return c.Prune(ctx, r.Workspace, RemotePDSGVR, selector, map[string]bool{name: true})
}

func (c *Composer) OperatorRepo(ctx context.Context, r Root, o Operator) (atprotorepo.Repo, string, error) {
	if strings.TrimSpace(o.PdsURL) == "" || strings.TrimSpace(o.Identifier) == "" {
		return nil, "", errors.New("compose: the operator account names no PDS URL or identifier")
	}
	if err := c.applyOperatorPDS(ctx, r, o); err != nil {
		return nil, "", err
	}
	if ok, msg := c.LinksAccepted(ctx, r, OperatorLinks(r, o)); !ok {
		return nil, "", errors.New(msg)
	}
	kv, err := c.KV(ctx, r, o.Vault, o.Mount+"/identity/*")
	if err != nil {
		return nil, "", err
	}
	data, err := readKV(ctx, kv, o.Mount, OperatorKeysPath)
	if err != nil {
		return nil, "", err
	}
	if o.Password != "" && data["password"] != o.Password {
		data["password"] = o.Password
		if err := kv.Write(ctx, o.Mount, OperatorKeysPath, data); err != nil {
			return nil, "", err
		}
	}
	if p, _ := data["password"].(string); p == "" {
		if !o.Create {
			return nil, "", errors.New("compose: the operator account has no password in the root's vault")
		}
		password, err := NewPassword()
		if err != nil {
			return nil, "", err
		}
		data["password"] = password
		if err := kv.Write(ctx, o.Mount, OperatorKeysPath, data); err != nil {
			return nil, "", err
		}
	}
	to := o.ref(r.Workspace)
	sessions, err := bus.Resolve[pds.Sessions](ctx, c.cfg.Bus, r.Ref(), to, bus.Capability{Name: pds.CapabilitySession, Scope: []string{ScopeAll}})
	if err != nil {
		return nil, "", err
	}
	creds := o.credentials()
	did := ""
	sess, err := sessions.SignIn(ctx, pds.SessionRequest{Identifier: o.Identifier, Credentials: creds})
	switch {
	case err == nil:
		did = sess.DID
	case o.Create:
		accounts, rerr := bus.Resolve[pds.Accounts](ctx, c.cfg.Bus, r.Ref(), to, bus.Capability{Name: pds.CapabilityAccount, Scope: []string{ScopeAll}})
		if rerr != nil {
			return nil, "", rerr
		}
		domain := o.Identifier[strings.Index(o.Identifier, ".")+1:]
		acct, cerr := accounts.CreateAccount(ctx, pds.AccountRequest{Handle: o.Identifier, Email: o.Identifier + "@" + domain, Credentials: creds})
		if cerr != nil {
			return nil, "", errors.Join(err, cerr)
		}
		did = acct.DID
	default:
		return nil, "", err
	}
	repo, err := bus.Resolve[atprotorepo.Repo](ctx, c.cfg.Bus, r.Ref(), to, bus.Capability{Name: pds.CapabilityAccount, Scope: []string{did}})
	if err != nil {
		return nil, "", err
	}
	return repo, did, nil
}

const (
	OperatorSignInOption = "operator-signin"

	OperatorSignInModeOption = "operator-signin-mode"

	OperatorPdsURLOption = "operator-pds-url"

	OperatorOAuthSessionPath = "identity/operator/oauth-session"

	operatorLinkSet = "operator"
)

var OperatorSignInScope = strings.Join([]string{atprotooauth.ScopeAtproto, atprotooauth.RepoTangledVouchWrite, atprotooauth.RepoAssociationWrite,
	atprotooauth.RepoEndorsementWrite}, " ")

type OperatorSignIn struct {
	Mode v1alpha1.MarketIdentityMode

	Account string

	PdsURL string

	CARef *commonv1.ObjectRef

	Vault string

	Mount string
}

func OperatorSignInSet(opts []v1alpha1.ApplicationOption) bool {
	return strings.TrimSpace(OptionValue(opts, OperatorSignInOption)) != ""
}

func OperatorSignInOf(opts []v1alpha1.ApplicationOption, vault, mount string, caRef *commonv1.ObjectRef) (OperatorSignIn, error) {
	account := strings.TrimSpace(OptionValue(opts, OperatorSignInOption))
	if account == "" || account == "true" {
		return OperatorSignIn{}, fmt.Errorf("compose: --%s names the operator account: its handle or DID", OperatorSignInOption)
	}
	s := OperatorSignIn{Account: account, Vault: vault, Mount: mount,
		Mode: v1alpha1.MarketIdentityMode(cmp.Or(strings.TrimSpace(OptionValue(opts, OperatorSignInModeOption)), string(v1alpha1.MarketIdentityModeOAuthQr)))}
	switch s.Mode {
	case v1alpha1.MarketIdentityModeOAuthQr:
		return s, nil
	case v1alpha1.MarketIdentityModePdsAccount:
		s.PdsURL = strings.TrimRight(strings.TrimSpace(OptionValue(opts, OperatorPdsURLOption)), "/")
		if s.PdsURL == "" {
			return OperatorSignIn{}, fmt.Errorf("compose: --%s pdsAccount needs --%s", OperatorSignInModeOption, OperatorPdsURLOption)
		}
		s.CARef = caRef
		return s, nil
	}
	return OperatorSignIn{}, fmt.Errorf("compose: --%s %q: want oauthQr or pdsAccount", OperatorSignInModeOption, s.Mode)
}

func (c *Composer) ApplyOperatorSignIn(ctx context.Context, r Root, s OperatorSignIn) (string, error) {
	ws := r.Workspace
	if s.Mode == v1alpha1.MarketIdentityModePdsAccount {
		op := Operator{PdsURL: s.PdsURL, CARef: s.CARef, Identifier: s.Account, Vault: s.Vault, Mount: s.Mount}
		if err := c.ApplyLinks(ctx, r, operatorLinkSet, OperatorLinks(r, op)); err != nil {
			return "", err
		}
		did := ""
		if id, err := c.Identity(ctx, ws, OperatorRepoName); err == nil && id.Spec.Mode == v1alpha1.MarketIdentityModePdsAccount {
			did = strings.TrimSpace(id.Status.Did)
		} else if err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
		if did == "" {
			var err error
			if _, did, err = c.OperatorRepo(ctx, r, op); err != nil {
				return "", err
			}
		} else if err := c.applyOperatorPDS(ctx, r, op); err != nil {
			return "", err
		}
		return did, c.applyOperatorRecordRepo(ctx, r, ws, op, did, operatorAccountIdentity(r, op))
	}
	spec := v1alpha1.MarketIdentitySpec{Mode: v1alpha1.MarketIdentityModeOAuthQr, Association: v1alpha1.AssociationSkip,
		SessionRef:   &commonv1.SecretRef{Vault: operatorVault(r, Operator{Vault: s.Vault}), Mount: s.Mount, Path: OperatorOAuthSessionPath},
		SessionScope: OperatorSignInScope}
	if strings.HasPrefix(s.Account, "did:") {
		spec.Did = s.Account
	} else {
		spec.Handle = s.Account
	}
	if err := c.ApplyIdentitySpec(ctx, r, ws, OperatorRepoName, spec); err != nil {
		return "", err
	}
	id, err := c.Identity(ctx, ws, OperatorRepoName)
	if err != nil {
		return "", err
	}
	did, pdsURL := strings.TrimSpace(id.Status.Did), strings.TrimSpace(id.Status.PdsURL)
	if id.Status.Phase != v1alpha1.MarketIdentityPhaseReady || did == "" || pdsURL == "" {
		return "", nil
	}
	op := Operator{PdsURL: pdsURL, Vault: s.Vault, Mount: s.Mount}
	if err := c.applyOperatorPDS(ctx, r, op); err != nil {
		return "", err
	}
	return did, c.applyOperatorRecordRepo(ctx, r, ws, op, did, spec)
}
