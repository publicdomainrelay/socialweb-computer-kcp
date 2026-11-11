package identlink

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	EndorsementNSID = "link.ident.endorsement"
	GrantNSID       = "com.publicdomainrelay.temp.market.association"
	AgentOfPrefix   = "https://ident.link/agent-of/"
	DefaultMaxDepth = 100
	DefaultBudget   = 256
)

type Doc struct {
	AlsoKnownAs []string `json:"alsoKnownAs"`
}

type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

type IO interface {
	ResolveDID(ctx context.Context, did string) (*Doc, error)
	GetRecord(ctx context.Context, repo, collection, rkey string) (map[string]any, error)
	ContractHeld(ctx context.Context, contract StrongRef, authorities []string) (bool, error)
	Now() time.Time
}

type Options struct {
	MaxDepth int `json:"maxDepth,omitempty"`
	Budget   int `json:"budget,omitempty"`
}

type Ancestor struct {
	DID       string     `json:"did"`
	Depth     int        `json:"depth"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type Result struct {
	Agent     string     `json:"agent"`
	Ancestors []Ancestor `json:"ancestors"`
	Roots     []string   `json:"roots"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Truncated bool       `json:"truncated"`
	Errors    []string   `json:"errors,omitempty"`
}

func (r Result) Has(did string) bool {
	return slices.ContainsFunc(r.Ancestors, func(a Ancestor) bool { return a.DID == did })
}

func (r Result) Side() []string {
	out := []string{r.Agent}
	for _, a := range r.Ancestors {
		out = append(out, a.DID)
	}
	return out
}

func IsDID(s string) bool {
	rest, ok := strings.CutPrefix(s, "did:")
	method, id, found := strings.Cut(rest, ":")
	if !ok || !found || len(s) > 2048 || method == "" || id == "" || strings.HasSuffix(id, ":") || strings.HasSuffix(id, "%") {
		return false
	}
	for _, c := range []byte(method) {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:%-", c) >= 0) {
			return false
		}
	}
	return true
}

func AgentOf(alsoKnownAs []string) []string {
	var out []string
	for _, entry := range alsoKnownAs {
		did, ok := strings.CutPrefix(entry, AgentOfPrefix)
		if ok && IsDID(did) && !slices.Contains(out, did) {
			out = append(out, did)
		}
	}
	return out
}

type edge struct {
	from, to  string
	expiresAt *time.Time
	contract  *StrongRef
	alive     bool
}

type node struct {
	did   string
	depth int
	path  []string
}

func Walk(ctx context.Context, io IO, agent string, opts Options) Result {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxDepth
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	res := Result{Agent: agent, Ancestors: []Ancestor{}, Roots: []string{}}
	now := io.Now()
	stuck := map[string]bool{}
	seen := map[string]bool{agent: true}
	var edges []*edge
	queue := []node{{did: agent, path: []string{agent}}}
	lookups := 0
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		doc, err := io.ResolveDID(ctx, x.did)
		if err != nil {
			res.Errors = append(res.Errors, "resolveDid "+x.did+": "+err.Error())
			continue
		}
		if doc == nil {
			continue
		}
		for _, p := range AgentOf(doc.AlsoKnownAs) {
			if slices.Contains(x.path, p) {
				stuck[x.did] = true
				continue
			}
			if x.depth < opts.MaxDepth {
				lookups += 2
			}
			if x.depth+1 > opts.MaxDepth || lookups > opts.Budget {
				res.Truncated = true
				stuck[x.did] = true
				continue
			}
			e, g := fetch(ctx, io, p, x.did, &res.Errors)
			if e == nil || e["agent"] != x.did || g == nil {
				continue
			}
			held, ok := grant(g, x.did, now)
			if !ok {
				continue
			}
			held.from, held.to = x.did, p
			edges = append(edges, held)
			if !seen[p] {
				seen[p] = true
				queue = append(queue, node{did: p, depth: x.depth + 1, path: append(slices.Clone(x.path), p)})
			}
		}
	}
	res.Errors = append(res.Errors, checkContracts(ctx, io, edges)...)
	settle(&res, edges, stuck, opts.MaxDepth)
	return res
}

func fetch(ctx context.Context, io IO, principal, agent string, errs *[]string) (map[string]any, map[string]any) {
	var e, g map[string]any
	var eErr, gErr error
	var wg sync.WaitGroup
	wg.Go(func() { e, eErr = io.GetRecord(ctx, principal, EndorsementNSID, agent) })
	wg.Go(func() { g, gErr = io.GetRecord(ctx, principal, GrantNSID, agent) })
	wg.Wait()
	for _, f := range []struct {
		collection string
		err        error
	}{{EndorsementNSID, eErr}, {GrantNSID, gErr}} {
		if f.err != nil {
			*errs = append(*errs, "getRecord at://"+principal+"/"+f.collection+"/"+agent+": "+f.err.Error())
		}
	}
	if eErr != nil {
		e = nil
	}
	if gErr != nil {
		g = nil
	}
	return e, g
}

func grant(g map[string]any, agent string, now time.Time) (*edge, bool) {
	if g["agent"] != agent {
		return nil, false
	}
	out := &edge{alive: true}
	if raw, ok := g["expiresAt"]; ok {
		text, _ := raw.(string)
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || !now.Before(at) {
			return nil, false
		}
		at = at.UTC()
		out.expiresAt = &at
	}
	if raw, ok := g["contract"]; ok {
		fields, _ := raw.(map[string]any)
		uri, okURI := fields["uri"].(string)
		cid, okCID := fields["cid"].(string)
		if !okURI || !okCID || out.expiresAt == nil {
			return nil, false
		}
		out.contract = &StrongRef{URI: uri, CID: cid}
	}
	return out, true
}

func earlier(a, b *time.Time) *time.Time {
	if a == nil || (b != nil && b.Before(*a)) {
		return b
	}
	return a
}

func up(edges []*edge, start string) []Ancestor {
	type at struct {
		did       string
		depth     int
		expiresAt *time.Time
	}
	out := []Ancestor{}
	seen := map[string]bool{start: true}
	queue := []at{{did: start}}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, e := range edges {
			if !e.alive || e.from != x.did || seen[e.to] {
				continue
			}
			seen[e.to] = true
			next := at{did: e.to, depth: x.depth + 1, expiresAt: earlier(x.expiresAt, e.expiresAt)}
			out = append(out, Ancestor{DID: next.did, Depth: next.depth, ExpiresAt: next.expiresAt})
			queue = append(queue, next)
		}
	}
	return out
}

func authoritiesOf(edges []*edge, principal string) []string {
	out := []string{principal}
	for _, a := range up(edges, principal) {
		out = append(out, a.DID)
	}
	return out
}

func checkContracts(ctx context.Context, io IO, edges []*edge) []string {
	var errs []string
	held := map[*edge][]string{}
	for {
		type check struct {
			e           *edge
			authorities []string
			held        bool
			err         error
		}
		var checks []*check
		for _, e := range edges {
			if !e.alive || e.contract == nil {
				continue
			}
			authorities := authoritiesOf(edges, e.to)
			if prev, ok := held[e]; ok && slices.Equal(prev, authorities) {
				continue
			}
			checks = append(checks, &check{e: e, authorities: authorities})
		}
		if len(checks) == 0 {
			return errs
		}
		var wg sync.WaitGroup
		for _, c := range checks {
			wg.Go(func() { c.held, c.err = io.ContractHeld(ctx, *c.e.contract, c.authorities) })
		}
		wg.Wait()
		for _, c := range checks {
			switch {
			case c.err != nil:
				errs = append(errs, "contractHeld "+c.e.contract.URI+": "+c.err.Error())
				c.e.alive = false
			case c.held:
				held[c.e] = c.authorities
			default:
				c.e.alive = false
			}
		}
	}
}

func settle(res *Result, edges []*edge, stuck map[string]bool, maxDepth int) {
	reached := up(edges, res.Agent)
	for _, a := range reached {
		if a.Depth <= maxDepth {
			res.Ancestors = append(res.Ancestors, a)
		}
	}
	res.Truncated = res.Truncated || len(res.Ancestors) < len(reached)
	type candidate struct {
		did       string
		expiresAt *time.Time
	}
	nodes := []candidate{{did: res.Agent}}
	for _, a := range res.Ancestors {
		nodes = append(nodes, candidate{did: a.DID, expiresAt: a.ExpiresAt})
	}
	for _, n := range nodes {
		if stuck[n.did] || slices.ContainsFunc(edges, func(e *edge) bool { return e.alive && e.from == n.did }) {
			continue
		}
		res.Roots = append(res.Roots, n.did)
		res.ExpiresAt = earlier(res.ExpiresAt, n.expiresAt)
	}
}

func HandleOK(ctx context.Context, dir identity.Directory, walk Result, doc *Doc, handle string) bool {
	h, err := syntax.ParseHandle(strings.TrimSpace(handle))
	if err != nil || dir == nil || doc == nil {
		return false
	}
	h = h.Normalize()
	declared := slices.ContainsFunc(doc.AlsoKnownAs, func(entry string) bool { return strings.EqualFold(entry, "at://"+h.String()) })
	if !declared {
		return false
	}
	if verifiedDID(ctx, dir, h) == walk.Agent {
		return true
	}
	for rest := h.String(); ; {
		_, parent, ok := strings.Cut(rest, ".")
		if !ok || !strings.Contains(parent, ".") {
			return false
		}
		rest = parent
		p, err := syntax.ParseHandle(parent)
		if err != nil {
			return false
		}
		if did := verifiedDID(ctx, dir, p); did != "" && walk.Has(did) {
			return true
		}
	}
}

func verifiedDID(ctx context.Context, dir identity.Directory, h syntax.Handle) string {
	ident, err := dir.LookupHandle(ctx, h)
	if err != nil || ident == nil {
		return ""
	}
	return ident.DID.String()
}
