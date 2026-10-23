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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

var vmNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

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

func (s *Service) RunCommand(ctx context.Context, account sshtransport.AuthorizedAccount, command string, env map[string]string, io sshtransport.CommandIO) error {
	if io == nil {
		return errors.New("requester: the session has no command io")
	}
	lc := lcEnv(env)
	if name := strings.TrimSpace(lc["LC_SESSION"]); name != "" {
		return s.runHeldCommand(ctx, account, name, command, lc, io)
	}
	return s.runTransientCommand(ctx, account, command, lc, io)
}

func (s *Service) sessionPolicy(lc map[string]string) (string, map[string]string, bool, error) {
	policyName := strings.TrimSpace(lc["LC_POLICY"])
	policyName = cmp.Or(policyName, s.cfg.PolicyWorkflow)
	usePolicy := true
	switch strings.ToLower(policyName) {
	case "", "none", "off":
		usePolicy = false
		policyName = ""
	}
	policyArgs := map[string]string{}
	if raw := strings.TrimSpace(lc["LC_POLICY_ARGS"]); raw != "" {
		parsed, err := parsePolicyArgs(raw)
		if err != nil {
			return "", nil, false, fmt.Errorf("LC_POLICY_ARGS: %w", err)
		}
		policyArgs = parsed
	}
	for key, value := range s.cfg.PolicyArgs {
		if _, ok := policyArgs[key]; !ok {
			policyArgs[key] = value
		}
	}
	return policyName, policyArgs, usePolicy, nil
}

func (s *Service) runTransientCommand(ctx context.Context, account sshtransport.AuthorizedAccount, command string, lc map[string]string, io sshtransport.CommandIO) error {
	policyName, policyArgs, usePolicy, err := s.sessionPolicy(lc)
	if err != nil {
		io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	vmName := strings.TrimSpace(lc["LC_VM_NAME"])
	if vmName == "" {
		vmName = s.generatedVMName()
	}
	if !vmNamePattern.MatchString(vmName) {
		err := fmt.Errorf("LC_VM_NAME %q is not a DNS-label-shaped name", vmName)
		io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	command = renderExecCommand(command, lc)
	sessionCtx, cancel := context.WithTimeout(ctx, s.cfg.SessionCap)
	defer cancel()
	ctx = sessionCtx
	publicKey, privateKeyPath, err := sshtransport.GenerateGuestKeypair(s.cfg.GuestKeyDir, vmName)
	if err != nil {
		io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	defer sshtransport.RemoveGuestKeypair(privateKeyPath)
	target, err := s.provisionSession(ctx, vmName, policyName, policyArgs, publicKey, usePolicy)
	if err != nil {
		io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	defer func() {
		if !truthy(lc["LC_KEEP_VM"]) {
			releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ReleaseTimeout)
			defer releaseCancel()
			if err := s.releaseSession(releaseCtx, target); err != nil {
				s.log.Warn("requester_session_release_failed", "vm", target.VMName, "err", err)
			}
		}
	}()
	ticket, err := ValidateTicket(target.Address)
	if err != nil {
		io.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	provider, err := sshtransport.NewGuestSessionProvider(s.guestDialer(), s.log)
	if err != nil {
		io.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	if !provider.PollReady(ctx, privateKeyPath, ticket, s.cfg.VMReadyTimeout, target.HostKeys) {
		io.WriteErr([]byte("guest session failed: the guest ssh did not answer\n"))
		io.Exit(1)
		return errors.New("the guest ssh did not answer")
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

func (s *Service) heldSession(key string) *heldSession {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	held := s.heldSessions[key]
	if held == nil {
		held = &heldSession{}
		s.heldSessions[key] = held
	}
	return held
}

func (s *Service) armHeldSession(key string, held *heldSession) {
	held.mu.Lock()
	if held.timer != nil {
		held.timer.Stop()
	}
	held.generation++
	generation := held.generation
	held.timer = time.AfterFunc(s.cfg.SessionIdle, func() {
		s.releaseHeldSession(key, held, generation, "idle")
	})
	held.mu.Unlock()
}

func (s *Service) releaseHeldSession(key string, held *heldSession, generation uint64, reason string) {
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
	s.heldMu.Lock()
	if s.heldSessions[key] == held {
		delete(s.heldSessions, key)
	}
	s.heldMu.Unlock()
	privateKeyPath := held.privateKeyPath
	contract := held.contract
	held.target = nil
	held.privateKeyPath = ""
	held.contract = ""
	held.mu.Unlock()
	if contract != "" {
		s.releaseHeldContract(contract)
		s.log.Info("requester_held_session_released", "contract", contract, "reason", reason)
	}
	_ = sshtransport.RemoveGuestKeypair(privateKeyPath)
}

func (s *Service) releaseAllHeldSessions(reason string) {
	s.heldMu.Lock()
	keys := make([]string, 0, len(s.heldSessions))
	held := make([]*heldSession, 0, len(s.heldSessions))
	for key, session := range s.heldSessions {
		keys = append(keys, key)
		held = append(held, session)
	}
	s.heldMu.Unlock()
	for i := range keys {
		s.releaseHeldSession(keys[i], held[i], 0, reason)
	}
}

func (s *Service) runHeldCommand(ctx context.Context, account sshtransport.AuthorizedAccount, name, command string, lc map[string]string, io sshtransport.CommandIO) error {
	policyName, policyArgs, usePolicy, err := s.sessionPolicy(lc)
	if err != nil {
		io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	vmName := strings.TrimSpace(lc["LC_VM_NAME"])
	if vmName == "" {
		vmName = s.generatedVMName()
	}
	if !vmNamePattern.MatchString(vmName) {
		err := fmt.Errorf("LC_VM_NAME %q is not a DNS-label-shaped name", vmName)
		io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	command = renderExecCommand(command, lc)
	sessionCtx, cancel := context.WithTimeout(ctx, s.cfg.SessionCap)
	defer cancel()
	ctx = sessionCtx
	provider, err := sshtransport.NewGuestSessionProvider(s.guestDialer(), s.log)
	if err != nil {
		io.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	contractName := heldContractName(account.DID, name)
	provision := func() (*SessionTarget, string, error) {
		publicKey, privateKeyPath, err := sshtransport.GenerateGuestKeypair(s.cfg.GuestKeyDir, vmName)
		if err != nil {
			return nil, "", err
		}
		requestedPolicy := ""
		if usePolicy {
			requestedPolicy = policyName
		}
		target, err := s.provisionHeldContract(ctx, contractName, vmName, account.DID, publicKey, requestedPolicy, policyArgs)
		if err != nil {
			s.releaseHeldContract(contractName)
			_ = sshtransport.RemoveGuestKeypair(privateKeyPath)
			return nil, "", err
		}
		return target, privateKeyPath, nil
	}
	key := account.DID + "\x00" + name
	held := s.heldSession(key)
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
			io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
			io.Exit(1)
			return err
		}
		contract = contractName
		held.vmName, held.privateKeyPath, held.target, held.contract = vmName, privateKeyPath, target, contract
	}
	ready := false
	if ticket, ticketErr := ValidateTicket(target.Address); ticketErr == nil {
		ready = provider.PollReady(ctx, privateKeyPath, ticket, s.cfg.VMReadyTimeout, target.HostKeys)
	}
	if !ready && reused {
		s.log.Warn("requester_held_session_lost", "contract", contract)
		held.target = nil
		held.privateKeyPath = ""
		held.contract = ""
		s.releaseHeldSessionTarget(contract, privateKeyPath)
		target, privateKeyPath, err = provision()
		if err != nil {
			held.mu.Unlock()
			io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
			io.Exit(1)
			return err
		}
		contract = contractName
		held.vmName, held.privateKeyPath, held.target, held.contract = vmName, privateKeyPath, target, contract
		fresh = true
		ticket, ticketErr := ValidateTicket(target.Address)
		if ticketErr != nil {
			held.mu.Unlock()
			io.WriteErr([]byte("guest session failed: " + ticketErr.Error() + "\n"))
			io.Exit(1)
			return ticketErr
		}
		ready = provider.PollReady(ctx, privateKeyPath, ticket, s.cfg.VMReadyTimeout, target.HostKeys)
	}
	if !ready {
		if fresh {
			held.target = nil
			held.privateKeyPath = ""
			held.contract = ""
			s.releaseHeldSessionTarget(contract, privateKeyPath)
		}
		held.mu.Unlock()
		io.WriteErr([]byte("guest session failed: the guest ssh did not answer\n"))
		io.Exit(1)
		return errors.New("the guest ssh did not answer")
	}
	ticket, err := ValidateTicket(target.Address)
	if err != nil {
		if fresh {
			held.target = nil
			held.privateKeyPath = ""
			held.contract = ""
			s.releaseHeldSessionTarget(contract, privateKeyPath)
		}
		held.mu.Unlock()
		io.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		io.Exit(1)
		return err
	}
	code := provider.RunSession(ctx, privateKeyPath, ticket, command, io, target.HostKeys)
	held.mu.Unlock()
	s.armHeldSession(key, held)
	io.Exit(code)
	return nil
}

func heldContractName(did, name string) string {
	sum := sha256.Sum256([]byte(did + "\x00" + name))
	return "ssh-" + hex.EncodeToString(sum[:10])
}

const sessionRKeyAlphabet = "234567abcdefghijklmnopqrstuvwxyz"

func sessionRKey(label string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(label))
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	sum := h.Sum(nil)
	out := make([]byte, 13)
	for i := range out {
		out[i] = sessionRKeyAlphabet[int(sum[i])%len(sessionRKeyAlphabet)]
	}
	return string(out)
}

func (s *Service) existingSignedRecord(ctx context.Context, collection, rkey string) (market.SignedRecord, bool, error) {
	record, err := s.repo.GetRecord(ctx, s.cfg.RepoDID, collection, rkey)
	if err != nil {
		if errors.Is(err, market.ErrNotFound) {
			return market.SignedRecord{}, false, nil
		}
		return market.SignedRecord{}, false, err
	}
	return market.SignedRecord{URI: record.URI, CID: record.CID, Record: record.Value}, true, nil
}

func (s *Service) existingOrPutRecord(ctx context.Context, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	existing, found, err := s.existingSignedRecord(ctx, collection, rkey)
	if err != nil {
		return market.StrongRef{}, err
	}
	if found {
		return market.NewStrongRef(existing.URI, existing.CID), nil
	}
	return s.repo.PutRecord(ctx, s.cfg.RepoDID, collection, rkey, record)
}

func contractTarget(contract *v1alpha1.ComputeContract) (*SessionTarget, bool, error) {
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
		VMName:  state.VMName,
		Address: ticket,
		HostKeys: sshtransport.GuestHostKeys{
			Alias: hostKeyAlias(state.Receipt.URI),
			Keys:  keys,
		},
	}, true, nil
}

func (s *Service) provisionHeldContract(ctx context.Context, name, vmName, did, publicKey, policyName string, policyArgs map[string]string) (*SessionTarget, error) {
	args := ""
	if len(policyArgs) > 0 {
		raw, err := json.Marshal(policyArgs)
		if err != nil {
			return nil, err
		}
		args = string(raw)
	}
	deadline := time.Now().Add(s.cfg.BidWindow + s.cfg.ReceiptTimeout + s.cfg.VMReadyTimeout)
	for {
		existing, err := s.cfg.Contracts.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			break
		}
		if existing.DeletionTimestamp == nil {
			if err := s.cfg.Contracts.Delete(ctx, name); err != nil {
				return nil, err
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the previous session contract was not removed in time")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	contract := &v1alpha1.ComputeContract{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeContract"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
			v1alpha1.AnnotationTTLAfterFinished: "8d",
		}},
		Spec: v1alpha1.ComputeContractSpec{
			VMName:            vmName,
			SshAuthorizedKeys: []string{publicKey},
			Policy:            policyName,
			PolicyArgs:        args,
			OwnerDid:          did,
		},
	}
	for {
		err := s.cfg.Contracts.Create(ctx, contract)
		if err == nil {
			break
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the session contract was not created in time")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return s.waitHeldContract(ctx, name, deadline)
}

func (s *Service) waitHeldContract(ctx context.Context, name string, deadline time.Time) (*SessionTarget, error) {
	for {
		contract, err := s.cfg.Contracts.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if contract == nil {
			return nil, errors.New("the session contract disappeared")
		}
		if contract.Status.Phase == v1alpha1.ComputeContractFailed {
			return nil, fmt.Errorf("the session contract failed: %s", contract.Status.Message)
		}
		if contract.Status.Phase == v1alpha1.ComputeContractReleased {
			return nil, errors.New("the session contract was released before the guest was ready")
		}
		target, ready, err := contractTarget(contract)
		if err != nil {
			return nil, err
		}
		if ready {
			return target, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the guest did not report onNetwork and hostKeys in time")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Service) releaseHeldContract(name string) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ReleaseTimeout)
	defer cancel()
	if err := s.cfg.Contracts.Release(ctx, name); err != nil && !apierrors.IsNotFound(err) {
		s.log.Warn("requester_held_contract_release_failed", "contract", name, "err", err)
	}
}

func (s *Service) releaseHeldSessionTarget(contract, privateKeyPath string) {
	if contract != "" {
		s.releaseHeldContract(contract)
	}
	_ = sshtransport.RemoveGuestKeypair(privateKeyPath)
}

func (s *Service) guestDialer() sshtransport.Dialer {
	if s.cfg.IrohSocket != "" {
		return sshtransport.UnixSocketDialer{Socket: s.cfg.IrohSocket, Log: s.log}
	}
	path := s.cfg.DumbpipePath
	path = cmp.Or(path, "dumbpipe")
	return sshtransport.DumbpipeDialer{Path: path, Log: s.log}
}

func (s *Service) provisionSession(ctx context.Context, vmName, policyName string, policyArgs map[string]string, publicKey string, usePolicy bool) (*SessionTarget, error) {
	userData := defaultUserData([]string{publicKey})
	if s.cfg.GuestUserData != "" {
		userData = s.cfg.GuestUserData
	}
	var err error
	userData, err = guestseed.WithIrohTransport(userData, []string{publicKey}, s.cfg.GuestHostAliases)
	if err != nil {
		return nil, fmt.Errorf("the guest iroh transport: %w", err)
	}
	userData = withGuestCACerts(userData, s.cfg.GuestCAPEM)
	vmRKey := sessionRKey("vm", s.cfg.RepoDID, vmName)
	payload, found, err := s.existingSignedRecord(ctx, market.ComputeVMNSID, vmRKey)
	if err != nil {
		return nil, err
	}
	if !found {
		payload, err = market.CreateSignedRecordAt(ctx, s.repo, s.cfg.RepoDID, market.ComputeVMNSID, vmRKey, map[string]any{
			"$type":     market.ComputeVMNSID,
			"role":      vmName,
			"disk":      DefaultComputeDisk,
			"user_data": userData,
			"createdAt": s.nowISO(),
		}, s.signer)
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
		record := BuildPolicyRecord(policyName, workflow, workflowText, s.cfg.MarketDID, policyArgs, s.nowISO())
		created, err := s.existingOrPutRecord(ctx, PolicyGhaLiteNSID, sessionRKey("policy", s.cfg.RepoDID, vmName, policyName), record)
		if err != nil {
			return nil, err
		}
		policyRef = &created
	}
	options := market.RFPOptions{
		Payload:   market.RecordRef{URI: payload.URI, CID: payload.CID},
		SubmitBid: s.submitBidRef(),
		Domain:    "compute",
		CreatedAt: s.nowISO(),
	}
	if policyRef != nil {
		options.Policies = []market.RecordRef{{URI: policyRef.URI, CID: policyRef.CID}}
	}
	rfpRKey := sessionRKey("rfp", s.cfg.RepoDID, vmName)
	rfp, found, err := s.existingSignedRecord(ctx, market.RFPNSID, rfpRKey)
	if err != nil {
		return nil, err
	}
	if !found {
		rfp, err = market.CreateRFPAt(ctx, s.repo, s.cfg.RepoDID, rfpRKey, options, s.signer)
		if err != nil {
			return nil, err
		}
	}
	bidders, err := s.market.Discover(ctx, s.now())
	if err != nil {
		s.log.Warn("requester_session_discovery_failed", "err", err)
	}
	s.rememberBidders(bidders)
	winner, winnerBidder, err := s.sessionWinner(ctx, rfp, bidders, usePolicy, PolicyWorkflowName(policyName))
	if err != nil {
		return nil, err
	}
	acceptRKey := sessionRKey("accept", s.cfg.RepoDID, vmName, rfp.URI, rfp.CID, winner.URI, winner.CID)
	accept, found, err := s.existingSignedRecord(ctx, market.AcceptNSID, acceptRKey)
	if err != nil {
		return nil, err
	}
	if !found {
		acceptRecord, err := market.BuildAccept(market.AcceptOptions{
			RFP:         market.RecordRef{URI: rfp.URI, CID: rfp.CID},
			Bid:         market.RecordRef{URI: winner.URI, CID: winner.CID},
			SubmitEvent: s.submitEventRef(),
			CreatedAt:   s.nowISO(),
		})
		if err != nil {
			return nil, err
		}
		accept, err = market.CreateSignedRecordAt(ctx, s.repo, s.cfg.RepoDID, market.AcceptNSID, acceptRKey, acceptRecord, s.signer)
		if err != nil {
			return nil, err
		}
	}
	receipt, err := s.submitAcceptAndWait(ctx, winnerBidder, accept.URI, accept.CID, winner.DID)
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, errors.New("no receipt arrived within " + s.cfg.ReceiptTimeout.String())
	}
	verification := market.VerifyReceipt(ctx, receipt.Value, receipt.URI, accept.Record, accept.URI, market.VerifyRecordOptions{
		KeysForDID:  market.NewDirectoryKeyLister(s.directory),
		KeyResolver: market.DefaultKeyResolver,
	})
	if !verification.OK {
		return nil, errors.New("the receipt signature or accept binding is invalid")
	}
	state := &v1alpha1.NodeClaimContract{
		VMName:       vmName,
		VMURI:        payload.URI,
		RFPURI:       rfp.URI,
		AcceptURI:    accept.URI,
		ReceiptURI:   receipt.URI,
		ReceiptCid:   receipt.CID,
		RequesterDid: s.callbackDID(),
		MarketDid:    s.cfg.MarketDID,
		Payload:      &commonv1.StrongRef{URI: payload.URI, CID: payload.CID},
		RFP:          &commonv1.StrongRef{URI: rfp.URI, CID: rfp.CID},
		Bid:          &commonv1.StrongRef{URI: winner.URI, CID: winner.CID},
		Winner:       &v1alpha1.ContractWinner{DID: winner.DID},
		Accept:       &commonv1.StrongRef{URI: accept.URI, CID: accept.CID},
		Receipt:      &commonv1.StrongRef{URI: receipt.URI, CID: receipt.CID},
		ReceiptOk:    new(true),
		Phase:        v1alpha1.ContractReceipt,
	}
	if policyRef != nil {
		state.Policy = &commonv1.StrongRef{URI: policyRef.URI, CID: policyRef.CID}
	}
	if submitEvent := stringOf(receipt.Value["submitEvent"]); submitEvent != "" {
		state.SubmitEventRef = submitEvent
	}
	address, hostKeys, termination, err := s.sessionGuest(ctx, receipt.URI, winner.DID)
	if err != nil {
		return nil, err
	}
	if termination != nil {
		return nil, fmt.Errorf("the bidder ended the contract: %s", termination.Reason)
	}
	state.VMAddress = address
	state.Phase = v1alpha1.ContractNetwork
	target := &SessionTarget{
		VMName:  vmName,
		Address: address,
		HostKeys: sshtransport.GuestHostKeys{
			Alias: hostKeyAlias(receipt.URI),
			Keys:  hostKeyLines(hostKeys),
		},
		Bidder: winnerBidder,
		State:  state,
	}
	return target, nil
}

func (s *Service) sessionWinner(ctx context.Context, rfp market.SignedRecord, bidders []Bidder, usePolicy bool, workflow string) (*BidRecord, Bidder, error) {
	if s.outboundXRPC() {
		for _, bidder := range bidders {
			if bidder.Endpoint == "" {
				continue
			}
			timeout := max(s.cfg.BidWindow, 5*time.Second)
			cctx, cancel := context.WithTimeout(ctx, timeout)
			bids, err := s.market.SubmitRFP(cctx, bidder, SubmitRFPRequest{URI: rfp.URI, CID: rfp.CID})
			cancel()
			if err != nil {
				s.log.Warn("requester_session_rfp_failed", "bidder", bidder.DID, "err", err)
				continue
			}
			for _, bid := range bids {
				s.queueBid(rfp.URI, bid)
			}
		}
	}
	deadline := time.Now().Add(s.cfg.BidWindow)
	byURI := map[string]BidRecord{}
	bidderByDID := map[string]Bidder{}
	for _, bidder := range bidders {
		if _, ok := bidderByDID[bidder.DID]; !ok {
			bidderByDID[bidder.DID] = bidder
		}
	}
	add := func(bids []BidRecord) {
		for _, bid := range bids {
			byURI[bid.URI] = bid
		}
	}
	for {
		add(s.takeBids(rfp.URI))
		if best := s.sessionBestBid(ctx, byURI, usePolicy, workflow); best != nil {
			bidder, ok := bidderByDID[best.DID]
			if ok {
				return best, bidder, nil
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, Bidder{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	add(s.takeBids(rfp.URI))
	if best := s.sessionBestBid(ctx, byURI, usePolicy, workflow); best != nil {
		if bidder, ok := bidderByDID[best.DID]; ok {
			return best, bidder, nil
		}
	}
	if len(byURI) == 0 {
		return nil, Bidder{}, errors.New("no bids arrived within " + s.cfg.BidWindow.String())
	}
	return nil, Bidder{}, errors.New("every bid was denied by policy")
}

func (s *Service) sessionBestBid(ctx context.Context, bids map[string]BidRecord, usePolicy bool, workflow string) *BidRecord {
	var best *BidRecord
	for _, bid := range bids {
		candidate := bid
		if err := s.classifyBid(ctx, &candidate); err != nil {
			continue
		}
		if !s.bidAllowedWithPolicy(ctx, candidate, usePolicy, workflow) {
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
		decision := s.policy.Evaluate(ctx, PolicyInput{
			SelfDid:          s.cfg.MarketDID,
			SubjectDid:       best.DID,
			CounterpartyDid:  best.DID,
			RootRequesterDid: s.cfg.MarketDID,
			Bid:              &PolicyStrongRef{URI: best.URI, CID: best.CID},
		})
		if !decision.Allow {
			return nil
		}
	}
	return best
}

func (s *Service) sessionGuest(ctx context.Context, receiptURI, bidderDID string) (string, []v1alpha1.ComputeVMHostKey, *v1alpha1.ContractTermination, error) {
	events, pending, closeEvents := s.registerEventSink(receiptURI)
	defer closeEvents()
	seen := map[string]bool{}
	address := ""
	var hostKeys []v1alpha1.ComputeVMHostKey
	var termination *v1alpha1.ContractTermination
	handle := func(event MarketEvent) bool {
		if guestEventSeen(seen, event) {
			return false
		}
		parsed, err := parseGuestEvent(ctx, s.directory, s.badgeBlueBinding(), event, receiptURI)
		if err != nil {
			s.log.Warn("requester_session_event_refused", "uri", event.Event.URI, "err", err)
			return false
		}
		switch parsed.Kind {
		case market.ComputeEventsVMOnNetworkNSID:
			address = cmp.Or(address, parsed.Address)
		case market.ComputeEventsVMHostKeysNSID:
			hostKeys = parsed.HostKeys
		case market.ComputeEventsVMTerminatedNSID:
			if termination != nil && sameTermination(termination, parsed.Termination) {
				return true
			}
			termination = parsed.Termination
			return true
		}
		return address != "" && len(hostKeys) > 0
	}
	if slices.ContainsFunc(pending, handle) {

	}
	deadline := time.Now().Add(s.cfg.VMReadyTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if termination != nil {
			return "", nil, termination, nil
		}
		if address != "" && len(hostKeys) > 0 {
			return address, hostKeys, nil, nil
		}
		if time.Now().After(deadline) {
			return "", nil, nil, errors.New("the guest did not report onNetwork and hostKeys within " + s.cfg.VMReadyTimeout.String())
		}
		select {
		case <-ctx.Done():
			return "", nil, nil, ctx.Err()
		case event := <-events:
			if handle(event) {
				continue
			}
		case <-ticker.C:
			found, err := s.market.FindEvents(ctx, bidderDID, receiptURI)
			if err != nil {
				continue
			}
			for _, event := range found {
				handle(event)
			}
		}
	}
}

func (s *Service) releaseSession(ctx context.Context, target *SessionTarget) error {
	if target == nil || target.State == nil || target.State.Receipt == nil {
		return nil
	}
	state := target.State
	payloadRKey := sessionRKey("delete", state.Receipt.URI)
	payload, found, err := s.existingSignedRecord(ctx, market.ComputeEventsVMDeleteNSID, payloadRKey)
	if err != nil {
		return err
	}
	if !found {
		payload, err = market.CreateSignedRecordAt(ctx, s.repo, s.cfg.RepoDID, market.ComputeEventsVMDeleteNSID, payloadRKey, map[string]any{
			"$type":     market.ComputeEventsVMDeleteNSID,
			"reason":    "session_ended",
			"createdAt": s.nowISO(),
		}, s.signer)
		if err != nil {
			return err
		}
	}
	eventRKey := sessionRKey("event", state.Receipt.URI)
	envelope, found, err := s.existingSignedRecord(ctx, market.EventNSID, eventRKey)
	if err != nil {
		return err
	}
	if !found {
		envelope, err = market.PutEvent(ctx, s.repo, s.cfg.RepoDID, eventRKey, market.EventOptions{
			Receipt:   market.RecordRef{URI: state.Receipt.URI, CID: state.Receipt.CID},
			Payload:   market.RecordRef{URI: payload.URI, CID: payload.CID},
			CreatedAt: s.nowISO(),
		}, s.signer)
		if err != nil {
			return err
		}
	}
	release := &v1alpha1.ContractRelease{
		Event:   commonv1.StrongRef{URI: envelope.URI, CID: envelope.CID},
		Payload: commonv1.StrongRef{URI: payload.URI, CID: payload.CID},
	}
	state.Release = release
	if err := s.persistSessionRelease(ctx, state.Receipt.URI, release); err != nil {
		return err
	}
	if !s.outboundXRPC() || target.Bidder.Endpoint == "" {
		release.OK = true
		if err := s.persistSessionRelease(ctx, state.Receipt.URI, release); err != nil {
			s.log.Warn("requester_session_release_status_failed", "receipt", state.Receipt.URI, "err", err)
		}
		return nil
	}
	var lastErr error
	for attempt := 1; ; attempt++ {
		status, err := s.market.SubmitEvent(ctx, target.Bidder, SubmitEventRequest{URI: envelope.URI, CID: envelope.CID, Record: envelope.Record})
		if err == nil && status >= 200 && status <= 299 {
			release.OK = true
			release.Status = &status
			if err := s.persistSessionRelease(ctx, state.Receipt.URI, release); err != nil {
				s.log.Warn("requester_session_release_status_failed", "receipt", state.Receipt.URI, "err", err)
			}
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("vm.delete answered HTTP %d", status)
			release.Status = &status
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(s.releaseRetryDelay(attempt)):
		}
	}
}

func (s *Service) persistSessionRelease(ctx context.Context, receiptURI string, release *v1alpha1.ContractRelease) error {
	if s.cfg.Contracts == nil || receiptURI == "" {
		return nil
	}
	contracts, err := s.cfg.Contracts.List(ctx)
	if err != nil {
		s.log.Warn("requester_session_release_lookup_failed", "receipt", receiptURI, "err", err)
		return nil
	}
	for i := range contracts {
		state := contracts[i].Status.Contract
		if state == nil || state.Receipt == nil || state.Receipt.URI != receiptURI {
			continue
		}
		status := v1alpha1.ComputeContractStatus{Contract: &v1alpha1.NodeClaimContract{Release: release}}
		if err := s.cfg.Contracts.PatchStatus(ctx, contracts[i].Name, status); err != nil {
			return fmt.Errorf("persist the release refs on contract %s: %w", contracts[i].Name, err)
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
	sortStrings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(env[key]))
		b.WriteString("; ")
	}
	b.WriteString(command)
	return b.String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
