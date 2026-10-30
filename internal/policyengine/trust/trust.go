package trust

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/cache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
)

const (
	VouchNSID            = "sh.tangled.graph.vouch"
	BadgeBlueKeysNSID    = "com.publicdomainrelay.temp.badgeBlueKeys"
	BidderAssociate      = "bidder_associate"
	RequesterAssociate   = "requester_associate"
	defaultTrustCacheTTL = 5 * time.Minute

	maxListedRecords      = 2000
	maxOperatorCandidates = 8
)

type Host interface {
	Resolve(ctx context.Context, ref atproto.StrongRef) (map[string]any, error)
	ResolveOperatorDid(ctx context.Context, did string) (string, bool, error)
	GetVouchedDids(ctx context.Context, did string) (map[string]bool, error)
	Log(level, msg string, meta map[string]any)
}

type Snapshot struct {
	OperatorOf       map[string]string   `json:"operatorOf"`
	VouchedBy        map[string][]string `json:"vouchedBy"`
	TrustedOperators []string            `json:"trustedOperators"`
}

type ResolverOptions struct {
	Client *atproto.Client
	Store  *cache.Store
	TTL    time.Duration
	Log    func(level, msg string, meta map[string]any)
}

type operatorKey struct {
	did     string
	service string
}

type operatorEntry struct {
	value string
	found bool
}

type claim struct {
	operator string
	subject  string
	service  string
}

type Resolver struct {
	client *atproto.Client
	store  *cache.Store
	log    func(level, msg string, meta map[string]any)
	ops    *ttlcache.Cache[operatorKey, operatorEntry]
	claims *ttlcache.Cache[string, map[claim]bool]
	vouch  *ttlcache.Cache[string, map[string]bool]
}

func NewResolver(opts ResolverOptions) *Resolver {
	client := opts.Client
	if client == nil {
		client = atproto.NewClient()
	}
	ttl := opts.TTL
	ttl = opt.Positive(ttl, defaultTrustCacheTTL)
	log := opts.Log
	if log == nil {
		log = func(string, string, map[string]any) {}
	}
	return &Resolver{
		client: client,
		store:  opts.Store,
		log:    log,
		ops:    ttlcache.New[operatorKey, operatorEntry](ttlcache.Options{TTL: ttl}),
		claims: ttlcache.New[string, map[claim]bool](ttlcache.Options{TTL: ttl}),
		vouch:  ttlcache.New[string, map[string]bool](ttlcache.Options{TTL: ttl}),
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

func (r *Resolver) ResolveOperatorDid(ctx context.Context, did string) (string, bool, error) {
	return r.ResolveOperatorDidFor(ctx, did, "")
}

func (r *Resolver) ResolveOperatorDidFor(ctx context.Context, did, service string) (string, bool, error) {
	did = strings.TrimSpace(did)
	service = strings.TrimSpace(service)
	if did == "" {
		return "", false, nil
	}
	key := operatorKey{did: did, service: service}
	if entry, ok := r.ops.Get(key); ok {
		return entry.value, entry.found, nil
	}
	operator := ""
	found := false
	records, err := r.client.ListRecords(ctx, did, BadgeBlueKeysNSID, maxListedRecords)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		r.log("warn", "resolveOperatorDid failed", map[string]any{"did": did, "error": err.Error()})
	}
	for _, mirror := range mirroredClaims(records, did, service) {
		held, err := r.claimHeld(ctx, mirror)
		if err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			r.log("warn", "resolveOperatorDid operator repo failed", map[string]any{"did": did, "operator": mirror.operator, "service": mirror.service, "error": err.Error()})
			continue
		}
		if !held {
			r.log("info", "resolveOperatorDid unclaimed mirror", map[string]any{"did": did, "operator": mirror.operator, "service": mirror.service})
			continue
		}
		operator = mirror.operator
		found = true
		break
	}
	r.ops.Set(key, operatorEntry{value: operator, found: found})
	return operator, found, nil
}

func ClaimRKey(service, subject string) string {
	return service + "-" + strings.ReplaceAll(subject, ":", "-")
}

func mirroredClaims(records []atproto.Record, subject, service string) []claim {
	seen := make(map[claim]bool)
	var out []claim
	for _, record := range records {
		mirror, ok := claimOf(record.Value)
		if !ok || mirror.subject != subject || mirror.operator == subject || seen[mirror] || !associationService(mirror.service, service) {
			continue
		}
		seen[mirror] = true
		out = append(out, mirror)
		if len(out) == maxOperatorCandidates {
			break
		}
	}
	return out
}

func (r *Resolver) claimHeld(ctx context.Context, want claim) (bool, error) {
	ref := atproto.StrongRef{URI: "at://" + want.operator + "/" + BadgeBlueKeysNSID + "/" + ClaimRKey(want.service, want.subject)}
	if value, err := r.client.GetRecord(ctx, ref); err == nil {
		if held, ok := claimOf(value); ok && held == want {
			return true, nil
		}
	} else if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if claims, ok := r.claims.Get(want.operator); ok {
		return claims[want], nil
	}
	records, err := r.client.ListRecords(ctx, want.operator, BadgeBlueKeysNSID, maxListedRecords)
	if err != nil {
		return false, err
	}
	claims := make(map[claim]bool)
	for _, record := range records {
		if atproto.RepoFromURI(record.URI) != want.operator {
			continue
		}
		if held, ok := claimOf(record.Value); ok && held.operator == want.operator && held.subject != want.operator && associationService(held.service, "") {
			claims[held] = true
		}
	}
	r.claims.Set(want.operator, claims)
	return claims[want], nil
}

func claimOf(value map[string]any) (claim, bool) {
	service, _ := value["service"].(string)
	subject, _ := value["keyId"].(string)
	operator, _ := value["challenge"].(string)
	c := claim{operator: strings.TrimSpace(operator), subject: strings.TrimSpace(subject), service: strings.TrimSpace(service)}
	return c, strings.HasPrefix(c.operator, "did:") && strings.HasPrefix(c.subject, "did:")
}

func associationService(recordService, want string) bool {
	if recordService != BidderAssociate && recordService != RequesterAssociate {
		return false
	}
	return want == "" || recordService == want
}

func (r *Resolver) GetVouchedDids(ctx context.Context, did string) (map[string]bool, error) {
	did = strings.TrimSpace(did)
	if did == "" {
		return map[string]bool{}, nil
	}
	if entry, ok := r.vouch.Get(did); ok {
		out := make(map[string]bool, len(entry))
		for value := range entry {
			out[value] = true
		}
		return out, nil
	}
	vouched := make(map[string]bool)
	records, err := r.client.ListRecords(ctx, did, VouchNSID, maxListedRecords)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		r.log("warn", "getVouchedDids failed", map[string]any{"did": did, "error": err.Error()})
	} else {
		for _, record := range records {
			kind, _ := record.Value["kind"].(string)
			if kind == "denounce" {
				continue
			}
			_, _, rkey := atproto.SplitURI(record.URI)
			if rkey == "" {
				parts := strings.Split(record.URI, "/")
				rkey = parts[len(parts)-1]
			}
			if strings.HasPrefix(rkey, "did:") {
				vouched[rkey] = true
			}
		}
	}
	clone := make(map[string]bool, len(vouched))
	for value := range vouched {
		clone[value] = true
	}
	r.vouch.Set(did, clone)
	return vouched, nil
}

func (r *Resolver) Log(level, msg string, meta map[string]any) {
	r.log(level, msg, meta)
}

func (r *Resolver) ApplyEvent(did, rkey string) {
	r.ops.DeleteKeys(func(key operatorKey) bool { return key.did == did })
	r.claims.Delete(did)
	for _, other := range []string{did, rkey} {
		if other == "" {
			continue
		}
		r.vouch.Delete(other)
	}
}

func (r *Resolver) Invalidate() {
	r.ops.DeleteKeys(func(operatorKey) bool { return true })
	r.claims.DeleteKeys(func(string) bool { return true })
	r.vouch.DeleteKeys(func(string) bool { return true })
}

type opResult struct {
	did      string
	operator string
	found    bool
	err      error
}

type vouchResult struct {
	did     string
	vouches map[string]bool
	err     error
}

type ServiceOperatorResolver interface {
	ResolveOperatorDidFor(ctx context.Context, did, service string) (string, bool, error)
}

func BuildSnapshot(ctx context.Context, host Host, selfDid, counterpartyDid string) (Snapshot, error) {
	return BuildSnapshotFor(ctx, host, selfDid, counterpartyDid, "", "")
}

func BuildSnapshotFor(ctx context.Context, host Host, selfDid, counterpartyDid, selfService, counterpartyService string) (Snapshot, error) {
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
	roleResolver, _ := host.(ServiceOperatorResolver)
	resolve := func(did, service string) (string, bool, error) {
		if roleResolver != nil {
			return roleResolver.ResolveOperatorDidFor(ctx, did, service)
		}
		return host.ResolveOperatorDid(ctx, did)
	}
	dids := []string{selfDid}
	if counterpartyDid != "" && counterpartyDid != selfDid {
		dids = append(dids, counterpartyDid)
	}
	results := make(chan opResult, len(dids))
	for _, did := range dids {
		service := selfService
		if did == counterpartyDid {
			service = counterpartyService
		}
		wg.Go(func() {
			operator, found, err := resolve(did, service)
			results <- opResult{did: did, operator: operator, found: found, err: err}
		})
	}
	operators := make(map[string]string, len(dids))
	for range dids {
		result := <-results
		if result.err != nil {
			return snapshot, result.err
		}
		if result.did == selfDid {
			if result.found && result.operator != "" {
				operators[selfDid] = result.operator
			} else {
				operators[selfDid] = selfDid
			}
			continue
		}
		if result.found && result.operator != "" {
			operators[result.did] = result.operator
		}
	}
	snapshot.OperatorOf = operators
	trusted := []string{selfDid}
	if operator := operators[selfDid]; operator != "" && operator != selfDid {
		trusted = append(trusted, operator)
	}
	snapshot.TrustedOperators = trusted
	vouchers := make([]string, 0, len(trusted)+1)
	seen := make(map[string]bool)
	for _, value := range trusted {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		vouchers = append(vouchers, value)
	}
	if operator := operators[counterpartyDid]; counterpartyDid != "" && operator != "" && !seen[operator] {
		seen[operator] = true
		vouchers = append(vouchers, operator)
	}
	vouchResults := make(chan vouchResult, len(vouchers))
	for _, voucher := range vouchers {
		wg.Go(func() {
			vouches, err := host.GetVouchedDids(ctx, voucher)
			vouchResults <- vouchResult{did: voucher, vouches: vouches, err: err}
		})
	}
	for range vouchers {
		result := <-vouchResults
		if result.err != nil {
			return snapshot, result.err
		}
		list := make([]string, 0, len(result.vouches))
		for did := range result.vouches {
			list = append(list, did)
		}
		snapshot.VouchedBy[result.did] = list
	}
	return snapshot, nil
}

func (s Snapshot) CacheFiles() models.CacheEntry {
	operatorOf, _ := json.Marshal(s.OperatorOf)
	vouchedBy, _ := json.Marshal(s.VouchedBy)
	trustedOperators, _ := json.Marshal(s.TrustedOperators)
	return models.CacheEntry{
		"operatorOf.json":       {Data: string(operatorOf), Encoding: models.CacheEncodingText},
		"vouchedBy.json":        {Data: string(vouchedBy), Encoding: models.CacheEncodingText},
		"trustedOperators.json": {Data: string(trustedOperators), Encoding: models.CacheEncodingText},
	}
}
