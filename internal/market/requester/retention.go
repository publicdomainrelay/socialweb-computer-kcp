package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
)

const (
	DefaultRetentionDays = 30

	DefaultInfraRetentionDays = 7

	DefaultRetentionMode = v1alpha1.RetentionModeDryRun

	RetentionMaxDays = 3650

	RetentionMaxDelaySeconds = 86400

	RetentionInterval = 24 * time.Hour

	RetentionStartDelay = 5 * time.Minute

	retentionMarkerFile = "retention-last-run"

	RetentionDeclaredRKeyPrefix = "fixture-"

	RetentionDeclaredSelfRKey = "self"

	RetentionListCap = 20000

	RetentionReadCap = 5000

	RetentionDeleteCap = 2000

	RetentionDeleteGap = time.Second

	RetentionReadGap = 100 * time.Millisecond

	RetentionTokenBound = 2 * AtircTokenLifetime

	retentionTIDSkew = time.Hour

	retentionPageSize = 100

	retentionCallTimeout = 30 * time.Second

	retentionSample = 20

	infraKindEphemeral = contractmarket.KindEphemeralInfra
)

var retentionPayloadNSIDs = []string{
	market.ComputeEventsVMDeleteNSID,
	market.ComputeEventsGuestPublicKeyNSID,
	market.ComputeEventsGuestSignalWebRTCNSID,
	market.ComputeEventsGuestSecretsNSID,
	market.ComputeEventsGuestAgentStartNSID,
	market.ComputeEventsGuestSecretsUpdatedNSID,
	market.ComputeEventsGuestR2StartNSID,
	market.ComputeEventsGuestR2ReleaseNSID,
}

func RetentionCollections() []string {
	out := []string{market.AcceptNSID, market.EventNSID, market.RFPNSID, market.ComputeVMNSID, PolicyGhaLiteNSID}
	out = append(out, retentionPayloadNSIDs...)
	return append(out, infra.EphemeralInfraNSID)
}

type infraBinding struct {
	kind string

	name string

	uris []string

	unbound bool
}

func (q *Requester) observeInfra(change contractmarket.Change) {
	key := change.Kind + "/" + change.Name
	var spec *v1alpha1.InfraRecordBinding
	var status *v1alpha1.InfraRecordStatus
	switch {
	case change.Gone:
		q.infras.Remove(key)
		return
	case change.Infra != nil:
		spec, status = change.Infra.Spec.Record, change.Infra.Status.Record
	case change.EphemeralInfra != nil:
		spec, status = change.EphemeralInfra.Spec.Record, change.EphemeralInfra.Status.Record
	default:
		q.infras.Remove(key)
		return
	}
	b := infraBinding{kind: change.Kind, name: change.Name, unbound: spec == nil || strings.TrimSpace(spec.URI) == ""}
	if spec != nil && strings.TrimSpace(spec.URI) != "" {
		b.uris = append(b.uris, strings.TrimSpace(spec.URI))
	}
	if status != nil && strings.TrimSpace(status.URI) != "" {
		b.uris = append(b.uris, strings.TrimSpace(status.URI))
	}
	q.infras.Put(key, b)
}

type retentionConfig struct {
	mode v1alpha1.RetentionMode

	age time.Duration

	infraAge time.Duration

	delay time.Duration

	delaySet bool

	keep []string

	rejected []string
}

func retentionDays(value *int32, fallback int32) time.Duration {
	days := min(max(ptr.Deref(value, fallback), 0), RetentionMaxDays)
	return time.Duration(days) * 24 * time.Hour
}

func (c Config) retention() retentionConfig {
	spec := ptr.Deref(c.Spec.Retention, v1alpha1.RequesterRetention{})
	out := retentionConfig{
		mode:     spec.Mode,
		age:      retentionDays(spec.Days, DefaultRetentionDays),
		infraAge: retentionDays(spec.InfraDays, DefaultInfraRetentionDays),
		delay:    RetentionStartDelay,
	}
	if spec.DelaySeconds != nil {
		out.delay, out.delaySet = time.Duration(min(max(*spec.DelaySeconds, 0), RetentionMaxDelaySeconds))*time.Second, true
	}
	if out.mode != v1alpha1.RetentionModeDelete {
		out.mode = DefaultRetentionMode
	}
	for _, k := range spec.Keep {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		normalized, ok := normalizeKeep(k)
		if !ok {
			out.rejected = append(out.rejected, k)
			continue
		}
		out.keep = append(out.keep, normalized)
	}
	return out
}

func normalizeKeep(k string) (string, bool) {
	rest, isURI := strings.CutPrefix(k, "at://")
	if !isURI {
		return strings.TrimSuffix(k, "/"), !strings.Contains(k, "://")
	}
	authority, _, _ := strings.Cut(rest, "/")
	if _, err := syntax.ParseDID(authority); err != nil {
		return "", false
	}
	if prefix, ok := strings.CutSuffix(k, "*"); ok {
		return prefix + "*", true
	}
	uri, err := syntax.ParseATURI(strings.TrimSuffix(k, "/"))
	if err != nil {
		return "", false
	}
	return uri.String(), true
}

func (r retentionConfig) kept(rec *retainedRecord) bool {
	if rec.rkey == RetentionDeclaredSelfRKey || strings.HasPrefix(rec.rkey, RetentionDeclaredRKeyPrefix) {
		return true
	}
	key := rec.collection + "/" + rec.rkey
	for _, k := range r.keep {
		if k == rec.uri || k == key || k == rec.collection {
			return true
		}
		if prefix, ok := strings.CutSuffix(k, "*"); ok && (strings.HasPrefix(rec.uri, prefix) || strings.HasPrefix(key, prefix)) {
			return true
		}
	}
	return false
}

type retainedRecord struct {
	uri string

	cid string

	collection string

	rkey string

	at time.Time

	ageReason string

	tidKeyed bool

	rfp string

	bid string

	payload string

	receipt string

	policies []string

	infra *infra.EphemeralInfra
}

type retentionLive struct {
	uris map[string]bool

	receipts map[string]bool

	infraURIs map[string]bool

	infraNames map[string]bool
}

type retentionRun struct {
	q *Requester

	cfg retentionConfig

	stop context.Context

	now time.Time

	listed map[string][]*retainedRecord

	byURI map[string]*retainedRecord

	live retentionLive

	verdicts map[string]string

	plannedURIs map[string]bool

	groups [][]*retainedRecord

	keptFor map[string]int

	reads int

	failures []string

	capped bool
}

func (q *Requester) startRetention() {
	cfg := q.cfg.retention()
	if len(cfg.rejected) > 0 {
		q.log.Warn("requester_retention_keep_rejected", "entries", cfg.rejected, "reason", "a keep at-uri must name a DID repo")
	}
	if cfg.age <= 0 && cfg.infraAge <= 0 {
		q.log.Info("requester_retention_off")
		return
	}
	q.retentionOnce.Do(func() {
		q.wg.Go(func() { q.runRetention(cfg) })
	})
}

func (q *Requester) runRetention(cfg retentionConfig) {
	stop := q.runContext()
	delay := cfg.delay
	if !cfg.delaySet && cfg.mode == v1alpha1.RetentionModeDelete {
		delay = q.retentionDelay(delay)
	}
	q.log.Info("requester_retention_scheduled", "mode", cfg.mode, "in", delay.Round(time.Second).String(), "every", RetentionInterval.String())
	if sleepCtx(stop, delay) != nil {
		return
	}
	q.sweepRetention(stop, cfg)
	ticker := time.NewTicker(RetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop.Done():
			return
		case <-ticker.C:
			q.sweepRetention(stop, cfg)
		}
	}
}

func (q *Requester) retentionDelay(delay time.Duration) time.Duration {
	path := q.cfg.stateFile(retentionMarkerFile)
	if path == "" {
		return delay
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return delay
	}
	last, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return delay
	}
	return max(delay, min(last.Add(RetentionInterval).Sub(q.now()), RetentionInterval))
}

func (q *Requester) markRetention() {
	if path := q.cfg.stateFile(retentionMarkerFile); path != "" {
		_ = os.WriteFile(path, []byte(q.now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
}

func (q *Requester) sweepRetention(stop context.Context, cfg retentionConfig) {
	run := &retentionRun{q: q, cfg: cfg, stop: stop, now: q.now(), listed: map[string][]*retainedRecord{}, byURI: map[string]*retainedRecord{},
		verdicts: map[string]string{}, plannedURIs: map[string]bool{}, keptFor: map[string]int{}}
	started := time.Now()
	run.live = q.retentionLive()
	listedCount, complete, err := run.list()
	if err != nil || !complete {
		q.log.Warn("requester_retention_skipped", "mode", cfg.mode, "listed", listedCount, "cap", RetentionListCap,
			"complete", complete, "err", fmt.Sprint(err), "reason", "no record is deleted unless every swept collection is listed in full")
		return
	}
	run.plan()
	would, deleted, noScope, dropped := run.apply()
	if stop.Err() != nil {
		return
	}
	if cfg.mode == v1alpha1.RetentionModeDelete {
		q.markRetention()
	}
	sample := []string{}
	for _, group := range run.groups {
		for _, rec := range group {
			if len(sample) < retentionSample {
				sample = append(sample, rec.uri)
			}
		}
	}
	q.log.Info("requester_retention_run", "mode", cfg.mode, "days", int(cfg.age/(24*time.Hour)), "infraDays", int(cfg.infraAge/(24*time.Hour)),
		"listed", run.counts(), "would", would, "deleted", deleted, "failed", len(run.failures), "failedSample", sampleOf(run.failures),
		"noDeleteScope", noScope, "liveAtDelete", dropped, "capped", run.capped, "kept", run.keptFor, "reads", run.reads,
		"seconds", int(time.Since(started).Seconds()), "sample", sample)
}

func sampleOf(values []string) []string {
	return values[:min(len(values), retentionSample)]
}

func (run *retentionRun) counts() map[string]int {
	out := map[string]int{}
	for collection, records := range run.listed {
		out[shortNSID(collection)] = len(records)
	}
	return out
}

func shortNSID(nsid string) string {
	for _, prefix := range []string{"com.publicdomainrelay.temp.", "computer.socialweb.temp.", "computer.socialweb."} {
		if rest, ok := strings.CutPrefix(nsid, prefix); ok {
			return rest
		}
	}
	return nsid
}

func retentionFinished(c v1alpha1.ComputeContract) bool {
	if c.DeletionTimestamp != nil {
		return false
	}
	if c.Status.Phase != v1alpha1.ComputeContractReleased && c.Status.Phase != v1alpha1.ComputeContractFailed {
		return false
	}
	if state := c.Status.Contract; state != nil && state.Receipt != nil && state.Receipt.URI != "" &&
		state.Termination == nil && (state.Release == nil || !state.Release.OK) {
		return false
	}
	switch c.Status.AtircRevokes {
	case v1alpha1.AtircRevokesDone, v1alpha1.AtircRevokesAbandoned:
		return true
	case v1alpha1.AtircRevokesPending:
		return false
	}
	return c.Spec.Agent == nil && c.Spec.Association == nil
}

func (q *Requester) retentionLive() retentionLive {
	live := retentionLive{uris: map[string]bool{}, receipts: map[string]bool{}, infraURIs: map[string]bool{}, infraNames: map[string]bool{}}
	for _, b := range q.infras.List() {
		for _, uri := range b.uris {
			live.infraURIs[uri] = true
		}
		live.infraNames[b.name] = true
	}
	for _, c := range q.contracts.List() {
		if kind, name := c.Labels[v1alpha1.InfraKindLabel], c.Labels[v1alpha1.InfraNameLabel]; name != "" && kind == infraKindEphemeral {
			live.infraNames[name] = true
		}
		for _, owner := range c.OwnerReferences {
			if owner.Kind == infraKindEphemeral {
				live.infraNames[owner.Name] = true
			}
		}
		if retentionFinished(c) {
			continue
		}
		live.protect(c.Status)
		if state := c.Status.Contract; state != nil && state.Receipt != nil && state.Receipt.URI != "" {
			live.receipts[state.Receipt.URI] = true
		}
	}
	q.mu.Lock()
	runners := slices.Collect(maps.Values(q.runners))
	q.mu.Unlock()
	for _, runner := range runners {
		if state := runner.stateCopy(); state != nil {
			live.protect(state)
			if state.Receipt != nil && state.Receipt.URI != "" {
				live.receipts[state.Receipt.URI] = true
			}
		}
	}
	return live
}

func (l retentionLive) protect(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return
	}
	collectATURIs(decoded, l.uris)
}

func (l retentionLive) holds(rec *retainedRecord) bool {
	if l.uris[rec.uri] || (rec.receipt != "" && l.receipts[rec.receipt]) {
		return true
	}
	if rec.collection == infra.EphemeralInfraNSID {
		return l.infraURIs[rec.uri] || l.infraNames[rec.rkey]
	}
	return false
}

func collectATURIs(v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for _, x := range t {
			collectATURIs(x, out)
		}
	case []any:
		for _, x := range t {
			collectATURIs(x, out)
		}
	case string:
		if strings.HasPrefix(t, "at://") {
			out[t] = true
		}
	}
}

func (run *retentionRun) list() (int, bool, error) {
	total := 0
	for _, collection := range RetentionCollections() {
		cursor := ""
		for {
			if total >= RetentionListCap {
				return total, false, nil
			}
			ctx, cancel := context.WithTimeout(run.stop, retentionCallTimeout)
			page, err := run.q.repo.ListRecords(ctx, run.q.repoDID, collection, retentionPageSize, cursor)
			cancel()
			if err != nil {
				return total, false, fmt.Errorf("list %s: %w", collection, err)
			}
			for _, rec := range page.Records {
				total++
				parsed := run.retainedOf(collection, rec)
				if parsed == nil {
					run.keepReason("unparsed")
					continue
				}
				run.listed[collection] = append(run.listed[collection], parsed)
				run.byURI[parsed.uri] = parsed
			}
			if page.Cursor == "" || len(page.Records) == 0 {
				break
			}
			if page.Cursor == cursor {
				return total, false, fmt.Errorf("list %s: the PDS repeated the cursor %q", collection, cursor)
			}
			cursor = page.Cursor
		}
	}
	return total, true, nil
}

func (run *retentionRun) retainedOf(collection string, rec market.ListedRecord) *retainedRecord {
	uri, err := syntax.ParseATURI(rec.URI)
	if err != nil || uri.Authority().String() != run.q.repoDID || uri.Collection().String() != collection || rec.CID == "" {
		return nil
	}
	out := &retainedRecord{uri: rec.URI, cid: rec.CID, collection: collection, rkey: uri.RecordKey().String()}
	tid, tidErr := syntax.ParseTID(out.rkey)
	raw, present := rec.Value["createdAt"]
	created, _ := raw.(string)
	switch {
	case present && strings.TrimSpace(created) != "":
		createdAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(created))
		switch {
		case err != nil:
			out.ageReason = "badCreatedAt"
		case createdAt.After(run.now):
			out.ageReason = "futureCreatedAt"
		default:
			out.at = createdAt
			if tidErr == nil {
				skew := tid.Time().Sub(createdAt)
				out.tidKeyed = skew < retentionTIDSkew && skew > -retentionTIDSkew
			}
		}
	case present:
		out.ageReason = "badCreatedAt"
	case tidErr == nil && !tid.Time().After(run.now):
		out.at = tid.Time()
		out.tidKeyed = true
	default:
		out.ageReason = "noAge"
	}
	switch collection {
	case market.AcceptNSID:
		out.rfp, out.bid, out.payload = refURI(rec.Value, "rfp"), refURI(rec.Value, "bid"), refURI(rec.Value, "payload")
		if bidder := market.ATURIAuthority(out.bid); strings.HasPrefix(bidder, "did:") && bidder != run.q.repoDID {
			out.receipt = "at://" + bidder + "/" + market.ReceiptNSID + "/" + market.ReceiptRKey(out.uri, out.cid)
		}
	case market.EventNSID:
		out.receipt, out.payload = refURI(rec.Value, "receipt"), refURI(rec.Value, "payload")
	case market.RFPNSID:
		out.payload = refURI(rec.Value, "payload")
		if list, ok := rec.Value["policies"].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					if u, _ := m["uri"].(string); u != "" {
						out.policies = append(out.policies, u)
					}
				}
			}
		}
	case infra.EphemeralInfraNSID:
		parsed, err := infra.ParseEphemeralInfra(rec.Value)
		if err != nil {
			return nil
		}
		out.infra = parsed
	}
	return out
}

func refURI(value map[string]any, key string) string {
	m, _ := value[key].(map[string]any)
	u, _ := m["uri"].(string)
	return strings.TrimSpace(u)
}

func (run *retentionRun) keepReason(reason string) {
	run.keptFor[reason]++
}

func (run *retentionRun) keep(reason string) bool {
	run.keepReason(reason)
	return false
}

func (run *retentionRun) eligible(rec *retainedRecord, age time.Duration) bool {
	switch {
	case rec == nil:
		return false
	case run.plannedURIs[rec.uri]:
		return true
	case rec.ageReason != "":
		return run.keep(rec.ageReason)
	case run.now.Sub(rec.at) < age:
		return run.keep("young")
	case run.live.holds(rec):
		return run.keep("liveContract")
	case run.cfg.kept(rec):
		return run.keep("declared")
	}
	return true
}

func (run *retentionRun) add(group ...*retainedRecord) {
	out := make([]*retainedRecord, 0, len(group))
	for _, rec := range group {
		if rec == nil || run.plannedURIs[rec.uri] {
			continue
		}
		run.plannedURIs[rec.uri] = true
		out = append(out, rec)
	}
	if len(out) > 0 {
		run.groups = append(run.groups, out)
	}
}

func (run *retentionRun) plan() {
	if run.cfg.age > 0 {
		run.planEvents()
		run.planRFPs(run.finishedAccepts())
	}
	if run.cfg.infraAge > 0 {
		run.planInfra()
	}
}

func (run *retentionRun) planEvents() {
	self := run.q.repoDID
	for _, event := range run.listed[market.EventNSID] {
		if event.receipt == "" || market.ATURIAuthority(event.receipt) == self || market.ATURIAuthority(event.payload) != self ||
			!slices.Contains(retentionPayloadNSIDs, market.NSIDFromURI(event.payload)) {
			run.keepReason("notRequesterEvent")
			continue
		}
		if !run.eligible(event, run.cfg.age) {
			continue
		}
		payload := run.byURI[event.payload]
		if payload != nil && !run.eligible(payload, run.cfg.age) {
			continue
		}
		if reason := run.terminated(event.receipt); reason != "" {
			run.keepReason(reason)
			continue
		}
		run.add(payload, event)
	}
}

func (run *retentionRun) finishedAccepts() map[string]bool {
	finished := map[string]bool{}
	for _, accept := range run.listed[market.AcceptNSID] {
		if !run.eligible(accept, run.cfg.age) {
			continue
		}
		if accept.receipt == "" {
			run.keepReason("noBidder")
			continue
		}
		if reason := run.acceptVerdict(accept.receipt, accept.bid); reason != "" {
			run.keepReason(reason)
			continue
		}
		finished[accept.uri] = true
	}
	return finished
}

func (run *retentionRun) planRFPs(finishedAccepts map[string]bool) {
	acceptsOf := map[string][]*retainedRecord{}
	for _, accept := range run.listed[market.AcceptNSID] {
		acceptsOf[accept.rfp] = append(acceptsOf[accept.rfp], accept)
		if run.byURI[accept.rfp] == nil {
			run.keepReason("orphanAccept")
		}
	}
	rfpsOf := map[string][]*retainedRecord{}
	for _, rfp := range run.listed[market.RFPNSID] {
		for _, ref := range append([]string{rfp.payload}, rfp.policies...) {
			rfpsOf[ref] = append(rfpsOf[ref], rfp)
		}
	}
	finishedRFP := map[string]bool{}
	for _, rfp := range run.listed[market.RFPNSID] {
		if !run.eligible(rfp, run.cfg.age) {
			continue
		}
		if !rfp.tidKeyed {
			run.keepReason("reusableRKey")
			continue
		}
		accepts := acceptsOf[rfp.uri]
		if len(accepts) == 0 && run.now.Sub(rfp.at) < market.RFPBidWindow+RetentionTokenBound {
			run.keepReason("rfpWindowOpen")
			continue
		}
		open := false
		for _, accept := range accepts {
			if !finishedAccepts[accept.uri] {
				open = true
			}
		}
		if open {
			run.keepReason("openAccept")
			continue
		}
		finishedRFP[rfp.uri] = true
	}
	for _, rfp := range run.listed[market.RFPNSID] {
		if !finishedRFP[rfp.uri] {
			continue
		}
		group, held := []*retainedRecord{}, false
		for _, ref := range append([]string{rfp.payload}, rfp.policies...) {
			dep := run.byURI[ref]
			if dep == nil || market.ATURIAuthority(ref) != run.q.repoDID || run.plannedURIs[ref] {
				continue
			}
			shared := false
			for _, other := range rfpsOf[ref] {
				if !finishedRFP[other.uri] {
					shared = true
				}
			}
			switch {
			case shared:
				run.keepReason("sharedWithOpenRFP")
				continue
			case run.cfg.kept(dep) || run.live.holds(dep):
				held = true
			case !run.eligible(dep, run.cfg.age):
				continue
			case !dep.tidKeyed:
				run.keepReason("reusableRKey")
				continue
			}
			group = append(group, dep)
		}
		if held {
			run.keepReason("declaredGroup")
			continue
		}
		group = append(group, acceptsOf[rfp.uri]...)
		run.add(append(group, rfp)...)
	}
}

func (run *retentionRun) planInfra() {
	for _, rec := range run.listed[infra.EphemeralInfraNSID] {
		if !run.eligible(rec, run.cfg.infraAge) {
			continue
		}
		if reason := run.infraVerdict(rec); reason != "" {
			run.keepReason(reason)
			continue
		}
		run.add(rec)
	}
}

func (run *retentionRun) infraVerdict(rec *retainedRecord) string {
	ei := rec.infra
	var end time.Time
	if ei.Status != nil && ei.Status.EndedAt != "" {
		end, _ = time.Parse(time.RFC3339Nano, ei.Status.EndedAt)
	}
	if end.IsZero() && ei.Status != nil && ei.Status.ExpiresAt != "" {
		end, _ = time.Parse(time.RFC3339Nano, ei.Status.ExpiresAt)
	}
	if end.IsZero() && ei.TtlSeconds > 0 {
		end = rec.at.Add(time.Duration(min(ei.TtlSeconds, int64(RetentionMaxDays)*86400)) * time.Second)
	}
	if end.IsZero() || run.now.Sub(end) < RetentionTokenBound {
		return "infraNotEnded"
	}
	status := map[string]infra.InfraContractStatus{}
	names := []string{}
	if ei.Status != nil {
		for _, s := range ei.Status.Contracts {
			status[s.Name] = s
			names = append(names, s.Name)
		}
	}
	spec := map[string]infra.InfraContract{}
	for _, c := range ei.Contracts {
		spec[c.Name] = c
		if _, ok := status[c.Name]; !ok {
			return "infraContractNoStatus"
		}
	}
	for _, name := range names {
		s := status[name]
		if s.Receipt != "" && run.live.receipts[s.Receipt] {
			return "liveContract"
		}
		switch v1alpha1.InfraContractPhase(s.Phase) {
		case v1alpha1.InfraContractPhaseReleased:
			continue
		case v1alpha1.InfraContractPhaseStaged:
			if c, ok := spec[name]; ok && c.State == "staged" && s.Receipt == "" {
				continue
			}
			return "infraContractOpen"
		case v1alpha1.InfraContractPhaseFailed:
			if s.Receipt == "" {
				continue
			}
		default:
			if s.Receipt == "" {
				return "infraContractOpen"
			}
		}
		if reason := run.terminated(s.Receipt); reason != "" {
			return reason
		}
	}
	return ""
}

func (run *retentionRun) read(repo, collection, rkey string) (market.ResolvedRecord, error) {
	if run.reads >= RetentionReadCap {
		return market.ResolvedRecord{}, errRetentionReadCap
	}
	if err := sleepCtx(run.stop, RetentionReadGap); err != nil {
		return market.ResolvedRecord{}, err
	}
	run.reads++
	ctx, cancel := context.WithTimeout(run.stop, retentionCallTimeout)
	defer cancel()
	return run.q.repo.GetRecord(ctx, repo, collection, rkey)
}

var errRetentionReadCap = errors.New("requester: the retention read cap is reached")

func recordAbsent(err error) bool {
	if errors.Is(err, atprotorepo.ErrNotFound) {
		return true
	}
	failure, ok := errors.AsType[*xrpc.Error](err)
	return ok && failure.Status == 400 && failure.Name == "RecordNotFound"
}

func unreadable(err error) string {
	if errors.Is(err, errRetentionReadCap) {
		return "readCap"
	}
	return "unreadable"
}

func (run *retentionRun) terminated(receipt string) string {
	key := "terminated|" + receipt
	if reason, ok := run.verdicts[key]; ok {
		return reason
	}
	reason := run.readTermination(receipt)
	if reason != "readCap" {
		run.verdicts[key] = reason
	}
	return reason
}

func (run *retentionRun) readTermination(receipt string) string {
	bidder := market.ATURIAuthority(receipt)
	if !strings.HasPrefix(bidder, "did:") {
		return "noBidder"
	}
	rkey, _ := market.EventRKeys(market.TerminationKey(receipt))
	got, err := run.read(bidder, market.ComputeEventsVMTerminatedNSID, rkey)
	switch {
	case err == nil:
	case recordAbsent(err):
		return "noTermination"
	default:
		return unreadable(err)
	}
	at := time.Time{}
	for _, key := range []string{"observedAt", "createdAt"} {
		if text, _ := got.Value[key].(string); text != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil && parsed.After(at) {
				at = parsed
			}
		}
	}
	if at.IsZero() || at.After(run.now) || run.now.Sub(at) < RetentionTokenBound {
		return "terminatedRecently"
	}
	return ""
}

func (run *retentionRun) acceptVerdict(receipt, bid string) string {
	reason := run.terminated(receipt)
	if reason != "noTermination" {
		return reason
	}
	key := "receipt|" + receipt
	if cached, ok := run.verdicts[key]; ok {
		return cached
	}
	verdict := run.readReceiptAbsence(receipt, bid)
	if verdict != "readCap" {
		run.verdicts[key] = verdict
	}
	return verdict
}

func (run *retentionRun) readReceiptAbsence(receipt, bid string) string {
	bidder := market.ATURIAuthority(receipt)
	aturi, err := syntax.ParseATURI(receipt)
	if err != nil {
		return "noTermination"
	}
	if _, err := run.read(bidder, market.ReceiptNSID, aturi.RecordKey().String()); err == nil {
		return "noTermination"
	} else if !recordAbsent(err) {
		return unreadable(err)
	}
	bidURI, err := syntax.ParseATURI(bid)
	if err != nil || bidURI.Authority().String() != bidder {
		return "noTermination"
	}
	if _, err := run.read(bidder, bidURI.Collection().String(), bidURI.RecordKey().String()); err != nil {
		return "noTermination"
	}
	return ""
}

func (run *retentionRun) deletable(collection string, cache map[string]bool) bool {
	if allowed, ok := cache[collection]; ok {
		return allowed
	}
	allowed := true
	if run.q.cfg.IdentityMode == IdentityModeOAuth {
		ctx, cancel := context.WithTimeout(run.stop, retentionCallTimeout)
		ok, err := run.q.oauthGranted(ctx, "repo:"+collection+"?action=delete")
		cancel()
		allowed = err == nil && ok
	}
	cache[collection] = allowed
	return allowed
}

func (run *retentionRun) apply() (map[string]int, map[string]int, map[string]int, int) {
	would, deleted, noScope := map[string]int{}, map[string]int{}, map[string]int{}
	dropped, attempts := 0, 0
	scopes := map[string]bool{}
	allowed := make([]bool, len(run.groups))
	for i, group := range run.groups {
		allowed[i] = true
		for _, rec := range group {
			would[shortNSID(rec.collection)]++
			if !run.deletable(rec.collection, scopes) {
				allowed[i] = false
			}
		}
		if !allowed[i] {
			for _, rec := range group {
				noScope[shortNSID(rec.collection)]++
			}
		}
	}
	if run.cfg.mode != v1alpha1.RetentionModeDelete {
		return would, deleted, noScope, dropped
	}
groups:
	for i, group := range run.groups {
		if !allowed[i] {
			continue
		}
		live := run.q.retentionLive()
		for _, rec := range group {
			if live.holds(rec) {
				dropped++
				continue groups
			}
		}
		for _, rec := range group {
			if attempts >= RetentionDeleteCap {
				run.capped = true
				return would, deleted, noScope, dropped
			}
			if err := sleepCtx(run.stop, RetentionDeleteGap); err != nil {
				return would, deleted, noScope, dropped
			}
			attempts++
			ctx, cancel := context.WithTimeout(run.stop, retentionCallTimeout)
			err := run.q.repo.DeleteRecordSwap(ctx, run.q.repoDID, rec.collection, rec.rkey, rec.cid)
			cancel()
			if err != nil {
				run.failures = append(run.failures, rec.uri+": "+err.Error())
				continue groups
			}
			deleted[shortNSID(rec.collection)]++
		}
	}
	return would, deleted, noScope, dropped
}
