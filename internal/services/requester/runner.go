package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
)

type contractRunner struct {
	svc *Service

	name string

	mu sync.Mutex

	contract v1alpha1.ComputeContract

	state *v1alpha1.NodeClaimContract

	bids []v1alpha1.ContractBid

	chosen *v1alpha1.ContractChosen

	release *v1alpha1.ComputeContractRelease

	resumed bool

	releaseCh chan struct{}

	releaseOnce sync.Once

	done chan struct{}

	nodeJoin *v1alpha1.ComputeContractNodeJoin

	grant *v1alpha1.NodeClaimGrant

	secretsURL string

	secretsFetchedAt string

	secrets *secretsState

	rbacRKey string
}

func newContractRunner(s *Service, contract v1alpha1.ComputeContract) *contractRunner {
	r := &contractRunner{
		svc:       s,
		name:      contract.Name,
		contract:  contract,
		bids:      append([]v1alpha1.ContractBid(nil), contract.Status.Bids...),
		releaseCh: make(chan struct{}),
		done:      make(chan struct{}),
	}
	if contract.Status.Chosen != nil {
		chosen := *contract.Status.Chosen
		r.chosen = &chosen
	}
	if contract.Status.Release != nil {
		release := *contract.Status.Release
		r.release = &release
	}
	if contract.Status.Contract != nil {
		r.state = cloneNodeClaimContract(contract.Status.Contract)
		r.resumed = recordedStage(contract.Status.Contract)
	}
	r.nodeJoin = contract.Spec.NodeJoin
	r.secretsURL = contract.Status.SecretsUrl
	r.secretsFetchedAt = contract.Status.SecretsFetchedAt
	if contract.Status.Grant != nil {
		grant := *contract.Status.Grant
		r.grant = &grant
	}
	return r
}

func recordedStage(state *v1alpha1.NodeClaimContract) bool {
	if state == nil {
		return false
	}
	return state.VMUri != "" || state.RFPUri != "" || state.AcceptUri != "" || state.ReceiptUri != "" || state.Release != nil
}

func (r *contractRunner) Update(contract *v1alpha1.ComputeContract) {
	if contract == nil {
		return
	}
	r.mu.Lock()
	r.contract = *contract
	r.mu.Unlock()
	if contract.DeletionTimestamp != nil || contract.Spec.Release {
		r.requestRelease()
	}
}

func (r *contractRunner) releaseStatus() *v1alpha1.ComputeContractRelease {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.release == nil {
		return nil
	}
	out := *r.release
	return &out
}

func (r *contractRunner) setRelease(release *v1alpha1.ComputeContractRelease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if release == nil {
		r.release = nil
		return
	}
	out := *release
	r.release = &out
}

func (r *contractRunner) requestRelease() {
	r.releaseOnce.Do(func() { close(r.releaseCh) })
}

func (r *contractRunner) released() bool {
	select {
	case <-r.releaseCh:
		return true
	default:
		return false
	}
}

func (r *contractRunner) snapshot() v1alpha1.ComputeContract {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.contract
}

func (r *contractRunner) stateCopy() *v1alpha1.NodeClaimContract {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		return nil
	}
	out := new(v1alpha1.NodeClaimContract)
	r.state.DeepCopyInto(out)
	return out
}

func (r *contractRunner) setState(state *v1alpha1.NodeClaimContract) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

func (r *contractRunner) log() *slog.Logger {
	return r.svc.log.With("contract", r.name)
}

func (r *contractRunner) Run(ctx context.Context) {
	defer close(r.done)
	defer r.cleanup()
	contract := r.snapshot()
	if contract.Spec.Adopt {
		r.runAdopt(ctx, contract)
		return
	}
	if contract.Spec.SecretsRef != nil && contract.Status.SecretsFetchedAt == "" {
		if err := r.ensureSecrets(ctx, contract); err != nil {
			r.recordFailure(ctx, r.stateCopy(), err, 1)
			return
		}
	}
	state := r.stateCopy()
	if state != nil && state.Receipt != nil {
		if err := r.resumeReceipt(ctx, state); err != nil && r.svc.ctx.Err() == nil {
			r.recordFailure(ctx, r.stateCopy(), err, 1)
		}
		return
	}
	if r.released() {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, r.currentBids(), r.currentChosen(), false, "released", 1)
		return
	}
	for attempt := 0; ; attempt++ {
		retry, err := r.requestOnce(ctx)
		if err == nil {
			return
		}
		if r.svc.ctx.Err() != nil {
			r.log().Info("requester_contract_left_for_restart", "err", err)
			return
		}
		if retry && !r.released() {
			r.log().Info("requester_contract_capacity_retry", "delay", CapacityRetryDelay)
			r.writeStatus(ctx, v1alpha1.ComputeContractPending, "every bidder declined the RFP with reason capacity; asking again in 60s", r.stateCopy(), nil, nil, true, "atCapacity", int32(attempt+1))
			select {
			case <-ctx.Done():
				return
			case <-r.releaseCh:
				r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", r.stateCopy(), nil, nil, false, "released", int32(attempt+1))
				return
			case <-time.After(CapacityRetryDelay):
			}
			r.setState(nil)
			continue
		}
		if r.released() {
			st := r.stateCopy()
			if st == nil || st.Receipt == nil {
				r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", st, r.currentBids(), r.currentChosen(), false, "released", int32(attempt+1))
				return
			}
		}
		r.recordFailure(ctx, r.stateCopy(), err, int32(attempt+1))
		return
	}
}

func (r *contractRunner) recordFailure(ctx context.Context, state *v1alpha1.NodeClaimContract, err error, attempts int32) {
	if state != nil && state.Release != nil && !state.Release.OK {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "the release did not finish: "+err.Error(), state, r.currentBids(), r.currentChosen(), false, "releaseFailed", attempts)
		return
	}
	if r.released() && (state == nil || state.Receipt == nil) {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, r.currentBids(), r.currentChosen(), false, "released", attempts)
		return
	}
	r.log().Warn("requester_contract_failed", "err", err)
	r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "the contract flow failed: "+err.Error(), state, r.currentBids(), r.currentChosen(), false, "failed", attempts)
	if state != nil && state.Receipt != nil && state.Termination == nil {
		if waitErr := r.waitForRelease(ctx, state, state.Receipt.URI, winnerDID(state)); waitErr != nil {
			r.log().Info("requester_contract_failed_hold_ended", "err", waitErr)
		}
	}
}

func (r *contractRunner) requestOnce(ctx context.Context) (bool, error) {
	contract := r.snapshot()
	vmName := strings.TrimSpace(contract.Spec.VMName)
	if vmName == "" {
		vmName = r.svc.generatedVMName()
	}
	authorizedKeys := append([]string(nil), contract.Spec.SshAuthorizedKeys...)
	authorizedKeys = append(authorizedKeys, r.svc.cfg.GuestAuthorizedKeys...)
	userData := contract.Spec.UserData
	if userData == "" {
		userData = r.svc.cfg.GuestUserData
	}
	if userData == "" {
		userData = defaultUserData(authorizedKeys)
	}
	var err error
	userData, err = guestseed.WithIrohTransport(userData, authorizedKeys, r.svc.cfg.GuestHostAliases)
	if err != nil {
		return false, fmt.Errorf("the guest iroh transport: %w", err)
	}
	userData = withGuestCACerts(userData, r.svc.cfg.GuestCAPEM)
	userData, err = r.contractModules(ctx, userData, contract)
	if err != nil {
		return false, err
	}
	policyName := strings.TrimSpace(contract.Spec.Policy)
	policyArgs, err := parsePolicyArgs(contract.Spec.PolicyArgs)
	if err != nil {
		return false, err
	}
	state := r.stateCopy()
	if state == nil {
		state = &v1alpha1.NodeClaimContract{
			Version:    intPtr(1),
			Phase:      v1alpha1.ContractRequested,
			Timestamps: &v1alpha1.ContractTimestamps{Requested: r.svc.nowISO()},
		}
	}
	if state.VMName == "" {
		state.VMName = vmName
	}
	if state.RequesterDid == "" {
		state.RequesterDid = r.svc.callbackDID()
	}
	if state.MarketDid == "" {
		state.MarketDid = r.svc.cfg.MarketDID
	}
	if state.Version == nil {
		state.Version = intPtr(1)
	}
	if state.Timestamps == nil {
		state.Timestamps = &v1alpha1.ContractTimestamps{Requested: r.svc.nowISO()}
	}
	if state.Phase == "" {
		state.Phase = v1alpha1.ContractRequested
	}
	r.setState(state)
	r.step(ctx, "preparing", 1)

	if state.Payload == nil || state.Payload.URI == "" {
		payloadRecord := map[string]any{
			"$type":     market.ComputeVMNSID,
			"role":      vmName,
			"cpus":      DefaultComputeCPUs,
			"mem":       DefaultComputeMem,
			"disk":      DefaultComputeDisk,
			"user_data": userData,
			"createdAt": r.svc.nowISO(),
		}
		payload, err := r.svc.repo.CreateRecord(ctx, r.svc.cfg.RepoDID, market.ComputeVMNSID, payloadRecord)
		if err != nil {
			return false, fmt.Errorf("create the compute.vm record: %w", err)
		}
		state.VMUri = payload.URI
		state.Payload = &v1alpha1.RecordRef{URI: payload.URI, CID: payload.CID}
		state.Phase = v1alpha1.ContractRFPSubmitted
	} else {
		state.VMUri = state.Payload.URI
	}

	r.step(ctx, "creatingRfp", 1)
	var policyRef *market.StrongRef
	policyWorkflow := ""
	if state.Policy != nil && state.Policy.URI != "" {
		policyRef = &market.StrongRef{Type: market.StrongRefNSID, URI: state.Policy.URI, CID: state.Policy.CID}
		if policyName != "" {
			policyWorkflow = PolicyWorkflowName(policyName)
			if _, ok := PolicyWorkflowText(policyName); !ok {
				return false, fmt.Errorf("policy %q names no bundled workflow", policyName)
			}
		}
	} else if policyName != "" {
		policyWorkflow = PolicyWorkflowName(policyName)
		workflowText, ok := PolicyWorkflowText(policyName)
		if !ok {
			return false, fmt.Errorf("policy %q names no bundled workflow", policyName)
		}
		policyRecord := BuildPolicyRecord(policyName, policyWorkflow, workflowText, r.svc.cfg.MarketDID, policyArgs, r.svc.nowISO())
		created, err := r.svc.repo.CreateRecord(ctx, r.svc.cfg.RepoDID, PolicyGhaLiteNSID, policyRecord)
		if err != nil {
			return false, fmt.Errorf("create the policy record: %w", err)
		}
		policyRef = &created
		state.Policy = &v1alpha1.RecordRef{URI: created.URI, CID: created.CID}
	}
	rfpURI, rfpCID := "", ""
	if state.RFP != nil && state.RFP.URI != "" {
		rfpURI, rfpCID = state.RFP.URI, state.RFP.CID
	} else {
		rfpOptions := market.RFPOptions{
			Payload:   market.RecordRef{URI: state.Payload.URI, CID: state.Payload.CID},
			SubmitBid: r.svc.callbackDID() + "#" + defaultMarketServiceID,
			Domain:    "compute",
			CreatedAt: r.svc.nowISO(),
		}
		if policyRef != nil {
			rfpOptions.Policies = []market.RecordRef{{URI: policyRef.URI, CID: policyRef.CID}}
		}
		rfp, err := market.CreateRFP(ctx, r.svc.repo, r.svc.cfg.RepoDID, rfpOptions, r.svc.signer)
		if err != nil {
			return false, fmt.Errorf("create the market.rfp record: %w", err)
		}
		state.RFPUri = rfp.URI
		state.RFP = &v1alpha1.RecordRef{URI: rfp.URI, CID: rfp.CID}
		state.Timestamps.RFPSubmitted = r.svc.nowISO()
		rfpURI, rfpCID = rfp.URI, rfp.CID
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the RFP is submitted; collecting bids", state, nil, nil, false, "collectingBids", 1)

	if state.Accept != nil && state.Accept.URI != "" {
		return r.resumeAccepted(ctx, state)
	}

	bidders, err := r.svc.market.Discover(ctx, r.svc.now())
	if err != nil {
		r.log().Warn("requester_bidder_discovery_failed", "err", err)
	}
	bidders = withoutDeniedBidders(bidders, r.snapshot().Spec.DenyBidderDids)
	r.svc.rememberBidders(bidders)
	responseBids := []BidRecord{}
	var submitWG sync.WaitGroup
	var submitMu sync.Mutex
	for _, bidder := range bidders {
		bidder := bidder
		submitWG.Add(1)
		go func() {
			defer submitWG.Done()
			timeout := r.svc.cfg.BidWindow
			if timeout < 5*time.Second {
				timeout = 5 * time.Second
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			bids, err := r.svc.market.SubmitRFP(cctx, bidder, SubmitRFPRequest{URI: rfpURI, CID: rfpCID})
			if err != nil {
				r.log().Warn("requester_submit_rfp_failed", "bidder", bidder.DID, "err", err)
				return
			}
			submitMu.Lock()
			responseBids = append(responseBids, bids...)
			submitMu.Unlock()
		}()
	}
	submitWG.Wait()

	usePolicy := policyRef != nil
	winner, allBids, retry, err := r.collectBids(ctx, rfpURI, responseBids, bidders, usePolicy, policyWorkflow)
	if err != nil {
		return false, err
	}
	if r.released() {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, allBids, nil, false, "released", 1)
		return false, nil
	}
	r.mu.Lock()
	r.bids = allBids
	r.mu.Unlock()
	if winner == nil {
		if retry {
			return true, nil
		}
		if len(allBids) == 0 {
			state.Outcome = v1alpha1.ContractOutcomeNoBids
			state.Error = fmt.Sprintf("no bids received within %s", r.svc.cfg.BidWindow)
			return false, errors.New(state.Error)
		}
		state.Outcome = v1alpha1.ContractOutcomeBidsDenied
		state.Error = fmt.Sprintf("all %d bids were denied by policy", len(allBids))
		return false, errors.New(state.Error)
	}
	if r.released() {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, allBids, nil, false, "released", 1)
		return false, nil
	}

	bidderByDID := map[string]Bidder{}
	for _, bidder := range bidders {
		if _, ok := bidderByDID[bidder.DID]; !ok {
			bidderByDID[bidder.DID] = bidder
		}
	}
	winnerBidder, ok := bidderByDID[winner.DID]
	if !ok {
		winnerBidder, ok = r.svc.bidderForBid(ctx, *winner)
	}
	if !ok {
		return false, fmt.Errorf("no endpoint is known for the winning bidder %s", winner.DID)
	}
	chosen := &v1alpha1.ContractChosen{
		URI:        winner.URI,
		Did:        winner.DID,
		Reason:     v1alpha1.ContractChosenBestWithinWindow,
		Considered: len(allBids),
	}
	if winner.Free {
		chosen.Reason = v1alpha1.ContractChosenFirstFree
	}
	if policyRef != nil && !winner.Free {
		decision := r.svc.policy.Evaluate(ctx, PolicyInput{
			SelfDid:          r.svc.cfg.MarketDID,
			SubjectDid:       winner.DID,
			CounterpartyDid:  winner.DID,
			RootRequesterDid: r.svc.cfg.MarketDID,
			RFP:              &PolicyStrongRef{URI: rfpURI, CID: rfpCID},
			Bid:              &PolicyStrongRef{URI: winner.URI, CID: winner.CID},
		})
		if !decision.Allow {
			state.Outcome = v1alpha1.ContractOutcomePolicyRejected
			state.Error = "winner rejected by policy: " + decision.Reason
			return false, errors.New(state.Error)
		}
		chosen.Reason = v1alpha1.ContractChosenPolicy
	}
	r.mu.Lock()
	r.chosen = chosen
	r.mu.Unlock()
	if err := r.prepareWorkloadGrant(ctx, state, winner); err != nil {
		return false, err
	}
	state.Phase = v1alpha1.ContractWinnerSelected
	state.Bid = &v1alpha1.RecordRef{URI: winner.URI, CID: winner.CID}
	state.Winner = &v1alpha1.ContractWinner{DID: winner.DID}
	state.Bids = intPtr(len(allBids))
	state.Timestamps.WinnerSelected = r.svc.nowISO()
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the winner is selected; accepting", state, allBids, chosen, false, "accepting", 1)

	acceptRecord, err := market.BuildAccept(market.AcceptOptions{
		RFP:         market.RecordRef{URI: rfpURI, CID: rfpCID},
		Bid:         market.RecordRef{URI: winner.URI, CID: winner.CID},
		SubmitEvent: r.svc.callbackDID() + "#" + defaultComputeEventServiceID,
		CreatedAt:   r.svc.nowISO(),
	})
	if err != nil {
		return false, err
	}
	accept, err := market.CreateSignedRecord(ctx, r.svc.repo, r.svc.cfg.RepoDID, market.AcceptNSID, acceptRecord, r.svc.signer)
	if err != nil {
		return false, fmt.Errorf("create the market.accept record: %w", err)
	}
	state.AcceptUri = accept.URI
	state.Accept = &v1alpha1.RecordRef{URI: accept.URI, CID: accept.CID}
	state.AcceptPending = &v1alpha1.ContractAcceptPending{BidUri: winner.URI}
	state.Phase = v1alpha1.ContractAccepted
	state.Timestamps.Accepted = r.svc.nowISO()
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the accept is submitted; waiting for the receipt", state, allBids, chosen, false, "awaitingReceipt", 1)

	receipt, err := r.svc.submitAcceptAndWait(ctx, winnerBidder, accept.URI, accept.CID, winner.DID)
	if err != nil {
		return false, err
	}
	if receipt == nil {
		return false, fmt.Errorf("no receipt arrived within %s", r.svc.cfg.ReceiptTimeout)
	}
	verification := market.VerifyReceipt(ctx, receipt.Value, receipt.URI, accept.Record, accept.URI, market.VerifyRecordOptions{
		KeysForDID:  market.NewDirectoryKeyLister(r.svc.directory),
		KeyResolver: market.DefaultKeyResolver,
	})
	state.Receipt = &v1alpha1.RecordRef{URI: receipt.URI, CID: receipt.CID}
	state.SubmitEventRef = stringOf(receipt.Value["submitEvent"])
	state.ReceiptOk = boolPtr(verification.OK)
	state.Phase = v1alpha1.ContractReceipt
	state.Timestamps.Receipt = r.svc.nowISO()
	if !verification.OK {
		state.Outcome = v1alpha1.ContractOutcomeReceiptInvalid
		state.Error = "the receipt signature or accept binding is invalid"
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, state.Error, state, allBids, chosen, false, "receiptInvalid", 1)
		return false, errors.New(state.Error)
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the receipt is verified; waiting for the guest", state, allBids, chosen, false, "awaitingGuestOnNetwork", 1)

	if state.Termination != nil {
		return false, nil
	}
	return false, r.runGuestAndHold(ctx, state, receipt.URI, winner.DID)
}

func (r *contractRunner) runGuestAndHold(ctx context.Context, state *v1alpha1.NodeClaimContract, receiptURI, bidderDID string) error {
	if err := r.waitGuest(ctx, state, receiptURI, bidderDID); err != nil {
		return err
	}
	if r.released() {
		return r.releaseContract(ctx, state, bidderDID)
	}
	if state.Termination != nil {
		return nil
	}
	if r.secrets != nil && r.secretsFetched() == "" {
		timeout := r.svc.cfg.VMReadyTimeout
		if timeout <= 0 {
			timeout = DefaultSecretsFetchTimeout
		}
		select {
		case <-r.secrets.fetched:
			r.setSecretsFetched(r.secrets.fetchedAtValue())
		case <-time.After(timeout):
			return fmt.Errorf("the guest did not fetch its join secrets within %s", timeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	state.Phase = v1alpha1.ContractNetwork
	state.Outcome = v1alpha1.ContractOutcomeComplete
	if state.Timestamps.Network == "" {
		state.Timestamps.Network = r.svc.nowISO()
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractHeld, "held: the guest is on the network; set spec.release to release it", state, r.currentBids(), r.currentChosen(), false, "held", 1)
	return r.waitForRelease(ctx, state, receiptURI, bidderDID)
}

func (r *contractRunner) resumeAccepted(ctx context.Context, state *v1alpha1.NodeClaimContract) (bool, error) {
	bidderDID := winnerDID(state)
	if bidderDID == "" {
		return false, errors.New("the recorded contract names no winning bidder")
	}
	bidder, ok := r.svc.bidderForDID(ctx, bidderDID)
	if !ok {
		return false, fmt.Errorf("no endpoint is known for the winning bidder %s", bidderDID)
	}
	acceptRef := market.RecordRef{URI: state.Accept.URI, CID: state.Accept.CID}
	accept, err := r.svc.repo.Resolve(ctx, acceptRef)
	if err != nil {
		return false, fmt.Errorf("read the recorded market.accept record: %w", err)
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the accept is recorded; waiting for the receipt", state, r.currentBids(), r.currentChosen(), false, "awaitingReceipt", 1)
	receipt, err := r.svc.submitAcceptAndWait(ctx, bidder, acceptRef.URI, acceptRef.CID, bidderDID)
	if err != nil {
		return false, err
	}
	if receipt == nil {
		return false, fmt.Errorf("no receipt arrived within %s", r.svc.cfg.ReceiptTimeout)
	}
	return false, r.acceptReceipt(ctx, state, accept.Value, acceptRef.URI, receipt)
}

func (r *contractRunner) acceptReceipt(ctx context.Context, state *v1alpha1.NodeClaimContract, acceptRecord map[string]any, acceptURI string, receipt *market.ResolvedRecord) error {
	verification := market.VerifyReceipt(ctx, receipt.Value, receipt.URI, acceptRecord, acceptURI, market.VerifyRecordOptions{
		KeysForDID:  market.NewDirectoryKeyLister(r.svc.directory),
		KeyResolver: market.DefaultKeyResolver,
	})
	state.Receipt = &v1alpha1.RecordRef{URI: receipt.URI, CID: receipt.CID}
	state.SubmitEventRef = stringOf(receipt.Value["submitEvent"])
	state.ReceiptOk = boolPtr(verification.OK)
	state.Phase = v1alpha1.ContractReceipt
	state.Timestamps.Receipt = r.svc.nowISO()
	if !verification.OK {
		state.Outcome = v1alpha1.ContractOutcomeReceiptInvalid
		state.Error = "the receipt signature or accept binding is invalid"
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, state.Error, state, r.currentBids(), r.currentChosen(), false, "receiptInvalid", 1)
		return errors.New(state.Error)
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the receipt is verified; waiting for the guest", state, r.currentBids(), r.currentChosen(), false, "awaitingGuestOnNetwork", 1)
	return r.runGuestAndHold(ctx, state, receipt.URI, winnerDID(state))
}

func (r *contractRunner) resumeReceipt(ctx context.Context, state *v1alpha1.NodeClaimContract) error {
	if state.Termination != nil {
		state.Phase = v1alpha1.ContractReleased
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "the bidder ended the contract: "+state.Termination.Reason, state, r.currentBids(), r.currentChosen(), false, "terminated", 1)
		return nil
	}
	bidderDID := winnerDID(state)
	if r.released() {
		return r.releaseContract(ctx, state, bidderDID)
	}
	receiptRef := market.RecordRef{URI: state.Receipt.URI, CID: state.Receipt.CID}
	verified, err := r.verifyRecordedReceipt(ctx, state, receiptRef)
	if err != nil {
		if state.ReceiptOk == nil || !*state.ReceiptOk {
			return err
		}
	} else if !verified {
		state.ReceiptOk = boolPtr(false)
		state.Outcome = v1alpha1.ContractOutcomeReceiptInvalid
		state.Error = "the receipt signature or accept binding is invalid"
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, state.Error, state, r.currentBids(), r.currentChosen(), false, "receiptInvalid", 1)
		return errors.New(state.Error)
	} else {
		state.ReceiptOk = boolPtr(true)
	}
	if state.Phase != v1alpha1.ContractNetwork && state.Phase != v1alpha1.ContractReleased {
		state.Phase = v1alpha1.ContractReceipt
	}
	if state.Timestamps.Receipt == "" {
		state.Timestamps.Receipt = r.svc.nowISO()
	}
	return r.runGuestAndHold(ctx, state, receiptRef.URI, bidderDID)
}

func (r *contractRunner) verifyRecordedReceipt(ctx context.Context, state *v1alpha1.NodeClaimContract, receiptRef market.RecordRef) (bool, error) {
	receipt, err := r.svc.repo.Resolve(ctx, receiptRef)
	if err != nil {
		return false, fmt.Errorf("read the recorded market.receipt record: %w", err)
	}
	if state.SubmitEventRef == "" {
		state.SubmitEventRef = stringOf(receipt.Value["submitEvent"])
	}
	if state.Accept == nil || state.Accept.URI == "" {
		return true, nil
	}
	accept, err := r.svc.repo.Resolve(ctx, market.RecordRef{URI: state.Accept.URI, CID: state.Accept.CID})
	if err != nil {
		return false, fmt.Errorf("read the recorded market.accept record: %w", err)
	}
	verification := market.VerifyReceipt(ctx, receipt.Value, receipt.URI, accept.Value, state.Accept.URI, market.VerifyRecordOptions{
		KeysForDID:  market.NewDirectoryKeyLister(r.svc.directory),
		KeyResolver: market.DefaultKeyResolver,
	})
	return verification.OK, nil
}

func (r *contractRunner) collectBids(ctx context.Context, rfpURI string, response []BidRecord, bidders []Bidder, usePolicy bool, workflow string) (*BidRecord, []v1alpha1.ContractBid, bool, error) {
	deadline := time.Now().Add(r.svc.cfg.BidWindow)
	byURI := map[string]BidRecord{}
	add := func(bids []BidRecord) {
		for _, bid := range bids {
			if bid.URI == "" {
				continue
			}
			byURI[bid.URI] = bid
		}
	}
	add(response)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	pollTicker := time.NewTicker(time.Second)
	defer pollTicker.Stop()
	var early *BidRecord
	var selected *BidRecord
	for {
		add(r.svc.takeBids(rfpURI))
		if early == nil {
			if candidate := r.freeWinner(ctx, byURI, usePolicy, workflow); candidate != nil {
				early = candidate
				selected = candidate
				break
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, false, ctx.Err()
		case <-r.releaseCh:
			return nil, nil, false, nil
		case <-ticker.C:
		case <-pollTicker.C:
			for _, bidder := range bidders {
				bids, err := r.svc.market.FindBids(ctx, bidder.DID, rfpURI)
				if err != nil {
					continue
				}
				add(bids)
			}
		}
	}
	add(r.svc.takeBids(rfpURI))
	if early == nil {
		selected = r.bestBid(ctx, byURI, usePolicy, workflow)
	}
	records := make([]v1alpha1.ContractBid, 0, len(byURI))
	retryCapacity := len(byURI) > 0
	for _, bid := range byURI {
		entry := v1alpha1.ContractBid{
			Did:       bid.DID,
			URI:       bid.URI,
			Verdict:   v1alpha1.ContractBidConsidered,
			ArrivedAt: bid.arrivedAtISO(),
		}
		if bid.Free {
			entry.Cost = 0
		} else {
			entry.Cost = int64(bid.Cost)
		}
		if selected != nil && selected.URI == bid.URI {
			entry.Verdict = v1alpha1.ContractBidChosen
			retryCapacity = false
		} else if !r.svc.bidAllowedWithPolicy(ctx, bid, usePolicy, workflow) {
			entry.Verdict = v1alpha1.ContractBidPolicyRejected
		} else if selected != nil {
			entry.Verdict = v1alpha1.ContractBidNotChosen
		} else {
			entry.Verdict = v1alpha1.ContractBidDenied
			if !(bid.Free && strings.EqualFold(bid.Reason, "capacity")) {
				retryCapacity = false
			}
		}
		records = append(records, entry)
		if len(records) >= MaxRecordBids {
			break
		}
	}
	if selected != nil {
		return selected, records, false, nil
	}
	return nil, records, retryCapacity && len(records) > 0, nil
}

func (r *contractRunner) freeWinner(ctx context.Context, bids map[string]BidRecord, usePolicy bool, workflow string) *BidRecord {
	for _, bid := range bids {
		bidCopy := bid
		if err := r.svc.classifyBid(ctx, &bidCopy); err != nil {
			continue
		}
		if r.svc.cfg.firstFree() && bidCopy.Free && r.svc.bidAllowedWithPolicy(ctx, bidCopy, usePolicy, workflow) {
			return &bidCopy
		}
	}
	return nil
}

func (r *contractRunner) bestBid(ctx context.Context, bids map[string]BidRecord, usePolicy bool, workflow string) *BidRecord {
	var best *BidRecord
	for _, bid := range bids {
		bidCopy := bid
		if err := r.svc.classifyBid(ctx, &bidCopy); err != nil {
			continue
		}
		if !r.svc.bidAllowedWithPolicy(ctx, bidCopy, usePolicy, workflow) {
			continue
		}
		if best == nil || bidCopy.Cost < best.Cost {
			best = &bidCopy
		}
	}
	return best
}

func (r *contractRunner) waitGuest(ctx context.Context, state *v1alpha1.NodeClaimContract, receiptURI, bidderDID string) error {
	events, pending, closeEvents := r.svc.registerEventSink(receiptURI)
	defer closeEvents()
	addressDeadline := time.Now().Add(r.svc.cfg.VMReadyTimeout)
	hostKeysDeadline := time.Now().Add(r.svc.cfg.HostKeysTimeout)
	seen := map[string]bool{}
	emptyLogged := false
	handle := func(event MarketEvent) bool {
		if seen[event.Event.URI] {
			return false
		}
		seen[event.Event.URI] = true
		parsed, err := parseGuestEvent(ctx, r.svc.directory, r.svc.badgeBlueBinding(), event, receiptURI)
		if err != nil {
			r.log().Warn("requester_guest_event_refused", "uri", event.Event.URI, "err", err)
			return false
		}
		switch parsed.Kind {
		case market.ComputeEventsVMOnNetworkNSID:
			if state.VMAddress == "" {
				state.VMAddress = parsed.Address
				state.Phase = v1alpha1.ContractNetwork
				state.Timestamps.Network = r.svc.nowISO()
				r.log().Info("requester_guest_on_network", "address", parsed.Address)
			}
		case market.ComputeEventsVMHostKeysNSID:
			state.HostKeys = &v1alpha1.ContractHostKeys{
				Keys:       contractHostKeys(parsed.HostKeys),
				ObservedAt: parsed.ObservedAt,
				Event:      v1alpha1.RecordRef{URI: parsed.Event.URI, CID: parsed.Event.CID},
				Payload:    v1alpha1.RecordRef{URI: parsed.Payload.URI, CID: parsed.Payload.CID},
			}
			r.log().Info("requester_guest_host_keys", "keys", len(parsed.HostKeys))
		case market.ComputeEventsVMTerminatedNSID:
			state.Termination = parsed.Termination
			state.Outcome = v1alpha1.ContractOutcomeReturned
			state.Phase = v1alpha1.ContractReleased
			state.Timestamps.Released = r.svc.nowISO()
			r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "the bidder ended the contract: "+parsed.Termination.Reason, state, r.currentBids(), r.currentChosen(), false, "terminated", 1)
			return true
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the guest reported "+payloadKind(parsed.Kind), state, r.currentBids(), r.currentChosen(), false, "awaitingGuestOnNetwork", 1)
		return false
	}
	for _, event := range pending {
		if handle(event) {
			return nil
		}
	}
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for {
		if state.Termination != nil {
			return nil
		}
		if state.VMAddress != "" && state.HostKeys != nil {
			return nil
		}
		if r.released() {
			return nil
		}
		now := time.Now()
		if state.VMAddress == "" && now.After(addressDeadline) {
			state.Outcome = v1alpha1.ContractOutcomeNetworkTimeout
			return fmt.Errorf("the guest did not come on network within %s", r.svc.cfg.VMReadyTimeout)
		}
		if state.VMAddress != "" && state.HostKeys == nil && now.After(hostKeysDeadline) {
			return fmt.Errorf("the guest reported no ssh host keys within %s", r.svc.cfg.HostKeysTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.releaseCh:
			return nil
		case event := <-events:
			if handle(event) {
				return nil
			}
		case <-poll.C:
			found, err := r.svc.market.FindEvents(ctx, bidderDID, receiptURI)
			if err != nil {
				r.log().Warn("requester_event_poll_failed", "bidder", bidderDID, "err", err)
				continue
			}
			if len(found) == 0 && !emptyLogged {
				emptyLogged = true
				r.log().Warn("requester_event_poll_empty", "bidder", bidderDID, "receipt", receiptURI)
			}
			for _, event := range found {
				if handle(event) {
					return nil
				}
			}
		}
	}
}

func (r *contractRunner) waitForRelease(ctx context.Context, state *v1alpha1.NodeClaimContract, receiptURI, bidderDID string) error {
	events, _, closeEvents := r.svc.registerEventSink(receiptURI)
	defer closeEvents()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if state.Termination != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			if r.svc.ctx.Err() != nil {
				r.log().Info("requester_contract_left_for_restart")
				return nil
			}
			return ctx.Err()
		case <-r.releaseCh:
			return r.releaseContract(ctx, state, bidderDID)
		case event := <-events:
			parsed, err := parseGuestEvent(ctx, r.svc.directory, r.svc.badgeBlueBinding(), event, receiptURI)
			if err != nil {
				continue
			}
			if parsed.Kind == market.ComputeEventsVMTerminatedNSID {
				state.Termination = parsed.Termination
				state.Outcome = v1alpha1.ContractOutcomeReturned
				state.Phase = v1alpha1.ContractReleased
				state.Timestamps.Released = r.svc.nowISO()
				r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "the bidder ended the contract: "+parsed.Termination.Reason, state, r.currentBids(), r.currentChosen(), false, "terminated", 1)
				return nil
			}
		case <-ticker.C:
			found, err := r.svc.market.FindEvents(ctx, bidderDID, receiptURI)
			if err != nil {
				continue
			}
			for _, event := range found {
				parsed, err := parseGuestEvent(ctx, r.svc.directory, r.svc.badgeBlueBinding(), event, receiptURI)
				if err != nil || parsed.Kind != market.ComputeEventsVMTerminatedNSID {
					continue
				}
				state.Termination = parsed.Termination
				state.Outcome = v1alpha1.ContractOutcomeReturned
				state.Phase = v1alpha1.ContractReleased
				state.Timestamps.Released = r.svc.nowISO()
				r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "the bidder ended the contract: "+parsed.Termination.Reason, state, r.currentBids(), r.currentChosen(), false, "terminated", 1)
				return nil
			}
		}
	}
}

func (r *contractRunner) releaseContract(ctx context.Context, state *v1alpha1.NodeClaimContract, bidderDID string) error {
	if state == nil || state.Receipt == nil {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, r.currentBids(), r.currentChosen(), false, "released", 1)
		r.deleteRBAC(ctx)
		return nil
	}
	if state.Termination != nil {
		state.Phase = v1alpha1.ContractReleased
		if state.Timestamps != nil && state.Timestamps.Released == "" {
			state.Timestamps.Released = r.svc.nowISO()
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "the bidder ended the contract: "+state.Termination.Reason, state, r.currentBids(), r.currentChosen(), false, "terminated", 1)
		r.deleteRBAC(ctx)
		return nil
	}
	if state.Release != nil && state.Release.OK {
		state.Phase = v1alpha1.ContractReleased
		if release := r.releaseStatus(); release != nil {
			release.LastError = ""
			r.setRelease(release)
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released", state, r.currentBids(), r.currentChosen(), false, "released", 1)
		r.deleteRBAC(ctx)
		return nil
	}

	release := r.releaseStatus()
	if release == nil {
		release = &v1alpha1.ComputeContractRelease{}
	}
	if release.StartedAt == "" {
		release.StartedAt = r.svc.nowISO()
	}
	deadline := r.svc.releaseDeadline(r.snapshot())
	if release.Deadline != "" {
		if parsed, err := time.Parse(time.RFC3339, release.Deadline); err == nil {
			deadline = parsed
		}
	} else {
		release.Deadline = deadline.UTC().Format(time.RFC3339)
	}
	r.setRelease(release)

	bidder, ok := r.svc.bidderForDID(ctx, bidderDID)
	if !ok {
		release.LastError = fmt.Sprintf("no endpoint is known for the bidder %s", bidderDID)
		r.setRelease(release)
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "the destroy could not be sent: "+release.LastError, state, r.currentBids(), r.currentChosen(), false, "releaseFailed", int32(release.Attempts))
		return errors.New(release.LastError)
	}

	r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "destroying the guest", state, r.currentBids(), r.currentChosen(), false, "releasing", int32(release.Attempts))

	eventRef, eventRecord, err := r.releaseEnvelope(ctx, state)
	if err != nil {
		release.LastError = err.Error()
		r.setRelease(release)
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "the destroy event could not be recorded: "+err.Error(), state, r.currentBids(), r.currentChosen(), false, "releaseFailed", int32(release.Attempts))
		return err
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "the destroy is recorded; sending it", state, r.currentBids(), r.currentChosen(), false, "releasing", int32(release.Attempts))

	for {
		release.Attempts++
		r.setRelease(release)
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, fmt.Sprintf("destroying the guest: attempt %d", release.Attempts), state, r.currentBids(), r.currentChosen(), false, "releasing", int32(release.Attempts))
		status, err := r.svc.market.SubmitEvent(ctx, bidder, SubmitEventRequest{URI: eventRef.URI, CID: eventRef.CID, Record: eventRecord})
		if err == nil && status >= 200 && status <= 299 {
			state.Release.OK = true
			state.Release.Status = &status
			state.Phase = v1alpha1.ContractReleased
			if state.Timestamps != nil && state.Timestamps.Released == "" {
				state.Timestamps.Released = r.svc.nowISO()
			}
			release.LastError = ""
			r.setRelease(release)
			r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released", state, r.currentBids(), r.currentChosen(), false, "released", int32(release.Attempts))
			r.deleteRBAC(ctx)
			return nil
		}
		detail := "no answer"
		if err != nil {
			detail = err.Error()
		} else {
			detail = fmt.Sprintf("HTTP %d", status)
			state.Release.Status = &status
		}
		release.LastError = detail
		r.setRelease(release)
		message := fmt.Sprintf("the destroy failed on attempt %d: %s", release.Attempts, detail)
		if time.Now().After(deadline) {
			r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, message+"; the deadline passed, the release intent stays recorded", state, r.currentBids(), r.currentChosen(), false, "releaseFailed", int32(release.Attempts))
			return fmt.Errorf("the guest was not destroyed by %s: %s", deadline.UTC().Format(time.RFC3339), detail)
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, message+"; retrying", state, r.currentBids(), r.currentChosen(), false, "releaseFailed", int32(release.Attempts))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.svc.releaseRetryDelay(release.Attempts)):
		}
	}
}

func (r *contractRunner) releaseEnvelope(ctx context.Context, state *v1alpha1.NodeClaimContract) (market.RecordRef, map[string]any, error) {
	if state.Release != nil && state.Release.Event.URI != "" {
		ref := market.RecordRef{URI: state.Release.Event.URI, CID: state.Release.Event.CID}
		resolved, err := r.svc.repo.Resolve(ctx, ref)
		if err != nil {
			return market.RecordRef{}, nil, fmt.Errorf("read the recorded destroy event: %w", err)
		}
		return ref, resolved.Value, nil
	}
	payloadRecord := map[string]any{
		"$type":     market.ComputeEventsVMDeleteNSID,
		"reason":    "released",
		"createdAt": r.svc.nowISO(),
	}
	payload, err := market.CreateSignedRecord(ctx, r.svc.repo, r.svc.cfg.RepoDID, market.ComputeEventsVMDeleteNSID, payloadRecord, r.svc.signer)
	if err != nil {
		return market.RecordRef{}, nil, fmt.Errorf("create the destroy payload: %w", err)
	}
	envelope, err := market.CreateEvent(ctx, r.svc.repo, r.svc.cfg.RepoDID, market.EventOptions{
		Receipt:   market.RecordRef{URI: state.Receipt.URI, CID: state.Receipt.CID},
		Payload:   market.RecordRef{URI: payload.URI, CID: payload.CID},
		CreatedAt: r.svc.nowISO(),
	}, r.svc.signer)
	if err != nil {
		return market.RecordRef{}, nil, fmt.Errorf("create the destroy event: %w", err)
	}
	state.Release = &v1alpha1.ContractRelease{
		Event:   v1alpha1.RecordRef{URI: envelope.URI, CID: envelope.CID},
		Payload: v1alpha1.RecordRef{URI: payload.URI, CID: payload.CID},
	}
	return market.RecordRef{URI: envelope.URI, CID: envelope.CID}, envelope.Record, nil
}

func (r *contractRunner) runAdopt(ctx context.Context, contract v1alpha1.ComputeContract) {
	state := r.stateCopy()
	if state == nil || state.Receipt == nil {
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "adopt: the contract status holds no bidder-signed receipt", state, nil, nil, false, "adopting", 1)
		return
	}
	if state.Termination != nil {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "adopt: the bidder reported the guest terminated ("+state.Termination.Reason+")", state, nil, nil, false, "adopted", 1)
		return
	}
	if state.HostKeys == nil {
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "adopt: the contract status holds no bidder-signed vm.hostKeys", state, nil, nil, false, "adopting", 1)
		return
	}
	state.Phase = v1alpha1.ContractNetwork
	r.writeStatus(ctx, v1alpha1.ComputeContractHeld, "adopted: no RFP was sent and no vm.delete was submitted", state, nil, nil, false, "adopted", 1)
	if err := r.waitForRelease(ctx, state, state.Receipt.URI, winnerDID(state)); err != nil && r.svc.ctx.Err() == nil {
		r.recordFailure(ctx, r.stateCopy(), err, 1)
	}
}

func (r *contractRunner) writeStatus(ctx context.Context, phase v1alpha1.ComputeContractPhase, message string, state *v1alpha1.NodeClaimContract, bids []v1alpha1.ContractBid, chosen *v1alpha1.ContractChosen, atCapacity bool, step string, attempts int32) {
	contract := r.snapshot()
	status := v1alpha1.ComputeContractStatus{
		ObservedGeneration: contract.Generation,
		Phase:              phase,
		Message:            message,
		Bids:               append([]v1alpha1.ContractBid(nil), bids...),
		Chosen:             chosen,
		Conditions:         append([]metav1.Condition(nil), contract.Status.Conditions...),
		Execution: &v1alpha1.ExecutionStatus{
			Step:          step,
			StepStartedAt: r.svc.nowISO(),
			LastEventAt:   r.svc.nowISO(),
			Attempts:      attempts,
		},
	}
	if state != nil {
		status.Contract = cloneNodeClaimContract(state)
	}
	if url := r.secretsURLValue(); url != "" {
		status.SecretsUrl = url
	}
	if fetched := r.secretsFetched(); fetched != "" {
		status.SecretsFetchedAt = fetched
	}
	if grant := r.grantValue(); grant != nil {
		status.Grant = grant
	}
	if release := r.releaseStatus(); release != nil {
		status.Release = release
	} else if contract.Status.Release != nil {
		preserved := *contract.Status.Release
		status.Release = &preserved
	}
	r.applyConditions(&status, contract.Generation, phase, message, state, atCapacity)
	if err := r.svc.contracts.PatchStatus(ctx, r.name, status); err != nil {
		if !isNotFound(err) {
			r.log().Warn("requester_contract_status_failed", "err", err)
		}
	}
}

func (r *contractRunner) applyConditions(status *v1alpha1.ComputeContractStatus, generation int64, phase v1alpha1.ComputeContractPhase, message string, state *v1alpha1.NodeClaimContract, atCapacity bool) {
	now := r.svc.now()
	addContractConditions(status, generation, phase, message, now, atCapacity)
	if r.resumed {
		setCondition(status, generation, v1alpha1.ComputeContractConditionResumed, metav1.ConditionTrue, "Recovered", "the recorded status.contract was resumed", now)
	}
	if phase == v1alpha1.ComputeContractReleasing {
		if release := r.releaseStatus(); release != nil && release.LastError != "" {
			setCondition(status, generation, v1alpha1.ComputeContractConditionReleaseFailed, metav1.ConditionTrue, "DestroyFailed", release.LastError, now)
		}
	}
	if phase == v1alpha1.ComputeContractReleased {
		acknowledged := state != nil && ((state.Release != nil && state.Release.OK) || state.Termination != nil)
		if acknowledged {
			setCondition(status, generation, v1alpha1.ComputeContractConditionReleaseAcknowledged, metav1.ConditionTrue, "Acknowledged", message, now)
			setCondition(status, generation, v1alpha1.ComputeContractConditionReleaseFailed, metav1.ConditionFalse, "Acknowledged", message, now)
		}
	}
}

func (r *contractRunner) step(ctx context.Context, step string, attempts int32) {
	state := r.stateCopy()
	r.writeStatus(ctx, r.phaseOf(state), "step "+step, state, r.currentBids(), r.currentChosen(), false, step, attempts)
}

func (r *contractRunner) phaseOf(state *v1alpha1.NodeClaimContract) v1alpha1.ComputeContractPhase {
	if r.svc.ctx.Err() != nil {
		return v1alpha1.ComputeContractReleasing
	}
	if state != nil && state.Termination != nil {
		return v1alpha1.ComputeContractReleased
	}
	return v1alpha1.ComputeContractRequesting
}

func (r *contractRunner) currentBids() []v1alpha1.ContractBid {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]v1alpha1.ContractBid(nil), r.bids...)
}

func (r *contractRunner) currentChosen() *v1alpha1.ContractChosen {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.chosen == nil {
		return nil
	}
	copy := *r.chosen
	return &copy
}

func (b BidRecord) RecordSubmitEvent() string {
	value, _ := b.Record["submitAccept"].(string)
	return value
}

func (b BidRecord) arrivedAtISO() string {
	if b.ArrivedAt.IsZero() {
		return market.NowISO()
	}
	return b.ArrivedAt.UTC().Format("2006-01-02T15:04:05.000Z")
}

func cloneNodeClaimContract(in *v1alpha1.NodeClaimContract) *v1alpha1.NodeClaimContract {
	if in == nil {
		return nil
	}
	out := new(v1alpha1.NodeClaimContract)
	in.DeepCopyInto(out)
	return out
}

func winnerDID(state *v1alpha1.NodeClaimContract) string {
	if state == nil || state.Winner == nil {
		return ""
	}
	return state.Winner.DID
}

func contractHostKeys(keys []v1alpha1.ComputeVMHostKey) []v1alpha1.ContractHostKey {
	out := make([]v1alpha1.ContractHostKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, v1alpha1.ContractHostKey{Type: key.Type, PublicKey: key.PublicKey, Fingerprint: key.Fingerprint})
	}
	return out
}

func payloadKind(nsid string) string {
	switch nsid {
	case market.ComputeEventsVMOnNetworkNSID:
		return "onNetwork"
	case market.ComputeEventsVMHostKeysNSID:
		return "hostKeys"
	case market.ComputeEventsVMTerminatedNSID:
		return "terminated"
	default:
		return nsid
	}
}

func parsePolicyArgs(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("policyArgs is not JSON: %w", err)
	}
	if value == nil {
		return map[string]string{}, nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("policyArgs must be a JSON object")
	}
	out := make(map[string]string, len(obj))
	for key, item := range obj {
		switch typed := item.(type) {
		case string:
			out[key] = typed
		case bool:
			out[key] = fmt.Sprintf("%t", typed)
		case float64:
			out[key] = fmt.Sprintf("%v", typed)
		default:
			data, err := json.Marshal(typed)
			if err != nil {
				return nil, err
			}
			out[key] = string(data)
		}
	}
	return out, nil
}

func defaultUserData(keys []string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	if len(keys) > 0 {
		b.WriteString("ssh_authorized_keys:\n")
		for _, key := range keys {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			b.WriteString("  - ")
			b.WriteString(key)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func withGuestCACerts(userData, pem string) string {
	pem = strings.TrimSpace(pem)
	if pem == "" {
		return userData
	}
	if !strings.HasPrefix(strings.TrimLeft(userData, " \t\r\n"), "#cloud-config") {
		return userData
	}
	var b strings.Builder
	b.WriteString(userData)
	if !strings.HasSuffix(userData, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("ca_certs:\n  trusted:\n    - |\n")
	for _, line := range strings.Split(pem, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		b.WriteString("      ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func intPtr(value int) *int {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}
