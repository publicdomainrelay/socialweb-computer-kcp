package admission

import (
	"context"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/queue"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

type Source interface {
	Parent(ctx context.Context, run queue.Run) (ref.Ref, bool, error)

	Runs(ctx context.Context, parent ref.Ref) ([]queue.Run, error)

	Capacity(ctx context.Context, parent ref.Ref) (queue.Capacity, *queue.Blocker, error)
}

type Options struct {
	Source Source

	Leases *queue.Leases

	LeaseTTL time.Duration

	Now func() time.Time

	Lifecycle queue.Lifecycle

	Wake func(kind string, r ref.Ref)

	RunKind string

	ParentKind string
}

type Admitter struct {
	opts Options
}

func New(opts Options) *Admitter {
	if opts.Leases == nil {
		opts.Leases = queue.NewLeases(opts.LeaseTTL)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Admitter{opts: opts}
}

func (a *Admitter) Leases() *queue.Leases {
	return a.opts.Leases
}

func (a *Admitter) Admit(ctx context.Context, run queue.Run) (queue.Admission, error) {
	parent, ok, err := a.opts.Source.Parent(ctx, run)
	if err != nil {
		return queue.Admission{}, err
	}
	if !ok {
		return queue.Admission{}, nil
	}
	runs, err := a.opts.Source.Runs(ctx, parent)
	if err != nil {
		return queue.Admission{}, err
	}
	capacity, blocker, err := a.opts.Source.Capacity(ctx, parent)
	if err != nil {
		return queue.Admission{}, err
	}
	now := a.opts.Now()
	observed := make(map[ref.Ref]string, len(runs))
	for _, candidate := range runs {
		observed[candidate.Ref] = candidate.Phase
	}
	reserved := a.opts.Leases.Count(parent, observed, now, a.opts.Lifecycle)
	if _, present := observed[run.Ref]; !present {
		runs = append(append([]queue.Run(nil), runs...), run)
	}
	admission := queue.PlanIndex(runs, capacity, blocker, reserved, a.opts.Lifecycle)[run.Ref]
	if admission.Gated && admission.Allowed {
		a.opts.Leases.Grant(run.Ref, parent, now)
	}
	return admission, nil
}

func (a *Admitter) Wake(ctx context.Context, parent ref.Ref) error {
	if a.opts.Wake == nil {
		return nil
	}
	runs, err := a.opts.Source.Runs(ctx, parent)
	if err != nil {
		return err
	}
	capacity, _, err := a.opts.Source.Capacity(ctx, parent)
	if err != nil {
		return err
	}
	limit, unlimited := queue.Limit(capacity.Policy, capacity.MaxConcurrent)
	if a.opts.RunKind != "" {
		running := int32(0)
		for _, run := range runs {
			if a.opts.Lifecycle.Running(run.Phase) {
				running++
			}
		}
		free := max(limit-running, 0)
		for _, run := range queue.WakeList(runs, a.opts.Lifecycle, free, unlimited) {
			a.opts.Wake(a.opts.RunKind, run)
		}
	}
	if a.opts.ParentKind != "" {
		a.opts.Wake(a.opts.ParentKind, parent)
	}
	return nil
}
