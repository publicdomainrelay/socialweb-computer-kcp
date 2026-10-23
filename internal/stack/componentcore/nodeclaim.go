package componentcore

import (
	"fmt"
	"log/slog"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	nodeclaimctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/nodeclaim"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

func newNodeClaims(handlers *component.HandlerRegistry, log *slog.Logger) *nodeclaimctl.Controller {
	return nodeclaimctl.New(nodeclaimctl.Options{Bus: handlers, Log: log})
}

func (c *Core) registerNodeClaims(mgr *ctlruntime.Manager, ctl *nodeclaimctl.Controller) error {
	restCfg, err := c.managerRestConfig()
	if err != nil {
		return fmt.Errorf("componentcore: the NodeClaim controller: %w", err)
	}
	return nodeclaimctl.Register(mgr, ctl, restCfg)
}
