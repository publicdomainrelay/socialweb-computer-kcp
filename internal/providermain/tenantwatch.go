package providermain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

const (
	TenantWatchRetryBase = time.Second

	TenantWatchRetryMax = time.Minute
)

var errTenantWatchClosed = errors.New("clusterprovider: the tenant watch closed")

type tenantWatch struct {
	tenant func(ctx context.Context, cluster string) (kubernetes.Interface, error)

	notify func(cluster string)

	log *slog.Logger

	mu sync.Mutex

	running map[string]context.CancelFunc

	wg sync.WaitGroup
}

func newTenantWatch(tenant func(ctx context.Context, cluster string) (kubernetes.Interface, error), notify func(string), log *slog.Logger) *tenantWatch {
	return &tenantWatch{tenant: tenant, notify: notify, log: log, running: map[string]context.CancelFunc{}}
}

func (t *tenantWatch) ensure(cluster string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.running[cluster]; ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.running[cluster] = cancel
	t.wg.Go(func() { t.run(ctx, cluster) })
}

func (t *tenantWatch) stop(cluster string) {
	t.mu.Lock()
	cancel, ok := t.running[cluster]
	delete(t.running, cluster)
	t.mu.Unlock()
	if ok {
		cancel()
	}
}

func (t *tenantWatch) stopAll() {
	t.mu.Lock()
	for cluster, cancel := range t.running {
		cancel()
		delete(t.running, cluster)
	}
	t.mu.Unlock()
	t.wg.Wait()
}

func (t *tenantWatch) run(ctx context.Context, cluster string) {
	retry := TenantWatchRetryBase
	for ctx.Err() == nil {
		started := time.Now()
		err := t.follow(ctx, cluster)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > TenantWatchRetryMax {
			retry = TenantWatchRetryBase
		}
		t.log.Debug("cluster provider: the tenant watch ended; it reconnects", "cluster", cluster,
			"retryIn", retry.String(), "err", fmt.Sprint(err))
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		retry = min(retry*2, TenantWatchRetryMax)
	}
}

type podMark struct {
	phase corev1.PodPhase
	node  string
	sched corev1.ConditionStatus
}

func (t *tenantWatch) follow(ctx context.Context, cluster string) error {
	client, err := t.tenant(ctx, cluster)
	if err != nil {
		return err
	}
	pods, err := client.CoreV1().Pods(metav1.NamespaceAll).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	defer pods.Stop()
	nodes, err := client.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	defer nodes.Stop()
	podMarks := map[string]podMark{}
	nodeMarks := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-pods.ResultChan():
			if !ok {
				return errTenantWatchClosed
			}
			if ev.Type == watch.Error {
				return fmt.Errorf("clusterprovider: the tenant pod watch of %s: %v", cluster, ev.Object)
			}
			pod, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			id := pod.Namespace + "/" + pod.Name
			if ev.Type == watch.Deleted {
				delete(podMarks, id)
				t.notify(cluster)
				continue
			}
			mark := podMark{phase: pod.Status.Phase, node: pod.Spec.NodeName}
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodScheduled {
					mark.sched = c.Status
				}
			}
			if prev, seen := podMarks[id]; !seen || prev != mark {
				podMarks[id] = mark
				t.notify(cluster)
			}
		case ev, ok := <-nodes.ResultChan():
			if !ok {
				return errTenantWatchClosed
			}
			if ev.Type == watch.Error {
				return fmt.Errorf("clusterprovider: the tenant node watch of %s: %v", cluster, ev.Object)
			}
			node, ok := ev.Object.(*corev1.Node)
			if !ok {
				continue
			}
			if ev.Type == watch.Deleted {
				delete(nodeMarks, node.Name)
				t.notify(cluster)
				continue
			}
			mark := fmt.Sprint(node.Spec.Unschedulable)
			for _, c := range node.Status.Conditions {
				if c.Type == corev1.NodeReady {
					mark += "/" + string(c.Status)
				}
			}
			if prev, seen := nodeMarks[node.Name]; !seen || prev != mark {
				nodeMarks[node.Name] = mark
				t.notify(cluster)
			}
		}
	}
}
