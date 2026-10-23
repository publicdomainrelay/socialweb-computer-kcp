package appctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/recordcontroller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

const (
	VouchNSID = "sh.tangled.graph.vouch"

	OperatorRepo = "operator"

	operatorVouchPrefix = "operator-vouch-"
)

type operatorOf struct {
	app string
	did string
	at  string
}

func (n *networks) declareOperatorVouches(ctx context.Context, apps []v1alpha1.NetworkApp) {
	var bidderOps, gatewayOps []operatorOf
	var bidders, gateways []string
	operator := func(ws, name, app string) (operatorOf, bool) {
		id, err := n.ctl.identityIn(ctx, ws, name)
		if err != nil || len(id.Spec.ApprovedAssociations) == 0 {
			return operatorOf{}, false
		}
		return operatorOf{app: app, did: id.Spec.ApprovedAssociations[0], at: id.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z")}, true
	}
	for _, app := range apps {
		if app.Stopped {
			continue
		}
		c, err := n.child(app)
		if err != nil {
			continue
		}
		var rec devinstance.Record
		if fsx.ReadJSON(c.layout.Record, &rec) != nil || rec.BaseWorkspace == "" {
			continue
		}
		switch app.Kind {
		case v1alpha1.ApplicationKindMarketBidder:
			bidders = append(bidders, rec.BaseWorkspace)
			if op, ok := operator(rec.BaseWorkspace, BidderIdentity, app.Name); ok {
				bidderOps = append(bidderOps, op)
			}
		case v1alpha1.ApplicationKindGateway:
			ws := rec.BaseWorkspace + ":" + ComponentWorkspace
			gateways = append(gateways, ws)
			if op, ok := operator(ws, RequesterIdentity, app.Name); ok {
				gatewayOps = append(gatewayOps, op)
			}
		}
	}
	declare := func(workspaces []string, ops []operatorOf, what string) {
		for _, ws := range workspaces {
			for _, op := range ops {
				if err := n.ctl.applyOperatorVouch(ctx, ws, op); err != nil {
					n.ctl.c.log().Warn(what, "workspace", ws, "app", op.app, "err", err.Error())
				}
			}
		}
	}
	declare(gateways, bidderOps, "declaring the gateway operator's vouch for a bidder's operator")
	declare(bidders, gatewayOps, "declaring the bidder operator's vouch for a gateway's operator")
}

func (ctl *Controller) applyOperatorVouch(ctx context.Context, ws string, op operatorOf) error {
	ri, err := ctl.client(ws, AtprotoRecordGVR)
	if err != nil {
		return err
	}
	record, err := json.Marshal(map[string]any{"$type": VouchNSID, "subject": op.did, "createdAt": op.at})
	if err != nil {
		return err
	}
	obj := v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: operatorVouchPrefix + op.app},
		Spec: v1alpha1.AtprotoRecordSpec{Repo: OperatorRepo, Collection: VouchNSID, Rkey: op.did, Record: record}}
	u, err := toUnstructured(&obj, "AtprotoRecord")
	if err != nil {
		return err
	}
	delete(u.Object, "status")
	if md, ok := u.Object["metadata"].(map[string]any); ok {
		delete(md, "creationTimestamp")
	}
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = ri.Patch(ctx, u.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

// ponytail: the operator's records are written and kept, never deleted when their object goes; the market network's PDS goes with the network, so add a finalizer when an operator repo outlives one.
func firstOwnerSession(sessionsFile string) (ownerSession, error) {
	b, err := os.ReadFile(sessionsFile)
	if err != nil {
		return ownerSession{}, err
	}
	var sessions map[string]ownerSession
	if err := json.Unmarshal(b, &sessions); err != nil {
		return ownerSession{}, fmt.Errorf("the owner sessions file %s: %w", sessionsFile, err)
	}
	owners := make([]string, 0, len(sessions))
	for did := range sessions {
		owners = append(owners, did)
	}
	if len(owners) == 0 {
		return ownerSession{}, fmt.Errorf("the owner sessions file %s holds no session", sessionsFile)
	}
	slices.Sort(owners)
	return sessions[owners[0]], nil
}

func (ctl *Controller) reconcileOperatorRecords(ctx context.Context, ws string, n *v1alpha1.MarketNetworkValue, login func() (ownerSession, error)) error {
	if n == nil {
		return nil
	}
	ri, err := ctl.client(ws, AtprotoRecordGVR)
	if err != nil {
		return err
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var records []v1alpha1.AtprotoRecord
	for i := range list.Items {
		var o v1alpha1.AtprotoRecord
		if err := fromUnstructured(&list.Items[i], &o); err != nil {
			return err
		}
		if o.Spec.Repo == OperatorRepo && o.DeletionTimestamp == nil {
			records = append(records, o)
		}
	}
	if len(records) == 0 {
		return nil
	}
	settled := true
	for i := range records {
		if records[i].Status.Phase != v1alpha1.AtprotoRecordPhaseApplied || records[i].Status.ObservedGeneration != records[i].Generation {
			settled = false
			break
		}
	}
	if settled {
		if last, ok := ctl.observedAt[ws]; ok && time.Since(last) < OperatorRecordObserveInterval {
			return nil
		}
	}
	observedAll := false
	defer func() {
		if observedAll {
			ctl.observedAt[ws] = time.Now()
			return
		}
		delete(ctl.observedAt, ws)
	}()
	s, err := login()
	if err != nil {
		return err
	}
	pds, err := ctl.pdsFor(s.Service, n.CABundle)
	if err != nil {
		return err
	}
	sess, err := pds.CreateSession(ctx, s.Identifier, s.Password)
	if err != nil {
		return err
	}
	operator := sess.DID
	rec, err := recordcontroller.New(recordcontroller.Options{Log: ctl.c.log()})
	if err != nil {
		return err
	}
	for _, o := range records {
		u, err := toUnstructured(&o, "AtprotoRecord")
		if err != nil {
			return err
		}
		if err := rec.ReconcileWithPDS(ctx, ri, u, pds, operator); err != nil {
			return fmt.Errorf("AtprotoRecord %s: %w", o.Name, err)
		}
	}
	observedAll = true
	return nil
}
