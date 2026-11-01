package infra

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

	IndexRetries = 5

	IndexProbes = 2

	IndexProbeEvery = time.Hour

	IndexFailedWait = 10 * time.Minute

	IndexBudget = 10 * time.Second

	IndexWatchManager = ctlruntime.FieldManagerPrefix + "infra-index"

	indexWatchPrefix = "infraindex-"
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
	until time.Time

	cid string

	own string

	reason string

	message string
}

type indexTarget struct {
	repo commonv1.ObjectRef

	did string

	key infra.IndexKey

	own string
}

func IndexWatchName(workspace string, repo commonv1.ObjectRef) string {
	name := indexWatchPrefix + repo.Name
	if ws := repo.InWorkspace(workspace); ws != workspace {
		sum := sha256.Sum256([]byte(ws))
		name += "-" + hex.EncodeToString(sum[:3])
	}
	if len(name) <= 253 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:253-13] + "-" + hex.EncodeToString(sum[:6])
}

func cacheKey(cluster, did string) string {
	return cluster + "|" + did
}

func (c *Controller) cachedView(cluster, did string) (infra.IndexView, indexCache, bool) {
	c.mu.Lock()
	cache, ok := c.indexes[cacheKey(cluster, did)]
	c.mu.Unlock()
	if !ok || !cache.present {
		return infra.IndexView{}, cache, ok
	}
	view, _ := infra.DecodeIndexView(cache.value)
	return view, cache, true
}

func (c *Controller) storeIndex(cluster, did string, cache indexCache) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.indexes[cacheKey(cluster, did)] = cache
}

func (c *Controller) loadIndex(ctx context.Context, reader atprotorepo.RecordReader, cluster, did string) (indexCache, error) {
	got, err := reader.GetRecord(ctx, did, record.InfraNSID, infra.ReservedRkey)
	if errors.Is(err, atprotorepo.ErrNotFound) {
		c.storeIndex(cluster, did, indexCache{})
		return indexCache{}, nil
	}
	if err != nil {
		return indexCache{}, fmt.Errorf("infra: reading %s: %w", infra.IndexURI(did), err)
	}
	out := indexCache{present: true, cid: got.CID, value: got.Value}
	c.storeIndex(cluster, did, out)
	return out, nil
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

func ownIndexWatch(workspace string, watch *v1alpha1.AtprotoRecordWatch) bool {
	return watch.Labels[v1alpha1.InfraIndexLabel] != "" && watch.Spec.RepoRef != nil && watch.Spec.Collection == record.InfraNSID &&
		slices.Equal(watch.Spec.Rkeys, []string{infra.ReservedRkey}) && watch.Name == IndexWatchName(workspace, *watch.Spec.RepoRef)
}

func (c *Controller) heldIndexLocked(cluster, workspace string, watch *v1alpha1.AtprotoRecordWatch) {
	suffix := "/" + record.InfraNSID + "/" + infra.ReservedRkey
	wanted := func(did string) bool {
		for k, want := range c.wants {
			if k.cluster == cluster && want.workspace == workspace && want.did == did {
				return true
			}
		}
		return false
	}
	for _, rec := range watch.Status.Records {
		did, ok := indexDID(rec.URI, suffix)
		if !ok || rec.Cid == "" || !wanted(did) {
			continue
		}
		if held, known := c.indexes[cacheKey(cluster, did)]; !known || held.cid != rec.Cid {
			c.indexes[cacheKey(cluster, did)] = indexCache{present: true, cid: rec.Cid, value: rec.Value}
		}
	}
	for _, uri := range watch.Status.Deleted {
		did, ok := indexDID(uri, suffix)
		if ok && wanted(did) && !slices.ContainsFunc(watch.Status.Records, func(r v1alpha1.WatchedRecord) bool { return r.URI == uri }) {
			c.indexes[cacheKey(cluster, did)] = indexCache{}
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
	if !ownIndexWatch(workspace, watch) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heldIndexLocked(cluster, workspace, watch)
	_, ensured := c.indexWatches[cluster+"|"+workspace+"|"+watch.Name]
	var out []ctlruntime.Request
	views := map[string]infra.IndexView{}
	for k, want := range c.wants {
		if k.kind != kind || k.cluster != cluster {
			continue
		}
		if !ensured && want.workspace == workspace {
			out = append(out, ctlruntime.Request{Cluster: k.cluster, Workspace: want.workspace, Name: k.name})
			continue
		}
		cache, known := c.indexes[cacheKey(cluster, want.did)]
		if !known {
			continue
		}
		view, done := views[want.did]
		if !done && cache.present {
			view, _ = infra.DecodeIndexView(cache.value)
			views[want.did] = view
		}
		if infra.IndexHolds(view.Live, want.key) != want.live {
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

func (c *Controller) failed(k indexKey, cid, own string) (indexFailure, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.indexFailed[k]
	if !ok || f.cid != cid || f.own != own || (!f.until.IsZero() && !time.Now().Before(f.until)) {
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

func (r *reconciler[T]) indexWatchOwner(ctx context.Context, req ctlruntime.Request, v view, repo commonv1.ObjectRef) metav1.OwnerReference {
	owner := ownerRef(v)
	owner.Controller = nil
	if repo.Workspace != req.Workspace {
		return owner
	}
	dyn, err := r.c.dynamic(req.Workspace)
	if err != nil {
		return owner
	}
	got, err := kube.Get[metav1.PartialObjectMetadata](ctx, dyn.Resource(v1alpha1.AtprotoRepoGVR), repo.Name)
	if err != nil || got.UID == "" {
		return owner
	}
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: RepoKind, Name: repo.Name, UID: got.UID}
}

func (r *reconciler[T]) ensureIndexWatch(ctx context.Context, req ctlruntime.Request, v view, repo commonv1.ObjectRef) error {
	name := IndexWatchName(req.Workspace, repo)
	memo := req.Cluster + "|" + req.Workspace + "|" + name
	r.c.mu.Lock()
	_, done := r.c.indexWatches[memo]
	r.c.mu.Unlock()
	if done {
		return nil
	}
	var sources []commonv1.ObjectRef
	if v.spec.Record != nil {
		sources = v.spec.Record.EventSourceRefs
	}
	if err := EnsureWatch(ctx, r.c.dynamic, IndexWatchManager, req.Workspace, name, map[string]string{v1alpha1.InfraIndexLabel: infra.ReservedRkey},
		r.indexWatchOwner(ctx, req, v, repo), repo, record.InfraNSID, []string{infra.ReservedRkey}, sources); err != nil {
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
	return indexTarget{repo: repo, did: uri.Authority().String(), key: infra.IndexKey{Collection: v.kind.NSID(), Rkey: rkey}, own: recordStatus.CID}, true
}

func (r *reconciler[T]) indexPass(ctx context.Context, req ctlruntime.Request, v view, status *v1alpha1.InfraStatus, rec *recordSync,
	ephemeral *v1alpha1.EphemeralInfraStatus, end infra.End, frozen bool, now time.Time) time.Duration {
	ctx, cancel := context.WithTimeout(ctx, IndexBudget)
	defer cancel()
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
		r.c.setWant(k, nil)
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
			target.own = rec.cid
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
		entry := infra.IndexEntry{Collection: target.key.Collection, Rkey: target.key.Rkey, Writer: r.c.writer}
		if rec != nil {
			entry.CreatedAt = rec.createdAt.UTC()
		}
		if ephemeral != nil {
			entry.ExpiresAt, _ = time.Parse(time.RFC3339, ephemeral.ExpiresAt)
		}
		change.Add, change.Refresh = &entry, rec != nil
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
	ctx, cancel := context.WithTimeout(ctx, IndexBudget)
	defer cancel()
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

func settledReason(change infra.IndexChange, did string) (string, string) {
	if change.Add != nil {
		return "Indexed", "the index at " + infra.IndexURI(did) + " lists this deployment"
	}
	return "Removed", "the index no longer lists this deployment; the record is kept"
}

func (r *reconciler[T]) mutateIndex(ctx context.Context, req ctlruntime.Request, v view, target indexTarget, change infra.IndexChange,
	now time.Time) (string, string, time.Duration) {
	k := keyOf(req, v)
	attrs := []any{"kind", v.kind, "workspace", req.Workspace, "name", req.Name, "index", infra.IndexURI(target.did), "rkey", target.key.Rkey}
	view, cache, known := r.c.cachedView(req.Cluster, target.did)
	if known && view.Settled(change) {
		r.c.indexRetries.Reset(k)
		reason, message := settledReason(change, target.did)
		return reason, message, 0
	}
	if f, ok := r.c.failed(k, cache.cid, target.own); ok {
		return f.reason, f.message, 0
	}
	did, reader, err := Repo(ctx, r.c.bus, r.c.dynamic, r.self(req), target.repo, record.InfraNSID, record.EphemeralInfraNSID)
	if err == nil && did != target.did {
		err = fmt.Errorf("infra: the AtprotoRepo %s is %s, not %s", target.repo.String(), did, target.did)
	}
	if err != nil {
		return r.indexFailed(k, cache.cid, target.own, attrs, err)
	}
	if cache, err = r.c.loadIndex(ctx, reader, req.Cluster, target.did); err != nil {
		return r.indexFailed(k, "", target.own, attrs, err)
	}
	if view, _, _ = r.c.cachedView(req.Cluster, target.did); view.Settled(change) {
		r.c.indexRetries.Reset(k)
		r.c.setFailed(k, nil)
		reason, message := settledReason(change, target.did)
		return reason, message, 0
	}
	writer, err := bus.Resolve[atprotorepo.RecordWriter](ctx, r.c.bus, r.self(req), target.repo,
		bus.Capability{Name: atprotorepo.CapabilityWrite, Scope: []string{record.InfraNSID}})
	if err != nil {
		return r.indexFailed(k, cache.cid, target.own, attrs, err)
	}
	swapper, ok := writer.(atprotorepo.SwapPutter)
	if !ok {
		return r.indexFailed(k, cache.cid, target.own, attrs, errors.New("infra: the repo cannot put a record by its cid"))
	}
	change.Dead = r.c.probe(ctx, reader, target, view.Live, now)
	var full error
	for attempt := 1; attempt <= IndexAttempts; attempt++ {
		var raw json.RawMessage
		if cache.present {
			raw = cache.value
		}
		value, changed, aerr := infra.ApplyIndex(raw, change, now)
		if aerr != nil && !errors.Is(aerr, infra.ErrIndexFull) {
			return r.indexFailed(k, cache.cid, target.own, attrs, aerr)
		}
		full = aerr
		if !changed {
			break
		}
		var ref commonv1.StrongRef
		var werr error
		if cache.present {
			ref, werr = swapper.PutRecordSwap(ctx, record.InfraNSID, infra.ReservedRkey, value, cache.cid)
		} else {
			ref, werr = writer.CreateRecord(ctx, record.InfraNSID, infra.ReservedRkey, value)
		}
		if werr == nil {
			cache = indexCache{present: true, cid: ref.CID, value: value}
			r.c.storeIndex(req.Cluster, target.did, cache)
			r.c.log.Info("infra_index_written", append(attrs, "cid", ref.CID, "attempt", attempt, "add", change.Add != nil,
				"remove", change.Remove != nil, "pruned", len(change.Dead), "full", full != nil)...)
			break
		}
		if cache.present && !errors.Is(werr, atprotorepo.ErrSwapMismatch) {
			return r.indexFailed(k, cache.cid, target.own, attrs, werr)
		}
		r.c.log.Info("infra_index_contended", append(attrs, "attempt", attempt, "err", werr.Error())...)
		was := cache.present
		var lerr error
		if cache, lerr = r.c.loadIndex(ctx, reader, req.Cluster, target.did); lerr != nil {
			return r.indexFailed(k, "", target.own, attrs, lerr)
		}
		if !was && !cache.present {
			return r.indexFailed(k, "", target.own, attrs, fmt.Errorf("infra: creating %s: %w", infra.IndexURI(target.did), werr))
		}
		if attempt == IndexAttempts {
			return r.indexFailed(k, cache.cid, target.own, attrs, errIndexContended)
		}
	}
	if full != nil {
		return r.indexFailed(k, cache.cid, target.own, attrs, full)
	}
	r.c.setFailed(k, nil)
	r.c.indexRetries.Reset(k)
	if view, _, _ = r.c.cachedView(req.Cluster, target.did); view.Settled(change) {
		reason, message := settledReason(change, target.did)
		return reason, message, 0
	}
	return "", "", 0
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d/2 + rand.N(d)
}

func (r *reconciler[T]) indexFailed(k indexKey, cid, own string, attrs []any, err error) (string, string, time.Duration) {
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
	retrying := reason == "Contended" || netretry.Transient(err)
	if retrying && reason != "Contended" {
		reason = "IndexWriteRetrying"
	}
	r.c.log.Warn("infra_index_write_failed", append(attrs, "reason", reason, "err", err.Error())...)
	if retrying {
		if n, delay := r.c.indexRetries.Fail(k); n < IndexRetries {
			return reason, err.Error(), max(jitter(delay), netretry.After(err))
		}
		r.c.indexRetries.Reset(k)
		message := fmt.Sprintf("%s; %d tries failed, the next index or record change tries again", err.Error(), IndexRetries)
		r.c.setFailed(k, &indexFailure{cid: cid, own: own, reason: reason, message: message})
		return reason, message, 0
	}
	r.c.setFailed(k, &indexFailure{until: time.Now().Add(IndexFailedWait), cid: cid, own: own, reason: reason, message: err.Error()})
	return reason, err.Error(), 0
}

func (c *Controller) probe(ctx context.Context, reader atprotorepo.RecordReader, target indexTarget, entries []infra.IndexEntry, now time.Time) []infra.IndexDead {
	candidates := slices.DeleteFunc(slices.Clone(entries), func(e infra.IndexEntry) bool {
		return e.Key() == target.key || e.Expired(now)
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
		if errors.Is(err, atprotorepo.ErrRecordNotFound) {
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
		if strings.HasPrefix(uri, "at://"+target.did+"/") &&
			!slices.ContainsFunc(entries, func(e infra.IndexEntry) bool { return uri == "at://"+target.did+"/"+e.Collection+"/"+e.Rkey }) {
			delete(c.probed, uri)
		}
	}
	c.mu.Unlock()
	return out
}
