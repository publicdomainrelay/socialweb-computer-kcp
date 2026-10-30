package trust

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
	MirrorRKey           = "association"
	BidderAssociate      = "bidder_associate"
	RequesterAssociate   = "requester_associate"
	defaultTrustCacheTTL = 5 * time.Minute

	maxLegacyListedRecords = 2000
)

type Host interface {
	Resolve(ctx context.Context, ref atproto.StrongRef) (map[string]any, error)
	ResolveOperatorDid(ctx context.Context, did string) (string, bool, error)
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

type vouchKey struct {
	voucher string
	subject string
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
	legacy *ttlcache.Cache[string, map[claim]bool]
	vouch  *ttlcache.Cache[vouchKey, bool]
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
		legacy: ttlcache.New[string, map[claim]bool](ttlcache.Options{TTL: ttl}),
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
	operator, found, err := r.operatorOf(ctx, did, service)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		r.log("warn", "resolveOperatorDid failed", map[string]any{"did": did, "service": service, "error": err.Error()})
	}
	r.ops.Set(key, operatorEntry{value: operator, found: found})
	return operator, found, nil
}

func (r *Resolver) operatorOf(ctx context.Context, did, service string) (string, bool, error) {
	mirror, ok, err := r.mirrorOf(ctx, did, service)
	if err != nil || !ok {
		return "", false, err
	}
	held, err := r.claimHeld(ctx, mirror)
	if err != nil {
		return "", false, err
	}
	if !held {
		r.log("info", "resolveOperatorDid unclaimed mirror", map[string]any{"did": did, "operator": mirror.operator, "service": mirror.service})
		return "", false, nil
	}
	return mirror.operator, true, nil
}

func (r *Resolver) mirrorOf(ctx context.Context, did, service string) (claim, bool, error) {
	value, err := r.getRecord(ctx, did, BadgeBlueKeysNSID, MirrorRKey)
	if err != nil || value == nil {
		return claim{}, false, err
	}
	mirror, ok := claimOf(value)
	return mirror, ok && mirror.subject == did && mirror.operator != did && associationService(mirror.service, service), nil
}

func ClaimRKey(service, subject string) string {
	return service + "-" + strings.ReplaceAll(subject, ":", "-")
}

func (r *Resolver) claimHeld(ctx context.Context, want claim) (bool, error) {
	value, err := r.getRecord(ctx, want.operator, BadgeBlueKeysNSID, ClaimRKey(want.service, want.subject))
	if err != nil {
		return false, err
	}
	if held, ok := claimOf(value); ok && held == want {
		return true, nil
	}
	return r.legacyClaimListed(ctx, want)
}

func (r *Resolver) legacyClaimListed(ctx context.Context, want claim) (bool, error) {
	r.log("info", "resolveOperatorDid legacy claim listing", map[string]any{"did": want.subject, "operator": want.operator, "service": want.service})
	if claims, ok := r.legacy.Get(want.operator); ok {
		return claims[want], nil
	}
	records, err := r.client.ListRecords(ctx, want.operator, BadgeBlueKeysNSID, maxLegacyListedRecords)
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
	r.legacy.Set(want.operator, claims)
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
	r.ops.DeleteKeys(func(key operatorKey) bool { return key.did == did })
	r.legacy.Delete(did)
	r.vouch.DeleteKeys(func(key vouchKey) bool {
		return (did != "" && key.voucher == did) || (rkey != "" && key.voucher == rkey)
	})
}

func (r *Resolver) Invalidate() {
	r.ops.DeleteKeys(func(operatorKey) bool { return true })
	r.legacy.DeleteKeys(func(string) bool { return true })
	r.vouch.DeleteKeys(func(vouchKey) bool { return true })
}

type opResult struct {
	did      string
	operator string
	found    bool
	err      error
}

type vouchResult struct {
	voucher string
	subject string
	vouched bool
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
	vouchers := distinct(append(append([]string(nil), trusted...), operators[counterpartyDid]))
	subjects := distinct([]string{selfDid, operators[selfDid], counterpartyDid, operators[counterpartyDid]})
	pairs := 0
	vouchResults := make(chan vouchResult, len(vouchers)*len(subjects))
	for _, voucher := range vouchers {
		snapshot.VouchedBy[voucher] = []string{}
		for _, subject := range subjects {
			if subject == voucher {
				continue
			}
			pairs++
			wg.Go(func() {
				vouched, err := host.Vouches(ctx, voucher, subject)
				vouchResults <- vouchResult{voucher: voucher, subject: subject, vouched: vouched, err: err}
			})
		}
	}
	for range pairs {
		result := <-vouchResults
		if result.err != nil {
			return snapshot, result.err
		}
		if result.vouched {
			snapshot.VouchedBy[result.voucher] = append(snapshot.VouchedBy[result.voucher], result.subject)
		}
	}
	for _, list := range snapshot.VouchedBy {
		slices.Sort(list)
	}
	return snapshot, nil
}

func distinct(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
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
