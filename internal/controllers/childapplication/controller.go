package childapplication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	Name = "childapplication"

	Export = "applications"

	HostProcessExport = "hostprocesses"

	Finalizer = "childapplication.socialweb.computer/workspace"

	ConditionReady = "Ready"

	ConditionProgressing = "Progressing"

	ConditionFailed = "Failed"

	ConditionOrphaned = "Orphaned"

	AppWorkspaceLabel = "apps.socialweb.computer/application"

	DevWorkspacePrefix = "root:dev"

	ComponentWorkspace = "app"
)

type Workspace struct {
	Name string

	Phase string

	Deleting bool
}

type Workspaces interface {
	Get(ctx context.Context, parent, name string) (Workspace, bool, error)
	ByLabel(ctx context.Context, parent, key, value string) (Workspace, bool, error)
}

type Lister interface {
	List(ctx context.Context, workspace string, list client.ObjectList) error
}

type Options struct {
	Log *slog.Logger

	Workspaces Workspaces

	Objects Lister

	WorkspaceEvents <-chan string

	Scope func(workspace string) bool
}

type networkKey struct {
	cluster string

	name string
}

type refs struct {
	network networkKey

	instance string

	workspaces []string
}

type Controller struct {
	log *slog.Logger

	workspaces Workspaces

	objects Lister

	mu sync.Mutex

	refs map[ctlruntime.Request]refs
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Workspaces == nil {
		return nil, errors.New("childapplication: a workspace reader is required")
	}
	if opts.Objects == nil {
		return nil, errors.New("childapplication: a workspace object lister is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{log: log.With("component", Name+"-controller"), workspaces: opts.Workspaces, objects: opts.Objects,
		refs: map[ctlruntime.Request]refs{}}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Finalizer(Finalizer), ctlruntime.Gone(c.gone),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.MarketNetwork) []ctlruntime.Request {
			return c.match(func(r refs) bool { return r.network == networkKey{cluster: src.Cluster, name: obj.Name} })
		}),
		ctlruntime.WatchesIn(HostProcessExport, func(_ context.Context, src ctlruntime.Request, _ *v1alpha1.HostProcess) []ctlruntime.Request {
			instance, ok := strings.CutPrefix(src.Workspace, DevWorkspacePrefix+":")
			if !ok || instance == "" {
				return nil
			}
			return c.match(func(r refs) bool { return r.instance == instance })
		}),
	}
	for _, kind := range componentKinds {
		options = append(options, kind.watch(func(_ context.Context, src ctlruntime.Request) []ctlruntime.Request {
			workspace, ok := strings.CutSuffix(src.Workspace, ":"+ComponentWorkspace)
			if !ok {
				return nil
			}
			return c.inWorkspace(workspace)
		}))
	}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if opts.WorkspaceEvents != nil {
		events, err := ctlruntime.Forward(m, Name+": workspaces", opts.WorkspaceEvents, c.inWorkspace)
		if err != nil {
			return nil, fmt.Errorf("childapplication: forwarding changes: %w", err)
		}
		options = append(options, ctlruntime.Events(events))
	}
	if err := ctlruntime.For[*v1alpha1.ChildApplication](m, Name, c, options...); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Controller) inWorkspace(workspace string) []ctlruntime.Request {
	return c.match(func(r refs) bool {
		for _, w := range r.workspaces {
			if w == workspace {
				return true
			}
		}
		return false
	})
}

func (c *Controller) match(keep func(refs) bool) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ctlruntime.Request
	for req, r := range c.refs {
		if keep(r) {
			out = append(out, req)
		}
	}
	return out
}

func (c *Controller) index(req ctlruntime.Request, r refs) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refs[ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}] = r
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.refs, ctlruntime.Request{Cluster: req.Cluster, Name: req.Name})
}
