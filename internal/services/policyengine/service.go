package policyengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/cache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/evaluator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/registry"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "PolicyEngine"

type Options struct {
	Store *kcpstore.Store

	Cluster string

	PollInterval time.Duration

	DefaultRunTTL time.Duration

	DenoPath string

	PLCURL string

	Limits controller.Limits

	VerdictTTL time.Duration

	ScopeTTL time.Duration

	NegativeScopeTTL time.Duration

	RecordTTL time.Duration

	TrustTTL time.Duration

	Host trust.Host

	Resolve func(ctx context.Context, ref atproto.StrongRef) (map[string]any, error)

	Records controller.RecordSource

	Handlers map[string]controller.ActionHandler

	MaxConcurrentEvaluations int

	Log *slog.Logger
}

type Service struct {
	options Options

	log *slog.Logger

	controller *controller.Controller

	evaluator *evaluator.Evaluator

	denoErr error

	mu sync.Mutex

	started bool

	ready bool

	cancel context.CancelFunc

	done chan struct{}
}

func New(options Options) (*Service, error) {
	if options.Store == nil {
		return nil, errors.New("policy engine service: a kcpstore store is required")
	}
	if options.Cluster == "" {
		options.Cluster = "root"
	}
	if options.Log == nil {
		options.Log = slog.New(slog.DiscardHandler)
	}
	logf := func(level, message string, meta map[string]any) {
		attributes := make([]any, 0, len(meta)*2)
		for key, value := range meta {
			attributes = append(attributes, key, value)
		}
		switch level {
		case "error":
			options.Log.Error(message, attributes...)
		case "warn":
			options.Log.Warn(message, attributes...)
		case "debug":
			options.Log.Debug(message, attributes...)
		default:
			options.Log.Info(message, attributes...)
		}
	}
	denoPath := options.DenoPath
	var denoErr error
	if denoPath == "" {
		denoPath, denoErr = deno.Resolve("")
	}
	if options.Host == nil && options.Resolve == nil {
		client := atproto.NewClient()
		if plcURL := strings.TrimSpace(options.PLCURL); plcURL != "" {
			client.PLCURL = plcURL
		}
		options.Host = trust.NewResolver(trust.ResolverOptions{Client: client, Log: logf})
	}
	denoOptions := executor.GhaLiteOptions{
		DenoPath:        denoPath,
		MaxConsoleBytes: options.Limits.MaxConsoleBytes,
		MaxOutputBytes:  options.Limits.MaxOutputBytes,
	}
	policyRegistry := registry.GhaLiteOnly(executor.NewGhaLite(denoOptions))
	resolve := options.Resolve
	if resolve == nil && options.Host != nil {
		resolve = options.Host.Resolve
	}
	records := options.Records
	if records == nil && resolve != nil {
		records = controller.NewRecordResolver(func(ctx context.Context, ref commonv1.StrongRef) (map[string]any, error) {
			return resolve(ctx, atproto.StrongRef{URI: ref.URI, CID: ref.CID})
		}, policyRegistry)
	}
	resources := controller.NewResources(options.Store, options.Cluster)
	base := controller.Config{
		Limits:           options.Limits,
		DenoPath:         denoPath,
		Host:             options.Host,
		VerdictTTL:       options.VerdictTTL,
		ScopeTTL:         options.ScopeTTL,
		NegativeScopeTTL: options.NegativeScopeTTL,
		RecordTTL:        options.RecordTTL,
		TrustTTL:         options.TrustTTL,
		Log:              logf,
	}
	handlers := make(map[string]controller.ActionHandler, len(options.Handlers))
	maps.Copy(handlers, options.Handlers)
	runs := &controller.RunReconciler{
		Workflows: resources.WorkflowSource(),
		Records:   records,
		Host:      options.Host,
	}
	workflows := &controller.WorkflowReconciler{
		Actions: executor.DefaultActionResolver(),
		Records: records,
	}
	triggers := &controller.TriggerReconciler{Handlers: handlers}
	ctl, err := controller.NewController(controller.ControllerConfig{
		Resources:     resources,
		Manager:       controller.NewEngineManager(controller.NewDefaultEngineFactory(base)),
		Workflows:     workflows,
		Runs:          runs,
		Triggers:      triggers,
		Log:           options.Log,
		PollInterval:  options.PollInterval,
		DefaultRunTTL: options.DefaultRunTTL,
	})
	if err != nil {
		return nil, err
	}
	scopeTTL := options.ScopeTTL
	if scopeTTL <= 0 {
		scopeTTL = 5 * time.Minute
	}
	negativeScopeTTL := options.NegativeScopeTTL
	if negativeScopeTTL <= 0 {
		negativeScopeTTL = 30 * time.Second
	}
	verdictTTL := options.VerdictTTL
	if verdictTTL <= 0 {
		verdictTTL = 30 * time.Second
	}
	scopeCache := evaluator.NewScopeCache(scopeTTL, negativeScopeTTL, 10000, logf)
	ev := evaluator.New(evaluator.Options{
		Registry:      policyRegistry,
		Resolve:       resolve,
		Host:          options.Host,
		ScopeCache:    scopeCache,
		VerdictStore:  cache.NewStore(1024, 16<<20),
		VerdictTTL:    verdictTTL,
		MaxConcurrent: options.MaxConcurrentEvaluations,
		Log:           logf,
	})
	return &Service{
		options:    options,
		log:        options.Log,
		controller: ctl,
		evaluator:  ev,
		denoErr:    denoErr,
	}, nil
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Options, error) {
	return configFrom(deps, spec, Options{})
}

func configFrom(deps component.Deps, spec component.ComponentSpec, options Options) (Options, error) {
	var decoded policyv1alpha1.PolicyEngineSpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Options{}, fmt.Errorf("policy engine service: decoding the component spec: %w", err)
	}
	cfg := options
	if cfg.Log == nil {
		cfg.Log = deps.Log
	}
	if decoded.Limits.MaxConcurrentRuns > 0 {
		cfg.Limits.MaxConcurrentRuns = int(decoded.Limits.MaxConcurrentRuns)
	}
	if decoded.Limits.QueueDepth > 0 {
		cfg.Limits.QueueDepth = int(decoded.Limits.QueueDepth)
	}
	if decoded.Limits.ActiveDeadlineSeconds != nil && *decoded.Limits.ActiveDeadlineSeconds > 0 {
		cfg.Limits.ActiveDeadline = time.Duration(*decoded.Limits.ActiveDeadlineSeconds) * time.Second
	}
	if decoded.Limits.MaxConsoleBytes > 0 {
		cfg.Limits.MaxConsoleBytes = decoded.Limits.MaxConsoleBytes
	}
	if decoded.Limits.MaxOutputBytes > 0 {
		cfg.Limits.MaxOutputBytes = decoded.Limits.MaxOutputBytes
	}
	if decoded.Limits.MaxCacheEntries > 0 {
		cfg.Limits.MaxCacheEntries = int(decoded.Limits.MaxCacheEntries)
	}
	if decoded.Limits.MaxCacheBytes > 0 {
		cfg.Limits.MaxCacheBytes = decoded.Limits.MaxCacheBytes
	}
	if decoded.Trust.TTLSeconds != nil && *decoded.Trust.TTLSeconds > 0 {
		cfg.TrustTTL = time.Duration(*decoded.Trust.TTLSeconds) * time.Second
	}
	if decoded.Cache.VerdictTTLSeconds != nil && *decoded.Cache.VerdictTTLSeconds > 0 {
		cfg.VerdictTTL = time.Duration(*decoded.Cache.VerdictTTLSeconds) * time.Second
	}
	if decoded.Cache.ScopeTTLSeconds != nil && *decoded.Cache.ScopeTTLSeconds > 0 {
		cfg.ScopeTTL = time.Duration(*decoded.Cache.ScopeTTLSeconds) * time.Second
	}
	if decoded.Cache.RecordTTLSeconds != nil && *decoded.Cache.RecordTTLSeconds > 0 {
		cfg.RecordTTL = time.Duration(*decoded.Cache.RecordTTLSeconds) * time.Second
	}
	return cfg, nil
}

func Factory(options Options) component.ServiceFactory {
	return func(ctx context.Context, deps component.Deps, spec component.ComponentSpec) (component.Service, error) {
		config, err := configFrom(deps, spec, options)
		if err != nil {
			return nil, err
		}
		workspace := spec.Workspace
		if workspace == "" {
			workspace = config.Cluster
		}
		if config.Store == nil {
			if deps.KCP == nil {
				return nil, errors.New("policy engine service: kcp clients are not configured")
			}
			client, err := deps.KCP.Client(workspace)
			if err != nil {
				return nil, fmt.Errorf("policy engine service: resolving the kcp client: %w", err)
			}
			restConfig := client.RESTConfig()
			if restConfig == nil {
				return nil, errors.New("policy engine service: the kcp client has no rest config")
			}
			store, err := kcpstore.New(kcpstore.Options{Host: restConfig.Host, RestConfig: restConfig})
			if err != nil {
				return nil, fmt.Errorf("policy engine service: opening the kcp store: %w", err)
			}
			config.Store = store
		}
		if config.Cluster == "" {
			config.Cluster = workspace
		}
		return New(config)
	}
}

func Register(runtime *component.Runtime, options Options) error {
	return runtime.Register(Kind, Factory(options))
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("policy engine service: already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	s.started = true
	s.ready = s.denoErr == nil
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.started = false
		s.ready = false
		s.cancel = nil
		s.done = nil
		s.mu.Unlock()
		close(done)
	}()
	return s.controller.Run(runCtx)
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return errors.New("policy engine service: not started")
	}
	if s.denoErr != nil {
		return s.denoErr
	}
	if !s.ready {
		return errors.New("policy engine service: the controller is starting")
	}
	return nil
}

func (s *Service) Endpoints() []component.Endpoint {
	return nil
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) Controller() *controller.Controller {
	return s.controller
}

func (s *Service) Evaluator() *evaluator.Evaluator {
	return s.evaluator
}

func (s *Service) Engine(name string) (*controller.Engine, bool) {
	if s.controller == nil {
		return nil, false
	}
	engine, ok := s.controller.Engine(name)
	return engine, ok
}

func (s *Service) EvaluatePolicies(ctx context.Context, refs []atproto.StrongRef, eval *executor.EvalContext) (executor.Verdict, error) {
	if s.evaluator == nil {
		return executor.Deny("the policy evaluator is not configured", "evaluator"), nil
	}
	return s.evaluator.EvaluatePolicies(ctx, refs, eval)
}

func (s *Service) EvaluateRecord(ctx context.Context, record executor.PolicyRecord, eval *executor.EvalContext, permissions map[string]any) (executor.Verdict, error) {
	if s.evaluator == nil {
		return executor.Deny("the policy evaluator is not configured", "evaluator"), nil
	}
	return s.evaluator.EvaluateRecord(ctx, record, eval, permissions)
}

func (s *Service) Scope(ctx context.Context, request evaluator.ScopeRequest) (executor.Verdict, error) {
	if s.evaluator == nil {
		return executor.Deny("the policy evaluator is not configured", "evaluator"), nil
	}
	return s.evaluator.Scope(ctx, request)
}
