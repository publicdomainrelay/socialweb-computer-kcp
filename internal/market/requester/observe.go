package requester

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

func (q *Requester) Observe(ctx context.Context, change contractmarket.Change) (time.Duration, error) {
	switch change.Kind {
	case contractmarket.KindComputeContract:
		if change.Gone || change.ComputeContract == nil {
			q.contractGone(change.Name)
			return 0, nil
		}
		contract := *change.ComputeContract
		q.contracts.Put(change.Name, contract)
		q.observePending(contract)
		q.reconcileContract(ctx, contract)
		mergeErr := q.mergeInboxFor(ctx, contract)
		err := errors.Join(mergeErr, q.processPendingEvents(ctx, change.Name))
		if contract.DeletionTimestamp == nil {
			return 0, err
		}
		return q.releaseWait(ctx, contract), err
	case contractmarket.KindStateSync:
		if change.Gone || change.StateSync == nil {
			if old, ok := q.syncs.Get(change.Name); ok {
				q.inboxed.Delete(string(old.UID) + "#" + old.Spec.IdempotencyKey)
			}
			q.syncs.Remove(change.Name)
			return 0, nil
		}
		return q.observeSync(ctx, *change.StateSync)
	}
	return 0, nil
}

func (q *Requester) observeSync(ctx context.Context, item v1alpha1.StateSync) (time.Duration, error) {
	q.syncs.Put(item.Name, item)
	var wait time.Duration
	if q.outbox.Handles(&item) {
		next, err := q.outbox.Reconcile(ctx, &item)
		if err != nil {
			return 0, err
		}
		wait = next
	}
	if item.DeletionTimestamp != nil {
		return wait, nil
	}
	switch item.Spec.Sink.Adapter {
	case BidInboxAdapter:
		if retry := q.processBidMailbox(ctx, item); retry > 0 && (wait == 0 || retry < wait) {
			wait = retry
		}
	case InboxAdapter:
		if err := q.mergeInboxMailbox(ctx, item); err != nil {
			return wait, err
		}
	}
	return wait, nil
}

func (q *Requester) forgetContract(name string) {
	q.mu.Lock()
	delete(q.finished, name)
	delete(q.waiting, name)
	delete(q.settled, name)
	q.mu.Unlock()
}

func (q *Requester) contractGone(name string) {
	last, known := q.contracts.Get(name)
	q.mu.Lock()
	runner := q.runners[name]
	settled := q.settled[name]
	q.mu.Unlock()
	q.contracts.Remove(name)
	q.forgetContract(name)
	q.forgetPending(name)
	if runner != nil {
		runner.stop()
	}
	if known && !settled && contractAccepted(last) && last.Status.Phase != v1alpha1.ComputeContractReleased {
		q.log.Warn("requester_contract_gone_unreleased", "contract", name, "phase", string(last.Status.Phase), "receipt", receiptURIOf(last.Status.Contract))
	}
}

func contractAccepted(contract v1alpha1.ComputeContract) bool {
	state := contract.Status.Contract
	return state != nil && (state.AcceptURI != "" || (state.Accept != nil && state.Accept.URI != "") || state.Receipt != nil)
}

func (q *Requester) releaseWait(ctx context.Context, contract v1alpha1.ComputeContract) time.Duration {
	name := contract.Name
	q.mu.Lock()
	running := q.runners[name] != nil
	finished := q.finished[name]
	q.mu.Unlock()
	release := contract.Status.Release
	deadline, started := releaseDeadlineOf(release)
	if !started {
		deadline = q.releaseDeadline(contract)
	}
	left := deadline.Sub(q.now())
	attempts := 0
	if release != nil {
		attempts = release.Attempts
	}
	switch {
	case contract.Status.Phase == v1alpha1.ComputeContractReleased:
	case started && left <= 0 && (finished || attempts > 0):
		q.failAtBound(ctx, contract, deadline)
	case running:
		if left > 0 {
			return left
		}
		return releaseRetryDelay(max(attempts, 1))
	case !contractAccepted(contract):
	case finished && (!started || contract.Status.Phase == v1alpha1.ComputeContractFailed):
	case left > 0:
		return left
	default:
		return DefaultReleaseRetryFirst
	}
	q.mu.Lock()
	q.settled[name] = true
	q.mu.Unlock()
	return 0
}

func releaseDeadlineOf(release *v1alpha1.ComputeContractRelease) (time.Time, bool) {
	if release == nil || release.Deadline == "" {
		return time.Time{}, false
	}
	deadline, err := time.Parse(time.RFC3339, release.Deadline)
	return deadline, err == nil
}

func (q *Requester) failAtBound(ctx context.Context, contract v1alpha1.ComputeContract, deadline time.Time) {
	if contract.Status.Phase == v1alpha1.ComputeContractFailed {
		return
	}
	message := fmt.Sprintf("vm.delete was not acknowledged by %s (the contract TTL)", deadline.UTC().Format(time.RFC3339))
	if last := contract.Status.Release.LastError; last != "" {
		message += ": " + last
	}
	status := contract.Status
	status.ObservedGeneration = contract.Generation
	status.Phase = v1alpha1.ComputeContractFailed
	status.Message = message
	addContractConditions(&status, contract.Generation, status.Phase, message, q.now(), false)
	q.log.Warn("requester_release_bound", "contract", contract.Name, "deadline", deadline.UTC().Format(time.RFC3339), "lastError", contract.Status.Release.LastError)
	q.mergeStatus(ctx, contract.Name, status)
}

func (q *Requester) runnerFor(name string) *contractRunner {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.runners[name]
}

func contractTerminal(contract v1alpha1.ComputeContract) bool {
	phase := cmp.Or(contract.Status.Phase, v1alpha1.ComputeContractPending)
	terminal := phase == v1alpha1.ComputeContractReleased
	if phase == v1alpha1.ComputeContractFailed {
		terminal = contract.Status.Contract == nil || contract.Status.Contract.Receipt == nil
	}
	if terminal {
		state := contract.Status.Contract
		if state != nil && state.Receipt != nil && state.Termination == nil && winnerDID(state) != "" {
			terminal = false
		}
	}
	return terminal
}

func (q *Requester) reconcileContract(ctx context.Context, contract v1alpha1.ComputeContract) {
	q.reconcileMu.Lock()
	defer q.reconcileMu.Unlock()
	if q.stopping() {
		return
	}
	q.mu.Lock()
	runner := q.runners[contract.Name]
	finished := q.finished[contract.Name]
	q.mu.Unlock()
	if runner != nil {
		runner.Update(contract)
		return
	}
	if finished {
		return
	}
	if contractTerminal(contract) {
		q.markFinished(contract.Name)
		return
	}
	state := contract.Status.Contract
	held := state != nil && (state.Receipt != nil || (state.Accept != nil && state.Accept.URI != "" && winnerDID(state) != ""))
	if !held && (contract.Spec.Release || contract.DeletionTimestamp != nil) {
		if contract.DeletionTimestamp == nil {
			status := contract.Status
			status.Phase = v1alpha1.ComputeContractReleased
			status.Message = "the contract was released before it started"
			addContractConditions(&status, contract.Generation, status.Phase, status.Message, q.now(), false)
			q.mergeStatus(ctx, contract.Name, status)
		}
		q.markFinished(contract.Name)
		return
	}
	account := contract.Spec.OwnerDid
	q.mu.Lock()
	admit, reason := q.admitLocked(account)
	if !admit {
		waiting := q.waiting[contract.Name]
		q.waiting[contract.Name] = true
		q.mu.Unlock()
		if !waiting && contract.Status.Phase != v1alpha1.ComputeContractReleased {
			q.log.Info("requester_contract_waiting", "contract", contract.Name, "reason", reason)
			status := contract.Status
			status.ObservedGeneration = contract.Generation
			status.Phase = v1alpha1.ComputeContractPending
			status.Message = reason
			addContractConditions(&status, contract.Generation, status.Phase, reason, q.now(), true)
			q.mergeStatus(ctx, contract.Name, status)
		}
		return
	}
	delete(q.waiting, contract.Name)
	q.running++
	if account != "" {
		q.perAccount[account]++
	}
	started := newContractRunner(q, contract)
	if contract.Spec.Release || contract.DeletionTimestamp != nil {
		started.requestRelease()
	}
	q.runners[contract.Name] = started
	runCtx, cancel := context.WithCancel(q.ctx)
	started.cancel = cancel
	q.wg.Go(func() {
		started.Run(runCtx)
		cancel()
		q.runnerDone(started, account)
	})
	q.mu.Unlock()
}

func (q *Requester) markFinished(name string) {
	q.mu.Lock()
	q.finished[name] = true
	q.mu.Unlock()
}

func (q *Requester) mergeStatus(ctx context.Context, name string, status v1alpha1.ComputeContractStatus) {
	status.PendingEvents = nil
	if err := q.deps.Intents.MergeComputeContractStatus(ctx, name, status); err != nil {
		q.log.Warn("requester_contract_status_failed", "contract", name, "err", err)
	}
}

func (q *Requester) admitLocked(account string) (bool, string) {
	if maxConcurrent := int(q.cfg.Spec.MaxConcurrent); q.running >= maxConcurrent {
		return false, fmt.Sprintf("waiting for admission: %d of %d contracts are running", q.running, maxConcurrent)
	}
	if perAccount := int(q.cfg.Spec.PerAccount); account != "" && q.perAccount[account] >= perAccount {
		return false, fmt.Sprintf("this account has %d of %d contracts running", q.perAccount[account], perAccount)
	}
	return true, ""
}

func (q *Requester) runnerDone(runner *contractRunner, account string) {
	q.mu.Lock()
	if current, ok := q.runners[runner.name]; ok && current == runner {
		gone := runner.isGone()
		delete(q.runners, runner.name)
		if q.running > 0 {
			q.running--
		}
		if account != "" {
			if q.perAccount[account] <= 1 {
				delete(q.perAccount, account)
			} else {
				q.perAccount[account]--
			}
		}
		if !gone {
			q.finished[runner.name] = true
		}
	}
	q.mu.Unlock()
	if q.stopping() {
		return
	}
	ctx := q.runContext()
	for _, contract := range q.contracts.List() {
		q.reconcileContract(ctx, contract)
	}
}

func contractPhaseDone(phase v1alpha1.ComputeContractPhase) bool {
	return phase == v1alpha1.ComputeContractReleased || phase == v1alpha1.ComputeContractFailed
}

func addContractConditions(status *v1alpha1.ComputeContractStatus, generation int64, phase v1alpha1.ComputeContractPhase, message string, now time.Time, atCapacity bool) {
	if atCapacity {
		conds.SetStatusAt(&status.Conditions, generation, "AtCapacity", metav1.ConditionTrue, "AdmissionFull", message, now)
	} else {
		meta.RemoveStatusCondition(&status.Conditions, "AtCapacity")
	}
	progressing := phase == v1alpha1.ComputeContractPending || phase == v1alpha1.ComputeContractRequesting || phase == v1alpha1.ComputeContractReleasing
	if progressing {
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionProgressing, metav1.ConditionTrue, "Working", message, now)
	} else {
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionProgressing, metav1.ConditionFalse, "Settled", message, now)
	}
	if phase == v1alpha1.ComputeContractHeld {
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionHeld, metav1.ConditionTrue, "Held", message, now)
	} else {
		conds.SetStatusAt(&status.Conditions, generation, v1alpha1.ComputeContractConditionHeld, metav1.ConditionFalse, "NotHeld", message, now)
	}
	switch phase {
	case v1alpha1.ComputeContractReleased:
		conds.SetStatusAt(&status.Conditions, generation, "Complete", metav1.ConditionTrue, "Released", message, now)
	case v1alpha1.ComputeContractFailed:
		conds.SetStatusAt(&status.Conditions, generation, "Failed", metav1.ConditionTrue, "Failed", message, now)
	}
}
