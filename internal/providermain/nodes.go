package providermain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/cloudprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodemarket"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodeprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/scale"
)

type tenantClient func(ctx context.Context, cluster string) (kubernetes.Interface, error)

var errNoTenantClient = errors.New(
	"clusterprovider: deleting the Node object this node registered needs its cluster's tenant " +
		"apiserver, and no tenant client was supplied -- a k3s agent does not delete its own Node " +
		"object when it stops (kubelet removes it only when a cloud provider is configured, and we " +
		"configure none), so a teardown that skips the delete leaves the tenant cluster counting a " +
		"node that is gone: scale.Decide reads a node count above zero with no intent on every pass " +
		"and calls Teardown again, and the cluster can never be observed at zero nodes, which is the " +
		"objective. A skipped cleanup and a completed one look identical from a green suite")

var errNoJoiningClient = errors.New(
	"clusterprovider: answering whether the tenant cluster reports this node needs that cluster's " +
		"apiserver, and no tenant client was supplied -- that answer is what moves a node from " +
		"Pending to Running, and a guess either promotes a node k3s never registered or holds back a " +
		"node that is already serving")

var errNoNodeAllocator = errors.New(
	"clusterprovider: the apiserver port a node is told to dial comes from the lease file, and no " +
		"allocator was supplied -- without the k3s-secure lease a node dials a port no lease owns, " +
		"which reads as an x509 error from another cluster's apiserver")

var errNoNodeRegistry = errors.New(
	"clusterprovider: every node phase is written through the cluster registry, and none was " +
		"supplied -- the Node CR is the idempotency record the scale decision re-reads on every " +
		"pass, so a node materialised without it is one nothing can count and nothing will tear down")

var errNoNodeStateDir = errors.New(
	"clusterprovider: a node needs a state dir it can write its record under, and none was " +
		"supplied -- the record is what lets a gateway that restarts reap the guest the last one " +
		"booted, and without it that guest keeps running while a new gateway provisions a second one " +
		"under the same node name")

var errNoAdvertiseAddress = errors.New(
	"clusterprovider: a node is told to dial this host at --advertise-address, and none was given. " +
		"A market node's dial address is the one the market's door reverse-forwards into the guest, " +
		"so there is no rule to derive it from: it is a parameter rather than a guess, and a wrong " +
		"one points a node at nothing usable")

var errNoNodeCluster = errors.New(
	"clusterprovider: a node is per cluster, and the empty name has no Cluster CR, no lease and no " +
		"data dir to hang one on")

type marketNodeOptions struct {
	Cluster string

	Server string

	NodeName string

	TokenFile string

	ServerCAFile string

	DataDir string

	AdvertiseAddress string

	ApiserverPort int

	DenyBidderDids []string

	DenyMore func() []string

	Claims nodemarket.Claims

	ClusterSource nodemarket.ClusterSource

	ReleaseTimeout time.Duration

	Record func(ctx context.Context, nodeName string, phase v1alpha1.NodePhase) error

	Log *slog.Logger

	Secrets nodemarket.NodeSecrets

	Links nodemarket.Links
}

func newMarketNodeHost(opts marketNodeOptions) (nodeHost, error) {
	return nodemarket.New(nodemarket.Options{
		Cluster:          opts.Cluster,
		NodeName:         opts.NodeName,
		AdvertiseAddress: opts.AdvertiseAddress,
		ApiserverPort:    opts.ApiserverPort,
		TokenFile:        opts.TokenFile,
		ServerCAFile:     opts.ServerCAFile,
		DataDir:          opts.DataDir,
		DenyBidderDids:   opts.DenyBidderDids,
		DenyMore:         opts.DenyMore,
		Resolve:          opts.ClusterSource,
		Claims:           opts.Claims,
		Record:           opts.Record,
		Log:              opts.Log,
		ReleaseTimeout:   opts.ReleaseTimeout,
		Secrets:          opts.Secrets,
		Links:            opts.Links,
	})
}

type nodeHostsOptions struct {
	Allocator *portallocator.Allocator

	Registry clusterprovider.Clusters

	Claims nodemarket.Claims

	StateDir string

	AdvertiseAddress string

	DenyBidderDids []string

	ClaimReleaseTimeout time.Duration

	Tenant tenantClient

	Log *slog.Logger

	Links nodemarket.Links

	NodeSecrets func(cluster, clusterDir string) nodemarket.NodeSecrets

	Now func() time.Time

	NodeLossGrace time.Duration

	NodeJoinGrace time.Duration

	Denies *bidderDenies

	Observe observeCluster

	PathUp func(ctx context.Context, cluster string) bool
}

const NodeLossGrace = time.Minute

const NodeJoinGrace = time.Minute

func (o nodeHostsOptions) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

func (o nodeHostsOptions) nodeJoinGrace() time.Duration {
	if o.NodeJoinGrace <= 0 {
		return NodeJoinGrace
	}
	return o.NodeJoinGrace
}

func (o nodeHostsOptions) nodeLossGrace() time.Duration {
	if o.NodeLossGrace <= 0 {
		return NodeLossGrace
	}
	return o.NodeLossGrace
}

var errNoNodeClaims = errors.New(
	"clusterprovider: a market node IS a NodeClaim written into its cluster's home workspace, and " +
		"none of the store that writes one was supplied -- a gateway that started anyway would " +
		"count a node it never asked anybody to rent, and the scale decision would ask again on " +
		"every pass for ever")

type nodeHost interface {
	scale.Nodes

	ConfirmJoined(ctx context.Context) error

	Reap(ctx context.Context) error
}

type guestWatcher interface {
	Terminal() string
}

func guestTerminal(host nodeHost) string {
	watcher, ok := host.(guestWatcher)
	if !ok {
		return ""
	}
	return watcher.Terminal()
}

type marketEnder interface {
	End() nodemarket.End
}

func guestEnd(host nodeHost) nodemarket.End {
	if ender, ok := host.(marketEnder); ok {
		return ender.End()
	}
	return nodemarket.End{Message: guestTerminal(host)}
}

type confirmOutcome struct {
	joined []string

	lost []string

	declined []string

	declinedWords string

	failed []string

	failedWords string
}

func (o confirmOutcome) declinedReason() string {
	var out []string
	if len(o.declined) > 0 {
		out = append(out, fmt.Sprintf("%d market claim(s) of this cluster ended with an RFP no bidder took (%s): %s",
			len(o.declined), strings.Join(o.declined, ", "), o.declinedWords))
	}
	if len(o.failed) > 0 {
		out = append(out, fmt.Sprintf("%d market claim(s) of this cluster failed or were lost after a bidder took them (%s): %s",
			len(o.failed), strings.Join(o.failed, ", "), o.failedWords))
	}
	return strings.Join(out, "; ")
}

const BidderDenyWindow = 2 * time.Minute

type bidderDenies struct {
	mu    sync.Mutex
	until map[string]map[string]time.Time
}

func (d *bidderDenies) deny(cluster, did string, until time.Time) {
	if d == nil || did == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.until == nil {
		d.until = map[string]map[string]time.Time{}
	}
	if d.until[cluster] == nil {
		d.until[cluster] = map[string]time.Time{}
	}
	d.until[cluster][did] = until
}

func (d *bidderDenies) active(cluster string, now time.Time) []string {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for did, until := range d.until[cluster] {
		if now.Before(until) {
			out = append(out, did)
		} else {
			delete(d.until[cluster], did)
		}
	}
	slices.Sort(out)
	return out
}

type tenantNodes struct {
	cluster  string
	nodeName string
	host     nodeHost
	tenant   tenantClient

	log *slog.Logger

	opts nodeHostsOptions

	mu            sync.Mutex
	started       int
	extra         map[int]heldNode
	live          map[string]bool
	joined        map[string]bool
	notReadySince map[string]time.Time
	readySeen     map[string]bool
	path          pathClock
}

type heldNode struct {
	name string
	host nodeHost
}

var _ scale.Nodes = (*tenantNodes)(nil)

func (n *tenantNodes) Provision(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	ordinal := 1
	primary := n.started == 0 && n.host != nil
	if primary {
		spent, err := n.primaryNameSpent(ctx)
		if err != nil {
			return err
		}
		if spent {
			n.forgetHeldLocked(n.nodeName)
			primary = false
		}
	}
	if !primary {
		next, err := n.nextOrdinal(ctx)
		if err != nil {
			return err
		}
		ordinal = next
	}

	host, name, held := n.hostFor(ordinal)
	if !held {
		id := nodeIDFor(ordinal)
		nodeName, err := nodeprovider.NodeName(n.cluster, id)
		if err != nil {
			return err
		}
		built, err := newNodeHost(n.opts, n.log, n.cluster, id)
		if err != nil {
			return err
		}
		host, name = built.host, nodeName
	}

	if err := host.Provision(ctx); err != nil {
		return fmt.Errorf("clusterprovider: provisioning node %s of %s: %w -- there is no lease to "+
			"give back here, because a node holds none of its own: every port its guest binds, its "+
			"kubelet's included, is bound inside the guest's own network namespace, and the "+
			"node host stops whatever it started before it returns", name, n.cluster, err)
	}
	if !held {
		n.retain(ordinal, heldNode{name: name, host: host})
	}
	n.markLive(name)
	n.started++
	if err := n.writeSnapshot(); err != nil {
		return err
	}
	n.log.Info("cluster provider: the claim is written and this pass returns without waiting "+
		"for the machine", "cluster", n.cluster, "node", name,
		"reason", "a market machine takes minutes to appear in the tenant cluster's node list, "+
			"and this pass is one of many in one process: blocking here would hold back every "+
			"other cluster for as long as the door takes to boot one",
		"next", "a later pass confirms the node once the claim reads Running AND the tenant "+
			"cluster reports the Node, and a claim that reads Failed fails the node here")
	return nil
}

func (n *tenantNodes) confirmHeld(ctx context.Context) (confirmOutcome, error) {
	var outcome confirmOutcome
	pathUp := true
	if n.opts.PathUp != nil {
		pathUp = n.opts.PathUp(ctx, n.cluster)
	}
	n.mu.Lock()
	n.path.observe(pathUp, n.opts.now())
	held := n.heldNodesLocked()
	n.mu.Unlock()

	var problems []error
	for _, node := range held {
		end := guestEnd(node.host)
		if end.Message != "" && !end.Declined {
			outcome.failed = append(outcome.failed, node.name)
			if outcome.failedWords == "" {
				outcome.failedWords = end.Message
			}
			if end.Bidder != "" {
				until := n.opts.now().Add(BidderDenyWindow)
				n.opts.Denies.deny(n.cluster, end.Bidder, until)
				n.log.Warn("cluster provider: a market claim of this cluster failed or was lost after a bidder took it; "+
					"that bidder is left out of this cluster's RFPs for a short window so another bidder can take the next one",
					"cluster", n.cluster, "node", node.name, "bidder", end.Bidder, "until", until.UTC().Format(time.RFC3339),
					"reason", end.Message)
			}
		}
		if end.Lost {
			n.log.Warn("cluster provider: the door marked a market node's claim Lost because its bidder "+
				"reported the guest terminated, so it is released now rather than after the NotReady "+
				"grace; its pods reschedule and the scale pass rents a replacement if the work still "+
				"needs one", "cluster", n.cluster, "node", node.name, "reason", end.Message)
			if err := n.teardownHeld(ctx, node); err != nil {
				problems = append(problems, err)
				continue
			}
			n.clearNotReady(node.name)
			outcome.lost = append(outcome.lost, node.name)
			continue
		} else if end.Message != "" {
			next := "the claim is released and the next pass may ask the market again"
			if end.Declined {
				outcome.declined = append(outcome.declined, node.name)
				if outcome.declinedWords == "" {
					outcome.declinedWords = end.Message
				}
				next = "the claim is released, and the market asked for it and took no bid, so " +
					"this cluster's next market provision waits out its back-off"
			}
			problems = append(problems, fmt.Errorf(
				"clusterprovider: node %s of %s stopped making progress before the tenant cluster "+
					"reported it, so it is given up rather than left counted: %s -- %s",
				node.name, n.cluster, end.Message, next))
			if err := n.teardownHeld(ctx, node); err != nil {
				problems = append(problems, err)
			}
			continue
		}
		if syncer, ok := node.host.(secretSyncer); ok {
			if err := syncer.SyncSecrets(ctx); err != nil {
				problems = append(problems, err)
			}
		}
		reported, err := readTenantNode(ctx, n.tenant, n.cluster, node.name)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if reported == nil {
			n.clearNotReady(node.name)
			continue
		}
		if lost, since := n.lost(reported); lost {
			n.log.Warn("cluster provider: a market node has been NotReady past the loss grace, so it "+
				"is released and its Node deleted; its pods reschedule and the scale pass rents a "+
				"replacement if the work still needs one",
				"cluster", n.cluster, "node", node.name, "notReadySince", since.UTC().Format(time.RFC3339),
				"grace", n.opts.nodeLossGrace())
			if err := n.teardownHeld(ctx, node); err != nil {
				problems = append(problems, err)
				continue
			}
			n.clearNotReady(node.name)
			outcome.lost = append(outcome.lost, node.name)
			continue
		}
		if err := node.host.ConfirmJoined(ctx); errors.Is(err, nodemarket.ErrAwaitingAdoption) {
			continue
		} else if err != nil {
			problems = append(problems, fmt.Errorf("clusterprovider: confirming node %s of %s "+
				"after the tenant cluster reported it: %w", node.name, n.cluster, err))
			continue
		}
		if n.markJoined(node.name) {
			outcome.joined = append(outcome.joined, node.name)
		}
	}
	return outcome, errors.Join(problems...)
}

type secretSyncer interface {
	SyncSecrets(ctx context.Context) error
}

func (n *tenantNodes) lost(node *corev1.Node) (bool, time.Time) {
	var ready *corev1.NodeCondition
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == corev1.NodeReady {
			ready = &node.Status.Conditions[i]
			break
		}
	}
	if ready != nil && ready.Status == corev1.ConditionTrue {
		n.clearNotReady(node.Name)
		n.mu.Lock()
		if n.readySeen == nil {
			n.readySeen = map[string]bool{}
		}
		n.readySeen[node.Name] = true
		n.mu.Unlock()
		return false, time.Time{}
	}
	if ready == nil {
		n.clearNotReady(node.Name)
		return false, time.Time{}
	}
	now := n.opts.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.path.holding() {
		return false, time.Time{}
	}
	grace := n.opts.nodeLossGrace()
	if !n.readySeen[node.Name] && ready.Status == corev1.ConditionFalse {
		grace = n.opts.nodeJoinGrace()
	}
	since := ready.LastTransitionTime.Time
	if since.IsZero() {
		if n.notReadySince == nil {
			n.notReadySince = map[string]time.Time{}
		}
		first, seen := n.notReadySince[node.Name]
		if !seen {
			first = now
			n.notReadySince[node.Name] = now
		}
		since = first
	}
	since = n.path.rebase(since)
	return now.Sub(since) >= grace, since
}

func (n *tenantNodes) clearNotReady(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.notReadySince, name)
}

func (n *tenantNodes) markJoined(name string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.joined[name] {
		return false
	}
	if n.joined == nil {
		n.joined = map[string]bool{}
	}
	n.joined[name] = true
	return true
}

func (n *tenantNodes) markLive(name string) {
	if n.live == nil {
		n.live = map[string]bool{}
	}
	n.live[name] = true
}

func (n *tenantNodes) snapshot() []cloudprovider.Instance {
	names := make([]string, 0, len(n.live))
	for name := range n.live {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]cloudprovider.Instance, 0, len(names))
	for _, name := range names {
		out = append(out, cloudprovider.Instance{NodeName: name, Cluster: n.cluster, Exists: true})
	}
	return out
}

func (n *tenantNodes) writeSnapshot() error {
	if n.opts.StateDir == "" {
		return fmt.Errorf("clusterprovider: %s's instance snapshot has no state dir to be written "+
			"under, so the cloud controller manager cannot be told which of its Nodes still have a "+
			"guest. A snapshot that was never written reads as \"the guest is gone\"", n.cluster)
	}
	dir, err := clusterprovider.ClusterDataDir(n.opts.StateDir, n.cluster)
	if err != nil {
		return err
	}
	if err := cloudprovider.WriteSnapshot(clusterstack.NodeSnapshotPath(dir), n.snapshot()); err != nil {
		return fmt.Errorf("clusterprovider: writing the instance snapshot for %s: %w. The cloud "+
			"controller manager reads it per call to decide whether a Node still has a guest, and a "+
			"write that landed short of the whole set would read as \"the guest is gone\"", n.cluster, err)
	}
	return nil
}

func (n *tenantNodes) hostFor(ordinal int) (nodeHost, string, bool) {
	if ordinal == 1 && n.host != nil {
		return n.host, n.nodeName, true
	}
	if held, ok := n.extra[ordinal]; ok {
		return held.host, held.name, true
	}
	return nil, "", false
}

func (n *tenantNodes) retain(ordinal int, held heldNode) {
	if ordinal == 1 {
		if n.host == nil {
			n.host, n.nodeName = held.host, held.name
		}
		return
	}
	if n.extra == nil {
		n.extra = map[int]heldNode{}
	}
	n.extra[ordinal] = held
}

func (n *tenantNodes) nextOrdinal(ctx context.Context) (int, error) {
	present := map[int]bool{}
	prefix := strings.ToLower(n.cluster) + "-node"
	for _, held := range n.heldNodesLocked() {
		if ordinal, ok := nodeOrdinalInName(prefix, held.name); ok {
			present[ordinal] = true
		}
	}
	if n.tenant != nil {
		client, err := n.tenant(ctx, n.cluster)
		if err != nil {
			return 0, fmt.Errorf("clusterprovider: the tenant client for %s, which says which of "+
				"its nodes already exist before the next one is named: %w", n.cluster, err)
		}
		list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, fmt.Errorf("clusterprovider: listing the nodes of %s before naming the next "+
				"one: %w -- a read that failed is not a cluster with no nodes, and naming a node "+
				"over one that already exists is the collision this list exists to avoid; nothing "+
				"was provisioned", n.cluster, err)
		}
		for i := range list.Items {
			if ordinal, ok := nodeOrdinalInName(prefix, list.Items[i].Name); ok {
				present[ordinal] = true
			}
		}
	}
	if n.opts.Claims != nil {
		target, err := claimClusterFor(n.opts.Registry, n.cluster)(ctx)
		if err != nil {
			return 0, fmt.Errorf("clusterprovider: the home workspace of %s, which holds the claims "+
				"that say which node names are already spent: %w", n.cluster, err)
		}
		claims, err := n.opts.Claims.ListNodeClaims(ctx, target.Workspace)
		if err != nil {
			return 0, fmt.Errorf("clusterprovider: listing the node claims already written in %s "+
				"before naming the next node of %s: %w -- a read that failed is not a workspace with "+
				"no claims, and a name whose claim has ended is a name whose next claim inherits the "+
				"old one's last words; nothing was provisioned", target.Workspace, n.cluster, err)
		}
		for i := range claims {
			if !endedClaimPhase(claims[i].Status.Phase) {
				continue
			}
			if ordinal, ok := nodeOrdinalInName(prefix, claims[i].Name); ok {
				present[ordinal] = true
			}
		}
	}
	for ordinal := 1; ; ordinal++ {
		if !present[ordinal] {
			return ordinal, nil
		}
	}
}

func endedClaimPhase(phase v1alpha1.NodeClaimPhase) bool {
	return phase == v1alpha1.NodeClaimFailed || phase == v1alpha1.NodeClaimReleased ||
		phase == v1alpha1.NodeClaimLost
}

func (n *tenantNodes) Teardown(ctx context.Context) error {
	if _, err := n.client(ctx); err != nil {
		return fmt.Errorf("clusterprovider: the tenant client for %s, which every teardown of its "+
			"nodes needs before it stops anything -- stopping an agent first would leave a node with "+
			"no process behind a Node object nothing here can delete: %w", n.cluster, err)
	}
	n.mu.Lock()
	held := n.heldNodesLocked()
	n.mu.Unlock()

	var problems []error
	for _, node := range held {
		if err := n.teardownHeld(ctx, node); err != nil {
			problems = append(problems, err)
		}
	}
	if err := n.writeSnapshot(); err != nil {
		problems = append(problems, err)
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	n.mu.Lock()
	n.extra = nil
	n.mu.Unlock()
	return nil
}

func (n *tenantNodes) teardownHeld(ctx context.Context, node heldNode) error {
	if err := node.host.Teardown(ctx); err != nil {
		return err
	}
	n.forgetHeld(node.name)
	n.mu.Lock()
	delete(n.live, node.name)
	n.mu.Unlock()
	client, err := n.client(ctx)
	if err != nil {
		return fmt.Errorf("clusterprovider: the tenant client, needed to delete the Node object of "+
			"%s after its guest was stopped: %w -- left behind it reads as a node that still exists, "+
			"so the next pass tears down a node that is already gone", node.name, err)
	}
	if err := client.CoreV1().Nodes().Delete(ctx, node.name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			n.log.Info("clusterprovider: the tenant cluster holds no Node object for this node",
				"cluster", n.cluster, "node", node.name)
			return nil
		}
		return fmt.Errorf("clusterprovider: deleting the tenant cluster's "+
			"Node object %s: %w -- the guest is stopped, and this object is the count the scale "+
			"decision reads: left behind it reads as a node that still exists, so the next pass "+
			"tears down a node that is already gone and the cluster never reaches the zero state",
			node.name, err)
	}
	n.log.Info("clusterprovider: removed the node from the tenant cluster",
		"cluster", n.cluster, "node", node.name)
	return nil
}

func (n *tenantNodes) primaryNameSpent(ctx context.Context) (bool, error) {
	if n.opts.Claims == nil || n.nodeName == "" {
		return false, nil
	}
	target, err := claimClusterFor(n.opts.Registry, n.cluster)(ctx)
	if err != nil {
		return false, fmt.Errorf("clusterprovider: the home workspace of %s, whose claim decides "+
			"whether the name this host built for its first node of %s is still usable: %w",
			n.cluster, n.cluster, err)
	}
	claim, err := n.opts.Claims.ReadNodeClaim(ctx, target.Workspace, n.nodeName)
	if err != nil {
		return false, fmt.Errorf("clusterprovider: reading the claim at %s before provisioning the "+
			"first node of %s under it: %w -- a read that failed is not a name that is free, and a "+
			"claim whose spec already matches is left exactly as it was, status included; nothing "+
			"was provisioned", n.nodeName, n.cluster, err)
	}
	return claim != nil && endedClaimPhase(claim.Status.Phase), nil
}

func (n *tenantNodes) forgetHeld(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.forgetHeldLocked(name)
}

func (n *tenantNodes) forgetHeldLocked(name string) {
	prefix := strings.ToLower(n.cluster) + "-node"
	ordinal, ok := nodeOrdinalInName(prefix, name)
	if !ok {
		return
	}
	if n.host != nil && n.nodeName == name {
		n.host, n.nodeName = nil, ""
	}
	delete(n.extra, ordinal)
	delete(n.joined, name)
}

func (n *tenantNodes) inFlight(reported map[string]bool) int {
	n.mu.Lock()
	held := n.heldNodesLocked()
	n.mu.Unlock()
	count := 0
	for _, node := range held {
		if !reported[node.name] {
			count++
		}
	}
	return count
}

func (n *tenantNodes) heldNodesLocked() []heldNode {
	out := make([]heldNode, 0, 1+len(n.extra))
	if n.host != nil {
		out = append(out, heldNode{name: n.nodeName, host: n.host})
	}
	ordinals := make([]int, 0, len(n.extra))
	for ordinal := range n.extra {
		ordinals = append(ordinals, ordinal)
	}
	slices.Sort(ordinals)
	for _, ordinal := range ordinals {
		out = append(out, n.extra[ordinal])
	}
	return out
}

func (n *tenantNodes) client(ctx context.Context) (kubernetes.Interface, error) {
	if n.tenant == nil {
		return nil, errNoTenantClient
	}
	client, err := n.tenant(ctx, n.cluster)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: the tenant client for %s: %w", n.cluster, err)
	}
	if client == nil {
		return nil, errNoTenantClient
	}
	return client, nil
}

type nodeHosts struct {
	opts nodeHostsOptions
	log  *slog.Logger

	mu        sync.Mutex
	hosts     map[string]*tenantNodes
	adopted   map[string]bool
	released  map[string]map[string]bool
	synced    map[string]time.Time
	seenReady map[string]map[string]time.Time
}

func (h *nodeHosts) observedReadySince(cluster, node string, ready bool, now time.Time) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seenReady == nil {
		h.seenReady = map[string]map[string]time.Time{}
	}
	nodes := h.seenReady[cluster]
	if nodes == nil {
		nodes = map[string]time.Time{}
		h.seenReady[cluster] = nodes
	}
	if !ready {
		delete(nodes, node)
		return time.Time{}
	}
	first, ok := nodes[node]
	if !ok {
		nodes[node] = now
		return now
	}
	return first
}

func releasedClaimKey(claim v1alpha1.NodeClaim) string {
	return claim.Name + "/" + string(claim.UID)
}

func (h *nodeHosts) unconfirmedReleases(cluster string, gone map[string]string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := h.released[cluster]
	kept := map[string]bool{}
	var pending []string
	for name, key := range gone {
		if seen[key] {
			kept[key] = true
			continue
		}
		pending = append(pending, name)
	}
	if h.released == nil {
		h.released = map[string]map[string]bool{}
	}
	h.released[cluster] = kept
	slices.Sort(pending)
	return pending
}

func (h *nodeHosts) confirmRelease(cluster, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released == nil {
		h.released = map[string]map[string]bool{}
	}
	if h.released[cluster] == nil {
		h.released[cluster] = map[string]bool{}
	}
	h.released[cluster][key] = true
}

func newNodeHosts(opts nodeHostsOptions) (*nodeHosts, error) {
	switch {
	case opts.Allocator == nil:
		return nil, errNoNodeAllocator
	case opts.Registry == nil:
		return nil, errNoNodeRegistry
	case opts.StateDir == "":
		return nil, errNoNodeStateDir
	case opts.Claims == nil:
		return nil, errNoNodeClaims
	case opts.AdvertiseAddress == "":
		return nil, errNoAdvertiseAddress
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.Denies == nil {
		opts.Denies = &bidderDenies{}
	}
	return &nodeHosts{opts: opts, log: log, hosts: map[string]*tenantNodes{},
		synced: map[string]time.Time{}}, nil
}

func (h *nodeHosts) forCluster(cluster string) (scale.Nodes, error) {
	if cluster == "" {
		return nil, errNoNodeCluster
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if n, ok := h.hosts[cluster]; ok {
		return n, nil
	}
	created, err := newNodeHost(h.opts, h.log, cluster, nodeID)
	if err != nil {
		return nil, err
	}
	h.hosts[cluster] = created
	return created, nil
}

func (h *nodeHosts) inFlight(cluster string, reported map[string]bool) int {
	h.mu.Lock()
	n, ok := h.hosts[cluster]
	h.mu.Unlock()
	if !ok {
		return 0
	}
	return n.inFlight(reported)
}

func (h *nodeHosts) clusters() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.hosts))
	for cluster := range h.hosts {
		out = append(out, cluster)
	}
	slices.Sort(out)
	return out
}

func (h *nodeHosts) resetSnapshot(cluster string) error {
	dir, err := clusterprovider.ClusterDataDir(h.opts.StateDir, cluster)
	if err != nil {
		return err
	}
	return cloudprovider.WriteSnapshot(clusterstack.NodeSnapshotPath(dir), nil)
}

func (h *nodeHosts) reconcile(ctx context.Context, cluster string) error {
	if h.opts.Claims == nil || h.opts.Tenant == nil {
		return nil
	}
	if dir, err := clusterprovider.ClusterDataDir(h.opts.StateDir, cluster); err == nil {
		if pem, err := os.ReadFile(clusterstack.OpenBaoTLSCAPath(dir)); err == nil {
			if err := publishCA(clusterstack.PublicOpenBaoCAPath(dir), pem); err != nil {
				h.log.Warn("cluster provider: the cluster's public OpenBao CA could not be published", "cluster", cluster, "err", err.Error())
			}
		}
	}
	target, err := claimClusterFor(h.opts.Registry, cluster)(ctx)
	if err != nil {
		return fmt.Errorf("clusterprovider: the home workspace of %s, which holds the claims that "+
			"say whose machine the door has taken away: %w", cluster, err)
	}
	claims, err := h.opts.Claims.ListNodeClaims(ctx, target.Workspace)
	if err != nil {
		return fmt.Errorf("clusterprovider: listing the node claims in %s to find the ones the door "+
			"confirmed released: %w -- a read that failed is not a workspace with no claims, and it is "+
			"the claims that say which Nodes have no machine behind them", target.Workspace, err)
	}
	claims, orphanErr := h.claimJoinedOrphans(ctx, cluster, claims)
	adoptErr := errors.Join(orphanErr, h.adoptClaims(cluster, claims), h.releaseStranded(ctx, cluster, claims))
	if orphanErr == nil {
		adoptErr = errors.Join(adoptErr, h.reconcileVault(ctx, cluster, claims))
	}
	released := map[string]string{}
	for i := range claims {
		if claims[i].Status.Phase == v1alpha1.NodeClaimReleased {
			released[claims[i].Name] = releasedClaimKey(claims[i])
		}
	}
	gone := h.unconfirmedReleases(cluster, released)
	if len(gone) == 0 {
		return adoptErr
	}
	client, err := h.opts.Tenant(ctx, cluster)
	if err != nil {
		return errors.Join(adoptErr, fmt.Errorf("clusterprovider: the tenant client for %s, needed to "+
			"delete the Nodes whose claims the door confirmed released: %w", cluster, err))
	}
	list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Join(adoptErr, fmt.Errorf("clusterprovider: listing the nodes of %s before "+
			"removing the ones whose claim reads Released: %w -- a claim the door released stays in "+
			"its workspace as the record of what happened, so the list of names to act on is not the "+
			"list of Nodes still there, and deleting by name alone spends one write per pass for ever "+
			"on a Node that went the first time", cluster, err))
	}
	present := map[string]bool{}
	for i := range list.Items {
		present[list.Items[i].Name] = true
	}
	problems := []error{adoptErr}
	for _, name := range gone {
		if !present[name] {
			h.confirmRelease(cluster, released[name])
			continue
		}
		err := client.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			problems = append(problems, fmt.Errorf("clusterprovider: deleting the tenant cluster's "+
				"Node %s of %s, whose claim reads Released: %w -- the door writes Released only where "+
				"it took the machine away, and a Node left behind it still reads Ready, so the "+
				"scheduler places pods on a guest that is gone and the workload waits out its own "+
				"timeout on a machine that is not coming back", name, cluster, err))
			continue
		}
		h.confirmRelease(cluster, released[name])
		h.log.Info("cluster provider: removed a Node the door has taken the machine away for",
			"cluster", cluster, "node", name)
	}
	return errors.Join(problems...)
}

func (h *nodeHosts) confirm(ctx context.Context, cluster string) (confirmOutcome, error) {
	h.mu.Lock()
	n, ok := h.hosts[cluster]
	h.mu.Unlock()
	if !ok {
		return confirmOutcome{}, nil
	}
	return n.confirmHeld(ctx)
}

func (h *nodeHosts) reapCluster(ctx context.Context, cluster string) error {
	h.mu.Lock()
	n, ok := h.hosts[cluster]
	if ok {
		delete(h.hosts, cluster)
		delete(h.adopted, cluster)
	}
	h.mu.Unlock()
	if !ok {
		return nil
	}
	n.mu.Lock()
	nodes := n.heldNodesLocked()
	n.mu.Unlock()
	var problems []error
	for _, node := range nodes {
		h.log.Info("cluster provider: reaping the node host of a cluster that is OFF or deleting",
			"cluster", cluster, "node", node.name)
		if err := node.host.Reap(ctx); err != nil {
			problems = append(problems, fmt.Errorf(
				"clusterprovider: reaping node %s of %s, whose CR is OFF or deleting: %w",
				node.name, cluster, err))
		}
	}
	n.mu.Lock()
	n.live = nil
	n.mu.Unlock()
	if err := n.writeSnapshot(); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func (h *nodeHosts) reap(ctx context.Context) error {
	h.mu.Lock()
	held := make(map[string]*tenantNodes, len(h.hosts))
	maps.Copy(held, h.hosts)
	h.mu.Unlock()
	clusters := make([]string, 0, len(held))
	for cluster := range held {
		clusters = append(clusters, cluster)
	}
	slices.Sort(clusters)

	h.log.Info("cluster provider: leaving the market nodes running for the next provider process",
		"nodeClusters", clusters,
		"reason", "a market guest joined with no session of ours and outlives this process; "+
			"deleting its claim is what makes the door destroy the machine, so a restart that "+
			"released here would reap every node on every deploy",
		"next", "the next provider adopts every live claim at startup and holds it again")
	return nil
}

func newNodeHost(opts nodeHostsOptions, log *slog.Logger, cluster, nodeID string) (*tenantNodes, error) {
	apiserver, err := opts.Allocator.Allocate(portallocator.Request{
		Cluster: cluster, Purpose: portallocator.PurposeK3sSecure,
	})
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: the apiserver lease for %s, which is both where "+
			"its k3s binds and what a node of the cluster is told to dial: %w", cluster, err)
	}
	name, err := nodeprovider.NodeName(cluster, nodeID)
	if err != nil {
		return nil, err
	}
	clusterDir, err := clusterprovider.ClusterDataDir(opts.StateDir, cluster)
	if err != nil {
		return nil, err
	}
	host, err := newNodeHostOf(opts, log, cluster, name, apiserver.Port, clusterDir)
	if err != nil {
		return nil, err
	}
	return &tenantNodes{
		cluster:  cluster,
		nodeName: name,
		host:     host,
		tenant:   opts.Tenant,
		log:      log,
		opts:     opts,
	}, nil
}

func nodeOrdinal(nodeID string) (int, error) {
	digits, ok := strings.CutPrefix(nodeID, "node")
	if !ok || digits == "" {
		return 0, fmt.Errorf("clusterprovider: the node id %q does not name which node of its "+
			"cluster it is, and that number is what picks the node's address out of the cluster's "+
			"range: the range holds one address per node, numbered from the gateway, so an id with "+
			"no ordinal in it leaves the node with no address to take. The id is composed with the "+
			"cluster into the node's name, and this host already builds ids as \"node<N>\"; reading "+
			"the number off the id rather than carrying a second constant beside it is what keeps "+
			"an address and a name from disagreeing", nodeID)
	}
	ordinal, err := strconv.Atoi(digits)
	if err != nil {
		return 0, fmt.Errorf("clusterprovider: the node id %q ends in %q, which is not the number "+
			"of the node within its cluster: %w", nodeID, digits, err)
	}
	if ordinal < 1 {
		return 0, fmt.Errorf("clusterprovider: the node id %q names node %d of its cluster, and "+
			"nodes are numbered from 1: the addresses in a cluster's range are numbered from 1 as "+
			"well, and address 1 is the gateway the guests dial, so there is no node 0 to give one "+
			"to", nodeID, ordinal)
	}
	return ordinal, nil
}

func nodeIDFor(ordinal int) string {
	return fmt.Sprintf("node%d", ordinal)
}

func nodeOrdinalInName(prefix, name string) (int, bool) {
	rest, ok := strings.CutPrefix(strings.ToLower(name), prefix)
	if !ok || rest == "" {
		return 0, false
	}
	ordinal, err := strconv.Atoi(rest)
	if err != nil || ordinal < 1 {
		return 0, false
	}
	return ordinal, true
}

func newNodeHostOf(opts nodeHostsOptions, log *slog.Logger, cluster, name string, apiserverPort int, clusterDir string) (nodeHost, error) {
	server := fmt.Sprintf("https://%s:%d", opts.AdvertiseAddress, apiserverPort)
	var secrets nodemarket.NodeSecrets
	if opts.NodeSecrets != nil {
		secrets = opts.NodeSecrets(cluster, clusterDir)
	}
	return newMarketNodeHost(marketNodeOptions{
		Cluster:          cluster,
		Server:           server,
		NodeName:         name,
		TokenFile:        clusterprovider.TokenPath(clusterDir),
		ServerCAFile:     filepath.Join(clusterstack.TLSDir(clusterDir), "server-ca.crt"),
		DataDir:          clusterprovider.NodeDataDir(opts.StateDir, cluster, name),
		AdvertiseAddress: opts.AdvertiseAddress,
		ApiserverPort:    apiserverPort,
		DenyBidderDids:   opts.DenyBidderDids,
		DenyMore:         func() []string { return opts.Denies.active(cluster, opts.now()) },
		Claims:           opts.Claims,
		ClusterSource:    claimClusterFor(opts.Registry, cluster),
		ReleaseTimeout:   opts.ClaimReleaseTimeout,
		Record:           nodeRecorder(opts.Registry, cluster, name, unlimitedNodeExpiry()),
		Log:              log,
		Secrets:          secrets,
		Links:            opts.Links,
	})
}

func claimClusterFor(registry clusterprovider.Clusters, cluster string) nodemarket.ClusterSource {
	return func(ctx context.Context) (nodemarket.Cluster, error) {
		ref, err := nodeRefFor(ctx, registry, cluster)
		if err != nil {
			return nodemarket.Cluster{}, err
		}
		if ref.Home == "" {
			return nodemarket.Cluster{}, fmt.Errorf(
				"clusterprovider: the Cluster CR for %s was found with no home workspace, so there "+
					"is no logical cluster to write this node's claim into: the claim lives in the "+
					"owner's own home, and a claim written anywhere else is one the door's listing "+
					"across clusters/* never sees", cluster)
		}
		cl, err := registry.Read(ctx, ref)
		if err != nil {
			return nodemarket.Cluster{}, fmt.Errorf(
				"clusterprovider: reading Cluster %s out of %s, whose spec.account is the DID the "+
					"market rents this node AS: %w -- the door fulfils the claim through that "+
					"owner's own OAuth session, so a claim with no owner DID is one no door can "+
					"act for", cluster, ref.Home, err)
		}
		if cl.Spec.Account == "" {
			return nodemarket.Cluster{}, fmt.Errorf(
				"clusterprovider: Cluster %s in %s carries no spec.account. That DID is the "+
					"requester the node is rented as, and it is not derivable from the cluster name: "+
					"a claim written without it names no session the door holds", cluster, ref.Home)
		}
		return nodemarket.Cluster{Workspace: ref.Home, OwnerDid: cl.Spec.Account, Name: cl.Name, UID: cl.UID}, nil
	}
}

func unlimitedNodeExpiry() *int64 {
	expiry := int64(clusterprovider.NodeExpiryUnlimited)
	return &expiry
}

func nodeRecorder(registry clusterprovider.Clusters, cluster, name string, expiry *int64) func(context.Context, string, v1alpha1.NodePhase) error {
	return func(ctx context.Context, _ string, phase v1alpha1.NodePhase) error {
		ref, err := nodeRefFor(ctx, registry, cluster)
		if err != nil {
			return err
		}
		return registry.WriteNode(ctx, ref, name, v1alpha1.NodeStatus{Phase: phase, NodeExpiry: expiry})
	}
}

func nodeRefFor(ctx context.Context, registry clusterprovider.Clusters, key string) (clusterprovider.Ref, error) {
	refs, err := registry.Refs(ctx)
	if err != nil {
		return clusterprovider.Ref{}, err
	}
	for _, ref := range refs {
		found, err := ref.Key()
		if err != nil {
			return clusterprovider.Ref{}, err
		}
		if found == key {
			return ref, nil
		}
	}
	return clusterprovider.Ref{}, fmt.Errorf(
		"clusterprovider: no Cluster CR with the key %s in the tree this registry searched, so this "+
			"node's phase has nowhere to be written and the scale decision would read a cluster with "+
			"no node on every pass", key)
}

func readTenantNode(ctx context.Context, tenant tenantClient, cluster, nodeName string) (*corev1.Node, error) {
	if tenant == nil {
		return nil, errNoJoiningClient
	}
	client, err := tenant(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: the tenant client for %s, which is what says "+
			"whether node %s is in that cluster's node list: %w", cluster, nodeName, err)
	}
	if client == nil {
		return nil, errNoJoiningClient
	}
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("clusterprovider: reading node %s from the tenant cluster %s: %w "+
			"-- a read that failed is not a node that is missing, and acting on the two alike holds "+
			"back a node that is already serving", nodeName, cluster, err)
	}
	return node, nil
}

func nodeReported(ctx context.Context, tenant tenantClient, cluster, nodeName string) (bool, error) {
	node, err := readTenantNode(ctx, tenant, cluster, nodeName)
	return node != nil, err
}
