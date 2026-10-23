package componentcore

import (
	"context"

	computevmctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/computevm"
)

func (c *Core) buildCompute() {
	if c.cfg.Compute == nil {
		return
	}
	cfg := *c.cfg.Compute
	c.runners = append(c.runners, controllerRunner{name: "computevm", run: func(ctx context.Context) error { return computevmctl.Run(ctx, cfg) }})
}
