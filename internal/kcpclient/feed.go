package kcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	kcpv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/clusterhome"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
)

var _ gateway.Feed = (*Client)(nil)

const (
	memberClusterLabel = "socialweb.computer/cluster"
	memberNameLabel    = "socialweb.computer/member"

	watchTimeoutSeconds = "300"
	shortWatch          = time.Second
	maxShortWatches     = 3
)

var errWatchGone = errors.New("kcpclient: the watch's resource version is gone")

type listOf[T any] struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Items []T `json:"items"`
}

type watchFrame struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

type source[T any] struct {
	r        rest.Interface
	resource string
	label    string
	field    string
	where    string
}

func (s source[T]) request() *rest.Request {
	req := s.r.Get().Resource(s.resource)
	if s.label != "" {
		req = req.Param("labelSelector", s.label)
	}
	if s.field != "" {
		req = req.Param("fieldSelector", s.field)
	}
	return req
}

func (s source[T]) list(ctx context.Context) ([]T, string, error) {
	raw, err := s.request().DoRaw(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("kcpclient: list %s in %s: %w", s.resource, s.where, err)
	}
	var out listOf[T]
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", fmt.Errorf("kcpclient: list %s in %s: %w", s.resource, s.where, err)
	}
	return out.Items, out.Metadata.ResourceVersion, nil
}

func (s source[T]) watch(ctx context.Context, rv string, on func(deleted bool, obj T) error) (string, error) {
	stream, err := s.request().
		Param("watch", "true").
		Param("resourceVersion", rv).
		Param("allowWatchBookmarks", "true").
		Param("timeoutSeconds", watchTimeoutSeconds).
		Stream(ctx)
	if err != nil {
		return rv, fmt.Errorf("kcpclient: watch %s in %s: %w", s.resource, s.where, err)
	}
	defer stream.Close()
	dec := json.NewDecoder(stream)
	for {
		var f watchFrame
		if err := dec.Decode(&f); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return rv, nil
			}
			return rv, fmt.Errorf("kcpclient: watch %s in %s: %w", s.resource, s.where, err)
		}
		if f.Type == "ERROR" {
			var st metav1.Status
			_ = json.Unmarshal(f.Object, &st)
			if st.Code == http.StatusGone {
				return rv, errWatchGone
			}
			return rv, fmt.Errorf("kcpclient: watch %s in %s: %s", s.resource, s.where, st.Message)
		}
		var head struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(f.Object, &head); err != nil {
			return rv, fmt.Errorf("kcpclient: watch %s in %s: %w", s.resource, s.where, err)
		}
		if head.Metadata.ResourceVersion != "" {
			rv = head.Metadata.ResourceVersion
		}
		if f.Type == "BOOKMARK" {
			continue
		}
		var obj T
		if err := json.Unmarshal(f.Object, &obj); err != nil {
			return rv, fmt.Errorf("kcpclient: watch %s in %s: %w", s.resource, s.where, err)
		}
		if err := on(f.Type == "DELETED", obj); err != nil {
			return rv, err
		}
	}
}

func follow[T any](ctx context.Context, s source[T], rv string, on func(deleted bool, obj T) error, resync func([]T) error) error {
	short := 0
	for {
		began := time.Now()
		next, err := s.watch(ctx, rv, on)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case err == nil:
			if time.Since(began) < shortWatch {
				short++
				if short >= maxShortWatches {
					return fmt.Errorf("kcpclient: the watch of %s in %s closed %d times in a row within %s",
						s.resource, s.where, short, shortWatch)
				}
			} else {
				short = 0
			}
			rv = next
		case errors.Is(err, errWatchGone):
			items, lrv, lerr := s.list(ctx)
			if lerr != nil {
				return lerr
			}
			if err := resync(items); err != nil {
				return err
			}
			rv = lrv
		default:
			return err
		}
	}
}

type feed struct {
	c    *Client
	did  string
	name string
	emit gateway.Emit

	mu    sync.Mutex
	sent  map[string]string
	homes map[string]context.CancelFunc
	errc  chan error
	wg    sync.WaitGroup
}

func (f *feed) fail(err error) {
	select {
	case f.errc <- err:
	default:
	}
}

func (f *feed) send(key, typ string, fingerprint string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent[key] == fingerprint {
		return nil
	}
	f.sent[key] = fingerprint
	return f.emit(gateway.Event{Type: typ, Payload: payload})
}

func (f *feed) gone(key, typ string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sent[key]; !ok {
		return nil
	}
	delete(f.sent, key)
	return f.emit(gateway.Event{Type: typ, Payload: payload})
}

func (f *feed) keysUnder(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.sent {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

var errAccountReady = errors.New("kcpclient: the account workspace is ready")

func (c *Client) waitForAccount(ctx context.Context, did string) error {
	rRoot, err := c.resource(clusterhome.RootWorkspace, tenancyResource)
	if err != nil {
		return err
	}
	spaces := source[kcpv1alpha1.Workspace]{r: rRoot, resource: "workspaces",
		field: "metadata.name=" + clusterhome.AccountHash(did), where: clusterhome.RootWorkspace}
	ready := func(ws kcpv1alpha1.Workspace) bool {
		return ws.DeletionTimestamp == nil && ws.Status.Phase == corev1alpha1.LogicalClusterPhaseReady
	}
	anyReady := func(items []kcpv1alpha1.Workspace) error {
		if slices.ContainsFunc(items, ready) {
			return errAccountReady
		}
		return nil
	}
	items, rv, err := spaces.list(ctx)
	if err != nil {
		return err
	}
	if anyReady(items) != nil {
		return nil
	}
	err = follow(ctx, spaces, rv, func(deleted bool, ws kcpv1alpha1.Workspace) error {
		if !deleted && ready(ws) {
			return errAccountReady
		}
		return nil
	}, anyReady)
	if errors.Is(err, errAccountReady) {
		return nil
	}
	return err
}

func (c *Client) Follow(ctx context.Context, caller gateway.Caller, name string, emit gateway.Emit) error {
	did := string(caller)
	if did == "" {
		return gateway.ErrUnauthorized
	}
	state, err := c.Lookup(ctx, did)
	if err != nil {
		return err
	}
	snapshotSent := false
	if state == revocation.DenyFirstLogin {
		if name != "" {
			return &gateway.XRPCError{Name: "ClusterNotFound", Status: 404,
				Message: "this account has no clusters yet: call createCluster, then subscribe"}
		}
		if err := emit(gateway.Event{Type: gateway.EventInfo, Payload: gateway.InfoEvent{
			Name:    gateway.InfoSnapshotComplete,
			Message: "this account has no clusters yet; events start when createCluster makes the first one",
		}}); err != nil {
			return err
		}
		snapshotSent = true
		if err := c.waitForAccount(ctx, did); err != nil {
			return err
		}
		if state, err = c.Lookup(ctx, did); err != nil {
			return err
		}
	}
	switch state {
	case revocation.DenyGranted, revocation.DenyFirstLogin:
	default:
		return gateway.ErrIdentityRevoked
	}

	ctx, cancel := context.WithCancel(ctx)
	f := &feed{c: c, did: did, name: name, emit: emit,
		sent: map[string]string{}, homes: map[string]context.CancelFunc{}, errc: make(chan error, 1)}
	defer func() {
		cancel()
		f.wg.Wait()
	}()

	account := clusterhome.AccountPath(did)
	rTenancy, err := c.resource(account, tenancyResource)
	if err != nil {
		return err
	}
	rAccount, err := c.resource(account, socialwebResource)
	if err != nil {
		return err
	}
	identities := source[v1alpha1.AtprotoIdentity]{r: rAccount, resource: "atprotoidentities",
		field: "metadata.name=" + IdentityRecordName(did), where: account}
	workspaces := source[kcpv1alpha1.Workspace]{r: rTenancy, resource: "workspaces", where: account}

	_, idRV, err := identities.list(ctx)
	if err != nil {
		return err
	}
	spaces, wsRV, err := workspaces.list(ctx)
	if err != nil {
		return err
	}

	found := name == ""
	var starts []func()
	for _, ws := range spaces {
		if !f.wanted(ws) {
			continue
		}
		found = true
		if ws.Status.Phase != corev1alpha1.LogicalClusterPhaseReady || ws.DeletionTimestamp != nil {
			continue
		}
		start, err := f.open(ctx, ws.Name, !snapshotSent)
		if err != nil {
			return err
		}
		starts = append(starts, start)
	}
	if !found {
		return gateway.ErrClusterNotFound
	}
	if !snapshotSent {
		if err := emit(gateway.Event{Type: gateway.EventInfo, Payload: gateway.InfoEvent{
			Name:    gateway.InfoSnapshotComplete,
			Message: "the snapshot is complete; what follows are live changes",
		}}); err != nil {
			return err
		}
	}

	for _, start := range starts {
		start()
	}
	f.wg.Go(func() {
		f.fail(follow(ctx, identities, idRV, f.onIdentity, f.resyncIdentity))
	})
	f.wg.Go(func() {
		f.fail(follow(ctx, workspaces, wsRV, func(deleted bool, ws kcpv1alpha1.Workspace) error {
			return f.onWorkspace(ctx, deleted, ws)
		}, func(items []kcpv1alpha1.Workspace) error {
			return f.resyncWorkspaces(ctx, items)
		}))
	})

	select {
	case err := <-f.errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *feed) wanted(ws kcpv1alpha1.Workspace) bool {
	return ws.Spec.Mount == nil && (f.name == "" || ws.Name == f.name)
}

func (f *feed) onIdentity(deleted bool, id v1alpha1.AtprotoIdentity) error {
	if deleted || id.Spec.Revoked || id.Spec.DID != f.did {
		return gateway.ErrIdentityRevoked
	}
	return nil
}

func (f *feed) resyncIdentity(items []v1alpha1.AtprotoIdentity) error {
	if len(items) == 0 {
		return gateway.ErrIdentityRevoked
	}
	return f.onIdentity(false, items[0])
}

func (f *feed) onWorkspace(ctx context.Context, deleted bool, ws kcpv1alpha1.Workspace) error {
	if !f.wanted(ws) {
		return nil
	}
	if deleted {
		return f.closeHome(ws.Name)
	}
	if ws.Status.Phase != corev1alpha1.LogicalClusterPhaseReady || ws.DeletionTimestamp != nil {
		return nil
	}
	f.mu.Lock()
	_, open := f.homes[ws.Name]
	f.mu.Unlock()
	if open {
		return nil
	}
	start, err := f.open(ctx, ws.Name, false)
	if err != nil {
		return err
	}
	start()
	return nil
}

func (f *feed) resyncWorkspaces(ctx context.Context, items []kcpv1alpha1.Workspace) error {
	present := map[string]bool{}
	for _, ws := range items {
		if f.wanted(ws) {
			present[ws.Name] = true
		}
		if err := f.onWorkspace(ctx, false, ws); err != nil {
			return err
		}
	}
	f.mu.Lock()
	var missing []string
	for name := range f.homes {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	f.mu.Unlock()
	for _, name := range missing {
		if err := f.closeHome(name); err != nil {
			return err
		}
	}
	return nil
}

func (f *feed) closeHome(cluster string) error {
	f.mu.Lock()
	cancel, ok := f.homes[cluster]
	delete(f.homes, cluster)
	f.mu.Unlock()
	if ok {
		cancel()
	}
	for _, k := range f.keysUnder("member/" + cluster + "/") {
		f.mu.Lock()
		delete(f.sent, k)
		f.mu.Unlock()
	}
	for _, k := range f.keysUnder("claim/" + cluster + "/") {
		f.mu.Lock()
		delete(f.sent, k)
		f.mu.Unlock()
	}
	return f.clusterGone(cluster)
}

func (f *feed) clusterGone(cluster string) error {
	if err := f.gone("cluster/"+cluster, gateway.EventCluster, gateway.ClusterEvent{
		Name: cluster, Phase: string(v1alpha1.ClusterUnavailable), Deleted: true, Time: clock.NowISO(),
	}); err != nil {
		return err
	}
	if f.name != "" {
		return &gateway.XRPCError{Name: "ClusterNotFound", Status: 404, Message: "the cluster was deleted"}
	}
	return nil
}

type memberView struct {
	member, phase, reason, message string
}

type stream[T any] struct {
	src    source[T]
	prefix string
	key    func(T) string
	on     func(bool, T) error
}

func (s stream[T]) prime(ctx context.Context, f *feed) (func(context.Context) error, error) {
	items, rv, err := s.src.list(ctx)
	if err != nil {
		return nil, err
	}
	if err := resyncSet(f, s.prefix, items, s.key, s.on); err != nil {
		return nil, err
	}
	return func(hctx context.Context) error {
		return follow(hctx, s.src, rv, s.on, func(items []T) error {
			return resyncSet(f, s.prefix, items, s.key, s.on)
		})
	}, nil
}

func memberStream[T any](f *feed, cluster string, src source[T], name func(T) string, view func(T) memberView, snapshot *bool) stream[T] {
	prefix := "member/" + cluster + "/" + src.resource + "/"
	key := func(o T) string { return prefix + name(o) }
	return stream[T]{src: src, prefix: prefix, key: key, on: func(deleted bool, o T) error {
		return f.sendMember(key(o), cluster, view(o), deleted, *snapshot)
	}}
}

func labelOr(labels map[string]string, fallback string) string {
	if m := labels[memberNameLabel]; m != "" {
		return m
	}
	return fallback
}

func (f *feed) open(ctx context.Context, cluster string, snapshot bool) (func(), error) {
	home := clusterhome.HomePath(f.did, cluster)
	key, err := clusterhome.Key(home)
	if err != nil {
		return nil, err
	}
	rHome, err := f.c.resource(home, socialwebResource)
	if err != nil {
		return nil, err
	}
	rHosts, err := f.c.resource(f.c.hostProcessWorkspace, socialwebResource)
	if err != nil {
		return nil, err
	}
	ws := f.c.hostProcessWorkspace
	selector := memberClusterLabel + "=" + key

	clusters := source[v1alpha1.Cluster]{r: rHome, resource: "clusters", field: "metadata.name=" + cluster, where: home}
	onCluster := func(deleted bool, o v1alpha1.Cluster) error {
		if deleted {
			return f.clusterGone(cluster)
		}
		return f.sendCluster(o, snapshot)
	}
	cl, clRV, err := clusters.list(ctx)
	if err != nil {
		return nil, err
	}
	if err := f.resyncClusters(cluster, cl, onCluster); err != nil {
		return nil, err
	}

	var runs []func(context.Context) error
	for _, prime := range []func() (func(context.Context) error, error){
		func() (func(context.Context) error, error) {
			return memberStream(f, cluster, source[v1alpha1.Postgres]{r: rHosts, resource: "postgreses", label: selector, where: ws},
				func(o v1alpha1.Postgres) string { return o.Name },
				func(o v1alpha1.Postgres) memberView {
					return memberView{labelOr(o.Labels, "postgres"), string(o.Status.Phase), "", o.Status.Message}
				}, &snapshot).prime(ctx, f)
		},
		func() (func(context.Context) error, error) {
			return memberStream(f, cluster, source[v1alpha1.OpenBaoServer]{r: rHosts, resource: "openbaoservers", label: selector, where: ws},
				func(o v1alpha1.OpenBaoServer) string { return o.Name },
				func(o v1alpha1.OpenBaoServer) memberView {
					return memberView{labelOr(o.Labels, "openbao"), string(o.Status.Phase), "", o.Status.Message}
				}, &snapshot).prime(ctx, f)
		},
		func() (func(context.Context) error, error) {
			return memberStream(f, cluster, source[v1alpha1.HostProcess]{r: rHosts, resource: "hostprocesses", label: selector, where: ws},
				func(o v1alpha1.HostProcess) string { return o.Name },
				func(o v1alpha1.HostProcess) memberView {
					return memberView{labelOr(o.Labels, o.Name), string(o.Status.Phase), string(o.Status.LostReason), o.Status.Message}
				}, &snapshot).prime(ctx, f)
		},
		func() (func(context.Context) error, error) {
			return stream[v1alpha1.NodeClaim]{
				src:    source[v1alpha1.NodeClaim]{r: rHome, resource: "nodeclaims", where: home},
				prefix: "claim/" + cluster + "/",
				key:    func(o v1alpha1.NodeClaim) string { return "claim/" + cluster + "/" + o.Name },
				on: func(deleted bool, o v1alpha1.NodeClaim) error {
					return f.sendClaim(cluster, o, deleted, snapshot)
				},
			}.prime(ctx, f)
		},
	} {
		run, err := prime()
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	snapshot = false

	hctx, cancel := context.WithCancel(ctx)
	f.mu.Lock()
	f.homes[cluster] = cancel
	f.mu.Unlock()

	runs = append(runs, func(hctx context.Context) error {
		return follow(hctx, clusters, clRV, onCluster, func(items []v1alpha1.Cluster) error {
			return f.resyncClusters(cluster, items, onCluster)
		})
	})
	return func() {
		for _, run := range runs {
			f.wg.Go(func() {
				if err := run(hctx); err != nil && hctx.Err() == nil {
					f.fail(err)
				}
			})
		}
	}, nil
}

func (f *feed) resyncClusters(cluster string, items []v1alpha1.Cluster, on func(bool, v1alpha1.Cluster) error) error {
	for _, o := range items {
		if o.Name == cluster {
			return on(false, o)
		}
	}
	f.mu.Lock()
	_, had := f.sent["cluster/"+cluster]
	f.mu.Unlock()
	if had {
		return f.clusterGone(cluster)
	}
	return nil
}

func resyncSet[T any](f *feed, prefix string, items []T, key func(T) string, on func(bool, T) error) error {
	present := map[string]T{}
	for _, o := range items {
		present[key(o)] = o
		if err := on(false, o); err != nil {
			return err
		}
	}
	for _, k := range f.keysUnder(prefix) {
		if _, ok := present[k]; ok {
			continue
		}
		if err := f.forget(k); err != nil {
			return err
		}
	}
	return nil
}

func (f *feed) forget(key string) error {
	f.mu.Lock()
	fp, ok := f.sent[key]
	delete(f.sent, key)
	f.mu.Unlock()
	if !ok {
		return nil
	}
	var stale struct {
		Type    string
		Payload json.RawMessage
	}
	if err := json.Unmarshal([]byte(fp), &stale); err != nil {
		return nil
	}
	switch stale.Type {
	case gateway.EventMember:
		var ev gateway.MemberEvent
		if json.Unmarshal(stale.Payload, &ev) == nil {
			ev.Deleted, ev.Snapshot, ev.Time = true, false, clock.NowISO()
			return f.emitLocked(gateway.Event{Type: gateway.EventMember, Payload: ev})
		}
	case gateway.EventNodeClaim:
		var ev gateway.NodeClaimEvent
		if json.Unmarshal(stale.Payload, &ev) == nil {
			ev.Deleted, ev.Snapshot, ev.Time = true, false, clock.NowISO()
			return f.emitLocked(gateway.Event{Type: gateway.EventNodeClaim, Payload: ev})
		}
	}
	return nil
}

func (f *feed) emitLocked(ev gateway.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.emit(ev)
}

func fingerprint(typ string, payload any) string {
	b, _ := json.Marshal(struct {
		Type    string
		Payload any
	}{typ, payload})
	return string(b)
}

func (f *feed) sendCluster(o v1alpha1.Cluster, snapshot bool) error {
	reason, message := explain(o.Status.Conditions)
	phase := string(o.Status.Phase)
	if phase == "" {
		phase = string(v1alpha1.ClusterUnavailable)
	}
	ev := gateway.ClusterEvent{
		Name:            o.Name,
		Phase:           phase,
		Running:         o.Spec.Running,
		Instance:        o.Status.Instance,
		Server:          o.Status.URL,
		Reason:          reason,
		Message:         message,
		TeardownBlocked: conditionTrue(o.Status.Conditions, v1alpha1.ConditionTeardownBlocked),
	}
	fp := fingerprint(gateway.EventCluster, ev)
	ev.Snapshot, ev.Time = snapshot, clock.NowISO()
	return f.send("cluster/"+o.Name, gateway.EventCluster, fp, ev)
}

func explain(conds []metav1.Condition) (string, string) {
	for _, t := range []string{v1alpha1.ConditionIdentityVerificationFailed, v1alpha1.ConditionTeardownBlocked} {
		if conditionTrue(conds, t) {
			c := meta.FindStatusCondition(conds, t)
			return c.Reason, c.Message
		}
	}
	if c := meta.FindStatusCondition(conds, "Ready"); c != nil {
		return c.Reason, c.Message
	}
	return "", ""
}

func (f *feed) sendMember(key, cluster string, v memberView, deleted, snapshot bool) error {
	phase := v.phase
	if phase == "" {
		phase = "Pending"
	}
	ev := gateway.MemberEvent{Cluster: cluster, Member: v.member, Phase: phase, Reason: v.reason, Message: v.message}
	if deleted {
		ev.Deleted, ev.Time = true, clock.NowISO()
		return f.gone(key, gateway.EventMember, ev)
	}
	fp := fingerprint(gateway.EventMember, ev)
	ev.Snapshot, ev.Time = snapshot, clock.NowISO()
	return f.send(key, gateway.EventMember, fp, ev)
}

func (f *feed) sendClaim(cluster string, o v1alpha1.NodeClaim, deleted, snapshot bool) error {
	phase := string(o.Status.Phase)
	if phase == "" {
		phase = string(v1alpha1.NodeClaimPending)
	}
	ev := gateway.NodeClaimEvent{
		Cluster:       cluster,
		Name:          o.Name,
		NodeName:      o.Spec.NodeName,
		Phase:         phase,
		Message:       o.Status.Message,
		ContractPhase: string(o.Status.Contract.Phase),
		Outcome:       string(o.Status.Contract.Outcome),
		FQDN:          o.Status.Fqdn,
	}
	if w := o.Status.Contract.Winner; w != nil {
		ev.Bidder = w.DID
	}
	if ev.Message == "" {
		ev.Message = o.Status.Contract.Error
	}
	key := "claim/" + cluster + "/" + o.Name
	if deleted {
		ev.Deleted, ev.Time = true, clock.NowISO()
		return f.gone(key, gateway.EventNodeClaim, ev)
	}
	fp := fingerprint(gateway.EventNodeClaim, ev)
	ev.Snapshot, ev.Time = snapshot, clock.NowISO()
	return f.send(key, gateway.EventNodeClaim, fp, ev)
}
