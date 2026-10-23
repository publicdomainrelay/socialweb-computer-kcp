package trust

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

type Lister interface {
	ListRecords(ctx context.Context, repo, collection string) ([]market.ListedRecord, error)
}

type ListerFunc func(ctx context.Context, repo, collection string) ([]market.ListedRecord, error)

func (f ListerFunc) ListRecords(ctx context.Context, repo, collection string) ([]market.ListedRecord, error) {
	return f(ctx, repo, collection)
}

func NewPageLister(reader market.RecordLister) Lister {
	return ListerFunc(func(ctx context.Context, repo, collection string) ([]market.ListedRecord, error) {
		out := []market.ListedRecord{}
		cursor := ""
		for {
			page, err := reader.ListRecords(ctx, repo, collection, 100, cursor)
			if err != nil {
				return nil, err
			}
			out = append(out, page.Records...)
			if page.Cursor == "" || len(page.Records) == 0 {
				return out, nil
			}
			cursor = page.Cursor
		}
	})
}

func NewHTTPReaderLister(reader *market.HTTPReader) Lister {
	return ListerFunc(func(ctx context.Context, repo, collection string) ([]market.ListedRecord, error) {
		return reader.ListAll(ctx, repo, collection, 100)
	})
}

type OwnLister interface {
	ListRecordsOwn(ctx context.Context, collection string) ([]market.ListedRecord, error)
}

type OwnListerFunc func(ctx context.Context, collection string) ([]market.ListedRecord, error)

func (f OwnListerFunc) ListRecordsOwn(ctx context.Context, collection string) ([]market.ListedRecord, error) {
	return f(ctx, collection)
}

type VouchResolver interface {
	GetVouchedDIDs(ctx context.Context, did string) (map[string]struct{}, error)
	IsVouched(ctx context.Context, voucher, vouchee string) (bool, error)
}

type OperatorDiscovery interface {
	DiscoverOperatorDIDs(ctx context.Context, atprotoDID string) ([]string, error)
}

type DelegatedTrustResolver interface {
	GetDelegatedTrustedDIDs(ctx context.Context, selfDID string) (map[string]struct{}, error)
}

type TangledGraphOptions struct {
	ListRecords Lister
	Log         market.LogFunc
}

type tangledGraphVouchResolver struct {
	list Lister
	log  market.LogFunc
}

func NewTangledGraphVouchResolver(opts TangledGraphOptions) VouchResolver {
	logf := opts.Log
	if logf == nil {
		logf = market.NoopLog
	}
	return &tangledGraphVouchResolver{list: opts.ListRecords, log: logf}
}

func (r *tangledGraphVouchResolver) GetVouchedDIDs(ctx context.Context, did string) (map[string]struct{}, error) {
	vouched := map[string]struct{}{}
	if r.list == nil {
		return vouched, nil
	}
	records, err := r.list.ListRecords(ctx, did, market.VouchNSID)
	if err != nil {
		r.log("warn", "tangled-graph vouch lookup failed", map[string]any{"did": did, "error": err.Error()})
		return vouched, nil
	}
	for _, rec := range records {
		if rec.Value == nil {
			continue
		}
		if kind, _ := rec.Value["kind"].(string); kind == "denounce" {
			continue
		}
		rkey := rec.URI
		if i := strings.LastIndexByte(rkey, '/'); i >= 0 {
			rkey = rkey[i+1:]
		}
		if strings.HasPrefix(rkey, "did:") {
			vouched[rkey] = struct{}{}
		}
	}
	return vouched, nil
}

func (r *tangledGraphVouchResolver) IsVouched(ctx context.Context, voucher, vouchee string) (bool, error) {
	set, err := r.GetVouchedDIDs(ctx, voucher)
	if err != nil {
		return false, err
	}
	_, ok := set[vouchee]
	return ok, nil
}

type OperatorDiscoveryOptions struct {
	ListRecordsOwn    OwnLister
	ListRecordsPublic Lister
	Service           string
	Log               market.LogFunc
}

type badgeBlueKeysOperatorDiscovery struct {
	own     OwnLister
	public  Lister
	service string
	log     market.LogFunc
	mu      sync.Mutex
	cache   map[string][]string
}

func NewBadgeBlueKeysOperatorDiscovery(opts OperatorDiscoveryOptions) OperatorDiscovery {
	logf := opts.Log
	if logf == nil {
		logf = market.NoopLog
	}
	return &badgeBlueKeysOperatorDiscovery{
		own:     opts.ListRecordsOwn,
		public:  opts.ListRecordsPublic,
		service: opts.Service,
		log:     logf,
		cache:   map[string][]string{},
	}
}

func operatorOf(value map[string]any, subjectDID, service string) string {
	if value == nil || strings.TrimSpace(service) == "" {
		return ""
	}
	if keyID, _ := value["keyId"].(string); keyID != subjectDID {
		return ""
	}
	if svc, _ := value["service"].(string); svc != service {
		return ""
	}
	challenge, _ := value["challenge"].(string)
	if strings.HasPrefix(challenge, "did:") {
		return challenge
	}
	return ""
}

func (d *badgeBlueKeysOperatorDiscovery) DiscoverOperatorDIDs(ctx context.Context, atprotoDID string) ([]string, error) {
	d.mu.Lock()
	cached, ok := d.cache[atprotoDID]
	d.mu.Unlock()
	if ok {
		return cached, nil
	}
	dids := []string{}
	if d.own != nil {
		if records, err := d.own.ListRecordsOwn(ctx, market.BadgeBlueKeysNSID); err == nil {
			for _, rec := range records {
				if op := operatorOf(rec.Value, atprotoDID, d.service); op != "" {
					dids = append(dids, op)
				}
			}
		}
	}
	if len(dids) == 0 && d.public != nil {
		if records, err := d.public.ListRecords(ctx, atprotoDID, market.BadgeBlueKeysNSID); err == nil {
			for _, rec := range records {
				if op := operatorOf(rec.Value, atprotoDID, d.service); op != "" {
					dids = append(dids, op)
				}
			}
		}
	}
	if len(dids) > 0 {
		d.mu.Lock()
		d.cache[atprotoDID] = dids
		d.mu.Unlock()
		d.log("info", "operator discovery: discovered operator DIDs", map[string]any{"bidderDid": atprotoDID, "operatorDids": dids})
	}
	return dids, nil
}

type OperatedDiscoveryOptions struct {
	Operators         []string
	Service           string
	ListRecordsPublic Lister
	Log               market.LogFunc
}

func associates(value map[string]any, operator, service string) string {
	if value == nil {
		return ""
	}
	if svc, _ := value["service"].(string); svc != service {
		return ""
	}
	if challenge, _ := value["challenge"].(string); challenge != operator {
		return ""
	}
	keyID, _ := value["keyId"].(string)
	if strings.HasPrefix(keyID, "did:") && keyID != operator {
		return keyID
	}
	return ""
}

func DiscoverOperatedDIDs(ctx context.Context, opts OperatedDiscoveryOptions) []string {
	logf := opts.Log
	if logf == nil {
		logf = market.NoopLog
	}
	if opts.ListRecordsPublic == nil {
		return nil
	}
	found := map[string]bool{}
	out := []string{}
	for _, operator := range opts.Operators {
		claimed, err := opts.ListRecordsPublic.ListRecords(ctx, operator, market.BadgeBlueKeysNSID)
		if err != nil {
			claimed = nil
		}
		candidates := []string{}
		seen := map[string]bool{}
		for _, rec := range claimed {
			if did := associates(rec.Value, operator, opts.Service); did != "" && !seen[did] {
				seen[did] = true
				candidates = append(candidates, did)
			}
		}
		for _, did := range candidates {
			own, err := opts.ListRecordsPublic.ListRecords(ctx, did, market.BadgeBlueKeysNSID)
			if err != nil {
				own = nil
			}
			mirrored := false
			for _, rec := range own {
				if associates(rec.Value, operator, opts.Service) == did {
					mirrored = true
					break
				}
			}
			if mirrored {
				if !found[did] {
					found[did] = true
					out = append(out, did)
				}
			} else {
				logf("info", "operated discovery: the associated repo does not name the operator back", map[string]any{"operator": operator, "did": did})
			}
		}
	}
	return out
}

type DelegatedTrustOptions struct {
	VouchResolver  VouchResolver
	ListOwnRecords OwnLister
	Service        string
	Log            market.LogFunc
}

type badgeBlueKeysDelegatedTrust struct {
	vouch   VouchResolver
	own     OwnLister
	service string
	log     market.LogFunc
}

func NewBadgeBlueKeysDelegatedTrustResolver(opts DelegatedTrustOptions) DelegatedTrustResolver {
	logf := opts.Log
	if logf == nil {
		logf = market.NoopLog
	}
	return &badgeBlueKeysDelegatedTrust{vouch: opts.VouchResolver, own: opts.ListOwnRecords, service: opts.Service, log: logf}
}

func (d *badgeBlueKeysDelegatedTrust) GetDelegatedTrustedDIDs(ctx context.Context, selfDID string) (map[string]struct{}, error) {
	vouched := map[string]struct{}{}
	if d.vouch != nil {
		if own, err := d.vouch.GetVouchedDIDs(ctx, selfDID); err == nil {
			for did := range own {
				vouched[did] = struct{}{}
			}
		}
	}
	if d.own == nil {
		return vouched, nil
	}
	badge, err := d.own.ListRecordsOwn(ctx, market.BadgeBlueKeysNSID)
	if err != nil {
		d.log("warn", "delegated trust badgeBlueKeys lookup failed", map[string]any{"selfDid": selfDID, "error": err.Error()})
		return vouched, nil
	}
	candidates := []string{}
	seen := map[string]bool{}
	add := func(did string) {
		if strings.HasPrefix(did, "did:") && !seen[did] {
			seen[did] = true
			candidates = append(candidates, did)
		}
	}
	for _, rec := range badge {
		value := rec.Value
		if value == nil {
			continue
		}
		d.log("info", "delegated trust scanning badgeBlueKeys", map[string]any{"challenge": value["challenge"], "service": value["service"], "keyId": value["keyId"], "selfDid": selfDID})
		service, _ := value["service"].(string)
		if strings.TrimSpace(d.service) == "" || service != d.service {
			continue
		}
		challenge, _ := value["challenge"].(string)
		keyID, _ := value["keyId"].(string)
		if keyID == selfDID && strings.HasPrefix(challenge, "did:") {
			add(challenge)
		}
	}
	if d.vouch == nil {
		return vouched, nil
	}
	for _, operator := range candidates {
		opVouches, err := d.vouch.GetVouchedDIDs(ctx, operator)
		if err != nil {
			d.log("warn", "delegated trust operator vouch lookup failed", map[string]any{"keyId": operator, "error": err.Error()})
			continue
		}
		d.log("info", "delegated trust resolved operator vouches", map[string]any{"keyId": operator, "opVouchCount": len(opVouches)})
		for did := range opVouches {
			vouched[did] = struct{}{}
		}
	}
	return vouched, nil
}

func SortedDIDs(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for did := range set {
		out = append(out, did)
	}
	sort.Strings(out)
	return out
}
