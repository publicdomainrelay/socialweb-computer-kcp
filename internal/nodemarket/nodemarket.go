package nodemarket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/scale"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type Claims interface {
	WriteNodeClaim(ctx context.Context, logicalCluster string, claim v1alpha1.NodeClaim) error

	ReadNodeClaim(ctx context.Context, logicalCluster, name string) (*v1alpha1.NodeClaim, error)

	DeleteNodeClaim(ctx context.Context, logicalCluster, name string) error

	ListNodeClaims(ctx context.Context, logicalCluster string) ([]v1alpha1.NodeClaim, error)

	AwaitNodeClaimGone(ctx context.Context, logicalCluster, name string) error
}

type Cluster struct {
	Workspace string

	OwnerDid string

	Name string

	UID types.UID
}

type ClusterSource func(ctx context.Context) (Cluster, error)

type Options struct {
	Cluster string

	NodeName string

	AdvertiseAddress string

	ApiserverPort int

	TokenFile string

	ServerCAFile string

	DataDir string

	DenyBidderDids []string

	DenyMore func() []string

	Resolve ClusterSource

	Claims Claims

	Record func(ctx context.Context, nodeName string, phase v1alpha1.NodePhase) error

	Log *slog.Logger

	ReleaseTimeout time.Duration

	Secrets NodeSecrets

	Links Links
}

const DefaultReleaseTimeout = 2 * time.Minute

const fatalReadTimeout = 2 * time.Second

var ErrAwaitingAdoption = errors.New("nodemarket: the claim waits for the door to match the joined node to its receipt")

const claimGoneMessage = "the claim this node was rented under no longer exists: it was written " +
	"before this node was counted, and the door releases a claim's machine before the claim can go, " +
	"so a counted node with no claim is a machine already released"

var ErrIncomplete = errors.New(
	"nodemarket: a market node is a claim written into the cluster's own home workspace and " +
		"fulfilled by a door in another process, and this one is missing an input that cannot be " +
		"derived: the workspace and owner DID it is claimed under, the store the claim is written " +
		"through, the paths the door reads the join material from, the address and port the node " +
		"dials its apiserver on, or the Node CR its phase is recorded in")

type Host struct {
	mu   sync.Mutex
	opts Options

	provisioned   bool
	policyWritten bool
	secretWritten bool
	heldBack      string
	granted       *v1alpha1.NodeClaimGrant
}

var _ scale.Nodes = (*Host)(nil)

func New(opts Options) (*Host, error) {
	opts = opts.withDefaults()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return &Host{opts: opts}, nil
}

func (o Options) withDefaults() Options {
	o.ReleaseTimeout = opt.Positive(o.ReleaseTimeout, DefaultReleaseTimeout)
	return o
}

func (o Options) Validate() error {
	var missing []string
	for _, req := range []struct{ name, value string }{
		{"Cluster", o.Cluster},
		{"NodeName", o.NodeName},
		{"AdvertiseAddress", o.AdvertiseAddress},
		{"TokenFile", o.TokenFile},
		{"ServerCAFile", o.ServerCAFile},
		{"DataDir", o.DataDir},
	} {
		if req.value == "" {
			missing = append(missing, req.name)
		}
	}
	if o.ApiserverPort <= 0 || o.ApiserverPort > 65535 {
		missing = append(missing, "ApiserverPort")
	}
	if o.Resolve == nil {
		missing = append(missing, "Resolve")
	}
	if o.Claims == nil {
		missing = append(missing, "Claims")
	}
	if o.Record == nil {
		missing = append(missing, "Record")
	}
	if o.Secrets != nil && o.Links == nil {
		missing = append(missing, "Links")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w (missing %s)", ErrIncomplete, strings.Join(missing, ", "))
	}
	return nil
}

func (h *Host) log() *slog.Logger {
	if h.opts.Log == nil {
		return slog.Default()
	}
	return h.opts.Log
}

func (h *Host) Provision(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.opts.Validate(); err != nil {
		return fmt.Errorf("nodemarket: node %s of %s: %w", h.opts.NodeName, h.opts.Cluster, err)
	}
	if err := os.MkdirAll(h.opts.DataDir, 0o700); err != nil {
		return fmt.Errorf("nodemarket: creating %s: %w", h.opts.DataDir, err)
	}
	passwordFile := NodePasswordPath(h.opts.DataDir)
	if _, err := MintOrReadNodePassword(passwordFile); err != nil {
		return fmt.Errorf("nodemarket: node %s of %s: %w -- the claim names this file as the path "+
			"the door reads the per-node password from, so a password that is not on disk is a "+
			"claim the door can only fail on", h.opts.NodeName, h.opts.Cluster, err)
	}
	target, err := h.opts.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("nodemarket: the workspace and owner DID node %s of %s is claimed under: %w",
			h.opts.NodeName, h.opts.Cluster, err)
	}
	if h.opts.Secrets != nil {
		if why := h.linksNotListening(ctx, target); why != "" {
			return fmt.Errorf("%w: node %s of %s: %s", ErrLinksNotListening, h.opts.NodeName,
				h.opts.Cluster, why)
		}
	}
	if err := h.opts.Record(ctx, h.opts.NodeName, v1alpha1.NodePending); err != nil {
		return fmt.Errorf("nodemarket: recording node %s of %s as Pending before its claim is "+
			"written: %w -- the phase is written first because the claim is about to exist, and a "+
			"gateway that died between the two would leave no record at all of a node it rented",
			h.opts.NodeName, h.opts.Cluster, err)
	}
	if err := h.writeSecretsLocked(ctx, target); err != nil {
		return fmt.Errorf("%w -- nothing was rented: the claim is written only once the node's "+
			"policy is in the cluster OpenBao", err)
	}
	claim := h.claimLocked(target, passwordFile)
	if err := h.opts.Claims.WriteNodeClaim(ctx, target.Workspace, claim); err != nil {
		return fmt.Errorf("nodemarket: writing the claim for node %s of %s into %s: %w -- the claim "+
			"is the whole request, so nothing was rented and the next pass may ask again",
			h.opts.NodeName, h.opts.Cluster, target.Workspace, err)
	}
	h.provisioned = true
	h.log().Info("nodemarket: node claimed from the market",
		"cluster", h.opts.Cluster, "node", h.opts.NodeName, "workspace", target.Workspace,
		"ownerDid", target.OwnerDid, "advertiseAddress", h.opts.AdvertiseAddress,
		"apiserverPort", h.opts.ApiserverPort, "phase", v1alpha1.NodePending,
		"next", "ConfirmJoined, once the door reports the claim Running")
	return nil
}

func (h *Host) claimLocked(target Cluster, passwordFile string) v1alpha1.NodeClaim {
	return v1alpha1.NodeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "NodeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{Name: h.opts.NodeName, OwnerReferences: clusterOwner(target)},
		Spec: v1alpha1.NodeClaimSpec{
			ClusterRef:       commonv1.LocalObjectRef{Name: h.opts.Cluster},
			NodeName:         h.opts.NodeName,
			OwnerDid:         target.OwnerDid,
			ApiserverPort:    h.opts.ApiserverPort,
			AdvertiseAddress: h.opts.AdvertiseAddress,
			TokenFile:        h.opts.TokenFile,
			NodePasswordFile: passwordFile,
			ServerCAFile:     h.opts.ServerCAFile,
			DenyBidderDids:   h.denied(),
		},
	}
}

func clusterOwner(target Cluster) []metav1.OwnerReference {
	if target.UID == "" || target.Name == "" {
		return nil
	}
	return []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Cluster", Name: target.Name, UID: target.UID}}
}

func (h *Host) denied() []string {
	out := append([]string(nil), h.opts.DenyBidderDids...)
	if h.opts.DenyMore != nil {
		for _, did := range h.opts.DenyMore() {
			if !slices.Contains(out, did) {
				out = append(out, did)
			}
		}
	}
	return out
}

func (h *Host) WriteAdoptionClaim(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.opts.Validate(); err != nil {
		return fmt.Errorf("nodemarket: node %s of %s: %w", h.opts.NodeName, h.opts.Cluster, err)
	}
	target, err := h.opts.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("nodemarket: the workspace and owner DID node %s of %s is claimed under: %w",
			h.opts.NodeName, h.opts.Cluster, err)
	}
	claim := h.claimLocked(target, NodePasswordPath(h.opts.DataDir))
	claim.Spec.AdoptJoinedNode = true
	if err := h.opts.Claims.WriteNodeClaim(ctx, target.Workspace, claim); err != nil {
		return fmt.Errorf("nodemarket: writing the adoption claim for node %s of %s into %s: %w",
			h.opts.NodeName, h.opts.Cluster, target.Workspace, err)
	}
	h.provisioned = true
	h.policyWritten = true
	h.secretWritten = true
	h.log().Info("nodemarket: a joined Node with no claim is claimed again for adoption",
		"cluster", h.opts.Cluster, "node", h.opts.NodeName, "workspace", target.Workspace,
		"next", "the door matches it to its receipt and moves the claim from Adopting to Running, or fails it")
	return nil
}

func (h *Host) ConfirmJoined(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	claim, err := h.read(ctx)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("nodemarket: node %s of %s is reported by its cluster and its claim is "+
			"gone: the door releases the claim only once the machine is deleted, so a cluster still "+
			"naming this node is a node nothing here can account for",
			h.opts.NodeName, h.opts.Cluster)
	}
	switch claim.Status.Phase {
	case v1alpha1.NodeClaimJoining, v1alpha1.NodeClaimRunning:
	case v1alpha1.NodeClaimAdopting:
		return ErrAwaitingAdoption
	case "", v1alpha1.NodeClaimPending:
		if claim.Spec.AdoptJoinedNode {
			return ErrAwaitingAdoption
		}
		return fmt.Errorf("nodemarket: node %s of %s is reported by its cluster while its claim is "+
			"%q, not Joining", h.opts.NodeName, h.opts.Cluster, claim.Status.Phase)
	case v1alpha1.NodeClaimFailed, v1alpha1.NodeClaimLost:
		return fmt.Errorf("nodemarket: node %s of %s: %s",
			h.opts.NodeName, h.opts.Cluster, endedMessage(claim))
	default:
		return fmt.Errorf("nodemarket: node %s of %s is reported by its cluster while its claim is "+
			"%q, not Joining. The door writes Joining only once the guest fetched this claim's "+
			"secrets, and only that guest holds the node password the agent registers with, so a "+
			"cluster reporting the node before that is a node something else registered under "+
			"this name", h.opts.NodeName, h.opts.Cluster, claim.Status.Phase)
	}
	if err := h.opts.Record(ctx, h.opts.NodeName, v1alpha1.NodeRunning); err != nil {
		return fmt.Errorf("nodemarket: recording node %s of %s as Running after the door reported "+
			"it: %w", h.opts.NodeName, h.opts.Cluster, err)
	}
	if claim.Status.Phase == v1alpha1.NodeClaimJoining {
		if m, ok := h.opts.Claims.(claimRunningMarker); ok {
			if target, err := h.opts.Resolve(ctx); err == nil {
				if err := m.MarkNodeClaimRunning(ctx, target.Workspace, h.opts.NodeName,
					"the node joined its cluster and the cluster reports it"); err != nil {
					return fmt.Errorf("nodemarket: marking the claim for node %s of %s Running: %w",
						h.opts.NodeName, h.opts.Cluster, err)
				}
			}
		}
	}
	return nil
}

type claimRunningMarker interface {
	MarkNodeClaimRunning(ctx context.Context, logicalCluster, name, message string) error
}

type End struct {
	Message string

	Declined bool

	Lost bool

	Bidder string
}

func MarketDeclined(claim *v1alpha1.NodeClaim) bool {
	if claim == nil || claim.Status.Phase != v1alpha1.NodeClaimFailed {
		return false
	}
	return claim.Status.Contract.RFPURI != "" && claim.Status.Contract.AcceptURI == ""
}

func (h *Host) End() End {
	ctx, cancel := context.WithTimeout(context.Background(), fatalReadTimeout)
	defer cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	claim, err := h.read(ctx)
	if err != nil {
		return End{}
	}
	if claim == nil {
		if !h.provisioned {
			return End{}
		}
		return End{Message: claimGoneMessage}
	}
	if !EndedClaimPhase(claim.Status.Phase) {
		return End{}
	}
	end := End{Message: endedMessage(claim), Declined: MarketDeclined(claim),
		Lost: claim.Status.Phase == v1alpha1.NodeClaimLost}
	if wn := claim.Status.Contract.Winner; wn != nil {
		end.Bidder = wn.DID
	}
	return end
}

func (h *Host) Terminal() string {
	return h.End().Message
}

func (h *Host) Teardown(ctx context.Context) error {
	return h.release(ctx)
}

func (h *Host) Reap(ctx context.Context) error {
	return h.release(ctx)
}

func (h *Host) release(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.opts.Validate(); err != nil {
		return fmt.Errorf("nodemarket: node %s of %s: %w", h.opts.NodeName, h.opts.Cluster, err)
	}
	target, err := h.opts.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("nodemarket: the workspace the claim for node %s of %s lives in: %w",
			h.opts.NodeName, h.opts.Cluster, err)
	}
	var problems []error
	if err := h.opts.Record(ctx, h.opts.NodeName, v1alpha1.NodeDraining); err != nil {
		problems = append(problems, fmt.Errorf("nodemarket: recording node %s of %s as Draining: %w",
			h.opts.NodeName, h.opts.Cluster, err))
	}
	h.log().Info("nodemarket: releasing the node", "cluster", h.opts.Cluster, "node", h.opts.NodeName)
	if err := h.opts.Claims.DeleteNodeClaim(ctx, target.Workspace, h.opts.NodeName); err != nil {
		problems = append(problems, fmt.Errorf("nodemarket: deleting the claim for node %s of %s: "+
			"%w -- the claim is the only handle on the rented machine, and a delete that did not "+
			"land leaves the door holding a session for a node nothing counts",
			h.opts.NodeName, h.opts.Cluster, err))
		return errors.Join(problems...)
	}
	h.revokeSecretsLocked(ctx)
	if err := h.awaitRelease(ctx, target.Workspace); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func (h *Host) awaitRelease(ctx context.Context, workspace string) error {
	wctx, cancel := context.WithTimeout(ctx, h.opts.ReleaseTimeout)
	defer cancel()
	err := h.opts.Claims.AwaitNodeClaimGone(wctx, workspace, h.opts.NodeName)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("nodemarket: waiting for the door to release node %s of %s: %w",
			h.opts.NodeName, h.opts.Cluster, ctx.Err())
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("nodemarket: the claim for node %s of %s is still there %s after it "+
			"was deleted. The door removes its finalizer only once the market VM is gone, so "+
			"this is a machine that has not been destroyed: closing is not destroying, and "+
			"reporting this as a completed teardown would leave the rent running",
			h.opts.NodeName, h.opts.Cluster, h.opts.ReleaseTimeout)
	}
	return fmt.Errorf("nodemarket: watching the claim for node %s of %s while waiting "+
		"for the door to release it: %w -- a watch that failed is not a claim that is gone, "+
		"and reporting a release nobody observed is how a machine that is still being paid "+
		"for reads as closed", h.opts.NodeName, h.opts.Cluster, err)
}

func (h *Host) read(ctx context.Context) (*v1alpha1.NodeClaim, error) {
	if err := h.opts.Validate(); err != nil {
		return nil, fmt.Errorf("nodemarket: node %s of %s: %w", h.opts.NodeName, h.opts.Cluster, err)
	}
	target, err := h.opts.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("nodemarket: the workspace the claim for node %s of %s lives in: %w",
			h.opts.NodeName, h.opts.Cluster, err)
	}
	claim, err := h.opts.Claims.ReadNodeClaim(ctx, target.Workspace, h.opts.NodeName)
	if err != nil {
		return nil, fmt.Errorf("nodemarket: reading the claim for node %s of %s from %s: %w",
			h.opts.NodeName, h.opts.Cluster, target.Workspace, err)
	}
	return claim, nil
}

func EndedClaimPhase(phase v1alpha1.NodeClaimPhase) bool {
	return phase == v1alpha1.NodeClaimFailed || phase == v1alpha1.NodeClaimReleased ||
		phase == v1alpha1.NodeClaimLost
}

func endedMessage(claim *v1alpha1.NodeClaim) string {
	if claim.Status.Message != "" {
		return claim.Status.Message
	}
	return "the door ended this claim and recorded no reason"
}
