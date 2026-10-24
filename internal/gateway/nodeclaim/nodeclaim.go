package nodeclaim

import (
	"cmp"
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

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/nodeclaim"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodemarket"
)

const (
	InfraNamePrefix = "nodes-"

	EntryNamePrefix = "node-"

	NodeSecretsDir = "/etc/socialweb/node"

	OpenBaoTicketPath = NodeSecretsDir + "/openbao-ticket"

	OpenBaoCAPath = NodeSecretsDir + "/openbao-ca.pem"

	NodeLinkEnvPath = NodeSecretsDir + "/apiserver.env"

	OpenBaoCARelativeToClusterDir = "openbao/tls/ca.crt"

	DefaultVMReadyTimeoutSec = 120

	bundleKey = "bundle"
)

var (
	Arches = []string{"x86_64", "aarch64"}

	ipv4AddressPattern = regexp.MustCompile(`^([0-9]{1,3}\.){3}[0-9]{1,3}$`)

	portNumberPattern = regexp.MustCompile(`^[0-9]{1,5}$`)

	dumbpipeTicketPattern = regexp.MustCompile(`^[a-z0-9]+$`)
)

var _ contract.Mapper = (*Mapper)(nil)

type SecretEntry struct {
	Path  string `json:"path"`
	Value string `json:"value"`
}

type Policy struct {
	Name string

	Args map[string]any
}

type Pin struct {
	DownloadBase string

	Sha256 map[string]string
}

type Config struct {
	VaultMount string

	VMReadyTimeoutSec int

	Policy *Policy

	ExtraBidderDIDs []string

	UserDataBase string

	Meshd *Pin

	NodeAgent *Pin
}

type Deps struct {
	ReadFile func(string) (string, error)

	CAFileOf func(v1alpha1.NodeClaimSpec) string

	MeshdBase func(context.Context) (string, bool)

	NodeAgentBase func(context.Context) (string, bool)

	Log *slog.Logger
}

type Mapper struct {
	cfg Config

	deps Deps

	mu sync.Mutex

	baseWaiting bool
}

func New(cfg Config, deps Deps) (*Mapper, error) {
	cfg.VaultMount = strings.Trim(cfg.VaultMount, "/")
	if cfg.VaultMount == "" {
		return nil, errors.New("nodeclaim: the node market needs a vault mount")
	}
	cfg.VMReadyTimeoutSec = opt.Positive(cfg.VMReadyTimeoutSec, DefaultVMReadyTimeoutSec)
	if err := checkPin("meshd", cfg.Meshd, deps.MeshdBase != nil); err != nil {
		return nil, err
	}
	if err := checkPin("node agent", cfg.NodeAgent, deps.NodeAgentBase != nil); err != nil {
		return nil, err
	}
	if deps.CAFileOf == nil {
		deps.CAFileOf = DefaultOpenBaoCAFile
	}
	if deps.ReadFile == nil {
		deps.ReadFile = func(p string) (string, error) {
			raw, err := os.ReadFile(p)
			return string(raw), err
		}
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.NodeAgent == nil {
		deps.Log.Info("node_agent_not_offered", "reason", "the downloads carry no sha256-pinned socialweb-node build")
	}
	return &Mapper{cfg: cfg, deps: deps}, nil
}

func checkPin(what string, pin *Pin, resolved bool) error {
	if pin == nil {
		return nil
	}
	if pin.DownloadBase == "" && !resolved {
		return fmt.Errorf("nodeclaim: the %s pin has no download base", what)
	}
	for _, arch := range Arches {
		if pin.Sha256[arch] == "" {
			return fmt.Errorf("nodeclaim: the %s pin names no %s build", what, arch)
		}
	}
	return nil
}

func (m *Mapper) InfraName(claim contract.Claim) string {
	return InfraNamePrefix + digest(claim.Workspace+"/"+claim.Object.Spec.ClusterRef.Name, 20)
}

func (m *Mapper) EntryName(claim contract.Claim) string {
	return EntryNamePrefix + digest(cmp.Or(string(claim.Object.UID), claim.Key()), 12)
}

func digest(basis string, n int) string {
	sum := sha256.Sum256([]byte(basis))
	return hex.EncodeToString(sum[:])[:n]
}

func (m *Mapper) BundleRef(name string) commonv1.SecretRef {
	return commonv1.SecretRef{Mount: m.cfg.VaultMount, Path: "contracts/" + name + "/bundle"}
}

func (m *Mapper) Wants(claim contract.Claim) (bool, bool) {
	phase := claim.Object.Status.Phase
	if phase == v1alpha1.NodeClaimAdopting || ((phase == "" || phase == v1alpha1.NodeClaimPending) && claim.Object.Spec.AdoptJoinedNode) {
		return true, true
	}
	if phase == "" || phase == v1alpha1.NodeClaimPending || phase == v1alpha1.NodeClaimBidding || phase == v1alpha1.NodeClaimProvisioning {
		return true, false
	}
	return false, false
}

func (m *Mapper) State(claim contract.Claim) v1alpha1.InfraContractState {
	phase := claim.Object.Status.Phase
	switch {
	case !claim.Object.DeletionTimestamp.IsZero(), phase == v1alpha1.NodeClaimFailed, phase == v1alpha1.NodeClaimLost,
		phase == v1alpha1.NodeClaimReleased:
		return v1alpha1.InfraContractReleased
	case phase == "", phase == v1alpha1.NodeClaimPending:
		return v1alpha1.InfraContractStaged
	}
	return v1alpha1.InfraContractRequested
}

func (m *Mapper) Bases(ctx context.Context, adopt bool) (contract.Bases, bool) {
	if adopt {
		return contract.Bases{}, true
	}
	mesh, ok := m.base(ctx, m.cfg.Meshd, m.deps.MeshdBase)
	if !ok {
		return contract.Bases{}, false
	}
	agent, ok := m.base(ctx, m.cfg.NodeAgent, m.deps.NodeAgentBase)
	if !ok {
		return contract.Bases{}, false
	}
	return contract.Bases{Meshd: mesh, NodeAgent: agent}, true
}

func (m *Mapper) base(ctx context.Context, pin *Pin, resolve func(context.Context) (string, bool)) (string, bool) {
	if pin == nil {
		return "", true
	}
	if resolve == nil {
		return pin.DownloadBase, true
	}
	resolved, ok := resolve(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ok {
		if !m.baseWaiting {
			m.baseWaiting = true
			m.deps.Log.Info("node_claim_download_base_waiting")
		}
		return "", false
	}
	if m.baseWaiting {
		m.baseWaiting = false
		m.deps.Log.Info("node_claim_download_base_resolved", "base", resolved)
	}
	return resolved, true
}

func (m *Mapper) LinkNames(claim contract.Claim) (string, string) {
	cluster := claim.Object.Spec.ClusterRef.Name
	return nodemarket.OpenBaoLinkName(cluster), nodemarket.ApiserverLinkName(cluster)
}

func (m *Mapper) Bundle(claim contract.Claim, links contract.Links) (map[string]any, error) {
	spec := claim.Object.Spec
	link := links.OpenBao
	if link.Err != nil {
		return nil, link.Err
	}
	if link.Status == nil || link.Status.Ticket == "" {
		message := fmt.Sprintf("the cluster's OpenBao link %s carries no ticket yet", link.Name)
		if link.Status != nil && link.Status.Message != "" {
			message += " (" + link.Status.Message + ")"
		}
		return nil, errors.New(message)
	}
	openBaoCA, err := m.deps.ReadFile(m.deps.CAFileOf(spec))
	if err != nil {
		return nil, err
	}
	bundle := []SecretEntry{
		{Path: OpenBaoTicketPath, Value: link.Status.Ticket},
		{Path: OpenBaoCAPath, Value: openBaoCA},
	}
	api := links.Apiserver
	linkEnv := ""
	if api.Err == nil && api.Status != nil && api.Status.Phase == v1alpha1.ClusterLinkListening && api.Status.Ticket != "" {
		linkEnv = nodeLinkEnv(spec.NodeName, spec.AdvertiseAddress, spec.ApiserverPort, api.Status.Ticket)
	}
	if linkEnv != "" {
		bundle = append(bundle, SecretEntry{Path: NodeLinkEnvPath, Value: linkEnv})
		m.deps.Log.Info("node_claim_apiserver_link_delivered", "claim", claim.Key(), "link", api.Name)
	} else {
		phase := ""
		ticketBytes := 0
		if api.Status != nil {
			phase = string(api.Status.Phase)
			ticketBytes = len(api.Status.Ticket)
		}
		var why any
		if api.Err != nil {
			why = api.Err
		}
		m.deps.Log.Info("node_claim_apiserver_link_withheld", "claim", claim.Key(), "link", api.Name,
			"phase", phase, "ticketBytes", ticketBytes, "error", why)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	return map[string]any{bundleKey: string(raw)}, nil
}

func (m *Mapper) Entry(claim contract.Claim, adopt bool, bases contract.Bases) (v1alpha1.InfraContract, v1alpha1.InfraContractNode) {
	spec := claim.Object.Spec
	entry := v1alpha1.InfraContract{VMName: spec.NodeName, VMReadyTimeoutSec: m.cfg.VMReadyTimeoutSec}
	node := v1alpha1.InfraContractNode{OwnerDid: spec.OwnerDid, NodeClaimRef: claim.Ref()}
	if len(spec.DenyBidderDids) > 0 {
		entry.DenyBidderDids = append([]string(nil), spec.DenyBidderDids...)
	}
	if len(m.cfg.ExtraBidderDIDs) > 0 {
		entry.BidderDids = append([]string(nil), m.cfg.ExtraBidderDIDs...)
	}
	if m.cfg.UserDataBase != "" {
		entry.UserData = m.cfg.UserDataBase
	}
	if m.cfg.Policy != nil {
		entry.Policy = m.cfg.Policy.Name
		args := m.cfg.Policy.Args
		if args == nil {
			args = map[string]any{}
		}
		if raw, err := json.Marshal(args); err == nil {
			entry.PolicyArgs = string(raw)
		}
	}
	if adopt {
		node.Adopt = true
		return entry, node
	}
	join := &v1alpha1.ComputeContractNodeJoin{ClusterRef: spec.ClusterRef}
	if m.cfg.Meshd != nil {
		mesh := &v1alpha1.ComputeContractMeshd{DownloadBase: bases.Meshd}
		for _, arch := range Arches {
			mesh.Pins = append(mesh.Pins, v1alpha1.ComputeContractMeshdPin{Arch: arch, Sha256: m.cfg.Meshd.Sha256[arch]})
		}
		join.Meshd = mesh
	}
	if m.cfg.NodeAgent != nil {
		agent := &v1alpha1.ComputeContractNodeAgent{DownloadBase: bases.NodeAgent}
		for _, arch := range Arches {
			agent.Pins = append(agent.Pins, v1alpha1.ComputeContractMeshdPin{Arch: arch, Sha256: m.cfg.NodeAgent.Sha256[arch]})
		}
		join.NodeAgent = agent
	}
	node.NodeJoin = join
	return entry, node
}

func (m *Mapper) Requested(status v1alpha1.NodeClaimStatus, adopt bool) v1alpha1.NodeClaimStatus {
	next := statusCopy(status)
	if adopt {
		next.Phase = v1alpha1.NodeClaimAdopting
		next.Message = "a joined Node with no claim: matching it to its receipt and vm.hostKeys record"
	} else {
		next.Phase = v1alpha1.NodeClaimBidding
		next.Message = "requesting a node from the compute market"
	}
	return next
}

func (m *Mapper) Failed(status v1alpha1.NodeClaimStatus, cause error) v1alpha1.NodeClaimStatus {
	next := statusCopy(status)
	next.Phase = v1alpha1.NodeClaimFailed
	next.Message = "the contract failed: " + cause.Error()
	return next
}

func (m *Mapper) Status(claim contract.Claim, cc v1alpha1.ComputeContract) *v1alpha1.NodeClaimStatus {
	current := claim.Object.Status
	phase := current.Phase
	if phase == v1alpha1.NodeClaimFailed || phase == v1alpha1.NodeClaimReleased || phase == v1alpha1.NodeClaimLost {
		return nil
	}
	status := cc.Status
	if status.Phase == "" {
		return nil
	}
	state := status.Contract
	adopt := cc.Spec.Adopt
	next := statusCopy(current)
	if state != nil {
		var copyOf v1alpha1.NodeClaimContract
		state.DeepCopyInto(&copyOf)
		if copyOf.Receipt != nil {
			copyOf.ReceiptURI = cmp.Or(copyOf.ReceiptURI, copyOf.Receipt.URI)
			copyOf.ReceiptCid = cmp.Or(copyOf.ReceiptCid, copyOf.Receipt.CID)
		}
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
			if state != nil && state.Accept != nil {
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
			next.Message = cmp.Or(status.Message, "adopted")
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
			next.Message = LostMessage(state)
		} else {
			next.Phase = v1alpha1.NodeClaimReleased
			next.Message = cmp.Or(status.Message, "the contract was released")
		}
	case v1alpha1.ComputeContractFailed:
		next.Phase = v1alpha1.NodeClaimFailed
		next.Message = cmp.Or(status.Message, "the contract failed")
	}
	if !adopt && next.Phase == v1alpha1.NodeClaimJoining && next.JoinedAt != "" {
		next.Phase = v1alpha1.NodeClaimRunning
		next.Message = "the node joined its cluster and the cluster reports it"
	}
	if reflect.DeepEqual(current, next) {
		return nil
	}
	return &next
}

func LostMessage(state *v1alpha1.NodeClaimContract) string {
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

func (m *Mapper) Settle(claim contract.Claim, cc *v1alpha1.ComputeContract) contract.Settlement {
	finished := cc == nil || cc.Status.Phase == v1alpha1.ComputeContractReleased || cc.Status.Phase == v1alpha1.ComputeContractFailed
	phase := claim.Object.Status.Phase
	abandoned := phase == v1alpha1.NodeClaimFailed || phase == v1alpha1.NodeClaimLost
	if !finished && abandoned && cc != nil && cc.Status.Phase == v1alpha1.ComputeContractReleasing {
		next := statusCopy(claim.Object.Status)
		next.Phase = v1alpha1.NodeClaimFailed
		next.Message = "the claim was deleted; the release of its VM is still retrying on ComputeContract " + cc.Name
		return contract.Settlement{Status: &next, Done: true}
	}
	if !finished {
		return contract.Settlement{}
	}
	var state *v1alpha1.NodeClaimContract
	if cc != nil {
		state = cc.Status.Contract
	}
	confirmed := cc == nil || cc.Status.Phase == v1alpha1.ComputeContractReleased || state == nil || state.Receipt == nil
	next := statusCopy(claim.Object.Status)
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
	return contract.Settlement{Status: &next, RemoveBundle: true, Done: true}
}

func statusCopy(status v1alpha1.NodeClaimStatus) v1alpha1.NodeClaimStatus {
	var out v1alpha1.NodeClaimStatus
	status.DeepCopyInto(&out)
	return out
}

func DefaultOpenBaoCAFile(spec v1alpha1.NodeClaimSpec) string {
	return path.Join(path.Dir(spec.TokenFile), OpenBaoCARelativeToClusterDir)
}

func PublishedOpenBaoCAFile(caDir string) func(v1alpha1.NodeClaimSpec) string {
	caDir = strings.TrimRight(caDir, "/")
	return func(spec v1alpha1.NodeClaimSpec) string {
		return caDir + "/" + path.Base(path.Dir(spec.TokenFile)) + ".crt"
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
