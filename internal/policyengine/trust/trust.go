package trust

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

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

type operatorEntry struct {
	value string
	found bool
	at    time.Time
}

type vouchEntry struct {
	value map[string]bool
	at    time.Time
}

type Resolver struct {
	client *atproto.Client
	store  *cache.Store
	ttl    time.Duration
	log    func(level, msg string, meta map[string]any)
	mu     sync.Mutex
	ops    map[string]operatorEntry
	vouch  map[string]vouchEntry
}

func NewResolver(opts ResolverOptions) *Resolver {
	client := opts.Client
	if client == nil {
		client = atproto.NewClient()
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = defaultTrustCacheTTL
	}
	log := opts.Log
	if log == nil {
		log = func(string, string, map[string]any) {}
	}
	return &Resolver{
		client: client,
		store:  opts.Store,
		ttl:    ttl,
		log:    log,
		ops:    make(map[string]operatorEntry),
		vouch:  make(map[string]vouchEntry),
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
	did = strings.TrimSpace(did)
	if did == "" {
		return "", false, nil
	}
	r.mu.Lock()
	entry, ok := r.ops[did]
	r.mu.Unlock()
	if ok && time.Since(entry.at) < r.ttl {
		return entry.value, entry.found, nil
	}
	operator := ""
	found := false
	records, err := r.client.ListRecords(ctx, did, BadgeBlueKeysNSID, 100)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		r.log("warn", "resolveOperatorDid failed", map[string]any{"did": did, "error": err.Error()})
	} else {
		for _, record := range records {
			service, _ := record.Value["service"].(string)
			if service != BidderAssociate && service != RequesterAssociate {
				continue
			}
			keyID, _ := record.Value["keyId"].(string)
			challenge, _ := record.Value["challenge"].(string)
			if keyID == did && strings.HasPrefix(challenge, "did:") {
				operator = challenge
				found = true
				break
			}
			if challenge != did {
				continue
			}
			if strings.HasPrefix(keyID, "did:") {
				operator = keyID
				found = true
				break
			}
		}
	}
	r.mu.Lock()
	r.ops[did] = operatorEntry{value: operator, found: found, at: time.Now()}
	r.mu.Unlock()
	return operator, found, nil
}

func (r *Resolver) GetVouchedDids(ctx context.Context, did string) (map[string]bool, error) {
	did = strings.TrimSpace(did)
	if did == "" {
		return map[string]bool{}, nil
	}
	r.mu.Lock()
	entry, ok := r.vouch[did]
	r.mu.Unlock()
	if ok && time.Since(entry.at) < r.ttl {
		out := make(map[string]bool, len(entry.value))
		for value := range entry.value {
			out[value] = true
		}
		return out, nil
	}
	vouched := make(map[string]bool)
	records, err := r.client.ListRecords(ctx, did, VouchNSID, 100)
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
	r.mu.Lock()
	clone := make(map[string]bool, len(vouched))
	for value := range vouched {
		clone[value] = true
	}
	r.vouch[did] = vouchEntry{value: clone, at: time.Now()}
	r.mu.Unlock()
	return vouched, nil
}

func (r *Resolver) Log(level, msg string, meta map[string]any) {
	r.log(level, msg, meta)
}

func (r *Resolver) ApplyEvent(did, rkey string) {
	r.mu.Lock()
	for _, other := range []string{did, rkey} {
		if other == "" {
			continue
		}
		delete(r.ops, other)
		delete(r.vouch, other)
	}
	r.mu.Unlock()
}

func (r *Resolver) Invalidate() {
	r.mu.Lock()
	r.ops = make(map[string]operatorEntry)
	r.vouch = make(map[string]vouchEntry)
	r.mu.Unlock()
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

func BuildSnapshot(ctx context.Context, host Host, selfDid, counterpartyDid string) (Snapshot, error) {
	snapshot := Snapshot{
		OperatorOf:       make(map[string]string),
		VouchedBy:        make(map[string][]string),
		TrustedOperators: []string{},
	}
	if host == nil {
		return snapshot, nil
	}
	dids := []string{selfDid}
	if counterpartyDid != "" && counterpartyDid != selfDid {
		dids = append(dids, counterpartyDid)
	}
	results := make(chan opResult, len(dids))
	for _, did := range dids {
		go func(value string) {
			operator, found, err := host.ResolveOperatorDid(ctx, value)
			results <- opResult{did: value, operator: operator, found: found, err: err}
		}(did)
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
		go func(value string) {
			vouches, err := host.GetVouchedDids(ctx, value)
			vouchResults <- vouchResult{did: value, vouches: vouches, err: err}
		}(voucher)
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
