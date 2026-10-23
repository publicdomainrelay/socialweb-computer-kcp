package policy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/policyengine"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/policytrigger"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/policyworkflow"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/policyworkflowrun"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	pecontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/registry"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

type Options struct {
	Bus *bus.Bus

	Actions policytrigger.ActionsFunc

	PLCURL string

	DefaultRunTTL time.Duration

	Disabled bool

	Log *slog.Logger
}

func Register(m *ctlruntime.Manager, opts Options) error {
	if opts.Disabled {
		return nil
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	logf := func(level, message string, meta map[string]any) {
		attributes := make([]any, 0, len(meta)*2)
		for key, value := range meta {
			attributes = append(attributes, key, value)
		}
		switch level {
		case "error":
			log.Error(message, attributes...)
		case "warn":
			log.Warn(message, attributes...)
		case "debug":
			log.Debug(message, attributes...)
		default:
			log.Info(message, attributes...)
		}
	}
	denoPath, _ := deno.Resolve("")
	client := atproto.NewClient()
	if plcURL := strings.TrimSpace(opts.PLCURL); plcURL != "" {
		client.PLCURL = plcURL
	}
	host := trust.NewResolver(trust.ResolverOptions{Client: client, Log: logf})
	policies := registry.GhaLiteOnly(executor.NewGhaLite(executor.GhaLiteOptions{DenoPath: denoPath}))
	records := pecontroller.NewRecordResolver(func(ctx context.Context, ref commonv1.StrongRef) (map[string]any, error) {
		return host.Resolve(ctx, atproto.StrongRef{URI: ref.URI, CID: ref.CID})
	}, policies)
	engines := pecontroller.NewEngineManager(pecontroller.NewDefaultEngineFactory(pecontroller.Config{DenoPath: denoPath, Host: host, Log: logf}))
	if _, err := policyengine.Register(m, policyengine.Options{Bus: opts.Bus, Engines: engines, Log: log}); err != nil {
		return err
	}
	if _, err := policyworkflow.Register(m, policyworkflow.Options{Workflows: &pecontroller.WorkflowReconciler{
		Actions: executor.DefaultActionResolver(), Records: records}}); err != nil {
		return err
	}
	if _, err := policyworkflowrun.Register(m, policyworkflowrun.Options{Engines: engines, DefaultRunTTL: opts.DefaultRunTTL, Log: log,
		Runs: &pecontroller.RunReconciler{Records: records, Host: host}}); err != nil {
		return err
	}
	_, err := policytrigger.Register(m, policytrigger.Options{Actions: opts.Actions, Log: log})
	return err
}
