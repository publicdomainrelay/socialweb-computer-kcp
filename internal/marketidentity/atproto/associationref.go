package atproto

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	recordclient "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const (
	ReasonAssociationRefVerified = "AssociationRefVerified"

	ReasonAssociationRefPending = "AssociationRefPending"

	ReasonAssociationRefInvalid = "AssociationRefInvalid"

	ReasonAssociationRefUnverified = "AssociationRefUnverified"

	associationRefTTL = 5 * time.Minute
)

type refKey struct {
	uri, cid, subject string
}

type refVerdict struct {
	authority string

	reason string

	message string

	expiresAt time.Time

	contract *commonv1.StrongRef
}

func newRefCache() *ttlcache.Cache[refKey, refVerdict] {
	return ttlcache.New[refKey, refVerdict](ttlcache.Options{TTL: associationRefTTL, MaxEntries: 1024})
}

func newVerifiedCache() *ttlcache.Cache[refKey, refVerdict] {
	return ttlcache.New[refKey, refVerdict](ttlcache.Options{MaxEntries: 1024})
}

func refCondition(status *v1alpha1.MarketIdentityStatus, generation int64) bool {
	c := meta.FindStatusCondition(status.Conditions, v1alpha1.ConditionAssociated)
	return c != nil && c.ObservedGeneration == generation && strings.HasPrefix(c.Reason, "AssociationRef")
}

var ErrAssociationRefStale = errors.New("marketidentity: the verified association record could not be read again")

func (a *Accounts) dropAssociationRef(id *v1alpha1.MarketIdentity, status *v1alpha1.MarketIdentityStatus) {
	previous := status.AssociationRef
	if previous == nil {
		return
	}
	if did := recordclient.RepoFromURI(previous.URI); !slices.Contains(uniqueDIDs(id.Spec.ApprovedAssociations), did) {
		status.Associations = slices.DeleteFunc(status.Associations, func(have string) bool { return have == did })
	}
	status.AssociationRef, status.AssociationScope = nil, nil
	setAssociated(status, id.Generation, id.Spec.Association)
	if id.Spec.Mode == v1alpha1.MarketIdentityModeLocalRepo && strings.TrimSpace(status.Did) != "" &&
		(status.Phase == v1alpha1.MarketIdentityPhaseReady || status.Phase == v1alpha1.MarketIdentityPhaseAwaitingApproval) {
		settleAssociation(status, id.Generation, id.Spec.Association, "the PDS reports "+status.Did)
	}
}

func (a *Accounts) ApplyAssociationRef(ctx context.Context, id *v1alpha1.MarketIdentity, status *v1alpha1.MarketIdentityStatus) (time.Duration, error) {
	ref := id.Spec.AssociationRef
	if ref == nil {
		a.dropAssociationRef(id, status)
		return 0, nil
	}
	want := commonv1.StrongRef{URI: strings.TrimSpace(ref.URI), CID: strings.TrimSpace(ref.CID)}
	subject := strings.TrimSpace(status.Did)
	key := refKey{uri: want.URI, cid: want.CID, subject: subject}
	verdict := refVerdict{reason: ReasonAssociationRefPending, message: "waiting for the identity DID before the association record " + want.URI + " is read"}
	var err error
	if subject != "" {
		verdict, err = a.refs.GetOrLoad(ctx, key, func(ctx context.Context) (refVerdict, time.Duration, error) {
			v, err := a.verifyAssociationRef(ctx, want, subject)
			ttl := associationRefTTL
			if !v.expiresAt.IsZero() {
				ttl = min(ttl, max(time.Until(v.expiresAt), time.Second))
			}
			return v, ttl, err
		})
	}
	if err != nil {
		a.log.Warn("the association record could not be read", "identity", id.Name, "uri", want.URI, "err", err.Error())
		last, held := a.verified.Get(key)
		live := held && (last.expiresAt.IsZero() || time.Now().Before(last.expiresAt))
		if live && status.AssociationRef != nil && *status.AssociationRef == want && refCondition(status, id.Generation) &&
			meta.IsStatusConditionTrue(status.Conditions, v1alpha1.ConditionAssociated) {
			if last.expiresAt.IsZero() {
				return 0, fmt.Errorf("%w: %w", ErrAssociationRefStale, err)
			}
			return max(time.Until(last.expiresAt), time.Second), fmt.Errorf("%w: %w", ErrAssociationRefStale, err)
		}
		verdict = refVerdict{reason: ReasonAssociationRefUnverified, message: "reading the association record " + want.URI + ": " + err.Error()}
	}
	previous := status.AssociationRef
	declared := uniqueDIDs(id.Spec.ApprovedAssociations)
	drop := func(did string) {
		if did != "" && !slices.Contains(declared, did) {
			status.Associations = slices.DeleteFunc(status.Associations, func(have string) bool { return have == did })
		}
	}
	if previous != nil {
		drop(recordclient.RepoFromURI(previous.URI))
	}
	if verdict.reason != ReasonAssociationRefVerified {
		a.verified.Delete(key)
		drop(recordclient.RepoFromURI(want.URI))
		status.AssociationRef, status.AssociationScope = nil, nil
		conds.Set(&status.Conditions, id.Generation, v1alpha1.ConditionAssociated, false, verdict.reason, verdict.message)
		a.settleRef(id, status)
		if verdict.reason == ReasonAssociationRefPending {
			return 0, nil
		}
		a.log.Info("association record refused", "identity", id.Name, "uri", want.URI, "reason", verdict.reason, "message", verdict.message)
		return 0, err
	}
	a.verified.Set(key, verdict)
	status.Associations = uniqueDIDs(append([]string{verdict.authority}, status.Associations...))
	status.AssociationRef, status.AssociationScope = &want, nil
	if verdict.contract != nil {
		status.AssociationScope = &v1alpha1.AssociationScope{Contract: *verdict.contract, ExpiresAt: verdict.expiresAt.UTC().Format(time.RFC3339Nano)}
	}
	message := "the grant " + want.URI + " of " + verdict.authority + " names " + subject + " and its identlink walk reaches " + verdict.authority
	if !verdict.expiresAt.IsZero() {
		message += " until " + verdict.expiresAt.UTC().Format(time.RFC3339)
	}
	conds.Set(&status.Conditions, id.Generation, v1alpha1.ConditionAssociated, true, ReasonAssociationRefVerified, message)
	a.settleRef(id, status)
	if verdict.expiresAt.IsZero() {
		return 0, nil
	}
	return max(time.Until(verdict.expiresAt), time.Second), nil
}

func (a *Accounts) settleRef(id *v1alpha1.MarketIdentity, status *v1alpha1.MarketIdentityStatus) {
	if id.Spec.Mode != v1alpha1.MarketIdentityModeLocalRepo || strings.TrimSpace(status.Did) == "" {
		return
	}
	if status.Phase == v1alpha1.MarketIdentityPhaseReady || status.Phase == v1alpha1.MarketIdentityPhaseAwaitingApproval {
		settleAssociation(status, id.Generation, v1alpha1.AssociationRequireApproval, "the PDS reports "+status.Did)
	}
}

func (a *Accounts) verifyAssociationRef(ctx context.Context, ref commonv1.StrongRef, subject string) (refVerdict, error) {
	invalid := func(format string, args ...any) (refVerdict, error) {
		return refVerdict{reason: ReasonAssociationRefInvalid, message: fmt.Sprintf(format, args...)}, nil
	}
	authority, collection, rkey := recordclient.SplitURI(ref.URI)
	switch {
	case !strings.HasPrefix(ref.URI, "at://") || !strings.HasPrefix(authority, "did:") || rkey == "" || ref.CID == "":
		return invalid("the association ref %s#%s is not an at://<did>/<collection>/<rkey> uri with a cid", ref.URI, ref.CID)
	case collection != identlink.GrantNSID || rkey != subject:
		return invalid("the association ref %s is not the %s grant at the identity's DID", ref.URI, identlink.GrantNSID)
	case authority == subject:
		return invalid("the association record %s is in the identity's own repo", ref.URI)
	}
	record, err := a.records.GetRecordAt(ctx, recordclient.StrongRef{URI: ref.URI, CID: ref.CID})
	if errors.Is(err, recordclient.ErrRecordNotFound) {
		return invalid("the association record %s no longer resolves", ref.URI)
	}
	if err != nil {
		return refVerdict{}, err
	}
	if record.CID != ref.CID {
		return invalid("the association record %s has cid %s, not the pinned %s", ref.URI, record.CID, ref.CID)
	}
	walked, err := a.trust.Walk(ctx, subject, trust.Association{})
	if err != nil {
		return refVerdict{}, err
	}
	i := slices.IndexFunc(walked.Ancestors, func(an identlink.Ancestor) bool { return an.DID == authority })
	if i < 0 {
		if len(walked.Errors) > 0 {
			return refVerdict{}, errors.New(strings.Join(walked.Errors, "; "))
		}
		return invalid("the identlink walk of %s does not reach %s", subject, authority)
	}
	verdict := refVerdict{reason: ReasonAssociationRefVerified, authority: authority}
	if at := walked.Ancestors[i].ExpiresAt; at != nil {
		verdict.expiresAt = *at
	}
	if fields, ok := record.Value["contract"].(map[string]any); ok {
		uri, _ := fields["uri"].(string)
		cid, _ := fields["cid"].(string)
		verdict.contract = &commonv1.StrongRef{URI: uri, CID: cid}
	}
	return verdict, nil
}
