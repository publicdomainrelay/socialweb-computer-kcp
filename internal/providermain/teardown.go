package providermain

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
)

const (
	TeardownFreeAttempts = 6

	TeardownBackoffBase = 4 * time.Second

	TeardownBackoffMax = 5 * time.Minute

	TeardownBlockedAfter = 3 * time.Minute

	ReasonTeardownStepFailing = "TeardownStepFailing"
)

type deleteBackoff struct {
	attempts int
	since    time.Time
	next     time.Time
}

func (p *Provider) deleteDue(key string) bool {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	b := p.deletes[key]
	return b == nil || !time.Now().Before(b.next)
}

func (p *Provider) deleteNext(key string) time.Time {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	if b := p.deletes[key]; b != nil {
		return b.next
	}
	return time.Time{}
}

func (p *Provider) deleteDone(key string) {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	delete(p.deletes, key)
}

func (p *Provider) deleteFailed(key string) (attempts int, since, next time.Time) {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	if p.deletes == nil {
		p.deletes = map[string]*deleteBackoff{}
	}
	b := p.deletes[key]
	if b == nil {
		b = &deleteBackoff{since: time.Now()}
		p.deletes[key] = b
	}
	b.attempts++
	delay := time.Duration(0)
	if b.attempts > TeardownFreeAttempts {
		delay = TeardownBackoffBase << (b.attempts - TeardownFreeAttempts - 1)
		if delay > TeardownBackoffMax || delay <= 0 {
			delay = TeardownBackoffMax
		}
	}
	b.next = time.Now().Add(delay)
	return b.attempts, b.since, b.next
}

func (p *Provider) teardown(ctx context.Context, ref clusterprovider.Ref, key string, cl *v1alpha1.Cluster) (time.Duration, error) {
	if !p.deleteDue(key) {
		return until(p.deleteNext(key)), nil
	}
	p.log.Info("cluster provider: this cluster is being deleted, so its group is reaped and its store removed",
		"cluster", ref.Name, "key", key, "home", ref.Home)
	step, err := p.teardownSteps(ctx, ref, key)
	if err == nil {
		p.deleteDone(key)
		return 0, nil
	}
	attempts, since, next := p.deleteFailed(key)
	retry := max(time.Until(next), TeardownBackoffBase)
	if attempts < TeardownFreeAttempts || time.Since(since) < TeardownBlockedAfter {
		return retry, nil
	}
	p.log.Warn("cluster provider: a step of deleting this cluster has not completed; the next attempt waits",
		"cluster", ref.Name, "key", key, "home", ref.Home, "step", step, "attempts", attempts,
		"next", next.UTC().Format(time.RFC3339), "error", err.Error())
	p.reportTeardown(ctx, ref, cl, step, attempts, next, err)
	return retry, nil
}

func (p *Provider) teardownSteps(ctx context.Context, ref clusterprovider.Ref, key string) (string, error) {
	if err := p.reapOne(ctx, key); err != nil {
		return "reap", err
	}
	p.removeLinks(ctx, ref, key)
	if err := p.removeStore(ctx, key); err != nil {
		return "store", err
	}
	if err := p.finishDelete(ctx, ref, key); err != nil {
		return "finish", err
	}
	return "", nil
}

func (p *Provider) reportTeardown(ctx context.Context, ref clusterprovider.Ref, cl *v1alpha1.Cluster, step string,
	attempts int, next time.Time, cause error) {
	st := cl.Status
	conds := append([]metav1.Condition(nil), st.Conditions...)
	meta.SetStatusCondition(&conds, metav1.Condition{
		Type:   v1alpha1.ConditionTeardownBlocked,
		Status: metav1.ConditionTrue,
		Reason: ReasonTeardownStepFailing,
		Message: fmt.Sprintf("the %s step of deleting this cluster has not completed in %d passes in a row over %s; "+
			"the next attempt is not made before %s; the last error: %s",
			step, attempts, TeardownBlockedAfter, next.UTC().Format(time.RFC3339), cause.Error()),
		ObservedGeneration: cl.Generation,
	})
	st.Conditions = conds
	if err := p.registry.WriteStatus(ctx, ref, st); err != nil {
		p.log.Warn("cluster provider: writing the teardown condition of a deleted cluster did not complete",
			"cluster", ref.Name, "home", ref.Home, "error", err.Error())
	}
}
