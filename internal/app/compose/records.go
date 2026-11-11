package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
)

const (
	VouchNSID = "sh.tangled.graph.vouch"

	OperatorRepoName = "operator"

	OperatorVouchPrefix = "operator-vouch-"

	OperatorRecordLabel = "apps.socialweb.computer/record-repo"

	OperatorSessionPath = "identity/operator/pds-session"

	operatorRecordLinkSet = "operator-records"
)

type OwnerSession struct {
	Service    string `json:"service"`
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

func ReadOwnerSessions(path string) (map[string]OwnerSession, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sessions map[string]OwnerSession
	if err := json.Unmarshal(b, &sessions); err != nil {
		return nil, fmt.Errorf("compose: the owner sessions file %s: %w", path, err)
	}
	return sessions, nil
}

func FirstOwnerSession(path string) (string, OwnerSession, error) {
	sessions, err := ReadOwnerSessions(path)
	if err != nil {
		return "", OwnerSession{}, err
	}
	owners := make([]string, 0, len(sessions))
	for did := range sessions {
		owners = append(owners, did)
	}
	if len(owners) == 0 {
		return "", OwnerSession{}, fmt.Errorf("compose: the owner sessions file %s holds no session", path)
	}
	slices.Sort(owners)
	return owners[0], sessions[owners[0]], nil
}

func OperatorRepoRef() *commonv1.ObjectRef {
	return &commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindAtprotoRepo, Name: OperatorRepoName}
}

func (c *Composer) ApplyOperatorVouch(ctx context.Context, r Root, ws, app, subject, at string) error {
	record, err := json.Marshal(map[string]any{"$type": VouchNSID, "subject": subject, "createdAt": at})
	if err != nil {
		return err
	}
	obj := &v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: OperatorVouchPrefix + app, Labels: map[string]string{OperatorRecordLabel: OperatorRepoName}},
		Spec: v1alpha1.AtprotoRecordSpec{RepoRef: OperatorRepoRef(), Collection: VouchNSID, Rkey: subject, Record: record}}
	return c.ApplyTyped(ctx, r, ws, AtprotoRecordGVR, v1alpha1.GroupVersion.String(), "AtprotoRecord", obj)
}

const (
	OperatorGrantRecord = "operator-grant"

	OperatorEndorsementRecord = "operator-endorsement"
)

func (c *Composer) ApplyOperatorPair(ctx context.Context, r Root, ws, identity, agent string) error {
	if !identlink.IsDID(agent) {
		return nil
	}
	labels := map[string]string{OperatorRecordLabel: OperatorRepoName}
	apply := func(name, collection string, value map[string]any) error {
		record, err := json.Marshal(value)
		if err != nil {
			return err
		}
		obj := &v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Spec: v1alpha1.AtprotoRecordSpec{RepoRef: OperatorRepoRef(), Collection: collection, Rkey: agent, Record: record}}
		return c.ApplyTyped(ctx, r, ws, AtprotoRecordGVR, v1alpha1.GroupVersion.String(), "AtprotoRecord", obj)
	}
	if err := apply(OperatorGrantRecord, market.AssociationNSID, market.GrantRecord(agent, identityField(identity, "metadata.creationTimestamp"))); err != nil {
		return err
	}
	grant, err := Get[v1alpha1.AtprotoRecord](ctx, c, ws, AtprotoRecordGVR, OperatorGrantRecord)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil || grant.Status.Phase != v1alpha1.AtprotoRecordPhaseApplied || grant.Status.ObservedGeneration != grant.Generation || grant.Status.Cid == "" {
		return err
	}
	if err := apply(OperatorEndorsementRecord, market.EndorsementNSID, market.EndorsementRecord(agent)); err != nil {
		return err
	}
	if e, err := Get[v1alpha1.AtprotoRecord](ctx, c, ws, AtprotoRecordGVR, OperatorEndorsementRecord); err == nil && e.Status.Phase == v1alpha1.AtprotoRecordPhaseFailed {
		c.log.Warn("operator_endorsement_skipped", "agent", agent, "message", e.Status.Message,
			"hint", "the operator session may lack the optional scope "+atprotooauth.RepoEndorsementWrite+"; sign the operator in again to grant it; until then the agent stays unassociated")
	}
	return nil
}

func (c *Composer) DeleteOperatorPair(ctx context.Context, ws string) error {
	ri, err := c.Client(ws, AtprotoRecordGVR)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, OperatorEndorsementRecord, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if _, err := Get[v1alpha1.AtprotoRecord](ctx, c, ws, AtprotoRecordGVR, OperatorEndorsementRecord); !apierrors.IsNotFound(err) {
		return err
	}
	if err := ri.Delete(ctx, OperatorGrantRecord, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (c *Composer) UpdateAgentOf(ctx context.Context, r Root, ws, did string, add, remove []string) error {
	if !identlink.IsDID(did) || len(add)+len(remove) == 0 {
		return nil
	}
	key := strings.Join(add, " ") + "|" + strings.Join(remove, " ")
	if held, ok := c.agentOf.Load(did); ok && held == key {
		return nil
	}
	accounts, err := bus.Resolve[pds.Accounts](ctx, c.cfg.Bus, r.Ref(), AppRef(ComponentKindPds, PdsComponent, ws),
		bus.Capability{Name: pds.CapabilityAccount, Scope: []string{ScopeAll}})
	if err != nil {
		return err
	}
	aka, err := accounts.UpdateAlsoKnownAs(ctx, did, add, remove)
	if err != nil {
		return fmt.Errorf("compose: the alsoKnownAs of %s: %w", did, err)
	}
	c.agentOf.Store(did, key)
	c.log.Info("agent_of_applied", "did", did, "alsoKnownAs", aka)
	return nil
}

const AssociationRequestRecordName = "association-request"

func (c *Composer) ApplyAssociationRequestRecord(ctx context.Context, r Root, ws string, repo commonv1.ObjectRef, identity string, req v1alpha1.AssociationRequest, accounts []string) error {
	principal, err := syntax.ParseDID(strings.TrimSpace(req.Principal))
	if err != nil || principal.String() != req.Principal {
		return fmt.Errorf("compose: spec.identity.associationRequest.principal %q is not a DID", req.Principal)
	}
	contract, err := syntax.ParseATURI(strings.TrimSpace(req.Contract))
	if err != nil || contract.Authority().String() != req.Principal || contract.RecordKey() == "" {
		return fmt.Errorf("compose: spec.identity.associationRequest.contract %q is not a record of %s", req.Contract, req.Principal)
	}
	if nonce := strings.TrimSpace(req.Nonce); nonce == "" || len(nonce) > 128 || nonce != req.Nonce {
		return fmt.Errorf("compose: spec.identity.associationRequest.nonce is not 1 to 128 characters")
	}
	value := map[string]any{"$type": market.AssociationRequestNSID, "principal": req.Principal,
		"contract": req.Contract, "nonce": req.Nonce, "createdAt": identityField(identity, "metadata.creationTimestamp")}
	if len(accounts) > 0 {
		value["accounts"] = accounts
	}
	record, err := json.Marshal(value)
	if err != nil {
		return err
	}
	obj := &v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: AssociationRequestRecordName},
		Spec: v1alpha1.AtprotoRecordSpec{RepoRef: &repo, Collection: market.AssociationRequestNSID, Rkey: req.Principal, Record: record}}
	return c.ApplyTyped(ctx, r, ws, AtprotoRecordGVR, v1alpha1.GroupVersion.String(), "AtprotoRecord", obj)
}

func AgentOfEntries(principals ...string) []string {
	var out []string
	for _, principal := range principals {
		if identlink.IsDID(principal) {
			out = append(out, identlink.AgentOfPrefix+principal)
		}
	}
	return out
}

func BorrowedHandle(handle string) []string {
	if handle = strings.ToLower(strings.TrimSpace(handle)); handle == "" {
		return nil
	}
	return []string{"at://" + handle}
}

func OperatorRecordLinks(ws string, remotePDS commonv1.ObjectRef, did string, caRef *commonv1.ObjectRef) []Link {
	repo := AppRef(KindAtprotoRepo, OperatorRepoName, ws)
	remote := AppRef(KindRemoteRepo, OperatorRepoName, ws)
	records := exposurev1alpha1.ComponentLinkSelector{Group: v1alpha1.GroupName, Kind: "AtprotoRecord", Workspace: ws,
		MatchLabels: map[string]string{OperatorRecordLabel: OperatorRepoName}}
	links := []Link{
		SelectorLink(LinkName("records", OperatorRepoName), records, repo, Cap(atprotorepo.CapabilityWrite, VouchNSID, market.AssociationNSID, market.EndorsementNSID)),
		RepoBackendLink(repo, remote),
	}
	identity := AppRef(KindMarketIdentity, OperatorRepoName, ws)
	if caRef != nil {
		links = append(links, TrustLink(identity, *caRef))
	}
	return append(links, RemoteRepoLinks(remote, remotePDS, identity, did)...)
}

func (c *Composer) ApplyOperatorRepo(ctx context.Context, r Root, ws string, op Operator) error {
	ri, err := c.Client(ws, AtprotoRecordGVR)
	if err != nil {
		return err
	}
	records, err := kube.List[v1alpha1.AtprotoRecord](ctx, ri, metav1.ListOptions{LabelSelector: OperatorRecordLabel + "=" + OperatorRepoName})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	did := ""
	if id, err := c.Identity(ctx, ws, OperatorRepoName); err == nil {
		did = strings.TrimSpace(id.Status.Did)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if did == "" {
		if _, did, err = c.OperatorRepo(ctx, r, op); err != nil {
			return err
		}
	} else if strings.TrimSpace(op.PdsURL) != "" {
		if err := c.applyOperatorPDS(ctx, r, op); err != nil {
			return err
		}
	}
	return c.applyOperatorRecordRepo(ctx, r, ws, op, did, operatorAccountIdentity(r, op))
}

func operatorVault(r Root, op Operator) *commonv1.ObjectRef {
	return &commonv1.ObjectRef{Kind: v1alpha1.KindOpenBaoServer, Name: op.Vault, Workspace: r.Workspace}
}

func operatorAccountIdentity(r Root, op Operator) v1alpha1.MarketIdentitySpec {
	identity := v1alpha1.MarketIdentitySpec{Mode: v1alpha1.MarketIdentityModePdsAccount, Handle: strings.TrimSpace(op.Identifier),
		PdsURL: strings.TrimRight(strings.TrimSpace(op.PdsURL), "/"), Association: v1alpha1.AssociationSkip,
		KeysRef:    &commonv1.SecretRef{Vault: operatorVault(r, op), Mount: op.Mount, Path: OperatorKeysPath},
		SessionRef: &commonv1.SecretRef{Vault: operatorVault(r, op), Mount: op.Mount, Path: OperatorSessionPath}}
	if op.CARef != nil && op.tls() {
		ca := *op.CARef
		identity.CARef = &ca
	}
	return identity
}

func (c *Composer) applyOperatorRecordRepo(ctx context.Context, r Root, ws string, op Operator, did string, identity v1alpha1.MarketIdentitySpec) error {
	var caRef *commonv1.ObjectRef
	if op.CARef != nil && op.tls() {
		ca := *op.CARef
		caRef = &ca
	}
	links := OperatorRecordLinks(ws, op.ref(r.Workspace), did, caRef)
	if err := c.AddLinks(ctx, r, operatorRecordLinkSet, links); err != nil {
		return err
	}
	if err := c.ApplyIdentitySpec(ctx, r, ws, OperatorRepoName, identity); err != nil {
		return err
	}
	pdsRef := op.ref(r.Workspace)
	remote := &v1alpha1.RemoteRepo{ObjectMeta: metav1.ObjectMeta{Name: OperatorRepoName}, Spec: v1alpha1.RemoteRepoSpec{DID: did, PdsRef: pdsRef,
		CredentialsRef: commonv1.CredentialsRef{ObjectRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindMarketIdentity, Name: OperatorRepoName}}}}
	if err := c.ApplyRepo(ctx, r, ws, OperatorRepoName, KindRemoteRepo, v1alpha1.RemoteRepoGVR, remote); err != nil {
		return err
	}
	return c.ApplyLinks(ctx, r, operatorRecordLinkSet, links, ws)
}
