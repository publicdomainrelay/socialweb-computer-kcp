package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
)

type EngineFactory func(ctx context.Context, engine *policyv1alpha1.PolicyEngine) (*Engine, error)

type managedEngine struct {
	engine *Engine
	hash   string
}

type EngineManager struct {
	mu      sync.Mutex
	factory EngineFactory
	engines map[string]*managedEngine
}

func NewEngineManager(factory EngineFactory) *EngineManager {
	return &EngineManager{factory: factory, engines: make(map[string]*managedEngine)}
}

func NewDefaultEngineFactory(base Config) EngineFactory {
	return func(ctx context.Context, engineCR *policyv1alpha1.PolicyEngine) (*Engine, error) {
		return NewEngine(EngineConfigFromCR(engineCR, base)), nil
	}
}

func (m *EngineManager) ReconcilePolicyEngine(ctx context.Context, key string, engineCR *policyv1alpha1.PolicyEngine) (EngineReconcileState, error) {
	if engineCR == nil {
		return EngineReconcileState{}, fmt.Errorf("policy engine is required")
	}
	if engineCR.DeletionTimestamp != nil {
		m.stop(ctx, key)
		state := EngineReconcileState{Phase: policyv1alpha1.PolicyEnginePending}
		meta.SetStatusCondition(&state.Conditions, metav1.Condition{
			Type:    policyengine.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "Terminating",
			Message: "the policy engine is being deleted",
		})
		return state, nil
	}
	if engineCR.Spec.Suspend {
		m.stop(ctx, key)
		state := EngineReconcileState{Phase: policyv1alpha1.PolicyEnginePending}
		meta.SetStatusCondition(&state.Conditions, metav1.Condition{
			Type:    policyengine.ConditionSuspended,
			Status:  metav1.ConditionTrue,
			Reason:  "Suspended",
			Message: "the policy engine is suspended",
		})
		return state, nil
	}
	spec, err := json.Marshal(engineCR.Spec)
	if err != nil {
		return EngineReconcileState{}, err
	}
	hash := string(spec)
	m.mu.Lock()
	managed := m.engines[key]
	if managed == nil || managed.hash != hash {
		m.mu.Unlock()
		m.stop(ctx, key)
		if m.factory == nil {
			return EngineReconcileState{Phase: policyv1alpha1.PolicyEngineFailed}, fmt.Errorf("policy engine factory is not configured")
		}
		engine, factoryErr := m.factory(ctx, engineCR)
		if factoryErr != nil {
			state := EngineReconcileState{Phase: policyv1alpha1.PolicyEngineFailed}
			meta.SetStatusCondition(&state.Conditions, metav1.Condition{
				Type:    policyengine.ConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "CreateFailed",
				Message: factoryErr.Error(),
			})
			return state, factoryErr
		}
		if engine == nil {
			return EngineReconcileState{Phase: policyv1alpha1.PolicyEngineFailed}, fmt.Errorf("policy engine factory returned nil")
		}
		if startErr := engine.Start(context.WithoutCancel(ctx)); startErr != nil {
			return EngineReconcileState{Phase: policyv1alpha1.PolicyEngineFailed}, startErr
		}
		m.mu.Lock()
		m.engines[key] = &managedEngine{engine: engine, hash: hash}
		managed = m.engines[key]
		stored := *managed
		m.mu.Unlock()
		return reconcileState(&stored), nil
	}
	stored := *managed
	m.mu.Unlock()
	return reconcileState(&stored), nil
}

func reconcileState(managed *managedEngine) EngineReconcileState {
	state := EngineReconcileState{ActiveRuns: managed.engine.ActiveRuns()}
	if managed.engine.Ready() {
		state.Phase = policyv1alpha1.PolicyEngineRunning
		state.Ready = true
		meta.SetStatusCondition(&state.Conditions, metav1.Condition{
			Type:    policyengine.ConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  "Ready",
			Message: "the in-process policy engine is ready",
		})
		return state
	}
	state.Phase = policyv1alpha1.PolicyEnginePending
	meta.SetStatusCondition(&state.Conditions, metav1.Condition{
		Type:    policyengine.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  "RuntimeUnavailable",
		Message: "the embedded or system deno runtime is unavailable",
	})
	return state
}

func (m *EngineManager) EngineFor(name string) (*Engine, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, ok := m.engines[name]
	if !ok {
		return nil, false
	}
	return managed.engine, true
}

func (m *EngineManager) Only(prefix string) (*Engine, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found *Engine
	for key, managed := range m.engines {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = managed.engine
	}
	return found, found != nil
}

func (m *EngineManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	engines := make([]*Engine, 0, len(m.engines))
	for name, managed := range m.engines {
		engines = append(engines, managed.engine)
		delete(m.engines, name)
	}
	m.mu.Unlock()
	var errs []error
	for _, engine := range engines {
		if err := engine.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *EngineManager) Delete(ctx context.Context, name string) error {
	m.mu.Lock()
	managed := m.engines[name]
	delete(m.engines, name)
	m.mu.Unlock()
	if managed == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return managed.engine.Shutdown(shutdownCtx)
}

func (m *EngineManager) stop(ctx context.Context, name string) {
	m.mu.Lock()
	managed := m.engines[name]
	delete(m.engines, name)
	m.mu.Unlock()
	if managed == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	managed.engine.Shutdown(shutdownCtx)
}

func EngineConfigFromCR(engineCR *policyv1alpha1.PolicyEngine, base Config) Config {
	config := base
	if engineCR == nil {
		return config
	}
	limits := engineCR.Spec.Limits
	if limits.MaxConcurrentRuns > 0 {
		config.Limits.MaxConcurrentRuns = int(limits.MaxConcurrentRuns)
	}
	if limits.QueueDepth > 0 {
		config.Limits.QueueDepth = int(limits.QueueDepth)
	}
	if limits.ActiveDeadlineSeconds != nil && *limits.ActiveDeadlineSeconds > 0 {
		config.Limits.ActiveDeadline = time.Duration(*limits.ActiveDeadlineSeconds) * time.Second
	}
	if limits.MaxConsoleBytes > 0 {
		config.Limits.MaxConsoleBytes = limits.MaxConsoleBytes
	}
	if limits.MaxOutputBytes > 0 {
		config.Limits.MaxOutputBytes = limits.MaxOutputBytes
	}
	if limits.MaxCacheEntries > 0 {
		config.Limits.MaxCacheEntries = int(limits.MaxCacheEntries)
	}
	if limits.MaxCacheBytes > 0 {
		config.Limits.MaxCacheBytes = limits.MaxCacheBytes
	}
	if engineCR.Spec.Trust.TTLSeconds != nil && *engineCR.Spec.Trust.TTLSeconds > 0 {
		config.TrustTTL = time.Duration(*engineCR.Spec.Trust.TTLSeconds) * time.Second
	}
	cacheConfig := engineCR.Spec.Cache
	if cacheConfig.VerdictTTLSeconds != nil && *cacheConfig.VerdictTTLSeconds > 0 {
		config.VerdictTTL = time.Duration(*cacheConfig.VerdictTTLSeconds) * time.Second
	}
	if cacheConfig.ScopeTTLSeconds != nil && *cacheConfig.ScopeTTLSeconds > 0 {
		config.ScopeTTL = time.Duration(*cacheConfig.ScopeTTLSeconds) * time.Second
	}
	if cacheConfig.RecordTTLSeconds != nil && *cacheConfig.RecordTTLSeconds > 0 {
		config.RecordTTL = time.Duration(*cacheConfig.RecordTTLSeconds) * time.Second
	}
	config.VerdictTTL = opt.Positive(config.VerdictTTL, 30*time.Second)
	config.ScopeTTL = opt.Positive(config.ScopeTTL, 5*time.Minute)
	config.NegativeScopeTTL = min(opt.Positive(config.NegativeScopeTTL, 30*time.Second), config.ScopeTTL)
	config.TrustTTL = opt.Positive(config.TrustTTL, 5*time.Minute)
	config.DisableActions = false
	return config
}

func EngineKey(cluster, name string) string {
	return cluster + "/" + name
}
