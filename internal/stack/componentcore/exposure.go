package componentcore

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
)

const exposureRescan = 30 * time.Second

func (c *Core) runExposure(ctx context.Context) {
	log := c.cfg.Log.With("component", "exposure-controller")
	seen := map[string]bool{}
	for ctx.Err() == nil {
		for _, workspace := range c.exposureWorkspaces(ctx) {
			workspace = strings.TrimSpace(workspace)
			if workspace == "" || seen[workspace] {
				continue
			}
			seen[workspace] = true
			go c.runWorkspaceExposure(ctx, workspace, log.With("workspace", workspace))
		}
		sleep(ctx, exposureRescan)
	}
}

func (c *Core) exposureWorkspaces(ctx context.Context) []string {
	if c.cfg.WorkspacesFunc != nil {
		return c.cfg.WorkspacesFunc(ctx)
	}
	return c.cfg.Workspaces
}

func (c *Core) runWorkspaceExposure(ctx context.Context, workspace string, log *slog.Logger) {
	common := exposure.ExposureOptions{
		Workspace: workspace,
		Dynamic:   exposure.DynamicFactory(c.cfg.DynamicFactory),
		Handlers:  c.handlers,
		Registry:  c.registry,
		Ports:     c.ports,
		TLS:       c.deps.TLS,
		Registrar: c.ingressRegistrar,
		Log:       log,
	}
	exposures, err := exposure.NewExposureController(common)
	if err != nil {
		log.Warn("the Exposure controller does not start", "err", err.Error())
		return
	}
	services, err := exposure.NewServiceController(exposure.ServiceOptions{
		Workspace: workspace,
		Dynamic:   exposure.DynamicFactory(c.cfg.DynamicFactory),
		Handlers:  c.handlers,
		Registry:  c.registry,
		Log:       log,
	})
	if err != nil {
		log.Warn("the Service controller does not start", "err", err.Error())
		return
	}
	go func() {
		if err := services.Run(ctx); err != nil && ctx.Err() == nil {
			log.Warn("the Service controller stopped", "err", err.Error())
		}
	}()
	log.Info("the Service and Exposure controllers reconcile in process")
	if err := exposures.Run(ctx); err != nil && ctx.Err() == nil {
		log.Warn("the Exposure controller stopped", "err", err.Error())
	}
}
