package gatewayapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodemarket"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
)

const (
	NodeClaimAnnotation = "socialweb.computer/nodeclaim"

	ContractTTLAnnotation = "socialweb.computer/ttl-after-finished"

	ContractTTL = "8d"

	NodeClaimUIDLabel = "socialweb.computer/nodeclaim-uid"

	ContractNamePrefix = "nodeclaim-"

	MeshdDownloadPrefix = "socialweb-meshd-"

	NodeAgentDownloadPrefix = "socialweb-node-"

	NodeSecretsDir = "/etc/socialweb/node"

	OpenBaoTicketPath = NodeSecretsDir + "/openbao-ticket"

	OpenBaoCAPath = NodeSecretsDir + "/openbao-ca.pem"

	NodeClaimDocPath = NodeSecretsDir + "/claim.json"

	NodeLinkEnvPath = NodeSecretsDir + "/apiserver.env"

	OpenBaoCARelativeToClusterDir = "openbao/tls/ca.crt"

	DefaultNodeVMReadyTimeoutSec = 120

	DefaultNodePollInterval = 5 * time.Second
)

var (
	ErrQuotaExceeded = errors.New("gatewayapi: the quota of ComputeContracts is used up")

	MeshdArches = []string{"x86_64", "aarch64"}

	ipv4AddressPattern = regexp.MustCompile(`^([0-9]{1,3}\.){3}[0-9]{1,3}$`)

	portNumberPattern = regexp.MustCompile(`^[0-9]{1,5}$`)

	dumbpipeTicketPattern = regexp.MustCompile(`^[a-z0-9]+$`)
)

type SecretEntry struct {
	Path  string `json:"path"`
	Value string `json:"value"`
}

type NodeClaim struct {
	Workspace string

	Claim v1alpha1.NodeClaim
}

func (c NodeClaim) Name() string { return c.Claim.Name }

func (c NodeClaim) Key() string { return c.Workspace + "/" + c.Claim.Name }

func (c NodeClaim) UID() string { return string(c.Claim.UID) }

func (c NodeClaim) Finalizers() []string { return c.Claim.Finalizers }

func (c NodeClaim) AdoptJoinedNode() bool {
	return c.Claim.Annotations[nodemarket.AnnotationAdoptJoinedNode] == "true"
}

type NodeClaimStore interface {
	List(ctx context.Context) ([]NodeClaim, error)

	Get(ctx context.Context, claim NodeClaim) (*NodeClaim, error)

	ReplaceStatus(ctx context.Context, claim NodeClaim, status v1alpha1.NodeClaimStatus) error

	AddFinalizer(ctx context.Context, claim NodeClaim) error

	RemoveFinalizer(ctx context.Context, claim NodeClaim) error
}

type NodeClaimWatcher interface {
	Watch(ctx context.Context, onChange func())
}

type ClusterLinkView struct {
	Workspace string

	Name string

	Status v1alpha1.ClusterLinkStatus
}

type ClusterLinkReader interface {
	Get(ctx context.Context, workspace, name string) (*ClusterLinkView, error)
}

type ContractClient interface {
	Get(ctx context.Context, name string) (*v1alpha1.ComputeContract, error)

	List(ctx context.Context) ([]v1alpha1.ComputeContract, error)

	CreateIfAbsent(ctx context.Context, contract *v1alpha1.ComputeContract) (bool, error)

	Patch(ctx context.Context, name string, patch any) error

	Delete(ctx context.Context, name string) error
}

type ContractWatcher interface {
	Watch(ctx context.Context, onChange func())
}

type Vault interface {
	Write(ctx context.Context, ref v1alpha1.VaultRef, data map[string]any) error

	Remove(ctx context.Context, ref v1alpha1.VaultRef) error
}

type OpenBaoVault struct {
	Client *openbao.Client
}

func (v OpenBaoVault) Write(ctx context.Context, ref v1alpha1.VaultRef, data map[string]any) error {
	if v.Client == nil {
		return errors.New("gatewayapi: no OpenBao client")
	}
	return v.Client.PutKV(ctx, kvDataPath(ref), data)
}

func (v OpenBaoVault) Remove(ctx context.Context, ref v1alpha1.VaultRef) error {
	if v.Client == nil {
		return errors.New("gatewayapi: no OpenBao client")
	}
	return v.Client.Delete(ctx, strings.Trim(ref.Mount, "/")+"/metadata/"+strings.Trim(ref.Path, "/"))
}

func kvDataPath(ref v1alpha1.VaultRef) string {
	return strings.Trim(ref.Mount, "/") + "/data/" + strings.Trim(ref.Path, "/")
}

type NodePolicy struct {
	Name string

	Args map[string]any
}

type MeshdPin struct {
	DownloadBase string

	Sha256 map[string]string
}

type NodeClaimMapperOptions struct {
	Store NodeClaimStore

	Links ClusterLinkReader

	Contracts ContractClient

	Vault Vault

	VaultMount string

	VMReadyTimeoutSec int

	Policy *NodePolicy

	ExtraBidderDIDs []string

	UserDataBase string

	PollInterval time.Duration

	OpenBaoCAFileOf func(v1alpha1.NodeClaimSpec) string

	ReadFile func(string) (string, error)

	Meshd *MeshdPin

	MeshdDownloadBase func(context.Context) (string, bool)

	NodeAgentPin *MeshdPin

	NodeAgentDownloadBase func(context.Context) (string, bool)

	Log *slog.Logger
}

type NodeClaimMapper struct {
	opts NodeClaimMapperOptions

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	signal chan struct{}

	lastClaims int

	meshBaseWaiting bool
}

func NewNodeClaimMapper(opts NodeClaimMapperOptions) (*NodeClaimMapper, error) {
	if opts.Store == nil {
		return nil, errors.New("gatewayapi: node claim mapper has no claim store")
	}
	if opts.Links == nil {
		return nil, errors.New("gatewayapi: node claim mapper has no cluster link reader")
	}
	if opts.Contracts == nil {
		return nil, errors.New("gatewayapi: node claim mapper has no contract client")
	}
	if opts.Vault == nil {
		return nil, errors.New("gatewayapi: node claim mapper has no vault")
	}
	if opts.VaultMount == "" {
		return nil, errors.New("gatewayapi: node claim mapper has no vault mount")
	}
	if opts.VMReadyTimeoutSec <= 0 {
		opts.VMReadyTimeoutSec = DefaultNodeVMReadyTimeoutSec
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultNodePollInterval
	}
	if opts.OpenBaoCAFileOf == nil {
		opts.OpenBaoCAFileOf = DefaultOpenBaoCAFile
	}
	if opts.ReadFile == nil {
		opts.ReadFile = func(p string) (string, error) {
			raw, err := os.ReadFile(p)
			return string(raw), err
		}
	}
	if opts.Meshd != nil {
		if opts.Meshd.DownloadBase == "" && opts.MeshdDownloadBase == nil {
			return nil, errors.New("gatewayapi: the meshd pin has no download base")
		}
		for _, arch := range MeshdArches {
			if opts.Meshd.Sha256[arch] == "" {
				return nil, fmt.Errorf("gatewayapi: the meshd pin names no %s build", arch)
			}
		}
	}
	if opts.NodeAgentPin != nil {
		if opts.NodeAgentPin.DownloadBase == "" && opts.NodeAgentDownloadBase == nil {
			return nil, errors.New("gatewayapi: the node agent pin has no download base")
		}
		for _, arch := range MeshdArches {
			if opts.NodeAgentPin.Sha256[arch] == "" {
				return nil, fmt.Errorf("gatewayapi: the node agent pin names no %s build", arch)
			}
		}
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.NodeAgentPin == nil {
		opts.Log.Info("node_agent_not_offered", "reason", "the downloads carry no sha256-pinned socialweb-node build")
	}
	return &NodeClaimMapper{opts: opts, signal: make(chan struct{}, 1), lastClaims: -1}, nil
}

func (m *NodeClaimMapper) wake() {
	select {
	case m.signal <- struct{}{}:
	default:
	}
}

func (m *NodeClaimMapper) Start(ctx context.Context) {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	done := make(chan struct{})
	m.done = done
	m.mu.Unlock()
	if w, ok := m.opts.Store.(NodeClaimWatcher); ok {
		go w.Watch(cctx, m.wake)
	}
	if w, ok := m.opts.Contracts.(ContractWatcher); ok {
		go w.Watch(cctx, m.wake)
	}
	go m.loop(cctx, done)
}

func (m *NodeClaimMapper) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(m.opts.PollInterval)
	defer ticker.Stop()
	if err := m.Pass(ctx); err != nil {
		m.opts.Log.Error("node_claim_list_failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.signal:
		}
		if err := m.Pass(ctx); err != nil {
			m.opts.Log.Error("node_claim_list_failed", "err", err)
		}
	}
}

func (m *NodeClaimMapper) Stop(ctx context.Context) error {
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.cancel = nil
	m.done = nil
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *NodeClaimMapper) Pass(ctx context.Context) error {
	claims, err := m.opts.Store.List(ctx)
	if err != nil {
		return err
	}
	contracts, err := m.opts.Contracts.List(ctx)
	if err != nil {
		return err
	}
	if len(claims) != m.lastClaims {
		m.lastClaims = len(claims)
		m.opts.Log.Info("node_claim_pass", "claims", len(claims), "contracts", len(contracts))
	}
	byName := make(map[string]*v1alpha1.ComputeContract, len(contracts))
	for i := range contracts {
		if contracts[i].Name != "" {
			byName[contracts[i].Name] = &contracts[i]
		}
	}
	for i := range claims {
		claim := claims[i]
		key := claim.Key()
		if err := m.passClaim(ctx, claim, byName); err != nil {
			m.opts.Log.Error("node_claim_pass_failed", "claim", key, "err", err)
		}
	}
	live := make(map[string]NodeClaim, len(claims))
	for _, claim := range claims {
		live[claim.Key()] = claim
	}
	for name, contract := range byName {
		owner := contract.Annotations[NodeClaimAnnotation]
		if owner == "" {
			continue
		}
		claim, found := live[owner]
		orphan := false
		if !found {
			workspace, claimName := splitClaimKey(owner)
			if claimName == "" {
				continue
			}
			fetched, err := m.opts.Store.Get(ctx, NodeClaim{Workspace: workspace, Claim: v1alpha1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: claimName},
			}})
			if err != nil {
				m.opts.Log.Warn("node_claim_orphan_lookup_failed", "claim", owner, "contract", name, "err", err)
				continue
			}
			if fetched == nil {
				orphan = true
			} else {
				claim = *fetched
				found = true
			}
		}
		if found && !orphan {
			if uid := contract.Labels[NodeClaimUIDLabel]; uid != "" {
				orphan = uid != claim.UID()
			} else {
				orphan = ContractNameOfClaim(claim) != name
			}
		}
		if !orphan {
			continue
		}
		switch contract.Status.Phase {
		case v1alpha1.ComputeContractReleased, v1alpha1.ComputeContractFailed:
			if err := m.opts.Contracts.Delete(ctx, name); err != nil {
				m.opts.Log.Error("node_claim_contract_delete_failed", "claim", owner, "err", err)
				continue
			}
			if err := m.opts.Vault.Remove(ctx, bundleRef(m.opts.VaultMount, name)); err != nil {
				m.opts.Log.Error("node_claim_bundle_remove_failed", "claim", owner, "err", err)
			}
			m.opts.Log.Info("node_claim_orphan_contract_removed", "claim", owner, "contract", name,
				"phase", contract.Status.Phase)
		case v1alpha1.ComputeContractReleasing:
		default:
			if contract.Spec.Release {
				continue
			}
			if err := m.opts.Contracts.Patch(ctx, name, map[string]any{"spec": map[string]any{"release": true}}); err != nil {
				m.opts.Log.Error("node_claim_orphan_release_patch_failed", "claim", owner, "contract", name, "err", err)
				continue
			}
			m.opts.Log.Info("node_claim_orphan_release_requested", "claim", owner, "contract", name,
				"phase", contract.Status.Phase)
		}
	}
	return nil
}

func (m *NodeClaimMapper) passClaim(ctx context.Context, claim NodeClaim, byName map[string]*v1alpha1.ComputeContract) error {
	if ctx.Err() != nil {
		return nil
	}
	if claim.Claim.DeletionTimestamp != nil {
		return m.settleDeleted(ctx, claim)
	}
	name := ContractNameOfClaim(claim)
	contract := byName[name]
	if contract == nil {
		phase := claim.Claim.Status.Phase
		if phase == v1alpha1.NodeClaimAdopting ||
			((phase == "" || phase == v1alpha1.NodeClaimPending) && claim.AdoptJoinedNode()) {
			return m.ensureContract(ctx, claim, true)
		}
		if phase == "" || phase == v1alpha1.NodeClaimPending ||
			phase == v1alpha1.NodeClaimBidding || phase == v1alpha1.NodeClaimProvisioning {
			return m.ensureContract(ctx, claim, false)
		}
		return nil
	}
	next := desiredStatus(claim, contract)
	if next == nil {
		return nil
	}
	m.replaceStatus(ctx, &claim, *next)
	m.opts.Log.Info("node_claim_mapped", "claim", claim.Key(), "contract", name, "phase", next.Phase)
	return nil
}

func (m *NodeClaimMapper) ensureContract(ctx context.Context, claim NodeClaim, adopt bool) error {
	key := claim.Key()
	name := ContractNameOfClaim(claim)
	if !hasFinalizer(claim.Finalizers(), v1alpha1.FinalizerNodeClaim) {
		if err := m.opts.Store.AddFinalizer(ctx, claim); err != nil {
			return err
		}
	}
	if existing, err := m.opts.Contracts.Get(ctx, name); err != nil {
		return err
	} else if existing != nil {
		return nil
	}
	meshBase := ""
	if !adopt && m.opts.Meshd != nil {
		meshBase = m.opts.Meshd.DownloadBase
		if m.opts.MeshdDownloadBase != nil {
			resolved, ok := m.opts.MeshdDownloadBase(ctx)
			if !ok {
				if !m.meshBaseWaiting {
					m.meshBaseWaiting = true
					m.opts.Log.Info("node_claim_download_base_waiting", "claim", key, "contract", name)
				}
				return nil
			}
			if m.meshBaseWaiting {
				m.meshBaseWaiting = false
				m.opts.Log.Info("node_claim_download_base_resolved", "claim", key, "contract", name, "base", resolved)
			}
			meshBase = resolved
		}
	}
	agentBase := ""
	if !adopt && m.opts.NodeAgentPin != nil {
		agentBase = m.opts.NodeAgentPin.DownloadBase
		if m.opts.NodeAgentDownloadBase != nil {
			resolved, ok := m.opts.NodeAgentDownloadBase(ctx)
			if !ok {
				if !m.meshBaseWaiting {
					m.meshBaseWaiting = true
					m.opts.Log.Info("node_claim_download_base_waiting", "claim", key, "contract", name)
				}
				return nil
			}
			if m.meshBaseWaiting {
				m.meshBaseWaiting = false
				m.opts.Log.Info("node_claim_download_base_resolved", "claim", key, "contract", name, "base", resolved)
			}
			agentBase = resolved
		}
	}
	if !adopt {
		bundle, err := m.buildBundle(ctx, claim)
		if err != nil {
			m.opts.Log.Error("node_claim_contract_failed", "claim", key, "err", err)
			m.replaceStatus(ctx, &claim, failedStatus(claim.Claim.Status, "the contract failed: "+err.Error()))
			return nil
		}
		raw, err := json.Marshal(bundle)
		if err != nil {
			return err
		}
		if err := m.opts.Vault.Write(ctx, bundleRef(m.opts.VaultMount, name), map[string]any{"bundle": string(raw)}); err != nil {
			m.opts.Log.Error("node_claim_contract_failed", "claim", key, "err", err)
			m.replaceStatus(ctx, &claim, failedStatus(claim.Claim.Status, "the contract failed: "+err.Error()))
			return nil
		}
	}
	contract := &v1alpha1.ComputeContract{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "ComputeContract",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{},
			Annotations: map[string]string{NodeClaimAnnotation: key, ContractTTLAnnotation: ContractTTL},
		},
		Spec: m.specOf(claim, name, adopt, meshBase, agentBase),
	}
	if claim.UID() != "" {
		contract.Labels[NodeClaimUIDLabel] = claim.UID()
	}
	created, err := m.opts.Contracts.CreateIfAbsent(ctx, contract)
	if err != nil {
		if errors.Is(err, ErrQuotaExceeded) {
			message := "AtCapacity: the quota of ComputeContracts of this gateway is used up; the claim waits"
			m.opts.Log.Warn("node_claim_contract_at_capacity", "claim", key, "err", err)
			if claim.Claim.Status.Message != message {
				next := *claimStatusCopy(claim.Claim.Status)
				next.Phase = v1alpha1.NodeClaimPending
				next.Message = message
				m.replaceStatus(ctx, &claim, next)
			}
			return nil
		}
		m.opts.Log.Error("node_claim_contract_failed", "claim", key, "err", err)
		m.replaceStatus(ctx, &claim, failedStatus(claim.Claim.Status, "the contract failed: "+err.Error()))
		return nil
	}
	m.opts.Log.Info("node_claim_contract_created", "claim", key, "contract", name, "adopt", adopt, "created", created)
	next := *claimStatusCopy(claim.Claim.Status)
	if adopt {
		next.Phase = v1alpha1.NodeClaimAdopting
		next.Message = "a joined Node with no claim: matching it to its receipt and vm.hostKeys record"
	} else {
		next.Phase = v1alpha1.NodeClaimBidding
		next.Message = "requesting a node from the compute market"
	}
	m.replaceStatus(ctx, &claim, next)
	return nil
}

func failedStatus(status v1alpha1.NodeClaimStatus, message string) v1alpha1.NodeClaimStatus {
	next := *claimStatusCopy(status)
	next.Phase = v1alpha1.NodeClaimFailed
	next.Message = message
	return next
}

func claimStatusCopy(status v1alpha1.NodeClaimStatus) *v1alpha1.NodeClaimStatus {
	out := &v1alpha1.NodeClaimStatus{}
	status.DeepCopyInto(out)
	return out
}

func (m *NodeClaimMapper) buildBundle(ctx context.Context, claim NodeClaim) ([]SecretEntry, error) {
	spec := claim.Claim.Spec
	linkName := nodemarket.OpenBaoLinkName(spec.Cluster)
	link, err := m.opts.Links.Get(ctx, claim.Workspace, linkName)
	if err != nil {
		return nil, err
	}
	if link == nil || link.Status.Ticket == "" {
		message := fmt.Sprintf("the cluster's OpenBao link %s carries no ticket yet", linkName)
		if link != nil && link.Status.Message != "" {
			message += " (" + link.Status.Message + ")"
		}
		return nil, errors.New(message)
	}
	openBaoCA, err := m.opts.ReadFile(m.opts.OpenBaoCAFileOf(spec))
	if err != nil {
		return nil, err
	}
	bundle := []SecretEntry{
		{Path: OpenBaoTicketPath, Value: link.Status.Ticket},
		{Path: OpenBaoCAPath, Value: openBaoCA},
	}
	apiserverName := nodemarket.ApiserverLinkName(spec.Cluster)
	apiserverLink, apiserverErr := m.opts.Links.Get(ctx, claim.Workspace, apiserverName)
	linkEnv := ""
	if apiserverErr == nil && apiserverLink != nil &&
		apiserverLink.Status.Phase == v1alpha1.ClusterLinkListening &&
		apiserverLink.Status.Ticket != "" {
		linkEnv = nodeLinkEnv(spec.NodeName, spec.AdvertiseAddress, spec.ApiserverPort, apiserverLink.Status.Ticket)
	}
	if linkEnv != "" {
		bundle = append(bundle, SecretEntry{Path: NodeLinkEnvPath, Value: linkEnv})
		m.opts.Log.Info("node_claim_apiserver_link_delivered", "claim", claim.Key(), "link", apiserverName)
	} else {
		phase := ""
		ticketBytes := 0
		if apiserverLink != nil {
			phase = string(apiserverLink.Status.Phase)
			ticketBytes = len(apiserverLink.Status.Ticket)
		}
		var why any
		if apiserverErr != nil {
			why = apiserverErr
		}
		m.opts.Log.Info("node_claim_apiserver_link_withheld", "claim", claim.Key(), "link", apiserverName,
			"phase", phase, "ticketBytes", ticketBytes, "error", why)
	}
	return bundle, nil
}

func (m *NodeClaimMapper) specOf(claim NodeClaim, name string, adopt bool, meshBase, agentBase string) v1alpha1.ComputeContractSpec {
	spec := claim.Claim.Spec
	out := v1alpha1.ComputeContractSpec{
		VMName:            spec.NodeName,
		OwnerDid:          spec.OwnerDid,
		VMReadyTimeoutSec: m.opts.VMReadyTimeoutSec,
	}
	if len(spec.DenyBidderDids) > 0 {
		out.DenyBidderDids = spec.DenyBidderDids
	}
	if len(m.opts.ExtraBidderDIDs) > 0 {
		out.BidderDids = m.opts.ExtraBidderDIDs
	}
	if m.opts.UserDataBase != "" {
		out.UserData = m.opts.UserDataBase
	}
	if m.opts.Policy != nil {
		out.Policy = m.opts.Policy.Name
		args := m.opts.Policy.Args
		if args == nil {
			args = map[string]any{}
		}
		raw, err := json.Marshal(args)
		if err == nil {
			out.PolicyArgs = string(raw)
		}
	}
	if adopt {
		out.Adopt = true
		return out
	}
	out.SecretsRef = &v1alpha1.VaultRef{Mount: m.opts.VaultMount, Path: "contracts/" + name + "/bundle"}
	join := &v1alpha1.ComputeContractNodeJoin{Cluster: spec.Cluster}
	if m.opts.Meshd != nil {
		mesh := &v1alpha1.ComputeContractMeshd{DownloadBase: meshBase}
		for _, arch := range MeshdArches {
			mesh.Pins = append(mesh.Pins, v1alpha1.ComputeContractMeshdPin{Arch: arch, Sha256: m.opts.Meshd.Sha256[arch]})
		}
		join.Meshd = mesh
	}
	if m.opts.NodeAgentPin != nil {
		agent := &v1alpha1.ComputeContractNodeAgent{DownloadBase: agentBase}
		for _, arch := range MeshdArches {
			agent.Pins = append(agent.Pins, v1alpha1.ComputeContractMeshdPin{Arch: arch, Sha256: m.opts.NodeAgentPin.Sha256[arch]})
		}
		join.NodeAgent = agent
	}
	out.NodeJoin = join
	return out
}

func desiredStatus(claim NodeClaim, contract *v1alpha1.ComputeContract) *v1alpha1.NodeClaimStatus {
	phase := claim.Claim.Status.Phase
	if phase == v1alpha1.NodeClaimFailed || phase == v1alpha1.NodeClaimReleased || phase == v1alpha1.NodeClaimLost {
		return nil
	}
	status := contract.Status
	if status.Phase == "" {
		return nil
	}
	state := status.Contract
	adopt := contract.Spec.Adopt
	next := claimStatusCopy(claim.Claim.Status)
	if state != nil {
		var copyOf v1alpha1.NodeClaimContract
		state.DeepCopyInto(&copyOf)
		next.Contract = copyOf
		if state.VMAddress != "" {
			next.Fqdn = state.VMAddress
		}
	}
	if status.Grant != nil {
		grant := *status.Grant
		next.Grant = &grant
	}
	inFlight := phase == "" || phase == v1alpha1.NodeClaimPending ||
		phase == v1alpha1.NodeClaimBidding || phase == v1alpha1.NodeClaimProvisioning ||
		phase == v1alpha1.NodeClaimAdopting
	switch status.Phase {
	case v1alpha1.ComputeContractPending:
	case v1alpha1.ComputeContractRequesting:
		if inFlight && !adopt {
			accepted := state != nil && state.Accept != nil
			if accepted {
				next.Phase = v1alpha1.NodeClaimProvisioning
				next.Message = "a bid was accepted; the VM is booting"
			} else {
				next.Phase = v1alpha1.NodeClaimBidding
				next.Message = "requesting a node from the compute market"
			}
		}
	case v1alpha1.ComputeContractHeld:
		switch {
		case adopt && (phase == v1alpha1.NodeClaimAdopting || inFlight):
			next.Phase = v1alpha1.NodeClaimRunning
			next.Message = status.Message
			if next.Message == "" {
				next.Message = "adopted"
			}
		case status.SecretsFetchedAt != "" && inFlight:
			next.Phase = v1alpha1.NodeClaimJoining
			next.Message = "the node fetched its join secrets and is joining its cluster"
		case status.SecretsFetchedAt == "" && inFlight:
			next.Phase = v1alpha1.NodeClaimProvisioning
			next.Message = "a bid was accepted; the VM is booting"
		}
	case v1alpha1.ComputeContractReleasing:
	case v1alpha1.ComputeContractReleased:
		if state != nil && state.Termination != nil {
			next.Phase = v1alpha1.NodeClaimLost
			next.Message = lostMessage(state)
		} else {
			next.Phase = v1alpha1.NodeClaimReleased
			next.Message = status.Message
			if next.Message == "" {
				next.Message = "the contract was released"
			}
		}
	case v1alpha1.ComputeContractFailed:
		next.Phase = v1alpha1.NodeClaimFailed
		next.Message = status.Message
		if next.Message == "" {
			next.Message = "the contract failed"
		}
	}
	if reflect.DeepEqual(claim.Claim.Status, *next) {
		return nil
	}
	return next
}

func lostMessage(state *v1alpha1.NodeClaimContract) string {
	t := state.Termination
	if t == nil {
		return "the node's guest is gone"
	}
	exit := ""
	if t.ExitCode != nil {
		exit = fmt.Sprintf(", exit code %d", *t.ExitCode)
	}
	return fmt.Sprintf("the bidder reported the guest terminated (%s%s) at %s; the node is "+
		"released without a vm.delete and replaced through a new claim and RFP", t.Reason, exit, t.ObservedAt)
}

func (m *NodeClaimMapper) settleDeleted(ctx context.Context, claim NodeClaim) error {
	key := claim.Key()
	if !hasFinalizer(claim.Finalizers(), v1alpha1.FinalizerNodeClaim) {
		return nil
	}
	name := ContractNameOfClaim(claim)
	contract, err := m.opts.Contracts.Get(ctx, name)
	if err != nil {
		m.opts.Log.Warn("node_claim_release_deferred", "claim", key, "err", err)
		return nil
	}
	if contract != nil && !contract.Spec.Release {
		if err := m.opts.Contracts.Patch(ctx, name, map[string]any{"spec": map[string]any{"release": true}}); err != nil {
			m.opts.Log.Warn("node_claim_release_patch_failed", "claim", key, "err", err)
		}
		m.opts.Log.Info("node_claim_release_requested", "claim", key, "contract", name)
		return nil
	}
	finished := contract == nil ||
		contract.Status.Phase == v1alpha1.ComputeContractReleased ||
		contract.Status.Phase == v1alpha1.ComputeContractFailed
	abandoned := claim.Claim.Status.Phase == v1alpha1.NodeClaimFailed ||
		claim.Claim.Status.Phase == v1alpha1.NodeClaimLost
	if !finished && abandoned && contract != nil && contract.Status.Phase == v1alpha1.ComputeContractReleasing {
		next := *claimStatusCopy(claim.Claim.Status)
		next.Phase = v1alpha1.NodeClaimFailed
		next.Message = "the claim was deleted; the release of its VM is still retrying on ComputeContract " + name
		m.replaceStatus(ctx, &claim, next)
		if err := m.opts.Store.RemoveFinalizer(ctx, claim); err != nil {
			m.opts.Log.Warn("node_claim_finalizer_release_failed", "claim", key, "err", err)
		}
		m.opts.Log.Info("node_claim_released_release_retrying", "claim", key, "contract", name)
		return nil
	}
	if !finished {
		return nil
	}
	var state *v1alpha1.NodeClaimContract
	if contract != nil {
		state = contract.Status.Contract
	}
	confirmed := contract == nil ||
		contract.Status.Phase == v1alpha1.ComputeContractReleased ||
		state == nil || state.Receipt == nil
	next := *claimStatusCopy(claim.Claim.Status)
	if confirmed {
		next.Phase = v1alpha1.NodeClaimReleased
		next.Message = "the claim was deleted and the node's VM released"
	} else {
		next.Phase = v1alpha1.NodeClaimFailed
		next.Message = "the claim was deleted but the node's VM could not be confirmed released"
	}
	if state != nil {
		var copyOf v1alpha1.NodeClaimContract
		state.DeepCopyInto(&copyOf)
		next.Contract = copyOf
	}
	m.replaceStatus(ctx, &claim, next)
	if contract != nil {
		if err := m.opts.Contracts.Delete(ctx, name); err != nil {
			m.opts.Log.Warn("node_claim_contract_delete_failed", "claim", key, "err", err)
		}
	}
	if err := m.opts.Vault.Remove(ctx, bundleRef(m.opts.VaultMount, name)); err != nil {
		m.opts.Log.Warn("node_claim_bundle_remove_failed", "claim", key, "err", err)
	}
	if err := m.opts.Store.RemoveFinalizer(ctx, claim); err != nil {
		m.opts.Log.Warn("node_claim_finalizer_release_failed", "claim", key, "err", err)
	}
	m.opts.Log.Info("node_claim_released", "claim", key, "contract", name, "confirmed", confirmed)
	return nil
}

func (m *NodeClaimMapper) replaceStatus(ctx context.Context, claim *NodeClaim, status v1alpha1.NodeClaimStatus) {
	if reflect.DeepEqual(claim.Claim.Status, status) {
		return
	}
	if err := m.opts.Store.ReplaceStatus(ctx, *claim, status); err != nil {
		m.opts.Log.Error("node_claim_status_write_failed", "claim", claim.Key(), "err", err)
		return
	}
	claim.Claim.Status = status
}

func splitClaimKey(key string) (string, string) {
	index := strings.LastIndex(key, "/")
	if index < 0 || index == len(key)-1 {
		return "", ""
	}
	return key[:index], key[index+1:]
}

func ContractNameOfClaim(claim NodeClaim) string {
	basis := claim.UID()
	if basis == "" {
		basis = claim.Key()
	}
	sum := sha256.Sum256([]byte(basis))
	return ContractNamePrefix + hex.EncodeToString(sum[:])[:20]
}

func bundleRef(mount, contractName string) v1alpha1.VaultRef {
	return v1alpha1.VaultRef{Mount: mount, Path: "contracts/" + contractName + "/bundle"}
}

func hasFinalizer(finalizers []string, wanted string) bool {
	for _, f := range finalizers {
		if f == wanted {
			return true
		}
	}
	return false
}

func MeshdPinsOf(downloads []PinnedDownload) map[string]string {
	sha := map[string]string{}
	for _, arch := range MeshdArches {
		found := false
		for _, d := range downloads {
			if d.Name == MeshdDownloadPrefix+arch {
				sha[arch] = d.Sha256
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return sha
}

func MeshdPinOf(downloads []PinnedDownload, downloadBase string) *MeshdPin {
	if downloadBase == "" {
		return nil
	}
	sha := MeshdPinsOf(downloads)
	if sha == nil {
		return nil
	}
	return &MeshdPin{DownloadBase: downloadBase, Sha256: sha}
}

func NodeAgentPinsOf(downloads []PinnedDownload) map[string]string {
	sha := map[string]string{}
	for _, arch := range MeshdArches {
		found := false
		for _, d := range downloads {
			if d.Name == NodeAgentDownloadPrefix+arch {
				sha[arch] = d.Sha256
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return sha
}

func NodeAgentPinOf(downloads []PinnedDownload, downloadBase string) *MeshdPin {
	if downloadBase == "" {
		return nil
	}
	sha := NodeAgentPinsOf(downloads)
	if sha == nil {
		return nil
	}
	return &MeshdPin{DownloadBase: downloadBase, Sha256: sha}
}

func DefaultOpenBaoCAFile(spec v1alpha1.NodeClaimSpec) string {
	return path.Join(path.Dir(spec.TokenFile), OpenBaoCARelativeToClusterDir)
}

func PublishedOpenBaoCAFile(caDir string) func(v1alpha1.NodeClaimSpec) string {
	caDir = strings.TrimRight(caDir, "/")
	return func(spec v1alpha1.NodeClaimSpec) string {
		dir := path.Dir(spec.TokenFile)
		return caDir + "/" + path.Base(dir) + ".crt"
	}
}

func nodeLinkEnv(node, advertiseAddress string, apiserverPort int, ticket string) string {
	if !dumbpipeTicketPattern.MatchString(ticket) {
		return ""
	}
	if !ipv4AddressPattern.MatchString(advertiseAddress) {
		return ""
	}
	if !portNumberPattern.MatchString(fmt.Sprintf("%d", apiserverPort)) {
		return ""
	}
	return fmt.Sprintf("NODE_NAME=%s\nADVERTISE_ADDRESS=%s\nAPISERVER_PORT=%d\nAPISERVER_TICKET=%s\n",
		node, advertiseAddress, apiserverPort, ticket)
}
