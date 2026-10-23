package openbao

import (
	"context"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

func (p *Plane) issuerLoop(ctx context.Context) {
	configured := ""
	waiting := false
	for {
		var retry <-chan time.Time
		issuer := p.IssuerURL()
		switch {
		case issuer == "":
			if !waiting {
				p.log.Info("the guest OIDC issuer waits for the guest control plane ingress exposure URL")
				waiting = true
			}
		case issuer != configured:
			waiting = false
			attempt, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
			err := p.identity.ConfigureIssuer(attempt, issuer)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				configured = issuer
				p.log.Info("the guest OIDC issuer is configured", "issuer", guestidentity.OIDCIssuer(issuer), "issuerUrl", guestidentity.OIDCIssuer(issuer))
			} else {
				p.log.Warn("configuring the guest OIDC issuer failed; retrying", "err", err)
				retry = p.clock.After(p.cfg.IssuerRetry)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-p.issuerKick:
		case <-retry:
		}
	}
}
