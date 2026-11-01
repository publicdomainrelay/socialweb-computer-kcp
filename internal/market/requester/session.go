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

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const (
	heldContractTTLSeconds = 691200

	transientContractTTLSeconds = 3600
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
}

func (q *Requester) RunCommand(ctx context.Context, account sshtransport.AuthorizedAccount, command string, env map[string]string, io sshtransport.CommandIO) error {
	if io == nil {
		return errors.New("requester: the session has no command io")
	}
	if strings.TrimSpace(account.DID) != q.repoDID {
		return commandFailed(io, "provisioning failed: ", fmt.Errorf("this requester rents only for %s, not for %s", q.repoDID, account.DID))
	}
	lc := lcEnv(env)
	atirc := sessionWantsAtirc(command, lc)
	delete(lc, SessionAtircEnv)
	delete(lc, sessionAtircTokenEnv)
	if name := strings.TrimSpace(lc["LC_SESSION"]); name != "" {
		return q.runHeldCommand(ctx, account, name, command, lc, io)
	}
	return q.runTransientCommand(ctx, account, command, lc, atirc, io)
}

func commandFailed(io sshtransport.CommandIO, prefix string, err error) error {
	io.WriteErr([]byte(prefix + err.Error() + "\n"))
	io.Exit(1)
	return err
}

func (q *Requester) sessionPolicy(lc map[string]string) (string, map[string]string, error) {
	policyName := strings.TrimSpace(lc["LC_POLICY"])
	switch strings.ToLower(policyName) {
	case "none", "off":
		return "", nil, fmt.Errorf("LC_POLICY %q: a policy always runs; name one, or open", policyName)
	}
	policyName = cmp.Or(policyName, q.cfg.PolicyWorkflow, DefaultPolicyWorkflow)
	policyArgs := map[string]string{}
	if raw := strings.TrimSpace(lc["LC_POLICY_ARGS"]); raw != "" {
		parsed, err := participant.ParsePolicyArgs(raw)
		if err != nil {
			return "", nil, fmt.Errorf("LC_POLICY_ARGS: %w", err)
		}
		policyArgs = parsed
	}
	return policyName, policyArgs, nil
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

func (q *Requester) runTransientCommand(ctx context.Context, account sshtransport.AuthorizedAccount, command string, lc map[string]string, atirc bool, io sshtransport.CommandIO) error {
	policyName, policyArgs, err := q.sessionPolicy(lc)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	vmName, err := q.sessionVMName(lc)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	sessionCtx, cancel := context.WithTimeout(ctx, q.cfg.SessionCap)
	defer cancel()
	ctx = sessionCtx
	contractName := transientContractName(account.DID, vmName)
	defer q.trackTransient(contractName, cancel)()
	publicKey, privateKeyPath, err := sshtransport.GenerateGuestKeypair(q.guestKeyDir(), vmName)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	defer sshtransport.RemoveGuestKeypair(privateKeyPath)
	agent := ""
	if sub, ok := sessionAgentRun(command); ok && atirc {
		if agent, err = agentSubhandle(sub, q.handle); err != nil {
			return commandFailed(io, "atirc agent run: ", err)
		}
	}
	userData := ""
	if atirc {
		if userData, err = sessionAtircUserData(defaultUserData([]string{publicKey})); err != nil {
			return commandFailed(io, "provisioning failed: ", fmt.Errorf("the guest atirc agent: %w", err))
		}
	}
	keep := truthy(lc["LC_KEEP_VM"])
	var labels map[string]string
	if keep {
		labels = map[string]string{keepVMLabel: "true"}
	}
	target, err := q.provisionHeldContract(ctx, contractName, vmName, account.DID, publicKey, policyName, policyArgs, userData, transientContractTTLSeconds, labels)
	if err != nil {
		q.releaseHeldContract(contractName)
		return commandFailed(io, "provisioning failed: ", err)
	}
	if !keep {
		defer q.releaseHeldContract(contractName)
	}
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
	minted, err := q.sessionAtirc(ctx, account, atirc, agent, q.contractState(contractName), io)
	if err != nil {
		io.Exit(1)
		return err
	}
	defer q.revokeSessionAtirc(ctx, minted)
	switch {
	case minted != nil && agent != "":
		command = atircCommand(sessionAgentCommand)
	case minted != nil:
		command = atircCommand(command)
	}
	code := provider.RunSessionEnv(ctx, privateKeyPath, ticket, renderExecCommand(command, lc), minted.env(), io, target.HostKeys)
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

func (h *heldSession) stopTimer() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.generation++
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
	policyName, policyArgs, err := q.sessionPolicy(lc)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
	vmName, err := q.sessionVMName(lc)
	if err != nil {
		return commandFailed(io, "provisioning failed: ", err)
	}
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
		target, err := q.provisionHeldContract(ctx, contractName, vmName, account.DID, publicKey, policyName, policyArgs, "", heldContractTTLSeconds, nil)
		if err != nil {
			q.releaseHeldContract(contractName)
			_ = sshtransport.RemoveGuestKeypair(privateKeyPath)
			return nil, "", err
		}
		return target, privateKeyPath, nil
	}
	key := account.DID + "\x00" + name
	held := q.heldSession(key)
	var reused bool
	var target *SessionTarget
	var privateKeyPath, contract string
	for {
		held.mu.Lock()
		held.stopTimer()
		reused = held.target != nil
		target, privateKeyPath, contract = held.target, held.privateKeyPath, held.contract
		if !reused {
			break
		}
		held.mu.Unlock()
		policyCtx, policyCancel := context.WithTimeout(ctx, HeldSessionPolicyTimeout)
		err := q.heldSessionAllowed(policyCtx, contract, policyName)
		policyCancel()
		held.mu.Lock()
		if held.target != target || held.contract != contract {
			held.mu.Unlock()
			held = q.heldSession(key)
			continue
		}
		held.stopTimer()
		if err == nil {
			break
		}
		q.log.Info("requester_held_session_policy_denied", "contract", contract, "err", err)
		if errors.Is(err, ErrPolicyRejected) {
			held.clear()
			q.releaseHeldSessionTarget(contract, privateKeyPath)
		}
		held.mu.Unlock()
		return commandFailed(io, "policy denied: ", err)
	}
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
	code := func() int {
		defer held.mu.Unlock()
		return provider.RunSession(ctx, privateKeyPath, ticket, renderExecCommand(command, lc), io, target.HostKeys)
	}()
	q.armHeldSession(key, held)
	io.Exit(code)
	return nil
}

func transientContractName(did, vmName string) string {
	sum := sha256.Sum256([]byte(did + "\x00" + vmName))
	return transientContractPrefix + hex.EncodeToString(sum[:10])
}

func (q *Requester) trackTransient(name string, cancel context.CancelFunc) func() {
	q.transientMu.Lock()
	if q.transient == nil {
		q.transient = map[string]context.CancelFunc{}
	}
	q.transient[name] = cancel
	q.transientWG.Add(1)
	q.transientMu.Unlock()
	return func() {
		q.transientMu.Lock()
		delete(q.transient, name)
		q.transientMu.Unlock()
		q.transientWG.Done()
	}
}

func (q *Requester) transientLive(name string) bool {
	q.transientMu.Lock()
	defer q.transientMu.Unlock()
	_, ok := q.transient[name]
	return ok
}

func (q *Requester) endTransientSessions(ctx context.Context, reason string) {
	q.transientMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(q.transient))
	for _, cancel := range q.transient {
		cancels = append(cancels, cancel)
	}
	q.transientMu.Unlock()
	if len(cancels) == 0 {
		return
	}
	q.log.Info("requester_transient_sessions_ended", "sessions", len(cancels), "reason", reason)
	for _, cancel := range cancels {
		cancel()
	}
	_ = supervise.Join(ctx, q.transientWG.Wait)
}

func (q *Requester) releaseStaleTransient() {
	started := q.startedAt.Load()
	for _, contract := range q.contracts.List() {
		switch {
		case !strings.HasPrefix(contract.Name, transientContractPrefix), !q.ownsContract(contract),
			contract.DeletionTimestamp != nil, contract.Spec.Release, contract.Labels[keepVMLabel] == "true",
			contract.Status.Phase == v1alpha1.ComputeContractReleased, contract.Status.Phase == v1alpha1.ComputeContractFailed,
			started != nil && !contract.CreationTimestamp.Time.Before(*started), q.transientLive(contract.Name):
			continue
		}
		q.log.Info("requester_stale_session_release", "contract", contract.Name, "phase", contract.Status.Phase,
			"reason", "no live ssh session holds this transient contract after a requester start")
		q.releaseHeldContract(contract.Name)
	}
}

func (q *Requester) contractState(name string) *v1alpha1.NodeClaimContract {
	contract, ok := q.contracts.Get(name)
	if !ok {
		return nil
	}
	return contract.Status.Contract
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

func (q *Requester) heldSessionAllowed(ctx context.Context, contract, policyName string) error {
	held, ok := q.contracts.Get(contract)
	if !ok || held.Status.Contract == nil {
		return errors.New("the held session contract " + contract + " is not known")
	}
	return q.recordedWinnerAllowed(ctx, held.Status.Contract, policyName)
}

func (q *Requester) provisionHeldContract(ctx context.Context, name, vmName, did, publicKey, policyName string, policyArgs map[string]string,
	userData string, ttlSeconds int32, labels map[string]string) (*SessionTarget, error) {
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
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: v1alpha1.ComputeContractSpec{
			ComputeContractTemplate: v1alpha1.ComputeContractTemplate{VMName: vmName, SshAuthorizedKeys: []string{publicKey},
				Policy: policyName, PolicyArgs: args, UserData: userData},
			OwnerDid:                did,
			TTLSecondsAfterFinished: new(ttlSeconds),
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
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(q.runContext()), DefaultReleaseTimeout)
		err := q.deps.Intents.ReleaseComputeContract(ctx, name)
		cancel()
		if err == nil || errors.Is(err, contractmarket.ErrNotFound) {
			if attempt > 1 {
				q.log.Info("requester_held_contract_released", "contract", name, "attempt", attempt)
			}
			return
		}
		q.log.Warn("requester_held_contract_release_failed", "contract", name, "attempt", attempt, "err", err)
		if attempt >= heldReleaseAttempts {
			q.log.Warn("requester_held_contract_release_left", "contract", name, "reason", "the next requester start releases it")
			return
		}
		time.Sleep(min(time.Second<<(attempt-1), heldReleaseBackoffMax))
	}
}

func (q *Requester) releaseHeldSessionTarget(contract, privateKeyPath string) {
	if contract != "" {
		q.releaseHeldContract(contract)
	}
	_ = sshtransport.RemoveGuestKeypair(privateKeyPath)
}

func (q *Requester) sessionBestBid(ctx context.Context, rfp market.SignedRecord, bids map[string]BidRecord, workflow PolicyGate, full map[string]bool, limit int) *BidRecord {
	fullKey := func(bid BidRecord) string {
		return strings.Join([]string{bid.URI, bid.CID, workflow.Key(), rfp.URI, rfp.CID}, "|")
	}
	var best *BidRecord
	for denied := 0; ; {
		if limit > 0 && denied >= limit {
			return nil
		}
		best = nil
		for _, bid := range bids {
			candidate := bid
			if err := q.classifyBid(ctx, &candidate); err != nil {
				continue
			}
			if allowed, done := full[fullKey(candidate)]; done && !allowed {
				continue
			}
			if !q.bidAllowedWithPolicy(ctx, candidate, workflow) {
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
		if best == nil {
			return nil
		}
		key := fullKey(*best)
		if _, done := full[key]; !done {
			decision := q.policy.Evaluate(ctx, workflow.Input(PolicyInput{
				SelfDid:          q.marketDID,
				SubjectDid:       best.DID,
				CounterpartyDid:  best.DID,
				RootRequesterDid: q.marketDID,
				RFP:              &PolicyRef{URI: rfp.URI, CID: rfp.CID},
				Bid:              &PolicyRef{URI: best.URI, CID: best.CID},
			}))
			full[key] = decision.Allow
			if !decision.Allow {
				denied++
				q.log.Info("requester_bid_policy_denied", "bidder", best.DID, "bid", best.URI, "mode", participant.PolicyModeFull, "reason", decision.Reason)
			}
		}
		if full[key] {
			break
		}
	}
	return best
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
