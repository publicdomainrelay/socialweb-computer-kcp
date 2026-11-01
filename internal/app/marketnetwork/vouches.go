package marketnetwork

import (
	"context"
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

const (
	NetworkVouchLabel = "apps.socialweb.computer/network-vouch"

	networkVouchLinkSet = "network-vouches"

	bidderVouchPrefix = "network-vouch-"
)

func requesterVouchApp(requesterDid string) string {
	return "requester-" + compose.Stamp(requesterDid)
}

func RequesterVouchName(requesterDid string) string {
	return compose.OperatorVouchPrefix + requesterVouchApp(requesterDid)
}

func BidderVouchName(bidderApp string) string {
	return bidderVouchPrefix + bidderApp
}

type operatorOf struct {
	app string
	did string
	at  string
}

type networkRequester struct {
	ws  string
	did string
	at  string
}

type networkBidder struct {
	app string
	ws  string
	did string
}

func (ctl *Controller) declareOperatorVouches(ctx context.Context, r compose.Root, apps []v1alpha1.NetworkApp) {
	var bidderOps, gatewayOps []operatorOf
	var bidders, gateways []string
	var bidderApps []networkBidder
	var requesters []networkRequester
	operator := func(ws, name, app string) (operatorOf, bool) {
		id, err := ctl.c.Identity(ctx, ws, name)
		if err != nil {
			return operatorOf{}, false
		}
		did := compose.OperatorDID(nil, id)
		if did == "" {
			return operatorOf{}, false
		}
		return operatorOf{app: app, did: did, at: id.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z")}, true
	}
	for _, app := range apps {
		if app.Stopped {
			continue
		}
		ch, err := ctl.child(app)
		if err != nil {
			continue
		}
		var rec devinstance.Record
		if fsx.ReadJSON(ch.layout.Record, &rec) != nil || rec.BaseWorkspace == "" {
			continue
		}
		switch app.Kind {
		case v1alpha1.ApplicationKindMarketBidder:
			bidders = append(bidders, rec.BaseWorkspace)
			if op, ok := operator(rec.BaseWorkspace, BidderIdentity, app.Name); ok {
				bidderOps = append(bidderOps, op)
			}
			if did := strings.TrimSpace(ctl.c.IdentityDid(ctx, rec.BaseWorkspace, BidderIdentity)); did != "" {
				bidderApps = append(bidderApps, networkBidder{app: app.Name, ws: rec.BaseWorkspace, did: did})
			}
		case v1alpha1.ApplicationKindGateway:
			ws := rec.BaseWorkspace + ":" + compose.ComponentWorkspace
			gateways = append(gateways, ws)
			if op, ok := operator(ws, RequesterIdentity, app.Name); ok {
				gatewayOps = append(gatewayOps, op)
			}
		case v1alpha1.ApplicationKindMarketRequester:
			id, err := ctl.c.Identity(ctx, rec.BaseWorkspace, RequesterIdentity)
			if err != nil || strings.TrimSpace(id.Status.Did) == "" {
				continue
			}
			requesters = append(requesters, networkRequester{ws: rec.BaseWorkspace, did: strings.TrimSpace(id.Status.Did),
				at: id.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z")})
		}
	}
	declare := func(workspaces []string, ops []operatorOf, what string) {
		for _, ws := range workspaces {
			for _, op := range ops {
				if err := ctl.c.ApplyOperatorVouch(ctx, r, ws, op.app, op.did, op.at); err != nil {
					ctl.log().Warn(what, "workspace", ws, "app", op.app, "err", err.Error())
				}
			}
		}
	}
	declare(gateways, bidderOps, "declaring the gateway operator's vouch for a bidder's operator")
	declare(bidders, gatewayOps, "declaring the bidder operator's vouch for a gateway's operator")
	for _, req := range requesters {
		for _, b := range bidderApps {
			if err := ctl.c.ApplyOperatorVouch(ctx, r, b.ws, requesterVouchApp(req.did), req.did, req.at); err != nil {
				ctl.log().Warn("declaring the bidder operator's vouch for a network requester", "workspace", b.ws, "requester", req.did, "err", err.Error())
			}
		}
		if err := ctl.applyRequesterVouches(ctx, r, req, bidderApps); err != nil {
			ctl.log().Warn("declaring a network requester's vouches for the network bidders", "workspace", req.ws, "requester", req.did, "err", err.Error())
		}
	}
}

func (ctl *Controller) applyRequesterVouches(ctx context.Context, r compose.Root, req networkRequester, bidders []networkBidder) error {
	if len(bidders) == 0 {
		return nil
	}
	repo := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: compose.KindAtprotoRepo, Name: atprotorepo.RepoName(req.did), Workspace: req.ws}
	records := exposurev1alpha1.ComponentLinkSelector{Group: v1alpha1.GroupName, Kind: "AtprotoRecord", Workspace: req.ws,
		MatchLabels: map[string]string{NetworkVouchLabel: "true"}}
	link := compose.SelectorLink(compose.LinkName("network-vouches", req.ws), records, repo, compose.Cap(atprotorepo.CapabilityWrite, compose.VouchNSID))
	if err := ctl.c.AddLinks(ctx, r, networkVouchLinkSet, []compose.Link{link}); err != nil {
		return err
	}
	for _, b := range bidders {
		record, err := json.Marshal(map[string]any{"$type": compose.VouchNSID, "subject": b.did, "createdAt": req.at})
		if err != nil {
			return err
		}
		repoRef := repo
		repoRef.Workspace = ""
		obj := &v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: BidderVouchName(b.app), Labels: map[string]string{NetworkVouchLabel: "true"}},
			Spec: v1alpha1.AtprotoRecordSpec{RepoRef: &repoRef, Collection: compose.VouchNSID, Rkey: b.did, Record: record,
				DeletionPolicy: v1alpha1.RecordDeletionRetain}}
		if err := ctl.c.ApplyTyped(ctx, r, req.ws, compose.AtprotoRecordGVR, v1alpha1.GroupVersion.String(), "AtprotoRecord", obj); err != nil {
			return err
		}
	}
	return nil
}
