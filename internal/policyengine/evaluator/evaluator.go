package evaluator

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/cache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/registry"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

type Options struct {
	Registry      *registry.Registry
	Resolve       func(context.Context, atproto.StrongRef) (map[string]any, error)
	Host          trust.Host
	ScopeCache    *cache.Scope[executor.Verdict]
	VerdictStore  *cache.Store
	VerdictTTL    time.Duration
	MaxConcurrent int
	Log           func(level, msg string, meta map[string]any)
}

type Evaluator struct {
	registry     *registry.Registry
	resolve      func(context.Context, atproto.StrongRef) (map[string]any, error)
	host         trust.Host
	scopeCache   *cache.Scope[executor.Verdict]
	verdictStore *cache.Store
	verdictTTL   time.Duration
	log          func(level, msg string, meta map[string]any)
	sem          chan struct{}
}

func NewScopeCache(positive, negative time.Duration, maxEntries int, log func(level, msg string, meta map[string]any)) *cache.Scope[executor.Verdict] {
	positive = opt.Positive(positive, 5*time.Minute)
	negative = opt.Positive(negative, 30*time.Second)
	maxEntries = opt.Positive(maxEntries, 10000)
	return cache.NewScope[executor.Verdict](cache.ScopeOptions[executor.Verdict]{
		PositiveTTL: positive,
		NegativeTTL: negative,
		MaxEntries:  maxEntries,
		Log:         log,
		TTLFor: func(verdict executor.Verdict) time.Duration {
			if verdict.Allow {
				return positive
			}
			return negative
		},
	})
}

func New(opts Options) *Evaluator {
	resolve := opts.Resolve
	if resolve == nil && opts.Host != nil {
		resolve = opts.Host.Resolve
	}
	ttl := opts.VerdictTTL
	ttl = opt.Positive(ttl, 30*time.Second)
	log := opts.Log
	if log == nil {
		log = func(string, string, map[string]any) {}
	}
	evaluator := &Evaluator{
		registry:     opts.Registry,
		resolve:      resolve,
		host:         opts.Host,
		scopeCache:   opts.ScopeCache,
		verdictStore: opts.VerdictStore,
		verdictTTL:   ttl,
		log:          log,
	}
	if opts.MaxConcurrent > 0 {
		evaluator.sem = make(chan struct{}, opts.MaxConcurrent)
	}
	return evaluator
}

func (e *Evaluator) Registry() *registry.Registry {
	return e.registry
}

func (e *Evaluator) EvaluatePolicies(ctx context.Context, refs []atproto.StrongRef, eval *executor.EvalContext) (executor.Verdict, error) {
	for _, ref := range refs {
		record, err := e.resolveRecord(ctx, executor.PolicyRecord{URI: ref.URI, CID: ref.CID})
		if err != nil {
			if ctx.Err() != nil {
				return executor.Verdict{}, ctx.Err()
			}
			return executor.Deny(fmt.Sprintf("failed to resolve policy ref %s: %v", ref.URI, err), ref.URI), nil
		}
		verdict, err := e.evaluateRecord(ctx, record, eval, nil)
		if err != nil {
			return executor.Verdict{}, err
		}
		if !verdict.Allow {
			return verdict, nil
		}
	}
	return executor.Allow(), nil
}

func (e *Evaluator) EvaluateRecord(ctx context.Context, record executor.PolicyRecord, eval *executor.EvalContext, permissions map[string]any) (executor.Verdict, error) {
	resolved, err := e.resolveRecord(ctx, record)
	if err != nil {
		if ctx.Err() != nil {
			return executor.Verdict{}, ctx.Err()
		}
		return executor.Deny(fmt.Sprintf("failed to resolve policy record %s: %v", record.URI, err), record.URI), nil
	}
	return e.evaluateRecord(ctx, resolved, eval, permissions)
}

func (e *Evaluator) evaluateRecord(ctx context.Context, record executor.PolicyRecord, eval *executor.EvalContext, permissions map[string]any) (executor.Verdict, error) {
	if e.registry == nil {
		return executor.Deny("policy registry is not configured", "registry"), nil
	}
	kind := executor.RecordType(record.URI, record.Value)
	item, ok := e.registry.Get(kind)
	if !ok {
		return executor.Deny("unknown policy record type: "+kind, kind), nil
	}
	if eval == nil {
		eval = &executor.EvalContext{}
	}
	local := *eval
	if e.host != nil {
		local.Host = e.host
	}
	local.PolicyName = policyName(record)
	ref := record.Ref()
	local.PolicyRef = &ref
	local.Perspective = cmp.Or(local.Perspective, executor.PerspectiveRequester)
	local.RFP = e.hydrateRecord(ctx, local.RFP)
	local.Bid = e.hydrateRecord(ctx, local.Bid)
	local.Accept = e.hydrateRecord(ctx, local.Accept)
	deriveDIDs(&local)
	if local.Cache == nil {
		local.Cache = models.Cache{}
	}
	key := ""
	if e.verdictStore != nil {
		key = cache.PolicyKey(policyCacheIdentity(record), recordSet(&local), local.Args)
		if data, ok := cache.ReadVerdict(e.verdictStore, key); ok {
			var cached executor.Verdict
			if json.Unmarshal(data, &cached) == nil {
				return cached, nil
			}
		}
	}
	if err := e.acquire(ctx); err != nil {
		return executor.Verdict{}, err
	}
	verdict, err := item.Execute(ctx, record, &local, permissions)
	e.release()
	if err != nil {
		if ctx.Err() != nil {
			return executor.Verdict{}, ctx.Err()
		}
		return executor.Deny("policy execution failed: "+err.Error(), kind), nil
	}
	if e.verdictStore != nil && key != "" {
		if data, marshalErr := json.Marshal(verdict); marshalErr == nil {
			cache.WriteVerdict(e.verdictStore, key, data, e.verdictTTL)
		}
	}
	return verdict, nil
}

func (e *Evaluator) Scope(ctx context.Context, request ScopeRequest) (executor.Verdict, error) {
	if e.registry == nil {
		return executor.Deny("policy registry is not configured", "registry"), nil
	}
	record := request.PolicyRecord
	if record == nil && request.Ref != nil {
		resolved, err := e.resolveRecord(ctx, executor.PolicyRecord{URI: request.Ref.URI, CID: request.Ref.CID})
		if err != nil {
			if ctx.Err() != nil {
				return executor.Verdict{}, ctx.Err()
			}
			return executor.Deny(fmt.Sprintf("failed to resolve policy ref %s: %v", request.Ref.URI, err), request.Ref.URI), nil
		}
		record = &resolved
	}
	if record == nil {
		return executor.Deny("scope requires a ref or a policyRecord", "policy"), nil
	}
	identity := cache.NameIdentity(policyName(*record))
	if record.URI != "" || record.CID != "" {
		identity = cache.RefIdentity(atproto.StrongRef{URI: record.URI, CID: record.CID})
	}
	if e.scopeCache == nil {
		return e.runScope(ctx, *record, request)
	}
	if !request.Refresh {
		if verdict, ok := e.scopeCache.Get(identity, request.CounterpartyDid, request.Args); ok {
			return verdict, nil
		}
	}
	if request.Refresh {
		verdict, err := e.runScope(ctx, *record, request)
		if err == nil {
			e.scopeCache.Set(identity, request.CounterpartyDid, request.Args, verdict)
		}
		return verdict, err
	}
	return e.scopeCache.Once(ctx, identity, request.CounterpartyDid, request.Args, func(runCtx context.Context) (executor.Verdict, error) {
		return e.runScope(runCtx, *record, request)
	})
}

func (e *Evaluator) runScope(ctx context.Context, record executor.PolicyRecord, request ScopeRequest) (executor.Verdict, error) {
	kind := executor.RecordType(record.URI, record.Value)
	item, ok := e.registry.Get(kind)
	if !ok {
		return executor.Deny("unknown policy record type: "+kind, kind), nil
	}
	input := executor.ScopeInput{
		Perspective:     request.Perspective,
		SelfDid:         request.SelfDid,
		CounterpartyDid: request.CounterpartyDid,
		Args:            request.Args,
	}
	if e.host != nil {
		input.ResolveOperatorDid = e.host.ResolveOperatorDid
		input.GetVouchedDids = e.host.GetVouchedDids
		if resolver, ok := e.host.(trust.ServiceOperatorResolver); ok {
			input.ResolveOperatorDidFor = resolver.ResolveOperatorDidFor
		}
	}
	if err := e.acquire(ctx); err != nil {
		return executor.Verdict{}, err
	}
	verdict, decided, err := item.Scope(ctx, record, input)
	e.release()
	if err != nil {
		if ctx.Err() != nil {
			return executor.Verdict{}, ctx.Err()
		}
		return executor.Deny("policy scope failed: "+err.Error(), kind), nil
	}
	if decided {
		e.log("debug", "scope verdict", map[string]any{"uri": record.URI, "counterpartyDid": request.CounterpartyDid, "allow": verdict.Allow})
		return verdict, nil
	}
	eval := &executor.EvalContext{
		PolicyName:       policyName(record),
		Args:             request.Args,
		Perspective:      request.Perspective,
		SelfDid:          request.SelfDid,
		SubjectDid:       request.CounterpartyDid,
		CounterpartyDid:  request.CounterpartyDid,
		RootRequesterDid: rootRequester(request.Perspective, request.SelfDid, request.CounterpartyDid),
	}
	return e.evaluateRecord(ctx, record, eval, nil)
}

func (e *Evaluator) hydrateRecord(ctx context.Context, record *executor.PolicyRecord) *executor.PolicyRecord {
	if record == nil || record.Value != nil {
		return record
	}
	resolved, err := e.resolveRecord(ctx, *record)
	if err != nil {
		return record
	}
	return &resolved
}

func deriveDIDs(eval *executor.EvalContext) {
	author := func(record *executor.PolicyRecord) string {
		if record == nil {
			return ""
		}
		return atproto.RepoFromURI(record.URI)
	}
	if eval.SubjectDid == "" {
		if eval.Perspective == executor.PerspectiveRequester {
			eval.SubjectDid = author(eval.Bid)
		} else if eval.RFP != nil {
			eval.SubjectDid = author(eval.RFP)
		} else {
			eval.SubjectDid = author(eval.Accept)
		}
	}
	eval.CounterpartyDid = cmp.Or(eval.CounterpartyDid, eval.SubjectDid)
	if eval.RootRequesterDid == "" {
		if eval.Perspective == executor.PerspectiveRequester {
			eval.RootRequesterDid = eval.SelfDid
		} else {
			eval.RootRequesterDid = eval.SubjectDid
		}
	}
}

func (e *Evaluator) resolveRecord(ctx context.Context, record executor.PolicyRecord) (executor.PolicyRecord, error) {
	if record.Value != nil {
		return record, nil
	}
	if e.resolve == nil {
		return record, fmt.Errorf("record resolver is not configured")
	}
	value, err := e.resolve(ctx, atproto.StrongRef{URI: record.URI, CID: record.CID})
	if err != nil {
		return record, err
	}
	record.Value = value
	return record, nil
}

func (e *Evaluator) acquire(ctx context.Context) error {
	if e.sem == nil {
		return nil
	}
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Evaluator) release() {
	if e.sem == nil {
		return
	}
	<-e.sem
}

func (e *Evaluator) ApplyEvent(did, rkey string) {
	if e.scopeCache != nil {
		e.scopeCache.ApplyEvent(did, rkey)
	}
	if resolver, ok := e.host.(*trust.Resolver); ok {
		resolver.ApplyEvent(did, rkey)
	}
}

type ScopeRequest struct {
	Ref             *atproto.StrongRef
	PolicyRecord    *executor.PolicyRecord
	Perspective     executor.Perspective
	SelfDid         string
	CounterpartyDid string
	Args            map[string]any
	Refresh         bool
}

func policyCacheIdentity(record executor.PolicyRecord) string {
	name := policyName(record)
	ref := record.Ref()
	if ref.URI == "" {
		return name
	}
	if ref.CID != "" {
		return name + "|" + ref.URI + "#" + ref.CID
	}
	return name + "|" + ref.URI
}

func policyName(record executor.PolicyRecord) string {
	if record.Value != nil {
		if name, ok := record.Value["name"].(string); ok && name != "" {
			return name
		}
	}
	return "policy"
}

func recordSet(eval *executor.EvalContext) cache.RecordSet {
	set := cache.RecordSet{}
	if eval.RFP != nil {
		ref := eval.RFP.Ref()
		set.RFP = &ref
	}
	if eval.Bid != nil {
		ref := eval.Bid.Ref()
		set.Bid = &ref
	}
	if eval.Accept != nil {
		ref := eval.Accept.Ref()
		set.Accept = &ref
	}
	return set
}

func rootRequester(perspective executor.Perspective, selfDid, counterpartyDid string) string {
	if perspective == executor.PerspectiveRequester {
		return selfDid
	}
	return counterpartyDid
}
