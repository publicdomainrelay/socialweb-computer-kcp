package requester

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/netretry"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

type contractRunner struct {
	q *Requester

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

	cancel context.CancelFunc

	gone atomic.Bool

	terminationNotReported atomic.Bool

	nodeJoin *v1alpha1.ComputeContractNodeJoin

	grant *v1alpha1.NodeClaimGrant

	secretsURL string

	secretsFetchedAt string

	secrets *secretsState

	rbacRKey string

	atircRevoke sync.Once

	releaseRequested string

	association *contractAssociation

	associationMu sync.Mutex

	associationKick chan struct{}

	bundleMu *sync.Mutex

	associationRecords map[string]string

	associationAttempts int

	associationRetry *time.Timer

	atircMint *atircMintOutcome
}

func newContractRunner(q *Requester, contract v1alpha1.ComputeContract) *contractRunner {
	r := &contractRunner{
		q:         q,
		name:      contract.Name,
		contract:  contract,
		bids:      append([]v1alpha1.ContractBid(nil), contract.Status.Bids...),
		releaseCh: make(chan struct{}),
		done:      make(chan struct{}),

		associationKick: make(chan struct{}, 1),

		bundleMu: q.bundleLock(contract.Name),
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
	r.secretsURL = contract.Status.SecretsURL
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
	return state.VMURI != "" || state.RFPURI != "" || state.AcceptURI != "" || state.ReceiptURI != "" || state.Release != nil
}

func (r *contractRunner) Update(contract v1alpha1.ComputeContract) {
	r.mu.Lock()
	r.contract = contract
	r.mu.Unlock()
	if contract.DeletionTimestamp != nil || contract.Spec.Release {
		r.requestRelease()
		return
	}
	r.associateLater(contract)
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

func (r *contractRunner) untilRelease(ctx context.Context, ref func(context.Context) (string, error)) (string, error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-r.releaseCh:
			cancel()
		case <-waitCtx.Done():
		}
	}()
	return ref(waitCtx)
}

func (r *contractRunner) stop() {
	r.gone.Store(true)
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *contractRunner) isGone() bool {
	return r.gone.Load()
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
	return cloneNodeClaimContract(r.state)
}

func (r *contractRunner) setState(state *v1alpha1.NodeClaimContract) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

func (r *contractRunner) log() *slog.Logger {
	if r.isGone() {
		return slog.New(slog.DiscardHandler)
	}
	return r.q.log.With("contract", r.name)
}

func (r *contractRunner) Run(ctx context.Context) {
	defer close(r.done)
	defer r.cleanup()
	contract := r.snapshot()
	if contract.Spec.Adopt {
		r.runAdopt(ctx, contract)
		return
	}
	state := r.stateCopy()
	tokenPathConflict := CheckTokenPaths(contract, nil)
	if tokenPathConflict != nil {
		if (state == nil || state.RFPURI == "") && !r.released() {
			r.log().Warn("requester_contract_refused", "err", tokenPathConflict.Error())
			r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "refused: "+tokenPathConflict.Error(), state, nil, nil, "refused", 1)
			return
		}
		r.log().Warn("requester_contract_token_path_conflict", "err", tokenPathConflict.Error())
	}
	if contract.Status.Phase == v1alpha1.ComputeContractReleased && state != nil && state.Receipt != nil {
		r.resumeReleased(ctx, state)
		return
	}
	if contract.Spec.Agent != nil && (state == nil || state.RFPURI == "") && !r.released() {
		if err := r.ensureAgentBundle(ctx, contract); err != nil {
			r.recordFailure(ctx, state, err, 1)
			return
		}
		if err := r.q.checkAgentHandle(ctx); err != nil {
			r.log().Warn("requester_contract_refused", "err", err.Error())
			r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "refused: "+err.Error(), state, nil, nil, "refused", 1)
			return
		}
	}
	if contract.Spec.SecretsRef != nil && contract.Status.SecretsFetchedAt == "" {
		if err := r.untilSettled(ctx, "ensureSecrets", func() error { return r.ensureSecrets(ctx, contract) }); err != nil {
			if !r.q.stopping() {
				r.recordFailure(ctx, r.stateCopy(), err, 1)
			}
			return
		}
	}
	state = r.stateCopy()
	if state != nil && state.Receipt != nil {
		if err := r.resumeReceipt(ctx, state); err != nil && !r.q.stopping() {
			r.recordFailure(ctx, r.stateCopy(), err, 1)
		}
		return
	}
	if r.released() {
		if r.recoverReceipt(ctx, state) {
			if err := r.releaseContract(ctx, state, winnerDID(state)); err != nil && !r.q.stopping() {
				r.recordFailure(ctx, r.stateCopy(), err, 1)
			}
			return
		}
		if acceptedWithoutReceipt(state) {
			if err := r.releaseDeniedAccept(ctx, state, winnerDID(state)); err != nil && !r.q.stopping() {
				r.recordFailure(ctx, r.stateCopy(), err, 1)
			}
			return
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, r.currentBids(), r.currentChosen(), "released", 1)
		return
	}
	err := r.requestOnce(ctx)
	if err == nil {
		return
	}
	if r.q.stopping() {
		r.log().Info("requester_contract_left_for_restart", "err", err)
		return
	}
	if r.released() {
		st := r.stateCopy()
		if st != nil && st.Receipt == nil && r.recoverReceipt(ctx, st) {
			if err := r.releaseContract(ctx, st, winnerDID(st)); err != nil && !r.q.stopping() {
				r.recordFailure(ctx, r.stateCopy(), err, 1)
			}
			return
		}
		if st == nil || st.Receipt == nil {
			r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", st, r.currentBids(), r.currentChosen(), "released", 1)
			return
		}
	}
	r.recordFailure(ctx, r.stateCopy(), err, 1)
}

func (r *contractRunner) recordFailure(ctx context.Context, state *v1alpha1.NodeClaimContract, err error, attempts int32) {
	if r.isGone() {
		return
	}
	if state != nil && state.Release != nil && !state.Release.OK {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "the release did not finish: "+err.Error(), state, r.currentBids(), r.currentChosen(), "releaseFailed", attempts)
		return
	}
	if r.released() && acceptedWithoutReceipt(state) {
		r.log().Warn("requester_contract_release_failed", "err", err)
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "the release did not find the receipt of the accept: "+err.Error(), state, r.currentBids(), r.currentChosen(), "releaseFailed", attempts)
		return
	}
	if r.released() && (state == nil || state.Receipt == nil) {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, r.currentBids(), r.currentChosen(), "released", attempts)
		return
	}
	r.log().Warn("requester_contract_failed", "err", err)
	r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "the contract flow failed: "+err.Error(), state, r.currentBids(), r.currentChosen(), "failed", attempts)
	if state != nil && state.Receipt != nil && state.Termination == nil {
		if waitErr := r.waitForRelease(ctx, state, state.Receipt.URI, winnerDID(state)); waitErr != nil {
			r.log().Info("requester_contract_failed_hold_ended", "err", waitErr)
		}
	}
}

func (q *Requester) generatedVMName() string {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%s%d", DefaultVMNamePrefix, time.Now().UnixNano()%100000000)
	}
	return DefaultVMNamePrefix + hex.EncodeToString(raw)
}

func (r *contractRunner) requestOnce(ctx context.Context) error {
	q := r.q
	contract := r.snapshot()
	vmName := strings.TrimSpace(contract.Spec.VMName)
	if vmName == "" {
		vmName = q.generatedVMName()
	}
	authorizedKeys := append([]string(nil), contract.Spec.SshAuthorizedKeys...)
	authorizedKeys = append(authorizedKeys, q.cfg.GuestAuthorizedKeys...)
	userData := contract.Spec.UserData
	if userData == "" {
		userData = defaultUserData(authorizedKeys)
	}
	var err error
	userData, err = guestseed.WithIrohTransport(userData, authorizedKeys, nil)
	if err != nil {
		return fmt.Errorf("the guest iroh transport: %w", err)
	}
	userData = withGuestCACerts(userData, q.guestCAPEM())
	userData, err = r.contractModules(userData, contract)
	if err != nil {
		return err
	}
	policyName := cmp.Or(strings.TrimSpace(contract.Spec.Policy), q.cfg.PolicyWorkflow, DefaultPolicyWorkflow)
	policyArgs, err := participant.ParsePolicyArgs(contract.Spec.PolicyArgs)
	if err != nil {
		return err
	}
	state := r.stateCopy()
	if state == nil {
		state = &v1alpha1.NodeClaimContract{
			Version:    new(1),
			Phase:      v1alpha1.ContractRequested,
			Timestamps: &v1alpha1.ContractTimestamps{Requested: q.nowISO()},
		}
	}
	state.VMName = cmp.Or(state.VMName, vmName)
	state.RequesterDid = cmp.Or(state.RequesterDid, q.callbackDID())
	state.MarketDid = cmp.Or(state.MarketDid, q.marketDID)
	if state.Version == nil {
		state.Version = new(1)
	}
	if state.Timestamps == nil {
		state.Timestamps = &v1alpha1.ContractTimestamps{Requested: q.nowISO()}
	}
	state.Phase = cmp.Or(state.Phase, v1alpha1.ContractRequested)
	r.setState(state)
	r.step(ctx, "preparing", 1)

	if state.Payload == nil || state.Payload.URI == "" {
		payloadRecord, err := market.EncodeRecord(market.ComputeVM{Role: vmName, Kind: string(contract.Spec.ComputeKind),
			Cpus: DefaultComputeCPUs, Mem: DefaultComputeMem, Disk: DefaultComputeDisk, UserData: userData, CreatedAt: q.nowISO()})
		if err != nil {
			return fmt.Errorf("encode the compute.vm record: %w", err)
		}
		payload, err := q.repo.CreateRecord(ctx, q.repoDID, market.ComputeVMNSID, payloadRecord)
		if err != nil {
			return fmt.Errorf("create the compute.vm record: %w", err)
		}
		state.VMURI = payload.URI
		state.Payload = &commonv1.StrongRef{URI: payload.URI, CID: payload.CID}
		state.Phase = v1alpha1.ContractRFPSubmitted
	} else {
		state.VMURI = state.Payload.URI
	}

	r.step(ctx, "creatingRfp", 1)
	var policyRef *market.StrongRef
	policyWorkflow := PolicyWorkflowName(policyName)
	workflowText, ok := PolicyWorkflowText(policyName)
	if !ok {
		return fmt.Errorf("policy %q names no bundled workflow", policyName)
	}
	if state.Policy != nil && state.Policy.URI != "" {
		policyRef = &market.StrongRef{Type: market.StrongRefNSID, URI: state.Policy.URI, CID: state.Policy.CID}
	} else {
		policyRecord := BuildPolicyRecord(policyName, policyWorkflow, workflowText, q.marketDID, policyArgs, q.nowISO())
		created, err := q.repo.CreateRecord(ctx, q.repoDID, PolicyGhaLiteNSID, policyRecord)
		if err != nil {
			return fmt.Errorf("create the policy record: %w", err)
		}
		policyRef = &created
		state.Policy = &commonv1.StrongRef{URI: created.URI, CID: created.CID}
	}
	gate, err := q.attachedPolicy(ctx, *policyRef, policyWorkflow)
	if err != nil {
		return err
	}
	rfpURI, rfpCID := "", ""
	if state.RFP != nil && state.RFP.URI != "" {
		rfpURI, rfpCID = state.RFP.URI, state.RFP.CID
	} else {
		submitBid, err := r.untilRelease(ctx, q.submitBidRef)
		if err != nil {
			return err
		}
		rfpOptions := market.RFPOptions{
			Payload:   market.RecordRef{URI: state.Payload.URI, CID: state.Payload.CID},
			SubmitBid: submitBid,
			Ingress:   q.ingressRecords(ctx),
			Domain:    "compute",
			CreatedAt: q.nowISO(),
		}
		rfpOptions.Policies = []market.RecordRef{{URI: policyRef.URI, CID: policyRef.CID}}
		if q.cfg.Spec.SuspendParticipation {
			return ErrParticipationSuspended
		}
		rfp, err := market.CreateRFP(ctx, q.repo, q.repoDID, rfpOptions, q.signer)
		if err != nil {
			return fmt.Errorf("create the market.rfp record: %w", err)
		}
		state.RFPURI = rfp.URI
		state.RFP = &commonv1.StrongRef{URI: rfp.URI, CID: rfp.CID}
		state.Timestamps.RFPSubmitted = q.nowISO()
		rfpURI, rfpCID = rfp.URI, rfp.CID
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the RFP is submitted; collecting bids", state, nil, nil, "collectingBids", 1)

	if state.Accept != nil && state.Accept.URI != "" {
		return r.resumeAccepted(ctx, state)
	}

	spec := r.snapshot().Spec
	only := BidderDIDList(spec.BidderDids)
	discovery, err := q.market.Discover(ctx, q.now(), only)
	if err != nil {
		r.log().Warn("requester_bidder_discovery_failed", "err", err)
	}
	discovery = discovery.Without(spec.DenyBidderDids)
	bidders := discovery.Bidders
	noBidders := ""
	if len(bidders) == 0 {
		noBidders = "requester_no_bidders: " + discovery.Why()
		r.log().Warn("requester_no_bidders", "restricted", discovery.Restricted, "candidates", discovery.Candidates, "dropped", discovery.DroppedText())
		if discovery.Restricted {
			state.Outcome = v1alpha1.ContractOutcomeNoBids
			state.Error = noBidders
			return errors.New(state.Error)
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the RFP is submitted; "+noBidders+"; waiting for bids", state, nil, nil, "collectingBids", 1)
	}
	q.rememberBidders(bidders)
	considered := consideredBidderDIDs(bidders, only)
	catchUpCtx, stopCatchUp := context.WithCancel(ctx)
	r.catchUpBids(catchUpCtx, considered, rfpURI, rfpCID)
	r.solicitBids(ctx, bidders, rfpURI, rfpCID)

	winner, allBids, err := r.collectBids(ctx, rfpURI, gate, only)
	stopCatchUp()
	if err != nil {
		return err
	}
	if r.released() {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, allBids, nil, "released", 1)
		return nil
	}
	r.mu.Lock()
	r.bids = allBids
	r.mu.Unlock()
	if winner == nil {
		if len(allBids) == 0 {
			state.Outcome = v1alpha1.ContractOutcomeNoBids
			state.Error = fmt.Sprintf("no bids received within %s", q.cfg.BidWindow)
			if noBidders != "" {
				state.Error += "; " + noBidders
			}
			return errors.New(state.Error)
		}
		state.Outcome = v1alpha1.ContractOutcomeBidsDenied
		state.Error = fmt.Sprintf("all %d bids were denied by policy", len(allBids))
		return errors.New(state.Error)
	}
	if r.released() {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, allBids, nil, "released", 1)
		return nil
	}

	bidderByDID := map[string]Bidder{}
	for _, bidder := range bidders {
		if _, ok := bidderByDID[bidder.DID]; !ok {
			bidderByDID[bidder.DID] = bidder
		}
	}
	winnerBidder, ok := bidderByDID[winner.DID]
	if !ok {
		winnerBidder, ok = q.bidderForBid(ctx, *winner)
	}
	if !ok {
		return fmt.Errorf("no endpoint is known for the winning bidder %s", winner.DID)
	}
	chosen := &v1alpha1.ContractChosen{
		URI:        winner.URI,
		Did:        winner.DID,
		Reason:     v1alpha1.ContractChosenBestWithinWindow,
		Considered: len(allBids),
	}
	decision := q.policy.Evaluate(ctx, gate.Input(PolicyInput{
		SelfDid:          q.marketDID,
		SubjectDid:       winner.DID,
		CounterpartyDid:  winner.DID,
		RootRequesterDid: q.marketDID,
		RFP:              &PolicyRef{URI: rfpURI, CID: rfpCID},
		Bid:              &PolicyRef{URI: winner.URI, CID: winner.CID},
	}))
	if !decision.Allow {
		state.Outcome = v1alpha1.ContractOutcomePolicyRejected
		state.Error = "winner rejected by policy: " + decision.Reason
		return errors.New(state.Error)
	}
	chosen.Reason = v1alpha1.ContractChosenPolicy
	if winner.Free {
		chosen.Reason = v1alpha1.ContractChosenFirstFree
	}
	r.mu.Lock()
	r.chosen = chosen
	r.mu.Unlock()
	acceptRKey := acceptRKeyFor(q.repoDID, r.name, rfpURI, rfpCID, winner.URI, winner.CID)
	if err := r.prepareWorkloadGrant(ctx, state, winner, acceptRKey); err != nil {
		return err
	}
	state.Phase = v1alpha1.ContractWinnerSelected
	state.Bid = &commonv1.StrongRef{URI: winner.URI, CID: winner.CID}
	state.Winner = &v1alpha1.ContractWinner{DID: winner.DID, SubmitAccept: strings.TrimSpace(stringOf(winner.Record["submitAccept"]))}
	state.Bids = new(len(allBids))
	state.Timestamps.WinnerSelected = q.nowISO()
	state.AcceptPending = &v1alpha1.ContractAcceptPending{BidURI: winner.URI}
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the winner is selected; accepting", state, allBids, chosen, "accepting", 1)

	accept, found, err := q.existingSignedRecord(ctx, market.AcceptNSID, acceptRKey)
	if err != nil {
		return fmt.Errorf("read the market.accept record: %w", err)
	}
	if !found && q.cfg.Spec.SuspendParticipation {
		return ErrParticipationSuspended
	}
	if !found {
		var submitEvent string
		submitEvent, err = r.untilRelease(ctx, q.submitEventRef)
		if err != nil {
			return err
		}
		accept, err = market.CreateAcceptAt(ctx, q.repo, q.repoDID, acceptRKey, market.AcceptOptions{
			RFP:         market.RecordRef{URI: rfpURI, CID: rfpCID},
			Bid:         market.RecordRef{URI: winner.URI, CID: winner.CID},
			SubmitEvent: submitEvent,
			Ingress:     q.ingressRecords(ctx),
			CreatedAt:   q.nowISO(),
		}, q.signer)
		if err != nil {
			return fmt.Errorf("create the market.accept record: %w", err)
		}
	}
	state.AcceptURI = accept.URI
	state.Accept = &commonv1.StrongRef{URI: accept.URI, CID: accept.CID}
	state.AcceptPending = &v1alpha1.ContractAcceptPending{BidURI: winner.URI}
	state.Phase = v1alpha1.ContractAccepted
	state.Timestamps.Accepted = q.nowISO()
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the accept is submitted; waiting for the receipt", state, allBids, chosen, "awaitingReceipt", 1)
	r.ensureAssociation(ctx, state.Accept, state)

	receipt, err := q.submitAcceptAndWait(ctx, winnerBidder, accept.URI, accept.CID, winner.DID)
	if err != nil {
		return err
	}
	if receipt == nil {
		return fmt.Errorf("no receipt arrived within %s", q.cfg.ReceiptTimeout)
	}
	return r.acceptReceipt(ctx, state, accept.Record, accept.URI, receipt)
}

const (
	submitRetryFirst = 500 * time.Millisecond

	submitRetryMax = 4 * time.Second
)

func submitRFPWithin(ctx context.Context, m MarketClient, bidder Bidder, req SubmitRFPRequest) ([]BidRecord, error) {
	wait := submitRetryFirst
	for {
		bids, err := m.SubmitRFP(ctx, bidder, req)
		if err == nil || !errors.Is(err, ErrBidderUnreachable) {
			return bids, err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C:
		}
		wait = min(2*wait, submitRetryMax)
	}
}

func (r *contractRunner) solicitBids(ctx context.Context, bidders []Bidder, rfpURI, rfpCID string) {
	for _, bidder := range bidders {
		if bidder.Endpoint == "" {
			continue
		}
		r.q.wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, max(r.q.cfg.BidWindow, 5*time.Second))
			defer cancel()
			bids, err := submitRFPWithin(cctx, r.q.market, bidder, SubmitRFPRequest{URI: rfpURI, CID: rfpCID})
			if err != nil {
				r.log().Warn("requester_submit_rfp_failed", "bidder", bidder.DID, "err", err)
				return
			}
			for _, bid := range bids {
				r.q.queueBid(rfpURI, bid)
			}
		})
	}
}

func (r *contractRunner) runGuestAndHold(ctx context.Context, state *v1alpha1.NodeClaimContract, receiptURI, bidderDID string) error {
	if err := r.waitGuest(ctx, state, receiptURI); err != nil {
		return err
	}
	if r.released() {
		return r.releaseContract(ctx, state, bidderDID)
	}
	if state.Termination != nil {
		return nil
	}
	if r.secrets != nil && r.secretsFetched() == "" && !r.q.cfg.SealedSecrets() {
		timer := time.NewTimer(DefaultVMReadyTimeout)
		defer timer.Stop()
		select {
		case <-r.secrets.fetched:
			r.setSecretsFetched(r.secrets.fetchedAtValue())
		case <-timer.C:
			return fmt.Errorf("the guest did not fetch its join secrets within %s", DefaultVMReadyTimeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	state.Phase = v1alpha1.ContractNetwork
	state.Outcome = v1alpha1.ContractOutcomeComplete
	if state.Timestamps.Network == "" {
		state.Timestamps.Network = r.q.nowISO()
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractHeld, "held: the guest is on the network; set spec.release to release it", state, r.currentBids(), r.currentChosen(), "held", 1)
	return r.waitForRelease(ctx, state, receiptURI, bidderDID)
}

func (r *contractRunner) resumeAccepted(ctx context.Context, state *v1alpha1.NodeClaimContract) error {
	bidderDID := winnerDID(state)
	if bidderDID == "" {
		return errors.New("the recorded contract names no winning bidder")
	}
	if err := r.untilSettled(ctx, "resumeAccepted", func() error { return r.q.recordedWinnerAllowed(ctx, state, r.contractPolicyName()) }); err != nil {
		if !errors.Is(err, ErrPolicyRejected) {
			return fmt.Errorf("the recorded accept is not resubmitted: %w", err)
		}
		state.Outcome = v1alpha1.ContractOutcomePolicyRejected
		state.Error = "the recorded accept is not resubmitted: " + err.Error()
		if releaseErr := r.releaseDeniedAccept(ctx, state, bidderDID); releaseErr != nil {
			return fmt.Errorf("%s; releasing it: %w", state.Error, releaseErr)
		}
		return errors.New(state.Error)
	}
	bidder, ok := r.q.bidderForDID(ctx, bidderDID)
	if !ok {
		return fmt.Errorf("no endpoint is known for the winning bidder %s", bidderDID)
	}
	acceptRef := market.RecordRef{URI: state.Accept.URI, CID: state.Accept.CID}
	accept, err := r.q.repo.Resolve(ctx, acceptRef)
	if err != nil {
		return fmt.Errorf("read the recorded market.accept record: %w", err)
	}
	r.ensureAssociation(ctx, state.Accept, state)
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the accept is recorded; waiting for the receipt", state, r.currentBids(), r.currentChosen(), "awaitingReceipt", 1)
	receipt, err := r.q.submitAcceptAndWait(ctx, bidder, acceptRef.URI, acceptRef.CID, bidderDID)
	if err != nil {
		return err
	}
	if receipt == nil {
		return fmt.Errorf("no receipt arrived within %s", r.q.cfg.ReceiptTimeout)
	}
	return r.acceptReceipt(ctx, state, accept.Value, acceptRef.URI, receipt)
}

const DeniedAcceptReceiptAttempts = 4

func acceptedWithoutReceipt(state *v1alpha1.NodeClaimContract) bool {
	return state != nil && state.Receipt == nil && state.Accept != nil && state.Accept.URI != "" && winnerDID(state) != ""
}

func (r *contractRunner) releaseDeniedAccept(ctx context.Context, state *v1alpha1.NodeClaimContract, bidderDID string) error {
	r.captureReleaseRequest()
	reason := cmp.Or(state.Error, "the contract is released")
	for attempt := 1; !r.recoverReceipt(ctx, state); attempt++ {
		if attempt > DeniedAcceptReceiptAttempts {
			r.captureReleaseRequest()
			release := r.releaseStatus()
			if release == nil {
				release = &v1alpha1.ComputeContractRelease{}
			}
			release.LastError = fmt.Sprintf("no receipt arrived for the accept %s after %d waits of %s; the guest the bidder may have provisioned stays until a release finds the receipt: run `socialweb-computer run request release %s` (spec.release) again, or the bidder ends it at its own TTL",
				state.Accept.URI, DeniedAcceptReceiptAttempts, r.q.cfg.ReceiptTimeout, r.name)
			r.setRelease(release)
			r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, reason+"; "+release.LastError, state, r.currentBids(), r.currentChosen(), "releaseFailed", int32(attempt-1))
			return errors.New(release.LastError)
		}
		release := r.releaseStatus()
		if release == nil {
			release = &v1alpha1.ComputeContractRelease{}
		}
		release.Attempts = attempt
		release.LastError = fmt.Sprintf("waiting for the bidder's receipt of the accept to release its guest (attempt %d of %d)", attempt, DeniedAcceptReceiptAttempts)
		r.setRelease(release)
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, reason+"; "+release.LastError, state, r.currentBids(), r.currentChosen(), "releasing", int32(attempt))
		receipt, err := r.q.waitReceiptWithin(ctx, bidderDID, state.Accept.URI, state.Accept.CID, r.q.cfg.ReceiptTimeout)
		if err != nil {
			return err
		}
		if receipt != nil && r.recoverReceipt(ctx, state) {
			break
		}
	}
	return r.releaseContract(ctx, state, bidderDID)
}

func (q *Requester) recordedWinnerAllowed(ctx context.Context, state *v1alpha1.NodeClaimContract, policyName string) error {
	bidderDID := winnerDID(state)
	if bidderDID == "" || state.Policy == nil || state.Policy.URI == "" || state.RFP == nil || state.RFP.URI == "" ||
		state.Bid == nil || state.Bid.URI == "" {
		return errors.New("the recorded contract names no winner, policy, RFP or bid")
	}
	gate, err := q.attachedPolicy(ctx, market.StrongRef{Type: market.StrongRefNSID, URI: state.Policy.URI, CID: state.Policy.CID},
		PolicyWorkflowName(policyName))
	if err != nil {
		return err
	}
	decision := q.policy.Evaluate(ctx, gate.Input(PolicyInput{
		SelfDid:          q.marketDID,
		SubjectDid:       bidderDID,
		CounterpartyDid:  bidderDID,
		RootRequesterDid: q.marketDID,
		RFP:              &PolicyRef{URI: state.RFP.URI, CID: state.RFP.CID},
		Bid:              &PolicyRef{URI: state.Bid.URI, CID: state.Bid.CID},
	}))
	switch {
	case decision.Allow:
		return nil
	case !decision.Completed:
		return retryable(errors.New("the policy run for the recorded winner " + bidderDID + " did not complete: " + decision.Reason))
	}
	return fmt.Errorf("%w: the recorded winner %s: %s", ErrPolicyRejected, bidderDID, decision.Reason)
}

type retryableError struct{ error }

func (e retryableError) Unwrap() error { return e.error }

func retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err}
}

func isRetryable(err error) bool {
	var target retryableError
	return errors.As(err, &target)
}

func (r *contractRunner) untilSettled(ctx context.Context, site string, try func() error) error {
	err := try()
	if err == nil || !isRetryable(err) || ctx.Err() != nil {
		return err
	}
	wake, cancel := r.q.watchBinds()
	defer cancel()
	giveUp := time.Now().Add(TransientResumeGiveUp)
	r.log().Info("requester_contract_waits_transient", "site", site, "err", err.Error(),
		"reason", "a transient start-up failure keeps the contract in its phase; a Vault, MarketIdentity or AtprotoRepo bind or the backoff wakes the next try; no success within "+TransientResumeGiveUp.String()+" fails it")
	for failures := 1; ; failures++ {
		wait := time.NewTimer(netretry.Delay(failures))
		select {
		case <-ctx.Done():
			wait.Stop()
			return err
		case <-r.releaseCh:
			wait.Stop()
			return err
		case <-wake:
			wait.Stop()
		case <-wait.C:
		}
		err = try()
		switch {
		case err == nil || !isRetryable(err):
			r.log().Info("requester_contract_transient_settled", "site", site, "attempts", failures+1, "err", fmt.Sprint(err))
			return err
		case ctx.Err() != nil:
			return err
		case !time.Now().Before(giveUp):
			r.log().Warn("requester_contract_transient_gave_up", "site", site, "attempts", failures+1, "err", err.Error())
			return err
		}
	}
}

func (r *contractRunner) contractPolicyName() string {
	return cmp.Or(strings.TrimSpace(r.snapshot().Spec.Policy), r.q.cfg.PolicyWorkflow, DefaultPolicyWorkflow)
}

func (r *contractRunner) recoverReceipt(ctx context.Context, state *v1alpha1.NodeClaimContract) bool {
	bidderDID := winnerDID(state)
	if state == nil || state.Receipt != nil || state.Accept == nil || state.Accept.URI == "" || bidderDID == "" {
		return false
	}
	receipt, err := r.q.readReceipt(ctx, bidderDID, state.Accept.URI, state.Accept.CID)
	if err != nil || receipt == nil {
		r.log().Info("requester_release_no_receipt", "accept", state.Accept.URI, "bidder", bidderDID, "err", fmt.Sprint(err))
		return false
	}
	accept, err := r.q.repo.Resolve(ctx, market.RecordRef{URI: state.Accept.URI, CID: state.Accept.CID})
	if err != nil {
		r.log().Warn("requester_release_accept_unread", "accept", state.Accept.URI, "err", err)
		return false
	}
	if verification := market.VerifyReceipt(ctx, receipt.Value, receipt.URI, accept.Value, state.Accept.URI, r.q.receiptOptions()); !verification.OK {
		r.log().Warn("requester_release_receipt_invalid", "receipt", receipt.URI)
		return false
	}
	state.Receipt = &commonv1.StrongRef{URI: receipt.URI, CID: receipt.CID}
	state.SubmitEventRef = stringOf(receipt.Value["submitEvent"])
	state.ReceiptOk = new(true)
	r.setState(state)
	r.log().Info("requester_release_found_receipt", "receipt", receipt.URI, "bidder", bidderDID)
	return true
}

func (r *contractRunner) acceptReceipt(ctx context.Context, state *v1alpha1.NodeClaimContract, acceptRecord map[string]any, acceptURI string, receipt *market.ResolvedRecord) error {
	verification := market.VerifyReceipt(ctx, receipt.Value, receipt.URI, acceptRecord, acceptURI, r.q.receiptOptions())
	state.Receipt = &commonv1.StrongRef{URI: receipt.URI, CID: receipt.CID}
	state.SubmitEventRef = stringOf(receipt.Value["submitEvent"])
	state.ReceiptOk = new(verification.OK)
	state.Phase = v1alpha1.ContractReceipt
	state.Timestamps.Receipt = r.q.nowISO()
	if !verification.OK {
		state.Outcome = v1alpha1.ContractOutcomeReceiptInvalid
		state.Error = "the receipt signature or accept binding is invalid"
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, state.Error, state, r.currentBids(), r.currentChosen(), "receiptInvalid", 1)
		return errors.New(state.Error)
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the receipt is verified; waiting for the guest", state, r.currentBids(), r.currentChosen(), "awaitingGuestOnNetwork", 1)
	if state.Termination != nil {
		return nil
	}
	r.ensureAtircToken(ctx, r.snapshot(), state)
	return r.runGuestAndHold(ctx, state, receipt.URI, winnerDID(state))
}

func (r *contractRunner) resumeReceipt(ctx context.Context, state *v1alpha1.NodeClaimContract) error {
	if r.refreshTermination(ctx, state) {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, terminationMessage(state.Termination), state, r.currentBids(), r.currentChosen(), "terminated", 1)
		return nil
	}
	bidderDID := winnerDID(state)
	if r.released() {
		return r.releaseContract(ctx, state, bidderDID)
	}
	if err := r.untilSettled(ctx, "resumeReceipt", func() error { return r.q.recordedWinnerAllowed(ctx, state, r.contractPolicyName()) }); err != nil {
		if r.released() && !errors.Is(err, ErrPolicyRejected) {
			return r.releaseContract(ctx, state, bidderDID)
		}
		if !errors.Is(err, ErrPolicyRejected) {
			return fmt.Errorf("the held contract is not resumed: %w", err)
		}
		state.Outcome = v1alpha1.ContractOutcomePolicyRejected
		state.Error = "the held contract is not resumed: " + err.Error()
		if releaseErr := r.releaseContract(ctx, state, bidderDID); releaseErr != nil {
			return fmt.Errorf("%s; releasing it: %w", state.Error, releaseErr)
		}
		return errors.New(state.Error)
	}
	if r.released() {
		return r.releaseContract(ctx, state, bidderDID)
	}
	r.ensureAssociation(ctx, state.Accept, state)
	receiptRef := market.RecordRef{URI: state.Receipt.URI, CID: state.Receipt.CID}
	verified, err := r.verifyRecordedReceipt(ctx, state, receiptRef)
	if err != nil {
		if state.ReceiptOk == nil || !*state.ReceiptOk {
			return err
		}
	} else if !verified {
		state.ReceiptOk = new(false)
		state.Outcome = v1alpha1.ContractOutcomeReceiptInvalid
		state.Error = "the receipt signature or accept binding is invalid"
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, state.Error, state, r.currentBids(), r.currentChosen(), "receiptInvalid", 1)
		return errors.New(state.Error)
	} else {
		state.ReceiptOk = new(true)
	}
	if r.released() {
		return r.releaseContract(ctx, state, bidderDID)
	}
	if state.Phase != v1alpha1.ContractNetwork && state.Phase != v1alpha1.ContractReleased {
		state.Phase = v1alpha1.ContractReceipt
	}
	if state.Timestamps == nil {
		state.Timestamps = &v1alpha1.ContractTimestamps{}
	}
	if state.Timestamps.Receipt == "" {
		state.Timestamps.Receipt = r.q.nowISO()
	}
	r.ensureAtircToken(ctx, r.snapshot(), state)
	return r.runGuestAndHold(ctx, state, receiptRef.URI, bidderDID)
}

func (r *contractRunner) resumeReleased(ctx context.Context, state *v1alpha1.NodeClaimContract) {
	terminated := r.refreshReleasedTermination(ctx, state)
	message := "released"
	step := "released"
	if terminated {
		message = terminationMessage(state.Termination)
		step = "terminated"
		r.log().Info("requester_contract_termination_resumed", "receipt", state.Receipt.URI, "event", state.Termination.Event.URI, "reason", state.Termination.Reason)
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractReleased, message, state, r.currentBids(), r.currentChosen(), step, 1)
}

func (r *contractRunner) refreshReleasedTermination(ctx context.Context, state *v1alpha1.NodeClaimContract) bool {
	terminated, answered := r.checkTermination(ctx, state)
	if !terminated && answered {
		r.terminationNotReported.Store(true)
	}
	return terminated
}

func (r *contractRunner) refreshTermination(ctx context.Context, state *v1alpha1.NodeClaimContract) bool {
	terminated, _ := r.checkTermination(ctx, state)
	return terminated
}

func (r *contractRunner) checkTermination(ctx context.Context, state *v1alpha1.NodeClaimContract) (bool, bool) {
	if state == nil {
		return false, false
	}
	if state.Termination != nil {
		state.Phase = v1alpha1.ContractReleased
		return true, true
	}
	if state.Receipt == nil || state.Receipt.URI == "" || r.q.market == nil {
		return false, false
	}
	bidderDID := winnerDID(state)
	if bidderDID == "" {
		return false, false
	}
	readCtx, cancel := context.WithTimeout(ctx, DefaultHTTPTimeout)
	defer cancel()
	readStart := time.Now()
	event, err := r.q.market.FindTermination(readCtx, bidderDID, state.Receipt.URI)
	readMs := time.Since(readStart).Milliseconds()
	r.log().Debug("requester_termination_checked", "bidder", bidderDID, "receipt", state.Receipt.URI, "found", event != nil, "readMs", readMs, "err", fmt.Sprint(err))
	r.q.noteFirstResume(r.name, readMs)
	if err != nil {
		if ctx.Err() == nil {
			r.log().Warn("requester_termination_resume_read_failed", "bidder", bidderDID, "receipt", state.Receipt.URI, "err", err)
		}
		return false, false
	}
	if event == nil {
		return false, true
	}
	parsed, err := r.q.parseGuestEvent(readCtx, *event, state.Receipt.URI)
	if err != nil {
		r.log().Warn("requester_termination_resume_refused", "uri", event.Event.URI, "err", err)
		return false, false
	}
	if parsed.Kind != market.ComputeEventsVMTerminatedNSID {
		return false, true
	}
	applyTermination(state, parsed.Termination, r.q.nowISO())
	r.log().Info("requester_termination_resumed", "bidder", bidderDID, "receipt", state.Receipt.URI, "event", parsed.Event.URI, "reason", parsed.Termination.Reason)
	return true, true
}

func (r *contractRunner) verifyRecordedReceipt(ctx context.Context, state *v1alpha1.NodeClaimContract, receiptRef market.RecordRef) (bool, error) {
	receipt, err := r.q.repo.Resolve(ctx, receiptRef)
	if err != nil {
		return false, fmt.Errorf("read the recorded market.receipt record: %w", err)
	}
	if state.SubmitEventRef == "" {
		state.SubmitEventRef = stringOf(receipt.Value["submitEvent"])
	}
	if state.Accept == nil || state.Accept.URI == "" {
		return true, nil
	}
	accept, err := r.q.repo.Resolve(ctx, market.RecordRef{URI: state.Accept.URI, CID: state.Accept.CID})
	if err != nil {
		return false, fmt.Errorf("read the recorded market.accept record: %w", err)
	}
	verification := market.VerifyReceipt(ctx, receipt.Value, receipt.URI, accept.Value, state.Accept.URI, r.q.receiptOptions())
	return verification.OK, nil
}

func (r *contractRunner) collectBids(ctx context.Context, rfpURI string, workflow PolicyGate, only []string) (*BidRecord, []v1alpha1.ContractBid, error) {
	q := r.q
	window := time.NewTimer(max(q.cfg.BidWindow, 0))
	defer window.Stop()
	byURI := map[string]BidRecord{}
	deny := r.snapshot().Spec.DenyBidderDids
	add := func(bids []BidRecord) {
		for _, bid := range bids {
			if bid.URI == "" || slices.Contains(deny, bid.DID) {
				continue
			}
			if len(only) > 0 && !slices.Contains(only, bid.DID) {
				r.log().Info("requester_bid_outside_contract_bidders", "bidder", bid.DID, "bid", bid.URI)
				continue
			}
			byURI[bid.URI] = bid
		}
	}
	var selected *BidRecord
	open := q.cfg.BidWindow > 0
	for selected == nil {
		arrived := q.bids.arrived()
		add(q.bids.take(rfpURI))
		if candidate := r.freeWinner(ctx, byURI, workflow); candidate != nil {
			selected = candidate
			break
		}
		if !open {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-r.releaseCh:
			return nil, nil, nil
		case <-window.C:
			open = false
		case <-arrived:
		}
	}
	add(q.bids.take(rfpURI))
	if selected == nil {
		selected = r.bestBid(ctx, byURI, workflow)
	}
	records := make([]v1alpha1.ContractBid, 0, len(byURI))
	for _, bid := range byURI {
		entry := v1alpha1.ContractBid{
			Did:       bid.DID,
			URI:       bid.URI,
			Verdict:   v1alpha1.ContractBidConsidered,
			ArrivedAt: bid.arrivedAtISO(),
		}
		if !bid.Free {
			entry.Cost = int64(bid.Cost)
		}
		switch {
		case selected != nil && selected.URI == bid.URI:
			entry.Verdict = v1alpha1.ContractBidChosen
		case !q.bidAllowedWithPolicy(ctx, bid, workflow):
			entry.Verdict = v1alpha1.ContractBidPolicyRejected
		case selected != nil:
			entry.Verdict = v1alpha1.ContractBidNotChosen
		default:
			entry.Verdict = v1alpha1.ContractBidDenied
		}
		records = append(records, entry)
		if len(records) >= MaxRecordBids {
			break
		}
	}
	return selected, records, nil
}

func (r *contractRunner) freeWinner(ctx context.Context, bids map[string]BidRecord, workflow PolicyGate) *BidRecord {
	for _, bid := range bids {
		bidCopy := bid
		if err := r.q.classifyBid(ctx, &bidCopy); err != nil {
			continue
		}
		if bidCopy.Free && r.q.bidAllowedWithPolicy(ctx, bidCopy, workflow) {
			return &bidCopy
		}
	}
	return nil
}

func (r *contractRunner) bestBid(ctx context.Context, bids map[string]BidRecord, workflow PolicyGate) *BidRecord {
	var best *BidRecord
	for _, bid := range bids {
		bidCopy := bid
		if err := r.q.classifyBid(ctx, &bidCopy); err != nil {
			continue
		}
		if !r.q.bidAllowedWithPolicy(ctx, bidCopy, workflow) {
			continue
		}
		if best == nil || bidCopy.Cost < best.Cost {
			best = &bidCopy
		}
	}
	return best
}

func (r *contractRunner) waitGuest(ctx context.Context, state *v1alpha1.NodeClaimContract, receiptURI string) error {
	events, pending, closeEvents := r.q.registerEventSink(receiptURI)
	defer closeEvents()
	addressDeadline := time.NewTimer(DefaultVMReadyTimeout)
	defer addressDeadline.Stop()
	hostKeysDeadline := time.NewTimer(DefaultHostKeysTimeout)
	defer hostKeysDeadline.Stop()
	addressLate, hostKeysLate := false, false
	seen := map[string]bool{}
	seedGuestEventSeen(seen, state)
	handle := func(event MarketEvent) bool {
		if guestEventSeen(seen, event) || isContractEventNSID(market.NSIDFromURI(event.Payload.URI)) {
			return false
		}
		parsed, err := r.q.parseGuestEvent(ctx, event, receiptURI)
		if err != nil {
			r.log().Warn("requester_guest_event_refused", "uri", event.Event.URI, "err", err)
			return false
		}
		switch parsed.Kind {
		case market.ComputeEventsVMOnNetworkNSID:
			if parsed.Tag != "" {
				if !recordNetwork(state, parsed) {
					return false
				}
				r.log().Info("requester_guest_network", "tag", parsed.Tag, "address", parsed.Address)
				break
			}
			if state.VMAddress != "" {
				return false
			}
			state.VMAddress = parsed.Address
			state.Phase = v1alpha1.ContractNetwork
			if state.Timestamps == nil {
				state.Timestamps = &v1alpha1.ContractTimestamps{}
			}
			state.Timestamps.Network = r.q.nowISO()
			r.log().Info("requester_guest_on_network", "address", parsed.Address)
		case market.ComputeEventsVMHostKeysNSID:
			if state.HostKeys != nil && guestEventRecorded(state.HostKeys.Event, state.HostKeys.Payload, parsed) {
				return false
			}
			state.HostKeys = &v1alpha1.ContractHostKeys{
				Keys:       contractHostKeys(parsed.HostKeys),
				ObservedAt: parsed.ObservedAt,
				Event:      commonv1.StrongRef{URI: parsed.Event.URI, CID: parsed.Event.CID},
				Payload:    commonv1.StrongRef{URI: parsed.Payload.URI, CID: parsed.Payload.CID},
			}
			r.log().Info("requester_guest_host_keys", "keys", len(parsed.HostKeys))
		case market.ComputeEventsVMTerminatedNSID:
			applyTermination(state, parsed.Termination, r.q.nowISO())
			r.writeStatus(ctx, v1alpha1.ComputeContractReleased, terminationMessage(parsed.Termination), state, r.currentBids(), r.currentChosen(), "terminated", 1)
			return true
		case market.ComputeEventsVMSandboxNSID:
			if parsed.Sandbox == nil {
				return false
			}
			state.Sandbox = parsed.Sandbox
			r.log().Info("requester_guest_sandbox", "exit", parsed.Sandbox.ExitCode, "timedOut", parsed.Sandbox.TimedOut,
				"stdout", parsed.Sandbox.Stdout, "stderr", parsed.Sandbox.Stderr)
		case market.ComputeEventsGuestSecretsRequestedNSID, market.ComputeEventsGuestSecretsAppliedNSID:
			if !applySecretsEvent(state, parsed) {
				return false
			}
			r.log().Info("requester_guest_secrets", "event", payloadKind(parsed.Kind), "via", parsed.SecretsVia)
		case market.ComputeEventsGuestRequesterDIDNSID:
			r.onGuestRequester(ctx, state, parsed)
			return false
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractRequesting, "the guest reported "+payloadKind(parsed.Kind), state, r.currentBids(), r.currentChosen(), "awaitingGuestOnNetwork", 1)
		if parsed.Kind == market.ComputeEventsGuestSecretsRequestedNSID {
			r.sealSecrets(ctx, state)
		}
		return false
	}
	if r.released() || slices.ContainsFunc(pending, handle) {
		return nil
	}
	var retry eventRetry
	defer retry.stop()
	retryC := retry.after(r.q.drainEvents(ctx, r.name, receiptURI))
	for {
		if state.Termination != nil {
			return nil
		}
		if state.VMAddress != "" && state.HostKeys != nil {
			return nil
		}
		if sandboxAddress(state.VMAddress) && state.Sandbox != nil {
			return nil
		}
		if r.released() {
			return nil
		}
		if state.VMAddress == "" && addressLate {
			state.Outcome = v1alpha1.ContractOutcomeNetworkTimeout
			return fmt.Errorf("the guest did not come on network within %s", DefaultVMReadyTimeout)
		}
		if state.VMAddress != "" && state.HostKeys == nil && !sandboxAddress(state.VMAddress) && hostKeysLate {
			return fmt.Errorf("the guest reported no ssh host keys within %s", DefaultHostKeysTimeout)
		}
		if sandboxAddress(state.VMAddress) && state.Sandbox == nil && addressLate {
			return fmt.Errorf("the sandbox reported no result within %s", DefaultVMReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.releaseCh:
			return nil
		case <-addressDeadline.C:
			addressLate = true
		case <-hostKeysDeadline.C:
			hostKeysLate = true
		case event := <-events:
			if handle(event) {
				return nil
			}
		case <-retryC:
			retryC = retry.after(r.q.drainEvents(ctx, r.name, receiptURI))
		case <-r.associationKick:
			r.associationStep(ctx, state)
		}
	}
}

func (r *contractRunner) waitForRelease(ctx context.Context, state *v1alpha1.NodeClaimContract, receiptURI, bidderDID string) error {
	events, pending, closeEvents := r.q.registerEventSink(receiptURI)
	defer closeEvents()
	seen := map[string]bool{}
	seedGuestEventSeen(seen, state)
	handle := func(event MarketEvent) bool {
		if guestEventSeen(seen, event) {
			return false
		}
		nsid := market.NSIDFromURI(event.Payload.URI)
		switch nsid {
		case market.ComputeEventsVMTerminatedNSID, market.ComputeEventsVMOnNetworkNSID,
			market.ComputeEventsGuestSecretsRequestedNSID, market.ComputeEventsGuestSecretsAppliedNSID,
			market.ComputeEventsGuestRequesterDIDNSID:
		default:
			return false
		}
		parsed, err := r.q.parseGuestEvent(ctx, event, receiptURI)
		if err != nil {
			if nsid == market.ComputeEventsVMOnNetworkNSID {
				r.log().Warn("requester_guest_event_refused", "uri", event.Event.URI, "err", err)
			}
			return false
		}
		if parsed.Kind == market.ComputeEventsVMOnNetworkNSID {
			if parsed.Tag != "" && recordNetwork(state, parsed) {
				r.log().Info("requester_guest_network", "tag", parsed.Tag, "address", parsed.Address)
				r.writeStatus(ctx, v1alpha1.ComputeContractHeld, "held: the guest is on the network; set spec.release to release it", state, r.currentBids(), r.currentChosen(), "held", 1)
			}
			return false
		}
		if parsed.Kind == market.ComputeEventsGuestRequesterDIDNSID {
			r.onGuestRequester(ctx, state, parsed)
			return false
		}
		if parsed.Kind == market.ComputeEventsGuestSecretsRequestedNSID || parsed.Kind == market.ComputeEventsGuestSecretsAppliedNSID {
			if applySecretsEvent(state, parsed) {
				r.log().Info("requester_guest_secrets", "event", payloadKind(parsed.Kind), "via", parsed.SecretsVia)
				r.writeStatus(ctx, v1alpha1.ComputeContractHeld, "held: the guest is on the network; set spec.release to release it", state, r.currentBids(), r.currentChosen(), "held", 1)
				if parsed.Kind == market.ComputeEventsGuestSecretsRequestedNSID {
					r.sealSecrets(ctx, state)
				}
			}
			return false
		}
		if parsed.Kind != market.ComputeEventsVMTerminatedNSID {
			return false
		}
		applyTermination(state, parsed.Termination, r.q.nowISO())
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, terminationMessage(parsed.Termination), state, r.currentBids(), r.currentChosen(), "terminated", 1)
		return true
	}
	if slices.ContainsFunc(pending, handle) {
		return nil
	}
	var retry eventRetry
	defer retry.stop()
	retryC := retry.after(r.q.drainEvents(ctx, r.name, receiptURI))
	for {
		if state.Termination != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			if r.q.stopping() {
				r.log().Info("requester_contract_left_for_restart")
				return nil
			}
			return ctx.Err()
		case <-r.releaseCh:
			return r.releaseContract(ctx, state, bidderDID)
		case event := <-events:
			if handle(event) {
				return nil
			}
		case <-retryC:
			retryC = retry.after(r.q.drainEvents(ctx, r.name, receiptURI))
		case <-r.associationKick:
			r.associationStep(ctx, state)
		}
	}
}

func receiptURIOf(state *v1alpha1.NodeClaimContract) string {
	if state == nil {
		return ""
	}
	if state.Receipt != nil && state.Receipt.URI != "" {
		return state.Receipt.URI
	}
	return state.ReceiptURI
}

func (r *contractRunner) finishRelease(ctx context.Context, state *v1alpha1.NodeClaimContract, release *v1alpha1.ComputeContractRelease, message string) {
	state.Release.OK = true
	state.Phase = v1alpha1.ContractReleased
	if state.Timestamps != nil && state.Timestamps.Released == "" {
		state.Timestamps.Released = r.q.nowISO()
	}
	release.LastError = ""
	r.setRelease(release)
	r.log().Info("requester_released", "receipt", receiptURIOf(state), "attempts", release.Attempts, "message", message)
	r.writeStatus(ctx, v1alpha1.ComputeContractReleased, message, state, r.currentBids(), r.currentChosen(), "released", int32(release.Attempts))
	r.deleteContractRecords(ctx)
}

func (r *contractRunner) captureReleaseRequest() {
	requested := r.snapshot().Spec.ReleaseRequestedAt
	r.mu.Lock()
	r.releaseRequested = requested
	r.mu.Unlock()
}

func (r *contractRunner) releaseRequestedAt() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releaseRequested
}

func guestMayRun(state *v1alpha1.NodeClaimContract) bool {
	return state != nil && state.Receipt != nil && state.ReceiptOk != nil && *state.ReceiptOk && state.Termination == nil &&
		(state.Release == nil || !state.Release.OK)
}

func (r *contractRunner) applyReleaseBacklog(ctx context.Context, state *v1alpha1.NodeClaimContract) {
	receiptURI := receiptURIOf(state)
	if receiptURI == "" {
		return
	}
	events, pending, closeEvents := r.q.registerEventSink(receiptURI)
	var skipped []MarketEvent
	defer func() {
		closeEvents()
		for _, event := range skipped {
			r.q.dispatchEvent(receiptURI, event)
		}
	}()
	apply := func(event MarketEvent) {
		switch market.NSIDFromURI(event.Payload.URI) {
		case market.ComputeEventsGuestRequesterDIDNSID, market.ComputeEventsVMTerminatedNSID:
		default:
			skipped = append(skipped, event)
			return
		}
		parsed, err := r.q.parseGuestEvent(ctx, event, receiptURI)
		if err != nil {
			r.log().Warn("requester_guest_event_refused", "uri", event.Event.URI, "err", err)
			return
		}
		switch parsed.Kind {
		case market.ComputeEventsGuestRequesterDIDNSID:
			r.recordGuestRequester(ctx, state, parsed)
		case market.ComputeEventsVMTerminatedNSID:
			if applyTermination(state, parsed.Termination, r.q.nowISO()) {
				r.log().Info("requester_release_found_termination", "event", parsed.Event.URI, "reason", parsed.Termination.Reason)
			}
		}
	}
	for _, event := range pending {
		apply(event)
	}
	_ = r.q.drainEvents(ctx, r.name, receiptURI)
	for {
		select {
		case event := <-events:
			apply(event)
		default:
			return
		}
	}
}

func (r *contractRunner) releaseContract(ctx context.Context, state *v1alpha1.NodeClaimContract, bidderDID string) error {
	q := r.q
	r.captureReleaseRequest()
	if state == nil || state.Receipt == nil {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released before a VM was held", state, r.currentBids(), r.currentChosen(), "released", 1)
		r.deleteContractRecords(ctx)
		return nil
	}
	if state.Release == nil || !state.Release.OK {
		r.applyReleaseBacklog(ctx, state)
	}
	if state.Termination != nil {
		state.Phase = v1alpha1.ContractReleased
		if state.Timestamps != nil && state.Timestamps.Released == "" {
			state.Timestamps.Released = q.nowISO()
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, terminationMessage(state.Termination), state, r.currentBids(), r.currentChosen(), "terminated", 1)
		r.deleteContractRecords(ctx)
		return nil
	}
	if state.Release != nil && state.Release.OK {
		state.Phase = v1alpha1.ContractReleased
		if release := r.releaseStatus(); release != nil {
			release.LastError = ""
			r.setRelease(release)
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "released", state, r.currentBids(), r.currentChosen(), "released", 1)
		r.deleteContractRecords(ctx)
		return nil
	}

	release := r.releaseStatus()
	if release == nil {
		release = &v1alpha1.ComputeContractRelease{}
	}
	if release.StartedAt == "" {
		release.StartedAt = q.nowISO()
	}
	deadline := q.releaseDeadline(r.snapshot())
	if release.Deadline != "" {
		if parsed, err := time.Parse(time.RFC3339, release.Deadline); err == nil {
			deadline = parsed
		}
	} else {
		release.Deadline = deadline.UTC().Format(time.RFC3339)
	}
	r.setRelease(release)

	bidder, ok := q.winnerBidder(ctx, state, bidderDID)
	if !ok {
		bidder, ok = q.bidderForDID(ctx, bidderDID)
	}
	if !ok {
		bidder = Bidder{}
	}

	r.log().Info("requester_release", "receipt", receiptURIOf(state), "bidder", bidderDID, "deadline", release.Deadline, "attempts", release.Attempts)
	r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "destroying the guest", state, r.currentBids(), r.currentChosen(), "releasing", int32(release.Attempts))

	r.releaseGuestRequester(ctx, state)
	r.revokeBeforeDestroy(ctx)
	eventRef, eventRecord, err := r.releaseEnvelope(ctx, state)
	for err != nil {
		release.Attempts++
		release.LastError = err.Error()
		r.setRelease(release)
		r.log().Info("requester_release_retry", "attempt", release.Attempts, "stage", "record", "err", release.LastError)
		message := fmt.Sprintf("the destroy event could not be recorded on attempt %d: %s", release.Attempts, release.LastError)
		if q.now().After(deadline) {
			r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, message+"; the deadline passed, the release intent stays recorded", state, r.currentBids(), r.currentChosen(), "releaseFailed", int32(release.Attempts))
			return fmt.Errorf("the destroy event was not recorded by %s: %w", deadline.UTC().Format(time.RFC3339), err)
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, message+"; retrying", state, r.currentBids(), r.currentChosen(), "releaseFailed", int32(release.Attempts))
		if err := sleepCtx(ctx, releaseRetryDelay(release.Attempts)); err != nil {
			return err
		}
		if r.settleTerminated(ctx, state) {
			return nil
		}
		eventRef, eventRecord, err = r.releaseEnvelope(ctx, state)
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, "the destroy is recorded; sending it", state, r.currentBids(), r.currentChosen(), "releasing", int32(release.Attempts))

	if bidder.Endpoint == "" {
		release.Attempts++
		r.finishRelease(ctx, state, release, "released; the firehose carries the destroy")
		return nil
	}

	for {
		release.Attempts++
		r.setRelease(release)
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, fmt.Sprintf("destroying the guest: attempt %d", release.Attempts), state, r.currentBids(), r.currentChosen(), "releasing", int32(release.Attempts))
		status, err := q.market.SubmitEvent(ctx, bidder, SubmitEventRequest{URI: eventRef.URI, CID: eventRef.CID, Record: eventRecord})
		if err == nil && status >= 200 && status <= 299 {
			state.Release.Status = &status
			r.finishRelease(ctx, state, release, "released")
			return nil
		}
		detail := "no answer"
		if status != 0 {
			detail = fmt.Sprintf("HTTP %d", status)
			state.Release.Status = &status
		}
		if err != nil {
			detail = err.Error()
		}
		release.LastError = detail
		r.setRelease(release)
		r.log().Info("requester_release_retry", "attempt", release.Attempts, "err", detail)
		message := fmt.Sprintf("the destroy failed on attempt %d: %s", release.Attempts, detail)
		if q.now().After(deadline) {
			r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, message+"; the deadline passed, the release intent stays recorded", state, r.currentBids(), r.currentChosen(), "releaseFailed", int32(release.Attempts))
			return fmt.Errorf("the guest was not destroyed by %s: %s", deadline.UTC().Format(time.RFC3339), detail)
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleasing, message+"; retrying", state, r.currentBids(), r.currentChosen(), "releaseFailed", int32(release.Attempts))
		if err := sleepCtx(ctx, releaseRetryDelay(release.Attempts)); err != nil {
			return err
		}
		if r.settleTerminated(ctx, state) {
			return nil
		}
	}
}

func (r *contractRunner) settleTerminated(ctx context.Context, state *v1alpha1.NodeClaimContract) bool {
	if terminated, _ := r.checkTermination(ctx, state); !terminated || state.Termination == nil {
		return false
	}
	if state.Timestamps != nil && state.Timestamps.Released == "" {
		state.Timestamps.Released = r.q.nowISO()
	}
	if release := r.releaseStatus(); release != nil {
		release.LastError = ""
		r.setRelease(release)
	}
	r.log().Info("requester_release_settled_by_termination", "receipt", receiptURIOf(state), "bidder", winnerDID(state), "reason", state.Termination.Reason)
	r.writeStatus(ctx, v1alpha1.ComputeContractReleased, terminationMessage(state.Termination), state, r.currentBids(), r.currentChosen(), "terminated", 1)
	r.deleteContractRecords(ctx)
	return true
}

func (q *Requester) winnerBidder(ctx context.Context, state *v1alpha1.NodeClaimContract, bidderDID string) (Bidder, bool) {
	if state.Winner == nil || state.Winner.DID != bidderDID {
		return Bidder{}, false
	}
	ref := strings.TrimSpace(state.Winner.SubmitAccept)
	if ref == "" {
		return Bidder{}, false
	}
	endpoint, audience, err := resolveBidderEndpoint(ctx, q.deps.Directory, ref)
	if err != nil || endpoint == "" {
		return Bidder{}, false
	}
	return Bidder{DID: bidderDID, Endpoint: endpoint, Audience: audience}, true
}

func sleepCtx(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (q *Requester) releaseDeadline(contract v1alpha1.ComputeContract) time.Time {
	base := contract.CreationTimestamp.Time
	if base.IsZero() {
		base = q.now()
	}
	bound := DefaultReleaseBound
	if ttl := contract.Spec.TTLSecondsAfterFinished; ttl != nil && *ttl > 0 {
		bound = time.Duration(*ttl) * time.Second
	}
	return base.Add(bound)
}

func releaseRetryDelay(attempt int) time.Duration {
	delay := DefaultReleaseRetryFirst
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= DefaultReleaseRetryMax {
			return DefaultReleaseRetryMax
		}
	}
	return min(delay, DefaultReleaseRetryMax)
}

func (r *contractRunner) releaseEnvelope(ctx context.Context, state *v1alpha1.NodeClaimContract) (market.RecordRef, map[string]any, error) {
	if state.Release != nil && state.Release.Event.URI != "" {
		ref := market.RecordRef{URI: state.Release.Event.URI, CID: state.Release.Event.CID}
		resolved, err := r.q.repo.Resolve(ctx, ref)
		if err != nil {
			return market.RecordRef{}, nil, fmt.Errorf("read the recorded destroy event: %w", err)
		}
		return ref, resolved.Value, nil
	}
	envelope, payload, err := r.q.destroyEnvelope(ctx, state.Receipt, "released")
	if err != nil {
		return market.RecordRef{}, nil, err
	}
	state.Release = &v1alpha1.ContractRelease{
		Event:   commonv1.StrongRef{URI: envelope.URI, CID: envelope.CID},
		Payload: commonv1.StrongRef{URI: payload.URI, CID: payload.CID},
	}
	return market.RecordRef{URI: envelope.URI, CID: envelope.CID}, envelope.Record, nil
}

func (q *Requester) destroyEnvelope(ctx context.Context, receipt *commonv1.StrongRef, reason string) (market.SignedRecord, market.SignedRecord, error) {
	payloadRKey := market.ReleasePayloadRKey(receipt.URI)
	payload, foundPayload, err := q.existingSignedRecord(ctx, market.ComputeEventsVMDeleteNSID, payloadRKey)
	if err != nil {
		return market.SignedRecord{}, market.SignedRecord{}, fmt.Errorf("read the destroy payload: %w", err)
	}
	if !foundPayload {
		payloadRecord, err := market.EncodeRecord(market.ComputeEventsVMDelete{Reason: reason, CreatedAt: q.nowISO()})
		if err != nil {
			return market.SignedRecord{}, market.SignedRecord{}, fmt.Errorf("encode the destroy payload: %w", err)
		}
		payload, err = market.CreateSignedRecordAt(ctx, q.repo, q.repoDID, market.ComputeEventsVMDeleteNSID, payloadRKey, payloadRecord, q.signer)
		if err != nil {
			return market.SignedRecord{}, market.SignedRecord{}, fmt.Errorf("create the destroy payload: %w", err)
		}
	}
	eventRKey := sessionRKey("event", receipt.URI)
	envelope, foundEvent, err := q.existingSignedRecord(ctx, market.EventNSID, eventRKey)
	if err != nil {
		return market.SignedRecord{}, market.SignedRecord{}, fmt.Errorf("read the destroy event: %w", err)
	}
	if !foundEvent {
		envelope, err = market.PutEvent(ctx, q.repo, q.repoDID, eventRKey, market.EventOptions{
			Receipt:   market.RecordRef{URI: receipt.URI, CID: receipt.CID},
			Payload:   market.RecordRef{URI: payload.URI, CID: payload.CID},
			CreatedAt: q.nowISO(),
		}, q.signer)
		if err != nil {
			return market.SignedRecord{}, market.SignedRecord{}, fmt.Errorf("create the destroy event: %w", err)
		}
	}
	return envelope, payload, nil
}

func (r *contractRunner) runAdopt(ctx context.Context, contract v1alpha1.ComputeContract) {
	state := r.stateCopy()
	if state == nil || state.Receipt == nil {
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "adopt: the contract status holds no bidder-signed receipt", state, nil, nil, "adopting", 1)
		return
	}
	terminated, answered := r.checkTermination(ctx, state)
	if terminated {
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "adopt: the bidder reported the guest terminated ("+state.Termination.Reason+")", state, nil, nil, "adopted", 1)
		return
	}
	if contract.Status.Phase == v1alpha1.ComputeContractReleased {
		if answered {
			r.terminationNotReported.Store(true)
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractReleased, "adopt: the contract is already released", state, nil, nil, "adopted", 1)
		return
	}
	if state.HostKeys == nil {
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "adopt: the contract status holds no bidder-signed vm.hostKeys", state, nil, nil, "adopting", 1)
		return
	}
	if err := r.untilSettled(ctx, "adopt", func() error { return r.q.recordedWinnerAllowed(ctx, state, r.contractPolicyName()) }); err != nil {
		if r.q.stopping() {
			return
		}
		if errors.Is(err, ErrPolicyRejected) {
			state.Outcome = v1alpha1.ContractOutcomePolicyRejected
			state.Error = "adopt: " + err.Error()
			if releaseErr := r.releaseContract(ctx, state, winnerDID(state)); releaseErr != nil && !r.q.stopping() {
				r.recordFailure(ctx, state, fmt.Errorf("%s; releasing it: %w", state.Error, releaseErr), 1)
			}
			return
		}
		r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "adopt: "+err.Error(), state, nil, nil, "adopting", 1)
		return
	}
	state.Phase = v1alpha1.ContractNetwork
	r.writeStatus(ctx, v1alpha1.ComputeContractHeld, "adopted: no RFP was sent and no vm.delete was submitted", state, nil, nil, "adopted", 1)
	if err := r.waitForRelease(ctx, state, state.Receipt.URI, winnerDID(state)); err != nil && !r.q.stopping() {
		r.recordFailure(ctx, r.stateCopy(), err, 1)
	}
}

func (r *contractRunner) writeStatus(ctx context.Context, phase v1alpha1.ComputeContractPhase, message string, state *v1alpha1.NodeClaimContract, bids []v1alpha1.ContractBid, chosen *v1alpha1.ContractChosen, step string, attempts int32) error {
	if r.isGone() {
		return errRunnerGone
	}
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
			StepStartedAt: r.q.nowISO(),
			LastEventAt:   r.q.nowISO(),
			Attempts:      attempts,
		},
		Contract:         cloneNodeClaimContract(state),
		SecretsURL:       r.secretsURLValue(),
		SecretsFetchedAt: r.secretsFetched(),
		Grant:            r.grantValue(),
	}
	if release := r.releaseStatus(); release != nil {
		status.Release = release
	} else if contract.Status.Release != nil {
		preserved := *contract.Status.Release
		status.Release = &preserved
	}
	if requested := r.releaseRequestedAt(); requested != "" && status.Release != nil &&
		(phase == v1alpha1.ComputeContractReleased || phase == v1alpha1.ComputeContractFailed) {
		status.Release.RequestedAt = requested
	}
	r.applyConditions(&status, contract.Generation, phase, message, state)
	var startRevokes func()
	if phase == v1alpha1.ComputeContractReleased || (phase == v1alpha1.ComputeContractFailed && !guestMayRun(state)) {
		r.atircRevoke.Do(func() { status.AtircRevokes, startRevokes = r.revokeAtircToken(ctx) })
	}
	err := r.q.deps.Intents.MergeComputeContractStatus(ctx, r.name, status)
	if err != nil && !notFound(err) {
		r.log().Warn("requester_contract_status_failed", "err", err)
	}
	if startRevokes != nil {
		startRevokes()
	}
	if phase == v1alpha1.ComputeContractReleased {
		r.q.dropUndelivered(receiptURIOf(state))
	}
	return err
}

var errRunnerGone = errors.New("requester: the contract runner is gone")

func notFound(err error) bool {
	return err != nil && (errors.Is(err, contractmarket.ErrNotFound) || strings.Contains(err.Error(), "not found"))
}

func (r *contractRunner) applyConditions(status *v1alpha1.ComputeContractStatus, generation int64, phase v1alpha1.ComputeContractPhase, message string, state *v1alpha1.NodeClaimContract) {
	now := r.q.now()
	addContractConditions(status, generation, phase, message, now, false)
	if r.resumed {
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionResumed, metav1.ConditionTrue, "Recovered", "the recorded status.contract was resumed", now)
	}
	if phase == v1alpha1.ComputeContractReleasing || phase == v1alpha1.ComputeContractFailed {
		if release := r.releaseStatus(); release != nil && release.LastError != "" {
			conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionReleaseFailed, metav1.ConditionTrue, "DestroyFailed", release.LastError, now)
		}
	}
	if phase == v1alpha1.ComputeContractReleased {
		acknowledged := state != nil && ((state.Release != nil && state.Release.OK) || state.Termination != nil)
		if acknowledged {
			conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionReleaseAcknowledged, metav1.ConditionTrue, "Acknowledged", message, now)
			conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionReleaseFailed, metav1.ConditionFalse, "Acknowledged", message, now)
		}
	}
	if state != nil && state.R2Release != nil {
		rel := state.R2Release
		ok := rel.Phase == v1alpha1.ContractR2ReleaseReleased
		message := "the guest requester's release is " + string(rel.Phase)
		if rel.Message != "" {
			message += ": " + rel.Message
		}
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionGuestRequesterReleased, conds.Status(ok), string(rel.Phase), message, now)
	}
	switch {
	case state != nil && state.SecretsApplied != nil && state.SecretsApplied.Via == market.GuestSecretsViaSealed:
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionSecretsApplied, metav1.ConditionTrue, "SealedFallback",
			"the guest wrote its secrets from the sealed fallback (compute.events.guest.secrets)", now)
	case state != nil && state.SecretsApplied != nil:
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionSecretsApplied, metav1.ConditionTrue, "GetSecrets",
			"the guest wrote its secrets from the requester's getSecrets route", now)
	case r.secretsFetched() != "":
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionSecretsApplied, metav1.ConditionTrue, "GetSecrets",
			"the guest fetched its secrets from the requester's getSecrets route", now)
	case secretsRequestPending(state):
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionSecretsApplied, metav1.ConditionFalse, "SealedFallbackRequested",
			"the guest asked for its secrets sealed to its key; getSecrets did not answer", now)
	}
	switch {
	case state != nil && state.Termination != nil:
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionTerminated, metav1.ConditionTrue, "BidderTerminated", terminationMessage(state.Termination), now)
	case phase == v1alpha1.ComputeContractReleased && r.terminationNotReported.Load():
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionTerminated, metav1.ConditionFalse, market.TerminationNotReportedReason,
			"the bidder wrote no vm.terminated for the receipt by the first start after the release; it is not read again", now)
	}
	if mint := r.agentMintOutcome(); mint != nil {
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionAtircTokenMinted, conds.Status(mint.ok), mint.reason, mint.message, now)
	}
}

func (r *contractRunner) step(ctx context.Context, step string, attempts int32) {
	state := r.stateCopy()
	r.writeStatus(ctx, r.phaseOf(state), "step "+step, state, r.currentBids(), r.currentChosen(), step, attempts)
}

func (r *contractRunner) phaseOf(state *v1alpha1.NodeClaimContract) v1alpha1.ComputeContractPhase {
	if r.q.stopping() {
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
	chosen := *r.chosen
	return &chosen
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

func (r *contractRunner) sealSecrets(ctx context.Context, state *v1alpha1.NodeClaimContract) {
	if r.released() {
		r.log().Info("requester_guest_secrets_not_sealed", "reason", "the release is requested")
		return
	}
	contract := r.snapshot()
	contract.Status.Contract = cloneNodeClaimContract(state)
	if grant := r.grantValue(); grant != nil {
		contract.Status.Grant = grant
	}
	if err := r.q.sealGuestSecrets(ctx, contract); err != nil && ctx.Err() == nil {
		r.log().Warn("requester_guest_secrets_seal_failed", "err", err)
	}
}

func payloadKind(nsid string) string {
	switch nsid {
	case market.ComputeEventsVMOnNetworkNSID:
		return "onNetwork"
	case market.ComputeEventsVMHostKeysNSID:
		return "hostKeys"
	case market.ComputeEventsVMTerminatedNSID:
		return "terminated"
	case market.ComputeEventsGuestSecretsRequestedNSID:
		return "secretsRequested"
	case market.ComputeEventsGuestSecretsAppliedNSID:
		return "secretsApplied"
	case market.ComputeEventsGuestRequesterDIDNSID:
		return "requesterDid"
	default:
		return nsid
	}
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
	for line := range strings.SplitSeq(pem, "\n") {
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
