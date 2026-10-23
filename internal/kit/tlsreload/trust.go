package tlsreload

import (
	"context"
	"log/slog"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

func TrustPool(ctx context.Context, trust vault.Trust, log *slog.Logger) (*Pool, func()) {
	p := &Pool{log: logOrDiscard(log)}
	if trust == nil {
		return p, func() {}
	}
	cancel := trust.Watch(func(bundle []byte) { p.feed(bundle) })
	if bundle, err := trust.CABundle(ctx); err != nil {
		p.log.Warn("no CA bundle is published yet; the pool fills when it is", "err", err)
	} else {
		p.feed(bundle)
	}
	return p, cancel
}

func (p *Pool) feed(bundle []byte) {
	if len(bundle) == 0 {
		return
	}
	if err := p.Set(bundle); err != nil {
		p.log.Warn("keeping the last CA pool", "err", err)
		return
	}
	p.log.Info("loaded the published CA pool")
}
