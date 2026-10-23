package marketnetwork

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

type operatorOf struct {
	app string
	did string
	at  string
}

func (ctl *Controller) declareOperatorVouches(ctx context.Context, r compose.Root, apps []v1alpha1.NetworkApp) {
	var bidderOps, gatewayOps []operatorOf
	var bidders, gateways []string
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
		case v1alpha1.ApplicationKindGateway:
			ws := rec.BaseWorkspace + ":" + compose.ComponentWorkspace
			gateways = append(gateways, ws)
			if op, ok := operator(ws, RequesterIdentity, app.Name); ok {
				gatewayOps = append(gatewayOps, op)
			}
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
}
