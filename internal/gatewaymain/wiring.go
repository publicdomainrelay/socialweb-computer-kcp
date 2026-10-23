package gatewaymain

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/plcsweep"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/xrpcserver"
)

type Deps struct {
	Store gateway.Store
	Deny  revocation.Deny

	Feed gateway.Feed

	Directory identity.Directory

	Revoker gateway.Revoker

	Certs gateway.CertRevoker

	Tenant gateway.TenantAPI

	Sweep *plcsweep.Sweeper

	SweepInterval time.Duration
}

type Gateway struct {
	handler       http.Handler
	surface       *xrpcserver.Server
	sweep         *plcsweep.Sweeper
	sweepInterval time.Duration
	log           *slog.Logger
}

func Build(ctx context.Context, cfg Config, d Deps, log *slog.Logger) (*Gateway, error) {
	if log == nil {
		log = slog.Default()
	}

	verbs := &gateway.Server{
		Deny: d.Deny, Store: d.Store, Revoker: d.Revoker, Certs: d.Certs, Feed: d.Feed,
		Tenant: d.Tenant,
	}

	surface, err := xrpcserver.New(ctx, xrpcserver.Options{
		Gateway:   verbs,
		Audiences: cfg.Audiences,
		Directory: d.Directory,
	})
	if err != nil {
		return nil, err
	}

	interval := d.SweepInterval
	interval = cmp.Or(interval, plcsweep.DefaultInterval)
	g := &Gateway{
		handler:       surface.Handler(),
		surface:       surface,
		sweep:         d.Sweep,
		sweepInterval: interval,
		log:           log,
	}
	g.report(d)
	return g, nil
}

func (g *Gateway) Handler() http.Handler { return g.handler }

func (g *Gateway) Close() { g.surface.Close() }

func (g *Gateway) Run(ctx context.Context) error {
	if g.sweep == nil {
		<-ctx.Done()
		return nil
	}
	err := g.sweep.Run(ctx, g.sweepInterval, g.onPass)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (g *Gateway) onPass(outcomes []plcsweep.Outcome) {
	for _, o := range outcomes {
		switch {
		case o.Skipped:
			g.log.Warn("plc sweep: did not resolve", "did", o.DID, "why", o.Why)
		case o.Revoke:
			g.log.Info("plc sweep: deny written", "did", o.DID, "why", o.Why)
		default:
			g.log.Debug("plc sweep: live", "did", o.DID, "why", o.Why)
		}
	}
}

func (g *Gateway) report(d Deps) {
	if g.sweep == nil {
		g.log.Warn("the PLC sweep is UNWIRED",
			"missing", "Deps.Sweep: plcsweep.Worklist, plcsweep.Resolver and plcsweep.Revoker are all implemented, and main.buildDeps builds and wires all three. This warning means the composition root skipped that construction, so g.Run -- the only caller of sweep.Run -- has nothing to run",
			"reached by", "a caller that supplies a nil Deps.Sweep, which today is the wiring smoke tests ONLY",
			"consequence", "an account deleted at PLC between logins is not noticed by this process")
	}
	if d.Revoker == nil {
		g.log.Warn("revokeIdentity is routed but UNWRITABLE",
			"missing", "gateway.Revoker -- the deny's write half has no implementation",
			"consequence", "revokeIdentity answers 503 ClusterUnavailable")
	}
}
