package requester

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
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
	policyName := strings.TrimSpace(lc["LC_POLICY"])
	if policyName == "" {
		policyName = s.cfg.PolicyWorkflow
	}
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
			io.WriteErr([]byte("provisioning failed: LC_POLICY_ARGS " + err.Error() + "\n"))
			io.Exit(1)
			return err
		}
		policyArgs = parsed
	}
	for key, value := range s.cfg.PolicyArgs {
		if _, ok := policyArgs[key]; !ok {
			policyArgs[key] = value
		}
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
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), s.cfg.ReleaseTimeout)
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

func (s *Service) guestDialer() sshtransport.Dialer {
	if s.cfg.IrohSocket != "" {
		return sshtransport.UnixSocketDialer{Socket: s.cfg.IrohSocket, Log: s.log}
	}
	path := s.cfg.DumbpipePath
	if path == "" {
		path = "dumbpipe"
	}
	return sshtransport.DumbpipeDialer{Path: path, Log: s.log}
}

func (s *Service) provisionSession(ctx context.Context, vmName, policyName string, policyArgs map[string]string, publicKey string, usePolicy bool) (*SessionTarget, error) {
	userData := defaultUserData([]string{publicKey})
	if s.cfg.GuestUserData != "" {
		userData = s.cfg.GuestUserData
	}
	payload, err := s.repo.CreateRecord(ctx, s.cfg.RepoDID, market.ComputeVMNSID, map[string]any{
		"$type":     market.ComputeVMNSID,
		"role":      vmName,
		"disk":      DefaultComputeDisk,
		"user_data": userData,
		"createdAt": s.nowISO(),
	})
	if err != nil {
		return nil, err
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
		created, err := s.repo.CreateRecord(ctx, s.cfg.RepoDID, PolicyGhaLiteNSID, record)
		if err != nil {
			return nil, err
		}
		policyRef = &created
	}
	options := market.RFPOptions{
		Payload:   market.RecordRef{URI: payload.URI, CID: payload.CID},
		SubmitBid: s.callbackDID() + "#" + defaultMarketServiceID,
		Domain:    "compute",
		CreatedAt: s.nowISO(),
	}
	if policyRef != nil {
		options.Policies = []market.RecordRef{{URI: policyRef.URI, CID: policyRef.CID}}
	}
	rfp, err := market.CreateRFP(ctx, s.repo, s.cfg.RepoDID, options, s.signer)
	if err != nil {
		return nil, err
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
	acceptRecord, err := market.BuildAccept(market.AcceptOptions{
		RFP:         market.RecordRef{URI: rfp.URI, CID: rfp.CID},
		Bid:         market.RecordRef{URI: winner.URI, CID: winner.CID},
		SubmitEvent: s.callbackDID() + "#" + defaultComputeEventServiceID,
		CreatedAt:   s.nowISO(),
	})
	if err != nil {
		return nil, err
	}
	accept, err := market.CreateSignedRecord(ctx, s.repo, s.cfg.RepoDID, market.AcceptNSID, acceptRecord, s.signer)
	if err != nil {
		return nil, err
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
		VMUri:        payload.URI,
		RFPUri:       rfp.URI,
		AcceptUri:    accept.URI,
		ReceiptUri:   receipt.URI,
		ReceiptCid:   receipt.CID,
		RequesterDid: s.callbackDID(),
		MarketDid:    s.cfg.MarketDID,
		Payload:      &v1alpha1.RecordRef{URI: payload.URI, CID: payload.CID},
		RFP:          &v1alpha1.RecordRef{URI: rfp.URI, CID: rfp.CID},
		Bid:          &v1alpha1.RecordRef{URI: winner.URI, CID: winner.CID},
		Winner:       &v1alpha1.ContractWinner{DID: winner.DID},
		Accept:       &v1alpha1.RecordRef{URI: accept.URI, CID: accept.CID},
		Receipt:      &v1alpha1.RecordRef{URI: receipt.URI, CID: receipt.CID},
		ReceiptOk:    boolPtr(true),
		Phase:        v1alpha1.ContractReceipt,
	}
	if policyRef != nil {
		state.Policy = &v1alpha1.RecordRef{URI: policyRef.URI, CID: policyRef.CID}
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
	for _, bidder := range bidders {
		timeout := s.cfg.BidWindow
		if timeout < 5*time.Second {
			timeout = 5 * time.Second
		}
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
		if seen[event.Event.URI] {
			return false
		}
		seen[event.Event.URI] = true
		parsed, err := parseGuestEvent(ctx, s.directory, s.badgeBlueBinding(), event, receiptURI)
		if err != nil {
			s.log.Warn("requester_session_event_refused", "uri", event.Event.URI, "err", err)
			return false
		}
		switch parsed.Kind {
		case market.ComputeEventsVMOnNetworkNSID:
			if address == "" {
				address = parsed.Address
			}
		case market.ComputeEventsVMHostKeysNSID:
			hostKeys = parsed.HostKeys
		case market.ComputeEventsVMTerminatedNSID:
			termination = parsed.Termination
			return true
		}
		return address != "" && len(hostKeys) > 0
	}
	for _, event := range pending {
		if handle(event) {
			break
		}
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
	payload, err := market.CreateSignedRecord(ctx, s.repo, s.cfg.RepoDID, market.ComputeEventsVMDeleteNSID, map[string]any{
		"$type":     market.ComputeEventsVMDeleteNSID,
		"reason":    "session_ended",
		"createdAt": s.nowISO(),
	}, s.signer)
	if err != nil {
		return err
	}
	envelope, err := market.CreateEvent(ctx, s.repo, s.cfg.RepoDID, market.EventOptions{
		Receipt:   market.RecordRef{URI: state.Receipt.URI, CID: state.Receipt.CID},
		Payload:   market.RecordRef{URI: payload.URI, CID: payload.CID},
		CreatedAt: s.nowISO(),
	}, s.signer)
	if err != nil {
		return err
	}
	status, err := s.market.SubmitEvent(ctx, target.Bidder, SubmitEventRequest{URI: envelope.URI, CID: envelope.CID, Record: envelope.Record})
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("vm.delete answered HTTP %d", status)
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
