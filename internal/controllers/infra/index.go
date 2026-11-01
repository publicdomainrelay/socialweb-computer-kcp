package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/api/meta"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	record "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/netretry"
)

const (
	IndexAttempts = 5

	IndexProbes = 2

	IndexProbeEvery = time.Hour

	IndexFailedWait = 10 * time.Minute

	IndexWatchManager = ctlruntime.FieldManagerPrefix + "infra-index"
)

var errIndexContended = fmt.Errorf("infra: the index changed under each of %d swapped writes", IndexAttempts)

type indexCache struct {
	present bool

	cid string

	value json.RawMessage
}

type indexWant struct {
	workspace string

	did string

	key infra.IndexKey

	live bool
}

type indexFailure struct {
	at time.Time

	cid string

	reason string

	message string
}

type indexTarget struct {
	repo commonv1.ObjectRef

	did string

	key infra.IndexKey
}

func IndexWatchName(repo string) string {
	return infra.ChildName(infra.KindInfra, "index", repo)
}

func (c *Controller) cachedIndex(did string) (indexCache, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	got, ok := c.indexes[did]
	return got, ok
}

func (c *Controller) storeIndex(did string, cache indexCache) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.indexes[did] = cache
}

func (c *Controller) loadIndex(ctx context.Context, reader atprotorepo.RecordReader, did string) (indexCache, error) {
	got, err := reader.GetRecord(ctx, did, record.InfraNSID, infra.ReservedRkey)
	if errors.Is(err, atprotorepo.ErrNotFound) {
		c.storeIndex(did, indexCache{})
		return indexCache{}, nil
	}
	if err != nil {
		return indexCache{}, fmt.Errorf("infra: reading %s: %w", infra.IndexURI(did), err)
	}
	out := indexCache{present: true, cid: got.CID, value: got.Value}
	c.storeIndex(did, out)
	return out, nil
}

func (c *Controller) indexEntries(did string) ([]infra.IndexEntry, indexCache, bool) {
	cache, ok := c.cachedIndex(did)
	if !ok || !cache.present {
		return nil, cache, ok
	}
	entries, err := infra.DecodeIndex(cache.value)
	if err != nil {
		return nil, cache, true
	}
	return entries, cache, true
}

func (c *Controller) observeIndexWatch(watch *v1alpha1.AtprotoRecordWatch, gone bool) {
	if watch.Labels[v1alpha1.InfraIndexLabel] == "" || (!gone && watch.DeletionTimestamp.IsZero()) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for memo := range c.indexWatches {
		if strings.HasSuffix(memo, "|"+watch.Name) {
			delete(c.indexWatches, memo)
		}
	}
}

func (c *Controller) heldIndexLocked(watch *v1alpha1.AtprotoRecordWatch) {
	suffix := "/" + record.InfraNSID + "/" + infra.ReservedRkey
	for _, rec := range watch.Status.Records {
		did, ok := indexDID(rec.URI, suffix)
		if !ok || rec.Cid == "" {
			continue
		}
		if held, known := c.indexes[did]; !known || held.cid != rec.Cid {
			c.indexes[did] = indexCache{present: true, cid: rec.Cid, value: rec.Value}
		}
	}
	for _, uri := range watch.Status.Deleted {
		if did, ok := indexDID(uri, suffix); ok && !slices.ContainsFunc(watch.Status.Records, func(r v1alpha1.WatchedRecord) bool { return r.URI == uri }) {
			c.indexes[did] = indexCache{}
		}
	}
}

func indexDID(uri, suffix string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return "", false
	}
	did, ok := strings.CutSuffix(rest, suffix)
	return did, ok && did != "" && !strings.Contains(did, "/")
}

func (c *Controller) indexUsers(cluster, workspace, kind string, watch *v1alpha1.AtprotoRecordWatch) []ctlruntime.Request {
	if watch.Labels[v1alpha1.InfraIndexLabel] == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heldIndexLocked(watch)
	_, ensured := c.indexWatches[cluster+"|"+workspace+"|"+watch.Name]
	var out []ctlruntime.Request
	decoded := map[string][]infra.IndexEntry{}
	for k, want := range c.wants {
		if k.kind != kind || k.cluster != cluster {
			continue
		}
		if !ensured && want.workspace == workspace {
			out = append(out, ctlruntime.Request{Cluster: k.cluster, Workspace: want.workspace, Name: k.name})
			continue
		}
		cache, known := c.indexes[want.did]
		if !known {
			continue
		}
		entries, done := decoded[want.did]
		if !done && cache.present {
			entries, _ = infra.DecodeIndex(cache.value)
			decoded[want.did] = entries
		}
		if infra.IndexHolds(entries, want.key) != want.live {
			out = append(out, ctlruntime.Request{Cluster: k.cluster, Workspace: want.workspace, Name: k.name})
		}
	}
	return out
}

func (c *Controller) setWant(k indexKey, want *indexWant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if want == nil {
		delete(c.wants, k)
		return
	}
	c.wants[k] = *want
}

func (c *Controller) failed(k indexKey, cid string) (indexFailure, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.indexFailed[k]
	if !ok || f.cid != cid || time.Since(f.at) >= IndexFailedWait {
		return indexFailure{}, false
	}
	return f, true
}

func (c *Controller) setFailed(k indexKey, f *indexFailure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f == nil {
		delete(c.indexFailed, k)
		return
	}
	c.indexFailed[k] = *f
}

func (r *reconciler[T]) ensureIndexWatch(ctx context.Context, req ctlruntime.Request, v view, repo commonv1.ObjectRef) error {
	name := IndexWatchName(repo.Name)
	memo := req.Cluster + "|" + req.Workspace + "|" + name
	r.c.mu.Lock()
	_, done := r.c.indexWatches[memo]
	r.c.mu.Unlock()
	if done {
		return nil
	}
	owner := ownerRef(v)
	owner.Controller = nil
	var sources []commonv1.ObjectRef
	if v.spec.Record != nil {
		sources = v.spec.Record.EventSourceRefs
	}
	if err := EnsureWatch(ctx, r.c.dynamic, IndexWatchManager, req.Workspace, name, map[string]string{v1alpha1.InfraIndexLabel: infra.ReservedRkey},
		owner, repo, record.InfraNSID, []string{infra.ReservedRkey}, sources); err != nil {
		return err
	}
	r.c.mu.Lock()
	r.c.indexWatches[memo] = true
	r.c.mu.Unlock()
	return nil
}

func (r *reconciler[T]) indexTarget(req ctlruntime.Request, v view, recordStatus *v1alpha1.InfraRecordStatus, ref *commonv1.ObjectRef) (indexTarget, bool) {
	if recordStatus == nil || ref == nil {
		return indexTarget{}, false
	}
	uri, err := syntax.ParseATURI(strings.TrimSpace(recordStatus.URI))
	if err != nil || uri.Collection().String() != v.kind.NSID() {
		return indexTarget{}, false
	}
	rkey := uri.RecordKey().String()
	if rkey == infra.ReservedRkey || !r.owns(v, rkey) {
		return indexTarget{}, false
	}
	repo, err := RepoRef(req.Workspace, *ref)
	if err != nil {
		return indexTarget{}, false
	}
	return indexTarget{repo: repo, did: uri.Authority().String(), key: infra.IndexKey{Collection: v.kind.NSID(), Rkey: rkey}}, true
}

func (r *reconciler[T]) indexPass(ctx context.Context, req ctlruntime.Request, v view, status *v1alpha1.InfraStatus, rec *recordSync,
	ephemeral *v1alpha1.EphemeralInfraStatus, end infra.End, frozen bool, now time.Time) time.Duration {
	k := keyOf(req, v)
	var ref *commonv1.ObjectRef
	if v.spec.Record != nil {
		ref = &v.spec.Record.RepoRef
	}
	target, ok := r.indexTarget(req, v, status.Record, ref)
	if !ok || (rec != nil && (rec.action == infra.ActionDelete || rec.action == infra.ActionReserved)) {
		r.c.setWant(k, nil)
		meta.RemoveStatusCondition(&status.Conditions, v1alpha1.InfraConditionIndexed)
		return 0
	}
	gen := status.ObservedGeneration
	if end.Ended && !frozen {
		return 0
	}
	var change infra.IndexChange
	if frozen {
		ended := infra.IndexEnded{Collection: target.key.Collection, Rkey: target.key.Rkey, EndedAt: now}
		if rec != nil {
			if at, err := time.Parse(time.RFC3339, rec.recorded.EndedAt); err == nil {
				ended.EndedAt = at
			}
			ended.EndReason = strings.ToLower(string(rec.recorded.EndReason))
		}
		change.Remove = &ended
	} else {
		present := status.Record.CID != ""
		if rec != nil {
			present = rec.cid != ""
			if rec.action == infra.ActionInvalid {
				r.c.setWant(k, nil)
				conds.Set(&status.Conditions, gen, v1alpha1.InfraConditionIndexed, false, "RecordInvalid", "an invalid record is not indexed")
				return 0
			}
		}
		if !present {
			r.c.setWant(k, nil)
			conds.Set(&status.Conditions, gen, v1alpha1.InfraConditionIndexed, false, "Pending", "the record is indexed once it is written")
			return 0
		}
		entry := infra.IndexEntry{Collection: target.key.Collection, Rkey: target.key.Rkey, CreatedAt: v.object.GetCreationTimestamp().Time.UTC(),
			Writer: r.c.writer}
		if rec != nil && !rec.createdAt.IsZero() {
			entry.CreatedAt = rec.createdAt.UTC()
		}
		if ephemeral != nil {
			entry.ExpiresAt, _ = time.Parse(time.RFC3339, ephemeral.ExpiresAt)
		}
		change.Add = &entry
	}
	r.c.setWant(k, &indexWant{workspace: req.Workspace, did: target.did, key: target.key, live: change.Add != nil})
	if err := r.ensureIndexWatch(ctx, req, v, target.repo); err != nil {
		r.c.log.Info("infra_index_watch_failed", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "err", err.Error())
	}
	reason, message, retry := r.mutateIndex(ctx, req, v, target, change, now)
	switch reason {
	case "":
		return 0
	case "Indexed":
		conds.Set(&status.Conditions, gen, v1alpha1.InfraConditionIndexed, true, reason, message)
	default:
		conds.Set(&status.Conditions, gen, v1alpha1.InfraConditionIndexed, false, reason, message)
	}
	return retry
}

func (r *reconciler[T]) indexEnd(ctx context.Context, req ctlruntime.Request, v view, ref *commonv1.ObjectRef) {
	k := keyOf(req, v)
	if ref == nil {
		if got, ok := r.childRepo(ctx, req, v); ok {
			ref = &got
		}
	}
	target, ok := r.indexTarget(req, v, v.status.Record, ref)
	if !ok {
		r.c.setWant(k, nil)
		return
	}
	observed := r.finalObserved(v)
	ended := infra.IndexEnded{Collection: target.key.Collection, Rkey: target.key.Rkey, EndReason: strings.ToLower(string(observed.EndReason))}
	ended.EndedAt, _ = time.Parse(time.RFC3339, observed.EndedAt)
	r.c.setWant(k, &indexWant{workspace: req.Workspace, did: target.did, key: target.key})
	if reason, message, _ := r.mutateIndex(ctx, req, v, target, infra.IndexChange{Remove: &ended}, time.Now()); reason != "" && reason != "Removed" {
		r.c.log.Info("infra_index_end_skipped", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "reason", reason, "err", message)
	}
}

func (r *reconciler[T]) childRepo(ctx context.Context, req ctlruntime.Request, v view) (commonv1.ObjectRef, bool) {
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return commonv1.ObjectRef{}, false
	}
	ar, err := kube.Get[v1alpha1.AtprotoRecord](ctx, dyn.Resource(v1alpha1.AtprotoRecordGVR), infra.ChildName(v.kind, v.object.GetName()))
	if err != nil || ar.Spec.RepoRef == nil {
		return commonv1.ObjectRef{}, false
	}
	return *ar.Spec.RepoRef, true
}

func (r *reconciler[T]) mutateIndex(ctx context.Context, req ctlruntime.Request, v view, target indexTarget, change infra.IndexChange,
	now time.Time) (string, string, time.Duration) {
	k := keyOf(req, v)
	attrs := []any{"kind", v.kind, "workspace", req.Workspace, "name", req.Name, "index", infra.IndexURI(target.did), "rkey", target.key.Rkey}
	settled := func(entries []infra.IndexEntry) (string, string, bool) {
		holds := infra.IndexHolds(entries, target.key)
		switch {
		case change.Add != nil && holds:
			return "Indexed", "the index at " + infra.IndexURI(target.did) + " lists this deployment", true
		case change.Remove != nil && !holds:
			return "Removed", "the index no longer lists this deployment; the record is kept", true
		}
		return "", "", false
	}
	entries, cache, known := r.c.indexEntries(target.did)
	if known {
		if reason, message, ok := settled(entries); ok {
			r.c.indexRetries.Reset(k)
			return reason, message, 0
		}
		if f, ok := r.c.failed(k, cache.cid); ok {
			return f.reason, f.message, 0
		}
	}
	did, reader, err := Repo(ctx, r.c.bus, r.c.dynamic, r.self(req), target.repo, record.InfraNSID, record.EphemeralInfraNSID)
	if err == nil && did != target.did {
		err = fmt.Errorf("infra: the AtprotoRepo %s is %s, not %s", target.repo.String(), did, target.did)
	}
	if err != nil {
		return r.indexFailed(k, cache.cid, attrs, err)
	}
	if !known {
		if cache, err = r.c.loadIndex(ctx, reader, target.did); err != nil {
			return r.indexFailed(k, "", attrs, err)
		}
		entries, _, _ = r.c.indexEntries(target.did)
		if reason, message, ok := settled(entries); ok {
			r.c.indexRetries.Reset(k)
			return reason, message, 0
		}
	}
	writer, err := bus.Resolve[atprotorepo.RecordWriter](ctx, r.c.bus, r.self(req), target.repo,
		bus.Capability{Name: atprotorepo.CapabilityWrite, Scope: []string{record.InfraNSID}})
	if err != nil {
		return r.indexFailed(k, cache.cid, attrs, err)
	}
	swapper, ok := writer.(atprotorepo.SwapPutter)
	if !ok {
		return r.indexFailed(k, cache.cid, attrs, errors.New("infra: the repo cannot put a record by its cid"))
	}
	change.Dead = r.c.probe(ctx, reader, target, entries, now)
	for attempt := 1; attempt <= IndexAttempts; attempt++ {
		var raw json.RawMessage
		if cache.present {
			raw = cache.value
		}
		value, changed, err := infra.ApplyIndex(raw, change, now)
		if err != nil {
			return r.indexFailed(k, cache.cid, attrs, err)
		}
		if !changed {
			break
		}
		var ref commonv1.StrongRef
		if cache.present {
			ref, err = swapper.PutRecordSwap(ctx, record.InfraNSID, infra.ReservedRkey, value, cache.cid)
		} else {
			ref, err = writer.CreateRecord(ctx, record.InfraNSID, infra.ReservedRkey, value)
		}
		if err == nil {
			r.c.storeIndex(target.did, indexCache{present: true, cid: ref.CID, value: value})
			r.c.log.Info("infra_index_written", append(attrs, "cid", ref.CID, "attempt", attempt, "add", change.Add != nil,
				"remove", change.Remove != nil, "pruned", len(change.Dead))...)
			break
		}
		if cache.present && !errors.Is(err, atprotorepo.ErrSwapMismatch) {
			return r.indexFailed(k, cache.cid, attrs, err)
		}
		r.c.log.Info("infra_index_contended", append(attrs, "attempt", attempt, "err", err.Error())...)
		was := cache.present
		if cache, err = r.c.loadIndex(ctx, reader, target.did); err != nil {
			return r.indexFailed(k, "", attrs, err)
		}
		if !was && !cache.present {
			return r.indexFailed(k, "", attrs, fmt.Errorf("infra: creating %s: %w", infra.IndexURI(target.did), err))
		}
		if attempt == IndexAttempts {
			return r.indexFailed(k, cache.cid, attrs, errIndexContended)
		}
	}
	entries, _, _ = r.c.indexEntries(target.did)
	r.c.setFailed(k, nil)
	r.c.indexRetries.Reset(k)
	if reason, message, ok := settled(entries); ok {
		return reason, message, 0
	}
	return "", "", 0
}

func (r *reconciler[T]) indexFailed(k indexKey, cid string, attrs []any, err error) (string, string, time.Duration) {
	reason := "IndexWriteFailed"
	switch {
	case errors.Is(err, errIndexContended):
		reason = "Contended"
	case errors.Is(err, infra.ErrIndexFull):
		reason = "IndexFull"
	case errors.Is(err, infra.ErrNotIndex):
		reason = "IndexNotIndex"
	case errors.Is(err, ErrRepoNoDID):
		reason = "RepoPending"
	}
	r.c.log.Warn("infra_index_write_failed", append(attrs, "reason", reason, "err", err.Error())...)
	if reason == "Contended" || netretry.Transient(err) {
		if reason != "Contended" {
			reason = "IndexWriteRetrying"
		}
		_, delay := r.c.indexRetries.Fail(k)
		return reason, err.Error(), delay
	}
	r.c.setFailed(k, &indexFailure{at: time.Now(), cid: cid, reason: reason, message: err.Error()})
	return reason, err.Error(), 0
}

func (c *Controller) probe(ctx context.Context, reader atprotorepo.RecordReader, target indexTarget, entries []infra.IndexEntry, now time.Time) []infra.IndexDead {
	candidates := slices.DeleteFunc(slices.Clone(entries), func(e infra.IndexEntry) bool {
		return e.Key() == target.key || e.Expired(now) || (e.Collection != record.InfraNSID && e.Collection != record.EphemeralInfraNSID)
	})
	slices.SortStableFunc(candidates, func(a, b infra.IndexEntry) int { return a.AddedAt.Compare(b.AddedAt) })
	var out []infra.IndexDead
	probed := 0
	for _, e := range candidates {
		if probed >= IndexProbes {
			break
		}
		uri := "at://" + target.did + "/" + e.Collection + "/" + e.Rkey
		c.mu.Lock()
		last, seen := c.probed[uri]
		due := !seen || now.Sub(last) >= IndexProbeEvery
		if due {
			c.probed[uri] = now
		}
		c.mu.Unlock()
		if !due {
			continue
		}
		probed++
		got, err := reader.GetRecord(ctx, target.did, e.Collection, e.Rkey)
		if errors.Is(err, atprotorepo.ErrNotFound) {
			out = append(out, infra.IndexDead{IndexEnded: infra.IndexEnded{Collection: e.Collection, Rkey: e.Rkey, EndedAt: now}, Gone: true})
			continue
		}
		if err != nil {
			continue
		}
		kind := infra.KindInfra
		if e.Collection == record.EphemeralInfraNSID {
			kind = infra.KindEphemeralInfra
		}
		decoded, derr := infra.Decode(kind, got.Value)
		if !errors.Is(derr, infra.ErrReserved) && !decoded.Over(kind, now) {
			continue
		}
		ended := infra.IndexEnded{Collection: e.Collection, Rkey: e.Rkey, EndedAt: now, EndReason: strings.ToLower(string(decoded.EndReason))}
		if at, err := time.Parse(time.RFC3339, decoded.EndedAt); err == nil {
			ended.EndedAt = at
		} else if decoded.Expired(kind, now) {
			ended.EndedAt, ended.EndReason = decoded.CreatedAt.Add(time.Duration(decoded.TTLSeconds)*time.Second), infra.IndexEndDeadline
		}
		out = append(out, infra.IndexDead{IndexEnded: ended})
	}
	c.mu.Lock()
	for uri := range c.probed {
		if !slices.ContainsFunc(entries, func(e infra.IndexEntry) bool { return uri == "at://"+target.did+"/"+e.Collection+"/"+e.Rkey }) &&
			strings.HasPrefix(uri, "at://"+target.did+"/") {
			delete(c.probed, uri)
		}
	}
	c.mu.Unlock()
	return out
}
