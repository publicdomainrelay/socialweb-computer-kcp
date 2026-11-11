package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	PeerNSID = "computer.socialweb.peer"

	PeerRecordLabel = "socialweb.computer/peer"

	PeerRecordName = "peer"

	ConditionRetired = "Retired"
)

var colocatedNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type Colocated struct {
	Name string

	Account LocalAccount
}

func CheckColocated(names []string) error {
	for _, name := range names {
		if !colocatedNameRE.MatchString(name) {
			return fmt.Errorf("compose: the co-located account %q is not a DNS label", name)
		}
	}
	return nil
}

func ColocatedHandle(name, pdsHandle string) string {
	return name + "." + pdsHandle
}

func ColocatedSessionPath(name string) string {
	return PdsSessionPath("colocated-" + name)
}

func PeerRKey(subhandle, handle string) string {
	if subhandle = strings.TrimSpace(subhandle); subhandle != "" {
		return subhandle
	}
	return handle
}

func colocatedRecordName(name, part string) string {
	return ObjectName("colocated", name+"-"+part)
}

func (c *Composer) applyPeerRecord(ctx context.Context, r Root, ws, name string, repo commonv1.ObjectRef, collection, rkey string, record map[string]any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	obj := &v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{PeerRecordLabel: r.Owner()}},
		Spec: v1alpha1.AtprotoRecordSpec{RepoRef: &repo, Collection: collection, Rkey: rkey, Record: raw, OnMissingReference: v1alpha1.MissingReferenceDelete}}
	return c.ApplyTyped(ctx, r, ws, AtprotoRecordGVR, v1alpha1.GroupVersion.String(), "AtprotoRecord", obj)
}

func identityField(identity, path string) map[string]any {
	return map[string]any{"valueFrom": map[string]any{"kind": KindMarketIdentity, "name": identity, "fieldPath": path}}
}

func recordField(name, path string) map[string]any {
	return map[string]any{"valueFrom": map[string]any{"kind": "AtprotoRecord", "name": name, "fieldPath": path}}
}

func (c *Composer) recordApplied(ctx context.Context, ws, name string) (exists, applied bool, err error) {
	rec, err := Get[v1alpha1.AtprotoRecord](ctx, c, ws, AtprotoRecordGVR, name)
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, rec.Status.Phase == v1alpha1.AtprotoRecordPhaseApplied && rec.Status.ObservedGeneration == rec.Generation && rec.Status.Cid != "", nil
}

func (c *Composer) ApplyPeerRecords(ctx context.Context, r Root, ws, identity string, id *v1alpha1.MarketIdentity, self LocalAccount,
	subhandle, iroh string, colocated []Colocated) error {
	keep := map[string]bool{}
	if id.Status.AssociationRef == nil {
		return c.Prune(ctx, ws, AtprotoRecordGVR, PeerRecordLabel+"="+r.Owner(), keep)
	}
	scope := id.Status.AssociationScope
	ownRKey := PeerRKey(subhandle, self.Handle)
	created := identityField(identity, "metadata.creationTimestamp")
	peerRecord := map[string]any{"$type": PeerNSID, "createdAt": created}
	if iroh != "" {
		peerRecord["iroh"] = iroh
	}
	keep[PeerRecordName] = true
	if err := c.applyPeerRecord(ctx, r, ws, PeerRecordName, RepoRefIn(self.DID, ws), PeerNSID, ownRKey, peerRecord); err != nil {
		return err
	}
	for _, other := range colocated {
		agent := other.Account.DID
		grantName, endorseName, peerName := colocatedRecordName(other.Name, "grant"), colocatedRecordName(other.Name, "endorsement"), colocatedRecordName(other.Name, "peer")
		grant := market.GrantRecord(agent, created)
		if scope != nil {
			grant = market.ContractGrantRecord(agent, map[string]any{"uri": scope.Contract.URI, "cid": scope.Contract.CID}, scope.ExpiresAt, created)
		}
		keep[grantName] = true
		if err := c.applyPeerRecord(ctx, r, ws, grantName, RepoRefIn(self.DID, ws), market.AssociationNSID, agent, grant); err != nil {
			return err
		}
		endorsed, endorseApplied, err := c.recordApplied(ctx, ws, endorseName)
		if err != nil {
			return err
		}
		if _, grantApplied, err := c.recordApplied(ctx, ws, grantName); err != nil {
			return err
		} else if !grantApplied && !endorsed {
			continue
		}
		keep[endorseName] = true
		if err := c.applyPeerRecord(ctx, r, ws, endorseName, RepoRefIn(self.DID, ws), market.EndorsementNSID, agent, market.EndorsementRecord(agent)); err != nil {
			return err
		}
		peered, _, err := c.recordApplied(ctx, ws, peerName)
		if err != nil {
			return err
		}
		if !endorseApplied && !peered {
			continue
		}
		keep[peerName] = true
		if err := c.applyPeerRecord(ctx, r, ws, peerName, RepoRefIn(agent, ws), PeerNSID, other.Name+"."+ownRKey, peerRecord); err != nil {
			return err
		}
	}
	return c.Prune(ctx, ws, AtprotoRecordGVR, PeerRecordLabel+"="+r.Owner(), keep)
}

func (c *Composer) RetireLocalAccounts(ctx context.Context, r Root, ws string, dids []string) (bool, error) {
	ri, err := c.Client(ws, AtprotoRecordGVR)
	if err != nil {
		return false, err
	}
	if err := c.Prune(ctx, ws, AtprotoRecordGVR, PeerRecordLabel+"="+r.Owner(), map[string]bool{}); err != nil {
		return false, err
	}
	left, err := kube.List[metav1.PartialObjectMetadata](ctx, ri, metav1.ListOptions{LabelSelector: PeerRecordLabel + "=" + r.Owner()})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if len(left) > 0 {
		return false, nil
	}
	accounts, err := bus.Resolve[pds.Accounts](ctx, c.cfg.Bus, r.Ref(), AppRef(ComponentKindPds, PdsComponent, ws),
		bus.Capability{Name: pds.CapabilityAccount, Scope: []string{ScopeAll}})
	if err != nil {
		return false, err
	}
	var errs []error
	for _, did := range dids {
		if err := accounts.Retire(ctx, did); err != nil && !errors.Is(err, pds.ErrNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", did, err))
			continue
		}
		c.log.Info("local_account_retired", "did", did, "plc", "tombstoned", "account", "deactivated")
	}
	return true, errors.Join(errs...)
}
