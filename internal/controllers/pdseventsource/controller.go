package pdseventsource

import (
	"context"
	"errors"
	"fmt"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/eventsource"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/events/pds"
)

const (
	Name = "pdseventsource"

	AtprotoRepoKind = "AtprotoRepo"
)

func Register(ctx context.Context, m *ctlruntime.Manager, opts eventsource.BackendOptions) (*eventsource.Backend[*eventsv1.PdsEventSource], error) {
	b := eventsource.NewBackend(ctx, eventsv1.PdsEventSourceKind, opts,
		func(_ context.Context, req ctlruntime.Request, obj *eventsv1.PdsEventSource) (eventsource.Upstream, error) {
			repo, err := repoRef(req.Workspace, obj.Spec.RepoRef)
			if err != nil {
				return eventsource.Upstream{}, err
			}
			self := commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: eventsv1.PdsEventSourceKind, Name: obj.Name, Workspace: req.Workspace}
			open := func(ctx context.Context, collections []string) (atprotorepo.Subscriber, error) {
				scope := collections
				if len(scope) == 0 {
					scope = []string{bus.ScopeAll}
				}
				return bus.Resolve[atprotorepo.Subscriber](ctx, opts.Bus, self, repo, bus.Capability{Name: atprotorepo.CapabilitySubscribe, Scope: scope})
			}
			source, err := pds.New(pds.Config{Repo: repo.Name}, pds.Deps{Open: open})
			if err != nil {
				return eventsource.Upstream{}, err
			}
			return eventsource.Upstream{Address: repo.String(), Source: source, CountGaps: true}, nil
		},
		func(o *eventsv1.PdsEventSource) *eventsv1.EventSourceStatus { return &o.Status })
	if err := ctlruntime.For[*eventsv1.PdsEventSource](m, Name, b, ctlruntime.Export(eventsource.Export)); err != nil {
		return nil, fmt.Errorf("pdseventsource: registering the PdsEventSource controller: %w", err)
	}
	return b, nil
}

func repoRef(workspace string, ref commonv1.ObjectRef) (commonv1.ObjectRef, error) {
	if !strings.EqualFold(strings.TrimSpace(ref.Kind), AtprotoRepoKind) {
		return commonv1.ObjectRef{}, fmt.Errorf("the PdsEventSource repoRef must name an AtprotoRepo, not %q", ref.Kind)
	}
	if g := strings.TrimSpace(ref.Group); g != "" && g != v1alpha1.GroupName {
		return commonv1.ObjectRef{}, fmt.Errorf("the PdsEventSource repoRef group %q is not %s", g, v1alpha1.GroupName)
	}
	if strings.TrimSpace(ref.Name) == "" {
		return commonv1.ObjectRef{}, errors.New("the PdsEventSource repoRef needs a name")
	}
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: AtprotoRepoKind, Name: strings.TrimSpace(ref.Name), Workspace: ref.InWorkspace(workspace)}, nil
}
