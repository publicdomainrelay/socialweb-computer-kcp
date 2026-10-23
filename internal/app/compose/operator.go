package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
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
	return links
}

func (o Operator) tls() bool {
	return strings.HasPrefix(strings.TrimRight(strings.TrimSpace(o.PdsURL), "/"), "https://")
}

func (c *Composer) OperatorRepo(ctx context.Context, r Root, o Operator) (atprotorepo.Repo, string, error) {
	if strings.TrimSpace(o.PdsURL) == "" || strings.TrimSpace(o.Identifier) == "" {
		return nil, "", errors.New("compose: the operator account names no PDS URL or identifier")
	}
	spec := pdsv1alpha1.RemotePDSSpec{URL: strings.TrimRight(strings.TrimSpace(o.PdsURL), "/"), DID: pdsServiceDID(o.PdsURL)}
	if o.CARef != nil && o.tls() {
		ca := *o.CARef
		spec.TLS = pdsv1alpha1.RemotePDSTLS{Enabled: true, CARef: &ca}
	}
	remote := &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: o.remoteName()}, Spec: spec}
	if _, err := c.ApplyRemotePDS(ctx, r, r.Workspace, remote); err != nil {
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
	creds := commonv1.SecretRef{Vault: v1alpha1.VaultServerRef(o.Vault), Mount: o.Mount, Path: OperatorKeysPath, Key: "password"}
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

func claimOf(rawURL string) (nonce, service string) {
	_, frag, _ := strings.Cut(rawURL, "#")
	q, _ := url.ParseQuery(frag)
	return q.Get("n"), q.Get("s")
}

func (c *Composer) WriteClaim(ctx context.Context, r Root, o Operator, id *v1alpha1.MarketIdentity, namePrefix, service string) (string, error) {
	if id == nil || strings.TrimSpace(service) == "" {
		return "", nil
	}
	nonce, pendingService := "", ""
	if action := id.Status.PendingAction; action != nil {
		nonce, pendingService = claimOf(action.URL)
	}
	if id.Spec.Mode != v1alpha1.MarketIdentityModeLocalRepo && nonce == "" {
		return "", nil
	}
	if pendingService != "" && pendingService != service {
		return "", fmt.Errorf("the pending association names service %q, not %q", pendingService, service)
	}
	didText := strings.TrimSpace(id.Status.Did)
	if _, err := syntax.ParseDID(didText); err != nil {
		return "", fmt.Errorf("the identity's did %q: %w", didText, err)
	}
	repo, operator, err := c.OperatorRepo(ctx, r, o)
	if err != nil {
		return "", err
	}
	rkey := service + "-" + strings.ReplaceAll(didText, ":", "-")
	if held, err := repo.GetRecord(ctx, operator, BadgeBlueKeysNSID, rkey); err == nil {
		var value map[string]any
		if json.Unmarshal(held.Value, &value) == nil && heldClaimMatches(value, nonce, operator, didText, service) {
			return "", nil
		}
	}
	fields := map[string]any{"$type": BadgeBlueKeysNSID, "keyId": didText, "challenge": operator, "service": service,
		"name": namePrefix + didText[len(didText)-8:], "createdAt": time.Now().UTC().Format(time.RFC3339)}
	if nonce != "" {
		fields["nonce"] = nonce
	}
	record, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	if _, err := repo.PutRecord(ctx, BadgeBlueKeysNSID, rkey, record); err != nil {
		return "", err
	}
	return operator, nil
}

func heldClaimMatches(value map[string]any, nonce, operator, subject, service string) bool {
	if value == nil {
		return false
	}
	challenge, _ := value["challenge"].(string)
	keyID, _ := value["keyId"].(string)
	if challenge != operator || keyID != subject {
		return false
	}
	if got, _ := value["service"].(string); got != service {
		return false
	}
	if got, _ := value["nonce"].(string); got != "" && got != nonce {
		return false
	}
	return true
}
