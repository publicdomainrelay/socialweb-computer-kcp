package app

import (
	"context"
	"errors"
	"time"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketbidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketnetwork"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/sshgateway"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const manifestRetry = 2 * time.Second

type Config struct {
	Compose compose.Config

	WebApp *gateway.WebApp

	SignInGuard marketbidder.SignInGuard
}

func Register(ctx context.Context, m *ctlruntime.Manager, cfg Config) error {
	if err := m.AddToScheme(v1alpha1.AddToScheme, exposurev1alpha1.AddToScheme, eventsv1alpha1.AddToScheme,
		policyv1alpha1.AddToScheme, pdsv1alpha1.AddToScheme); err != nil {
		return err
	}
	c, err := compose.New(cfg.Compose)
	if err != nil {
		return err
	}
	go applyManifests(ctx, c)
	return errors.Join(
		marketnetwork.Register(m, c),
		marketbidder.Register(m, c, cfg.SignInGuard),
		marketrequester.Register(m, c),
		gateway.Register(m, c, cfg.WebApp),
		sshgateway.Register(m, c),
	)
}

func applyManifests(ctx context.Context, c *compose.Composer) {
	for failures := 0; ; failures++ {
		err := compose.ApplySet(ctx, c)
		if err == nil {
			c.Log().Info("the application roots reconcile this instance's root objects", "workspace", c.Workspace())
			return
		}
		if ctx.Err() != nil {
			return
		}
		retryIn := ctlruntime.BackoffDelay(manifestRetry, ctlruntime.DefaultBackoffMax, failures)
		c.Log().Warn("applying the applications set; retrying with backoff", "failures", failures+1, "retryIn", retryIn.String(), "err", err.Error())
		timer := time.NewTimer(retryIn)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
