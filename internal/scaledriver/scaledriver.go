package scaledriver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/scale"
)

var ErrNoFactory = errors.New(
	"scaledriver: no Nodes factory; the decision would be made and nothing could perform it, " +
		"which is the state this package exists to end")

type Factory func(ctx context.Context, cluster string) (scale.Nodes, error)

type Options struct {
	Scale scale.Options

	Now func() time.Time

	Log *slog.Logger

	Observe func(ctx context.Context, cluster string) (scale.Observation, int, error)
}

type Driver struct {
	factory Factory
	opts    Options

	mu     sync.Mutex
	states map[string]scale.State
	capped map[string]string
	wakes  map[string]time.Time
}

func New(f Factory, opts Options) (*Driver, error) {
	if f == nil {
		return nil, ErrNoFactory
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Driver{factory: f, opts: opts, states: map[string]scale.State{}, capped: map[string]string{}, wakes: map[string]time.Time{}}, nil
}

func (d *Driver) Pass(ctx context.Context, cluster string, o scale.Observation, observedNodes int) (scale.Decision, error) {
	if cluster == "" {
		return scale.Decision{}, errors.New("scaledriver: Pass needs a cluster")
	}
	log := d.opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := d.opts.Now()

	d.mu.Lock()
	st := d.states[cluster]
	st.Nodes = observedNodes
	d.mu.Unlock()

	dec := scale.Decide(o, st, now, d.opts.Scale)
	d.reportCap(log, cluster, dec.Capped)
	defer d.noteWake(cluster, o, now)

	if dec.Action == scale.None {
		d.advance(cluster, st.Next(dec, now))
		if !dec.BackoffUntil.IsZero() {
			log.Info("scaledriver: backing off; no market provision this pass", "cluster", cluster,
				"nodes", dec.Nodes, "backoffUntil", dec.BackoffUntil.UTC().Format(time.RFC3339),
				"reason", dec.Reason)
			return dec, nil
		}
		log.Debug("scaledriver: no change", "cluster", cluster, "nodes", dec.Nodes, "reason", dec.Reason)
		return dec, nil
	}

	nodes, err := d.factory(ctx, cluster)
	if err != nil {
		return dec, fmt.Errorf("scaledriver: building the actuator for %s: %w", cluster, err)
	}

	switch dec.Action {
	case scale.Provision:
		for counted := observedNodes; counted < dec.Nodes; counted++ {
			if err := nodes.Provision(ctx); err != nil {
				return dec, fmt.Errorf("scaledriver: provisioning node %d of %d for %s: %w",
					counted+1, dec.Nodes, cluster, err)
			}
		}
	case scale.Teardown:
		if err := nodes.Teardown(ctx); err != nil {
			return dec, fmt.Errorf("scaledriver: tearing down the node for %s: %w", cluster, err)
		}
	case scale.Release:
		releaser, ok := nodes.(scale.Releaser)
		if !ok {
			return dec, fmt.Errorf("%w: %s, node %s", ErrNoReleaser, cluster, dec.Release)
		}
		if err := releaser.Release(ctx, dec.Release); err != nil {
			return dec, fmt.Errorf("scaledriver: releasing node %s of %s: %w", dec.Release, cluster, err)
		}
	default:
		return dec, fmt.Errorf("scaledriver: scale.Decide returned action %d, which this driver "+
			"does not know how to perform", dec.Action)
	}

	d.advance(cluster, st.Next(dec, now))
	log.Info("scaledriver: node count changed", "cluster", cluster, "action", dec.Action,
		"nodes", dec.Nodes, "release", dec.Release, "reason", dec.Reason)
	return dec, nil
}

var ErrNoReleaser = errors.New(
	"scaledriver: the decision released one node and the actuator cannot release a single node; " +
		"a decision nothing can perform is the state this package exists to end")

var ErrNoObserver = errors.New(
	"scaledriver: no Observer, so a pass has nothing to decide from -- Pass takes its " +
		"observation as an argument, and RunPass is the entry that does not")

func (d *Driver) RunPass(ctx context.Context, cluster string) (scale.Decision, error) {
	if d.opts.Observe == nil {
		return scale.Decision{}, ErrNoObserver
	}
	o, nodes, err := d.opts.Observe(ctx, cluster)
	if err != nil {
		return scale.Decision{}, fmt.Errorf("scaledriver: observing %s: %w", cluster, err)
	}
	return d.Pass(ctx, cluster, o, nodes)
}

func (d *Driver) reportCap(log *slog.Logger, cluster, capped string) {
	d.mu.Lock()
	last := d.capped[cluster]
	if capped == "" {
		delete(d.capped, cluster)
	} else {
		d.capped[cluster] = capped
	}
	d.mu.Unlock()
	switch {
	case capped != "" && capped != last:
		log.Warn("scaledriver: the max-nodes cap blocks a scale-up", "cluster", cluster, "detail", capped)
	case capped == "" && last != "":
		log.Info("scaledriver: the max-nodes cap no longer blocks a scale-up", "cluster", cluster)
	}
}

func (d *Driver) hold() time.Duration {
	switch h := d.opts.Scale.Hold; {
	case h == 0:
		return scale.DefaultHold
	case h < 0:
		return 0
	default:
		return h
	}
}

func (d *Driver) noteWake(cluster string, o scale.Observation, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.states[cluster]
	var at time.Time
	next := func(t time.Time) {
		if !t.After(now) {
			return
		}
		if at.IsZero() || t.Before(at) {
			at = t
		}
	}
	if st.Backoff.Holds(now) {
		next(st.Backoff.Until)
	}
	if st.Nodes > 0 && !st.IdleSince.IsZero() {
		next(st.IdleSince.Add(d.hold()))
	}
	for _, since := range st.NodeIdleSince {
		next(since.Add(d.hold()))
	}
	if scale.Intent(o) && scale.Wanted(o, d.opts.Scale, now).Hold != "" {
		next(now.Add(scale.UnschedulableSettle))
	}
	if at.IsZero() {
		delete(d.wakes, cluster)
		return
	}
	d.wakes[cluster] = at
}

func (d *Driver) Wake(cluster string) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.wakes[cluster]
}

func (d *Driver) State(cluster string) scale.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.states[cluster]
}

func (d *Driver) Forget(cluster string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.states, cluster)
	delete(d.capped, cluster)
	delete(d.wakes, cluster)
}

func (d *Driver) log() *slog.Logger {
	if d.opts.Log == nil {
		return slog.Default()
	}
	return d.opts.Log
}

func (d *Driver) Declined(cluster, reason string) scale.Backoff {
	now := d.opts.Now()
	d.mu.Lock()
	st := d.states[cluster].Declined(now, reason, d.opts.Scale)
	d.states[cluster] = st
	d.mu.Unlock()
	d.log().Info("scaledriver: backing off market provisions for this cluster",
		"cluster", cluster, "until", st.Backoff.Until.UTC().Format(time.RFC3339),
		"wait", st.Backoff.Until.Sub(now).String(), "endsInARow", st.Backoff.Ends,
		"why", reason,
		"resets", "a node of this cluster joining, or the cluster's intent dropping to none",
		"memory", "held in this process only, so a provider restart starts the cluster from no back-off")
	return st.Backoff
}

func (d *Driver) Joined(cluster string) {
	d.mu.Lock()
	st, ok := d.states[cluster]
	held := ok && st.Backoff != (scale.Backoff{})
	if held {
		d.states[cluster] = st.Joined()
	}
	d.mu.Unlock()
	if held {
		d.log().Info("scaledriver: a node of this cluster joined, so its market back-off is dropped",
			"cluster", cluster, "endsInARow", st.Backoff.Ends,
			"wasUntil", st.Backoff.Until.UTC().Format(time.RFC3339))
	}
}

func (d *Driver) advance(cluster string, st scale.State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.states[cluster] = st
}
