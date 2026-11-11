package trust

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/cache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
)

const (
	VouchNSID            = "sh.tangled.graph.vouch"
	defaultTrustCacheTTL = 5 * time.Minute
	trustErrorTTL        = 5 * time.Second
)

type Host interface {
	Resolve(ctx context.Context, ref atproto.StrongRef) (map[string]any, error)
	Walk(ctx context.Context, did string, assoc Association) (identlink.Result, error)
	Vouches(ctx context.Context, voucher, subject string) (bool, error)
	Log(level, msg string, meta map[string]any)
}

type Snapshot struct {
	OperatorOf       map[string]string   `json:"operatorOf"`
	VouchedBy        map[string][]string `json:"vouchedBy"`
	TrustedOperators []string            `json:"trustedOperators"`
}

type ResolverOptions struct {
	Client *atproto.Client
	Source identlink.Source
	Store  *cache.Store
	TTL    time.Duration
	Log    func(level, msg string, meta map[string]any)
}

type walkKey struct {
	did   string
	assoc Association
}

type vouchKey struct {
	voucher string
	subject string
}

type Resolver struct {
	client *atproto.Client
	source identlink.Source
	store  *cache.Store
	log    func(level, msg string, meta map[string]any)
	now    func() time.Time
	ttl    time.Duration
	walks  *ttlcache.Cache[walkKey, identlink.Result]
	vouch  *ttlcache.Cache[vouchKey, bool]
}

func NewResolver(opts ResolverOptions) *Resolver {
	client := opts.Client
	if client == nil {
		client = atproto.NewClient()
	}
	source := opts.Source
	if source == nil {
		source = identlink.NewRemote(identlink.PLCDirectory(client.PLCURL, client.HTTP), client.HTTP)
	}
	ttl := opt.Positive(opts.TTL, defaultTrustCacheTTL)
	log := opts.Log
	if log == nil {
		log = func(string, string, map[string]any) {}
	}
	return &Resolver{
		client: client,
		source: source,
		store:  opts.Store,
		log:    log,
		now:    time.Now,
		ttl:    ttl,
		walks:  ttlcache.New[walkKey, identlink.Result](ttlcache.Options{TTL: ttl}),
		vouch:  ttlcache.New[vouchKey, bool](ttlcache.Options{TTL: ttl}),
	}
}

func (r *Resolver) Resolve(ctx context.Context, ref atproto.StrongRef) (map[string]any, error) {
	if r.store != nil {
		if value, ok := cache.ReadRecord(r.store, ref); ok {
			return value, nil
		}
	}
	value, err := r.client.GetRecord(ctx, ref)
	if err != nil {
		return nil, err
	}
	if r.store != nil {
		cache.WriteRecord(r.store, ref, value)
	}
	return value, nil
}

func (r *Resolver) getRecord(ctx context.Context, repo, collection, rkey string) (map[string]any, error) {
	value, err := r.client.GetRecord(ctx, atproto.StrongRef{URI: "at://" + repo + "/" + collection + "/" + rkey})
	if errors.Is(err, atproto.ErrRecordNotFound) {
		return nil, nil
	}
	return value, err
}

type walkIO struct {
	r               *Resolver
	requireContract bool
}

func (w walkIO) ResolveDID(ctx context.Context, did string) (*identlink.Doc, error) {
	return w.r.source.ResolveDID(ctx, did)
}

func (w walkIO) GetRecord(ctx context.Context, repo, collection, rkey string) (map[string]any, error) {
	value, err := w.r.source.GetRecord(ctx, repo, collection, rkey)
	if _, scoped := value["contract"]; w.requireContract && collection == identlink.GrantNSID && !scoped {
		return nil, err
	}
	return value, err
}

func (w walkIO) ContractHeld(ctx context.Context, contract identlink.StrongRef, authorities []string) (bool, error) {
	return w.r.contractHeld(ctx, atproto.StrongRef{URI: contract.URI, CID: contract.CID}, authorities)
}

func (w walkIO) Now() time.Time {
	return w.r.now()
}

func (r *Resolver) Walk(ctx context.Context, did string, assoc Association) (identlink.Result, error) {
	did = strings.TrimSpace(did)
	assoc = assoc.normal()
	key := walkKey{did: did, assoc: assoc}
	if res, ok := r.walks.Get(key); ok {
		return res, nil
	}
	res := identlink.Walk(ctx, walkIO{r: r, requireContract: assoc.RequireContract}, did, identlink.Options{MaxDepth: assoc.MaxDepth})
	if err := ctx.Err(); err != nil {
		return identlink.Result{}, err
	}
	if len(res.Errors) > 0 {
		r.log("warn", "identlink walk left edges unconfirmed", map[string]any{"did": did, "errors": res.Errors})
		r.walks.SetTTL(key, res, trustErrorTTL)
		return res, nil
	}
	ttl := r.ttl
	for _, a := range res.Ancestors {
		if a.ExpiresAt != nil {
			ttl = min(ttl, max(a.ExpiresAt.Sub(r.now()), time.Second))
		}
	}
	r.walks.SetTTL(key, res, ttl)
	return res, nil
}

func (r *Resolver) Vouches(ctx context.Context, voucher, subject string) (bool, error) {
	voucher = strings.TrimSpace(voucher)
	subject = strings.TrimSpace(subject)
	if voucher == "" || !strings.HasPrefix(subject, "did:") {
		return false, nil
	}
	key := vouchKey{voucher: voucher, subject: subject}
	if vouched, ok := r.vouch.Get(key); ok {
		return vouched, nil
	}
	value, err := r.getRecord(ctx, voucher, VouchNSID, subject)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		r.log("warn", "vouches failed", map[string]any{"voucher": voucher, "subject": subject, "error": err.Error()})
		return false, nil
	}
	kind, _ := value["kind"].(string)
	vouched := value != nil && kind != "denounce"
	r.vouch.Set(key, vouched)
	return vouched, nil
}

func (r *Resolver) Log(level, msg string, meta map[string]any) {
	r.log(level, msg, meta)
}

func (r *Resolver) ApplyEvent(did, rkey string) {
	named := func(x string) bool { return x != "" && (x == did || x == rkey) }
	r.walks.DeleteFunc(func(key walkKey, res identlink.Result) bool {
		return named(key.did) || slices.ContainsFunc(res.Ancestors, func(a identlink.Ancestor) bool { return named(a.DID) })
	})
	if did != "" {
		r.source.Forget(did)
	}
	r.vouch.DeleteKeys(func(key vouchKey) bool { return named(key.voucher) })
}

func (r *Resolver) Invalidate() {
	r.walks.DeleteKeys(func(walkKey) bool { return true })
	r.vouch.DeleteKeys(func(vouchKey) bool { return true })
}

type walked struct {
	res identlink.Result
	err error
}

type vouchResult struct {
	voucher string
	subject string
	vouched bool
	err     error
}

func BuildSnapshot(ctx context.Context, host Host, selfDid, counterpartyDid string, assoc Association) (Snapshot, error) {
	snapshot := Snapshot{
		OperatorOf:       make(map[string]string),
		VouchedBy:        make(map[string][]string),
		TrustedOperators: []string{},
	}
	if host == nil {
		return snapshot, nil
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var self, cp walked
	wg.Go(func() { self.res, self.err = host.Walk(ctx, selfDid, Association{MaxDepth: assoc.MaxDepth}) })
	if counterpartyDid != "" && counterpartyDid != selfDid {
		wg.Go(func() { cp.res, cp.err = host.Walk(ctx, counterpartyDid, assoc) })
	}
	wg.Wait()
	if err := errors.Join(self.err, cp.err); err != nil {
		return snapshot, err
	}
	snapshot.OperatorOf = operatorsOf(selfDid, counterpartyDid, self.res, cp.res)
	selfSide := self.res.Side()
	snapshot.TrustedOperators = selfSide
	if counterpartyDid == "" || counterpartyDid == selfDid {
		snapshot.VouchedBy[selfDid] = []string{}
		return snapshot, nil
	}
	cpSide := cp.res.Side()
	pairs := vouchPairs(selfSide, cpSide)
	for _, p := range pairs {
		snapshot.VouchedBy[p.voucher] = []string{}
	}
	results := make(chan vouchResult, len(pairs))
	for _, p := range pairs {
		wg.Go(func() {
			vouched, err := host.Vouches(ctx, p.voucher, p.subject)
			results <- vouchResult{voucher: p.voucher, subject: p.subject, vouched: vouched, err: err}
		})
	}
	for range pairs {
		result := <-results
		if result.err != nil {
			return snapshot, result.err
		}
		if !result.vouched {
			continue
		}
		snapshot.VouchedBy[result.voucher] = append(snapshot.VouchedBy[result.voucher], result.subject)
		switch {
		case slices.Contains(selfSide, result.voucher) && slices.Contains(cpSide, result.subject):
			snapshot.VouchedBy[result.voucher] = append(snapshot.VouchedBy[result.voucher], counterpartyDid)
		case slices.Contains(cpSide, result.voucher) && slices.Contains(selfSide, result.subject):
			snapshot.VouchedBy[counterpartyDid] = append(snapshot.VouchedBy[counterpartyDid], selfDid)
		}
	}
	for voucher, list := range snapshot.VouchedBy {
		slices.Sort(list)
		snapshot.VouchedBy[voucher] = slices.Compact(list)
	}
	return snapshot, nil
}

func operatorsOf(selfDid, counterpartyDid string, self, cp identlink.Result) map[string]string {
	out := map[string]string{selfDid: selfDid}
	if len(self.Roots) > 0 {
		out[selfDid] = self.Roots[0]
	}
	if counterpartyDid == "" || counterpartyDid == selfDid {
		return out
	}
	for _, root := range self.Roots {
		if slices.Contains(cp.Roots, root) {
			out[selfDid], out[counterpartyDid] = root, root
			return out
		}
	}
	for _, root := range cp.Roots {
		if root != counterpartyDid {
			out[counterpartyDid] = root
			break
		}
	}
	return out
}

type vouchPair struct {
	voucher, subject string
}

func vouchPairs(selfSide, cpSide []string) []vouchPair {
	var out []vouchPair
	add := func(voucher, subject string) {
		p := vouchPair{voucher: voucher, subject: subject}
		if voucher != subject && len(out) < identlink.DefaultBudget && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for sum := 0; sum <= len(selfSide)+len(cpSide)-2; sum++ {
		for i := max(0, sum-len(cpSide)+1); i <= min(sum, len(selfSide)-1); i++ {
			add(selfSide[i], cpSide[sum-i])
			add(cpSide[sum-i], selfSide[i])
		}
	}
	return out
}

func (s Snapshot) ActionVouchedBy() map[string][]string {
	out := make(map[string][]string, len(s.VouchedBy))
	for voucher, list := range s.VouchedBy {
		out[voucher] = slices.Clone(list)
	}
	if len(s.TrustedOperators) == 0 {
		return out
	}
	self := s.TrustedOperators[0]
	merged := []string{}
	for _, operator := range s.TrustedOperators {
		merged = append(merged, s.VouchedBy[operator]...)
	}
	slices.Sort(merged)
	out[self] = slices.Compact(merged)
	return out
}

func (s Snapshot) CacheFiles() models.CacheEntry {
	operatorOf, _ := json.Marshal(s.OperatorOf)
	vouchedBy, _ := json.Marshal(s.ActionVouchedBy())
	trustedOperators, _ := json.Marshal(s.TrustedOperators)
	return models.CacheEntry{
		"operatorOf.json":       {Data: string(operatorOf), Encoding: models.CacheEncodingText},
		"vouchedBy.json":        {Data: string(vouchedBy), Encoding: models.CacheEncodingText},
		"trustedOperators.json": {Data: string(trustedOperators), Encoding: models.CacheEncodingText},
	}
}
