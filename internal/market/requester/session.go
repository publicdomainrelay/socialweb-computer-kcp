package requester

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const (
	heldContractTTLSeconds = 691200
)

var vmNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var localeNames = map[string]bool{
	"LC_ALL":            true,
	"LC_CTYPE":          true,
	"LC_NUMERIC":        true,
	"LC_TIME":           true,
	"LC_COLLATE":        true,
	"LC_MONETARY":       true,
	"LC_MESSAGES":       true,
	"LC_PAPER":          true,
	"LC_NAME":           true,
	"LC_ADDRESS":        true,
	"LC_TELEPHONE":      true,
	"LC_MEASUREMENT":    true,
	"LC_IDENTIFICATION": true,
}

type SessionTarget struct {
	VMName string

	Address string

	HostKeys sshtransport.GuestHostKeys

	Bidder Bidder

	State *v1alpha1.NodeClaimContract
}

func (q *Requester) RunCommand(ctx context.Context, account sshtransport.AuthorizedAccount, command string, env map[string]string, io sshtransport.CommandIO) error {
	if io == nil {
		return errors.New("requester: the session has no command io")
	}
	lc := lcEnv(env)
	if name := strings.TrimSpace(lc["LC_SESSION"]); name != "" {
		return q.runHeldCommand(ctx, account, name, command, lc, io)
	}
	return q.runTransientCommand(ctx, account, command, lc, io)
}

func commandFailed(io sshtransport.CommandIO, prefix string, err error) error {
	io.WriteErr([]byte(prefix + err.Error() + "\n"))
	io.Exit(1)
	return err
}

func (q *Requester) sessionPolicy(lc map[string]string) (string, map[string]string, bool, error) {
	policyName := cmp.Or(strings.TrimSpace(lc["LC_POLICY"]), q.cfg.PolicyWorkflow)
	usePolicy := true
	switch strings.ToLower(policyName) {
	case "", "none", "off":
		usePolicy = false
		policyName = ""
	}
	policyArgs := map[string]string{}
	if raw := strings.TrimSpace(lc["LC_POLICY_ARGS"]); raw != "" {
		parsed, err := participant.ParsePolicyArgs(raw)
		if err != nil {
			return "", nil, false, fmt.Errorf("LC_POLICY_ARGS: %w", err)
		}
		policyArgs = parsed
	}
	return policyName, policyArgs, usePolicy, nil
}

func (q *Requester) sessionVMName(lc map[string]string) (string, error) {
	vmName := strings.TrimSpace(lc["LC_VM_NAME"])
	if vmName == "" {
		vmName = q.generatedVMName()
	}
	if len(vmName) > 63 || !vmNamePattern.MatchString(vmName) {
		return "", fmt.Errorf("LC_VM_NAME %q is not a DNS-label-shaped name", vmName)
	}
	return vmName, nil
}

func (q *Requester) guestDialer() sshtransport.Dialer {
	if socket := strings.TrimSpace(q.cfg.Spec.IrohSocket); socket != "" {
		return sshtransport.UnixSocketDialer{Socket: socket, Log: q.log}
	}
	return sshtransport.DumbpipeDialer{Path: "dumbpipe", Log: q.log}
}

func (q *Requester) runTransientCommand(ctx context.Context, account sshtransport.AuthorizedAccount, command string, lc map[string]string, io sshtransport.CommandIO) error {
	policyName, policyArgs, usePolicy, err := q.sessionPolicy(lc)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	vmName, err := q.sessionVMName(lc)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	command = renderExecCommand(command, lc)
	sessionCtx, cancel := context.WithTimeout(ctx, q.cfg.SessionCap)
	defer cancel()
	ctx = sessionCtx
	publicKey, privateKeyPath, err := sshtransport.GenerateGuestKeypair(q.guestKeyDir(), vmName)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	defer sshtransport.RemoveGuestKeypair(privateKeyPath)
	target, err := q.provisionSession(ctx, vmName, policyName, policyArgs, publicKey, usePolicy)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	defer func() {
		if !truthy(lc["LC_KEEP_VM"]) {
			releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultReleaseTimeout)
			defer releaseCancel()
			if err := q.releaseSession(releaseCtx, target); err != nil {
				q.log.Warn("requester_session_release_failed", "vm", target.VMName, "err", err)
			}
		}
	}()
	ticket, err := ValidateTicket(target.Address)
	if err != nil {
		return commandFailed(io, "guest session failed: ", err)
	}
	provider, err := sshtransport.NewGuestSessionProvider(q.guestDialer(), q.log)
	if err != nil {
		return commandFailed(io, "guest session failed: ", err)
	}
	if !provider.PollReady(ctx, privateKeyPath, ticket, DefaultVMReadyTimeout, target.HostKeys) {
		return commandFailed(io, "guest session failed: ", errors.New("the guest ssh did not answer"))
	}
	code := provider.RunSession(ctx, privateKeyPath, ticket, command, io, target.HostKeys)
	io.Exit(code)
	return nil
}

type heldSession struct {
	mu sync.Mutex

	vmName string

	privateKeyPath string

	contract string

	target *SessionTarget

	timer *time.Timer

	generation uint64
}

func (h *heldSession) clear() {
	h.target = nil
	h.privateKeyPath = ""
	h.contract = ""
}

func (q *Requester) heldSession(key string) *heldSession {
	q.heldMu.Lock()
	defer q.heldMu.Unlock()
	held := q.heldSessions[key]
	if held == nil {
		held = &heldSession{}
		q.heldSessions[key] = held
	}
	return held
}

func (q *Requester) armHeldSession(key string, held *heldSession) {
	held.mu.Lock()
	if held.timer != nil {
		held.timer.Stop()
	}
	held.generation++
	generation := held.generation
	held.timer = time.AfterFunc(q.cfg.SessionIdle, func() {
		q.releaseHeldSession(key, held, generation, "idle")
	})
	held.mu.Unlock()
}

func (q *Requester) releaseHeldSession(key string, held *heldSession, generation uint64, reason string) {
	held.mu.Lock()
	if generation != 0 && generation != held.generation {
		held.mu.Unlock()
		return
	}
	if held.timer != nil {
		held.timer.Stop()
		held.timer = nil
	}
	held.generation++
	q.heldMu.Lock()
	if q.heldSessions[key] == held {
		delete(q.heldSessions, key)
	}
	q.heldMu.Unlock()
	privateKeyPath := held.privateKeyPath
	contract := held.contract
	held.clear()
	held.mu.Unlock()
	if contract != "" {
		q.releaseHeldContract(contract)
		q.log.Info("requester_held_session_released", "contract", contract, "reason", reason)
	}
	_ = sshtransport.RemoveGuestKeypair(privateKeyPath)
}

func (q *Requester) releaseAllHeldSessions(reason string) {
	q.heldMu.Lock()
	keys := make([]string, 0, len(q.heldSessions))
	held := make([]*heldSession, 0, len(q.heldSessions))
	for key, session := range q.heldSessions {
		keys = append(keys, key)
		held = append(held, session)
	}
	q.heldMu.Unlock()
	for i := range keys {
		q.releaseHeldSession(keys[i], held[i], 0, reason)
	}
}

func (q *Requester) runHeldCommand(ctx context.Context, account sshtransport.AuthorizedAccount, name, command string, lc map[string]string, io sshtransport.CommandIO) error {
	policyName, policyArgs, usePolicy, err := q.sessionPolicy(lc)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	vmName, err := q.sessionVMName(lc)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	command = renderExecCommand(command, lc)
	sessionCtx, cancel := context.WithTimeout(ctx, q.cfg.SessionCap)
	defer cancel()
	ctx = sessionCtx
	provider, err := sshtransport.NewGuestSessionProvider(q.guestDialer(), q.log)
	if err != nil {
		return commandFailed(io, "guest session failed: ", err)
	}
	contractName := heldContractName(account.DID, name)
	provision := func() (*SessionTarget, string, error) {
		publicKey, privateKeyPath, err := sshtransport.GenerateGuestKeypair(q.guestKeyDir(), vmName)
		if err != nil {
			return nil, "", err
		}
		requestedPolicy := ""
		if usePolicy {
			requestedPolicy = policyName
		}
		target, err := q.provisionHeldContract(ctx, contractName, vmName, account.DID, publicKey, requestedPolicy, policyArgs)
		if err != nil {
			q.releaseHeldContract(contractName)
			_ = sshtransport.RemoveGuestKeypair(privateKeyPath)
			return nil, "", err
		}
		return target, privateKeyPath, nil
	}
	key := account.DID + "\x00" + name
	held := q.heldSession(key)
	held.mu.Lock()
	if held.timer != nil {
		held.timer.Stop()
		held.timer = nil
	}
	reused := held.target != nil
	target, privateKeyPath, contract := held.target, held.privateKeyPath, held.contract
	fresh := target == nil
	if target == nil {
		target, privateKeyPath, err = provision()
		if err != nil {
			held.mu.Unlock()
			return commandFailed(io, "provisioning failed: ", err)
		}
		contract = contractName
		held.vmName, held.privateKeyPath, held.target, held.contract = vmName, privateKeyPath, target, contract
	}
	ready := false
	if ticket, ticketErr := ValidateTicket(target.Address); ticketErr == nil {
		ready = provider.PollReady(ctx, privateKeyPath, ticket, DefaultVMReadyTimeout, target.HostKeys)
	}
	if !ready && reused {
		q.log.Warn("requester_held_session_lost", "contract", contract)
		held.clear()
		q.releaseHeldSessionTarget(contract, privateKeyPath)
		target, privateKeyPath, err = provision()
		if err != nil {
			held.mu.Unlock()
			return commandFailed(io, "provisioning failed: ", err)
		}
		contract = contractName
		held.vmName, held.privateKeyPath, held.target, held.contract = vmName, privateKeyPath, target, contract
		fresh = true
		ticket, ticketErr := ValidateTicket(target.Address)
		if ticketErr != nil {
			held.mu.Unlock()
			return commandFailed(io, "guest session failed: ", ticketErr)
		}
		ready = provider.PollReady(ctx, privateKeyPath, ticket, DefaultVMReadyTimeout, target.HostKeys)
	}
	if !ready {
		if fresh {
			held.clear()
			q.releaseHeldSessionTarget(contract, privateKeyPath)
		}
		held.mu.Unlock()
		return commandFailed(io, "guest session failed: ", errors.New("the guest ssh did not answer"))
	}
	ticket, err := ValidateTicket(target.Address)
	if err != nil {
		if fresh {
			held.clear()
			q.releaseHeldSessionTarget(contract, privateKeyPath)
		}
		held.mu.Unlock()
		return commandFailed(io, "guest session failed: ", err)
	}
	code := provider.RunSession(ctx, privateKeyPath, ticket, command, io, target.HostKeys)
	held.mu.Unlock()
	q.armHeldSession(key, held)
	io.Exit(code)
	return nil
}

func heldContractName(did, name string) string {
	sum := sha256.Sum256([]byte(did + "\x00" + name))
	return "ssh-" + hex.EncodeToString(sum[:10])
}

func sessionRKey(label string, parts ...string) string {
	return market.SessionRKey(label, parts...)
}

func (q *Requester) existingSignedRecord(ctx context.Context, collection, rkey string) (market.SignedRecord, bool, error) {
	record, err := q.repo.GetRecord(ctx, q.repoDID, collection, rkey)
	if err != nil {
		if errors.Is(err, market.ErrNotFound) {
			return market.SignedRecord{}, false, nil
		}
		return market.SignedRecord{}, false, err
	}
	return market.SignedRecord{URI: record.URI, CID: record.CID, Record: record.Value}, true, nil
}

func (q *Requester) existingOrPutRecord(ctx context.Context, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	existing, found, err := q.existingSignedRecord(ctx, collection, rkey)
	if err != nil {
		return market.StrongRef{}, err
	}
	if found {
		return market.NewStrongRef(existing.URI, existing.CID), nil
	}
	return q.repo.PutRecord(ctx, q.repoDID, collection, rkey, record)
}

func contractTarget(contract v1alpha1.ComputeContract) (*SessionTarget, bool, error) {
	state := contract.Status.Contract
	if state == nil || state.VMAddress == "" || state.HostKeys == nil || len(state.HostKeys.Keys) == 0 ||
		state.Receipt == nil || state.Receipt.URI == "" {
		return nil, false, nil
	}
	ticket, err := ValidateTicket(state.VMAddress)
	if err != nil {
		return nil, false, err
	}
	keys := make([]string, 0, len(state.HostKeys.Keys))
	for _, key := range state.HostKeys.Keys {
		keys = append(keys, key.Type+" "+key.PublicKey)
	}
	return &SessionTarget{
		VMName:   state.VMName,
		Address:  ticket,
		HostKeys: sshtransport.GuestHostKeys{Alias: hostKeyAlias(state.Receipt.URI), Keys: keys},
	}, true, nil
}

func (q *Requester) awaitContractChange(ctx context.Context) error {
	calls := 0
	return q.contracts.Await(ctx, func() bool {
		calls++
		return calls > 1
	})
}

func (q *Requester) provisionHeldContract(ctx context.Context, name, vmName, did, publicKey, policyName string, policyArgs map[string]string) (*SessionTarget, error) {
	args := ""
	if len(policyArgs) > 0 {
		raw, err := json.Marshal(policyArgs)
		if err != nil {
			return nil, err
		}
		args = string(raw)
	}
	deadline := q.now().Add(q.cfg.BidWindow + q.cfg.ReceiptTimeout + DefaultVMReadyTimeout)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	contract := v1alpha1.ComputeContract{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: contractmarket.KindComputeContract},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ComputeContractSpec{
			ComputeContractTemplate: v1alpha1.ComputeContractTemplate{VMName: vmName, SshAuthorizedKeys: []string{publicKey},
				Policy: policyName, PolicyArgs: args},
			OwnerDid:                did,
			TTLSecondsAfterFinished: new(int32(heldContractTTLSeconds)),
		},
	}
	for {
		if existing, ok := q.contracts.Get(name); ok {
			if existing.DeletionTimestamp == nil {
				if err := q.deps.Intents.DeleteComputeContract(ctx, name); err != nil {
					return nil, err
				}
			}
			gone := func() bool {
				_, present := q.contracts.Get(name)
				return !present
			}
			if err := q.contracts.Await(waitCtx, gone); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, errors.New("the previous session contract was not removed in time")
			}
			continue
		}
		err := q.deps.Intents.CreateComputeContract(ctx, contract)
		if err == nil {
			break
		}
		if !errors.Is(err, contractmarket.ErrExists) {
			return nil, err
		}
		if err := q.awaitContractChange(waitCtx); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("the session contract was not created in time")
		}
	}
	return q.waitHeldContract(waitCtx, ctx, name)
}

func (q *Requester) waitHeldContract(waitCtx, ctx context.Context, name string) (*SessionTarget, error) {
	var target *SessionTarget
	var failure error
	seen := false
	_ = q.contracts.Await(waitCtx, func() bool {
		contract, ok := q.contracts.Get(name)
		if !ok {
			if seen {
				failure = errors.New("the session contract disappeared")
			}
			return seen
		}
		seen = true
		switch contract.Status.Phase {
		case v1alpha1.ComputeContractFailed:
			failure = fmt.Errorf("the session contract failed: %s", contract.Status.Message)
			return true
		case v1alpha1.ComputeContractReleased:
			failure = errors.New("the session contract was released before the guest was ready")
			return true
		}
		found, ready, err := contractTarget(contract)
		if err != nil {
			failure = err
			return true
		}
		target = found
		return ready
	})
	switch {
	case failure != nil:
		return nil, failure
	case target != nil:
		return target, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	return nil, errors.New("the guest did not report onNetwork and hostKeys in time")
}

func (q *Requester) releaseHeldContract(name string) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.runContext()), DefaultReleaseTimeout)
	defer cancel()
	if err := q.deps.Intents.ReleaseComputeContract(ctx, name); err != nil && !errors.Is(err, contractmarket.ErrNotFound) {
		q.log.Warn("requester_held_contract_release_failed", "contract", name, "err", err)
	}
}

func (q *Requester) releaseHeldSessionTarget(contract, privateKeyPath string) {
	if contract != "" {
		q.releaseHeldContract(contract)
	}
	_ = sshtransport.RemoveGuestKeypair(privateKeyPath)
}

func (q *Requester) provisionSession(ctx context.Context, vmName, policyName string, policyArgs map[string]string, publicKey string, usePolicy bool) (*SessionTarget, error) {
	userData, err := guestseed.WithIrohTransport(defaultUserData([]string{publicKey}), []string{publicKey}, nil)
	if err != nil {
		return nil, fmt.Errorf("the guest iroh transport: %w", err)
	}
	userData = withGuestCACerts(userData, q.guestCAPEM())
	vmRKey := sessionRKey("vm", q.repoDID, vmName)
	payload, found, err := q.existingSignedRecord(ctx, market.ComputeVMNSID, vmRKey)
	if err != nil {
		return nil, err
	}
	if !found {
		record, err := market.EncodeRecord(market.ComputeVM{Role: vmName, Disk: DefaultComputeDisk, UserData: userData, CreatedAt: q.nowISO()})
		if err != nil {
			return nil, err
		}
		payload, err = market.CreateSignedRecordAt(ctx, q.repo, q.repoDID, market.ComputeVMNSID, vmRKey, record, q.signer)
		if err != nil {
			return nil, err
		}
	}
	var policyRef *market.StrongRef
	policyName = strings.TrimSpace(policyName)
	if policyName != "" {
		workflow := PolicyWorkflowName(policyName)
		workflowText, ok := PolicyWorkflowText(policyName)
		if !ok {
			return nil, fmt.Errorf("policy %q names no bundled workflow", policyName)
		}
		record := BuildPolicyRecord(policyName, workflow, workflowText, q.marketDID, policyArgs, q.nowISO())
		created, err := q.existingOrPutRecord(ctx, PolicyGhaLiteNSID, sessionRKey("policy", q.repoDID, vmName, policyName), record)
		if err != nil {
			return nil, err
		}
		policyRef = &created
	}
	options := market.RFPOptions{
		Payload:   market.RecordRef{URI: payload.URI, CID: payload.CID},
		SubmitBid: q.submitBidRef(),
		Ingress:   q.ingressRecords(ctx),
		Domain:    "compute",
		CreatedAt: q.nowISO(),
	}
	if policyRef != nil {
		options.Policies = []market.RecordRef{{URI: policyRef.URI, CID: policyRef.CID}}
	}
	rfpRKey := sessionRKey("rfp", q.repoDID, vmName)
	rfp, found, err := q.existingSignedRecord(ctx, market.RFPNSID, rfpRKey)
	if err != nil {
		return nil, err
	}
	if !found && q.cfg.Spec.SuspendParticipation {
		return nil, ErrParticipationSuspended
	}
	if !found {
		rfp, err = market.CreateRFPAt(ctx, q.repo, q.repoDID, rfpRKey, options, q.signer)
		if err != nil {
			return nil, err
		}
	}
	discovery, err := q.market.Discover(ctx, q.now(), nil)
	if err != nil {
		q.log.Warn("requester_session_discovery_failed", "err", err)
	}
	noBidders := ""
	if len(discovery.Bidders) == 0 {
		noBidders = "requester_no_bidders: " + discovery.Why()
		q.log.Warn("requester_no_bidders", "session", vmName, "candidates", discovery.Candidates, "dropped", discovery.DroppedText())
	}
	q.rememberBidders(discovery.Bidders)
	winner, winnerBidder, err := q.sessionWinner(ctx, rfp, discovery.Bidders, usePolicy, PolicyWorkflowName(policyName))
	if err != nil {
		if noBidders != "" {
			return nil, fmt.Errorf("%w; %s", err, noBidders)
		}
		return nil, err
	}
	acceptRKey := sessionRKey("accept", q.repoDID, vmName, rfp.URI, rfp.CID, winner.URI, winner.CID)
	accept, found, err := q.existingSignedRecord(ctx, market.AcceptNSID, acceptRKey)
	if err != nil {
		return nil, err
	}
	if !found {
		accept, err = market.CreateAcceptAt(ctx, q.repo, q.repoDID, acceptRKey, market.AcceptOptions{
			RFP:         market.RecordRef{URI: rfp.URI, CID: rfp.CID},
			Bid:         market.RecordRef{URI: winner.URI, CID: winner.CID},
			SubmitEvent: q.submitEventRef(),
			Ingress:     q.ingressRecords(ctx),
			CreatedAt:   q.nowISO(),
		}, q.signer)
		if err != nil {
			return nil, err
		}
	}
	receipt, err := q.submitAcceptAndWait(ctx, winnerBidder, accept.URI, accept.CID, winner.DID)
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, errors.New("no receipt arrived within " + q.cfg.ReceiptTimeout.String())
	}
	if !market.VerifyReceipt(ctx, receipt.Value, receipt.URI, accept.Record, accept.URI, q.receiptOptions()).OK {
		return nil, errors.New("the receipt signature or accept binding is invalid")
	}
	state := &v1alpha1.NodeClaimContract{
		VMName:         vmName,
		VMURI:          payload.URI,
		RFPURI:         rfp.URI,
		AcceptURI:      accept.URI,
		ReceiptURI:     receipt.URI,
		ReceiptCid:     receipt.CID,
		RequesterDid:   q.callbackDID(),
		MarketDid:      q.marketDID,
		Payload:        &commonv1.StrongRef{URI: payload.URI, CID: payload.CID},
		RFP:            &commonv1.StrongRef{URI: rfp.URI, CID: rfp.CID},
		Bid:            &commonv1.StrongRef{URI: winner.URI, CID: winner.CID},
		Winner:         &v1alpha1.ContractWinner{DID: winner.DID},
		Accept:         &commonv1.StrongRef{URI: accept.URI, CID: accept.CID},
		Receipt:        &commonv1.StrongRef{URI: receipt.URI, CID: receipt.CID},
		ReceiptOk:      new(true),
		Phase:          v1alpha1.ContractReceipt,
		SubmitEventRef: stringOf(receipt.Value["submitEvent"]),
	}
	if policyRef != nil {
		state.Policy = &commonv1.StrongRef{URI: policyRef.URI, CID: policyRef.CID}
	}
	address, hostKeys, termination, err := q.sessionGuest(ctx, receipt.URI)
	if err != nil {
		return nil, err
	}
	if termination != nil {
		return nil, fmt.Errorf("the bidder ended the contract: %s", termination.Reason)
	}
	state.VMAddress = address
	state.Phase = v1alpha1.ContractNetwork
	return &SessionTarget{
		VMName:   vmName,
		Address:  address,
		HostKeys: sshtransport.GuestHostKeys{Alias: hostKeyAlias(receipt.URI), Keys: hostKeyLines(hostKeys)},
		Bidder:   winnerBidder,
		State:    state,
	}, nil
}

func (q *Requester) sessionWinner(ctx context.Context, rfp market.SignedRecord, bidders []Bidder, usePolicy bool, workflow string) (*BidRecord, Bidder, error) {
	for _, bidder := range bidders {
		if bidder.Endpoint == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, max(q.cfg.BidWindow, 5*time.Second))
		bids, err := q.market.SubmitRFP(cctx, bidder, SubmitRFPRequest{URI: rfp.URI, CID: rfp.CID})
		cancel()
		if err != nil {
			q.log.Warn("requester_session_rfp_failed", "bidder", bidder.DID, "err", err)
			continue
		}
		for _, bid := range bids {
			q.queueBid(rfp.URI, bid)
		}
	}
	window := time.NewTimer(max(q.cfg.BidWindow, 0))
	defer window.Stop()
	byURI := map[string]BidRecord{}
	bidderByDID := map[string]Bidder{}
	for _, bidder := range bidders {
		if _, ok := bidderByDID[bidder.DID]; !ok {
			bidderByDID[bidder.DID] = bidder
		}
	}
	pick := func() (*BidRecord, Bidder, bool) {
		for _, bid := range q.bids.take(rfp.URI) {
			byURI[bid.URI] = bid
		}
		if best := q.sessionBestBid(ctx, byURI, usePolicy, workflow); best != nil {
			if bidder, ok := bidderByDID[best.DID]; ok {
				return best, bidder, true
			}
		}
		return nil, Bidder{}, false
	}
	open := q.cfg.BidWindow > 0
	for open {
		arrived := q.bids.arrived()
		if best, bidder, ok := pick(); ok {
			return best, bidder, nil
		}
		select {
		case <-ctx.Done():
			return nil, Bidder{}, ctx.Err()
		case <-window.C:
			open = false
		case <-arrived:
		}
	}
	if best, bidder, ok := pick(); ok {
		return best, bidder, nil
	}
	if len(byURI) == 0 {
		return nil, Bidder{}, errors.New("no bids arrived within " + q.cfg.BidWindow.String())
	}
	return nil, Bidder{}, errors.New("every bid was denied by policy")
}

func (q *Requester) sessionBestBid(ctx context.Context, bids map[string]BidRecord, usePolicy bool, workflow string) *BidRecord {
	var best *BidRecord
	for _, bid := range bids {
		candidate := bid
		if err := q.classifyBid(ctx, &candidate); err != nil {
			continue
		}
		if !q.bidAllowedWithPolicy(ctx, candidate, usePolicy, workflow) {
			continue
		}
		switch {
		case best == nil:
			best = &candidate
		case best.Free:
		case candidate.Free:
			best = &candidate
		case candidate.Cost < best.Cost:
			best = &candidate
		}
	}
	if usePolicy && best != nil && !best.Free {
		decision := q.policy.Evaluate(ctx, PolicyInput{
			SelfDid:          q.marketDID,
			SubjectDid:       best.DID,
			CounterpartyDid:  best.DID,
			RootRequesterDid: q.marketDID,
			Bid:              &PolicyRef{URI: best.URI, CID: best.CID},
		})
		if !decision.Allow {
			return nil
		}
	}
	return best
}

func (q *Requester) sessionGuest(ctx context.Context, receiptURI string) (string, []v1alpha1.ComputeVMHostKey, *v1alpha1.ContractTermination, error) {
	events, pending, closeEvents := q.registerEventSink(receiptURI)
	defer closeEvents()
	seen := map[string]bool{}
	address := ""
	var hostKeys []v1alpha1.ComputeVMHostKey
	var termination *v1alpha1.ContractTermination
	handle := func(event MarketEvent) bool {
		if guestEventSeen(seen, event) || isContractEventNSID(market.NSIDFromURI(event.Payload.URI)) {
			return false
		}
		parsed, err := q.parseGuestEvent(ctx, event, receiptURI)
		if err != nil {
			q.log.Warn("requester_session_event_refused", "uri", event.Event.URI, "err", err)
			return false
		}
		switch parsed.Kind {
		case market.ComputeEventsVMOnNetworkNSID:
			if parsed.Tag == "" {
				address = cmp.Or(address, parsed.Address)
			}
		case market.ComputeEventsVMHostKeysNSID:
			hostKeys = parsed.HostKeys
		case market.ComputeEventsVMTerminatedNSID:
			termination = parsed.Termination
			return true
		}
		return address != "" && len(hostKeys) > 0
	}
	_ = slices.ContainsFunc(pending, handle)
	deadline := time.NewTimer(DefaultVMReadyTimeout)
	defer deadline.Stop()
	var retry eventRetry
	defer retry.stop()
	retryC := retry.after(q.drainEvents(ctx, "", receiptURI))
	for {
		if termination != nil {
			return "", nil, termination, nil
		}
		if address != "" && len(hostKeys) > 0 {
			return address, hostKeys, nil, nil
		}
		select {
		case <-ctx.Done():
			return "", nil, nil, ctx.Err()
		case <-deadline.C:
			return "", nil, nil, errors.New("the guest did not report onNetwork and hostKeys within " + DefaultVMReadyTimeout.String())
		case event := <-events:
			handle(event)
		case <-retryC:
			retryC = retry.after(q.drainEvents(ctx, "", receiptURI))
		}
	}
}

func (q *Requester) releaseSession(ctx context.Context, target *SessionTarget) error {
	if target == nil || target.State == nil || target.State.Receipt == nil {
		return nil
	}
	state := target.State
	envelope, payload, err := q.destroyEnvelope(ctx, state.Receipt, "session_ended")
	if err != nil {
		return err
	}
	release := &v1alpha1.ContractRelease{
		Event:   commonv1.StrongRef{URI: envelope.URI, CID: envelope.CID},
		Payload: commonv1.StrongRef{URI: payload.URI, CID: payload.CID},
	}
	state.Release = release
	if err := q.persistSessionRelease(ctx, state.Receipt.URI, release); err != nil {
		return err
	}
	if target.Bidder.Endpoint == "" {
		release.OK = true
		if err := q.persistSessionRelease(ctx, state.Receipt.URI, release); err != nil {
			q.log.Warn("requester_session_release_status_failed", "receipt", state.Receipt.URI, "err", err)
		}
		return nil
	}
	var lastErr error
	for attempt := 1; ; attempt++ {
		status, err := q.market.SubmitEvent(ctx, target.Bidder, SubmitEventRequest{URI: envelope.URI, CID: envelope.CID, Record: envelope.Record})
		if err == nil && status >= 200 && status <= 299 {
			release.OK = true
			release.Status = &status
			if err := q.persistSessionRelease(ctx, state.Receipt.URI, release); err != nil {
				q.log.Warn("requester_session_release_status_failed", "receipt", state.Receipt.URI, "err", err)
			}
			return nil
		}
		if status != 0 {
			release.Status = &status
		}
		lastErr = err
		if err == nil {
			lastErr = fmt.Errorf("vm.delete answered HTTP %d", status)
		}
		if sleepCtx(ctx, releaseRetryDelay(attempt)) != nil {
			return lastErr
		}
	}
}

func (q *Requester) persistSessionRelease(ctx context.Context, receiptURI string, release *v1alpha1.ContractRelease) error {
	if receiptURI == "" {
		return nil
	}
	for _, contract := range q.contracts.List() {
		state := contract.Status.Contract
		if state == nil || state.Receipt == nil || state.Receipt.URI != receiptURI {
			continue
		}
		status := v1alpha1.ComputeContractStatus{Contract: &v1alpha1.NodeClaimContract{Release: release}}
		if err := q.deps.Intents.MergeComputeContractStatus(ctx, contract.Name, status); err != nil {
			return fmt.Errorf("persist the release refs on contract %s: %w", contract.Name, err)
		}
	}
	return nil
}

func lcEnv(env map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range env {
		if !strings.HasPrefix(key, "LC_") || localeNames[key] {
			continue
		}
		out[key] = value
	}
	return out
}

func renderExecCommand(command string, env map[string]string) string {
	if len(env) == 0 {
		return command
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString("'" + strings.ReplaceAll(env[key], "'", "'\"'\"'") + "'")
		b.WriteString("; ")
	}
	b.WriteString(command)
	return b.String()
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
