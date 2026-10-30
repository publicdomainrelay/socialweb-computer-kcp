package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"k8s.io/apimachinery/pkg/api/meta"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const maxAssociationRefFile = 4 << 10

func associationRefOf(value string) (*commonv1.StrongRef, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "", "false":
		return nil, nil
	case "true":
		value = cloudinit.DefaultAssociationRefFile
	}
	if strings.HasPrefix(value, "/") {
		raw, err := os.ReadFile(value)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxAssociationRefFile {
			return nil, fmt.Errorf("%s holds more than %d bytes", value, maxAssociationRefFile)
		}
		value = strings.TrimSpace(string(raw))
	}
	uri, cid, ok := strings.Cut(value, "#")
	if !ok {
		return nil, fmt.Errorf("%q: want at://<authority>/<collection>/<rkey>#<cid>", value)
	}
	ref, err := market.ParseRecordRef(strings.TrimSpace(uri), strings.TrimSpace(cid))
	if err != nil {
		return nil, err
	}
	return &commonv1.StrongRef{URI: ref.URI, CID: ref.CID}, nil
}

func (r *requestRun) associationRef(network bool) error {
	ref, err := associationRefOf(r.vals.String("association-ref"))
	if err != nil || ref == nil {
		return err
	}
	switch {
	case network:
		return errors.New("it is for a standalone invocation; a market network composes its child's identity")
	case r.root.Identity.Association == v1alpha1.AssociationSkip:
		return errors.New("it needs association RequireApproval; drop -- --skip-qr")
	}
	r.root.Identity.Association = v1alpha1.AssociationRequireApproval
	r.root.Identity.AssociationRef = ref
	r.log("the requester identity bootstraps its association from a pinned record", "uri", ref.URI, "cid", ref.CID)
	return nil
}

func associationRefRefused(obj *v1alpha1.MarketIdentity) error {
	if obj == nil || obj.Spec.AssociationRef == nil {
		return nil
	}
	c := meta.FindStatusCondition(obj.Status.Conditions, v1alpha1.ConditionAssociated)
	if c == nil || c.ObservedGeneration != obj.Generation || c.Reason != atproto.ReasonAssociationRefInvalid {
		return nil
	}
	return fmt.Errorf("the association record is refused: %s", c.Message)
}

func associationRefHint(obj *v1alpha1.MarketIdentity, did string) string {
	ref := obj.Spec.AssociationRef
	state := "no Associated condition yet"
	if c := meta.FindStatusCondition(obj.Status.Conditions, v1alpha1.ConditionAssociated); c != nil {
		state = c.Reason + ": " + c.Message
	}
	return strings.Join([]string{
		"The requester's DID " + did + " bootstraps its association from " + ref.URI + " (cid " + ref.CID + ").",
		"The record must be in the authority's repo and name " + did + " for requester_associate; " + state,
	}, "\n")
}

func associateOf(value string) (*v1alpha1.ComputeContractAssociation, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	did, err := syntax.ParseDID(value)
	if err != nil || did.String() != value {
		return nil, fmt.Errorf("%q is not a DID", value)
	}
	return &v1alpha1.ComputeContractAssociation{Subject: value, RefFile: cloudinit.DefaultAssociationRefFile}, nil
}

func associationFor(subject string) func(*v1alpha1.ComputeContract) bool {
	rkey := "/" + market.AssociationNSID + "/" + trust.ClaimRKey(trust.RequesterAssociate, subject)
	return func(c *v1alpha1.ComputeContract) bool {
		if c.Status.Phase == v1alpha1.ComputeContractReleased || c.Status.Phase == v1alpha1.ComputeContractFailed {
			return true
		}
		ref := c.Status.Association
		return ref != nil && strings.HasSuffix(ref.URI, rkey) && ref.CID != ""
	}
}

func runRequestAssociate(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	association, err := associateOf(vals.String("associate"))
	if err != nil {
		return a.fail(exitRefused, "run request associate: --associate: %v", err)
	}
	if association == nil {
		return a.fail(exitRefused, "run request associate: --associate <did> names the subject requester's repo DID")
	}
	r, started, code := a.openRequest(ctx, vals, rest, "associate")
	if r == nil {
		return code
	}
	if started {
		defer r.stop(context.WithoutCancel(ctx), false)
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		return a.fail(exitFailed, "run request associate: %v", err)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run request associate: the contract is %s, not Held: %s", c.Status.Phase, c.Status.Message)
	}
	if err := r.kcp.Associate(ctx, association.Subject); err != nil {
		return a.fail(exitFailed, "run request associate: setting spec.association: %v", err)
	}
	r.log("spec.association is set; the requester writes the contract-scoped market.association record", "subject", association.Subject)
	c, err = r.kcp.WaitContract(ctx, r.contractTimeout, associationFor(association.Subject), r.progress)
	if err != nil {
		return a.fail(exitFailed, "run request associate: %v", err)
	}
	if !associationFor(association.Subject)(c) || c.Status.Association == nil {
		return a.fail(exitFailed, "run request associate: the contract is %s before the association record was written: %s", c.Status.Phase, c.Status.Message)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run request associate: the contract is %s: %s", c.Status.Phase, c.Status.Message)
	}
	fmt.Fprintln(a.stdout, c.Status.Association.URI+"#"+c.Status.Association.CID)
	return exitOK
}
