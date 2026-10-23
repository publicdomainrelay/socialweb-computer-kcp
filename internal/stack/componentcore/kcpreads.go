package componentcore

import (
	"context"
	"errors"
	"strings"
	"sync"

	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/childapplication"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketpresence"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/openbaoserver"
)

var schemes = []func(*kruntime.Scheme) error{v1alpha1.AddToScheme, pdsv1alpha1.AddToScheme, policyv1alpha1.AddToScheme,
	eventsv1alpha1.AddToScheme, exposurev1alpha1.AddToScheme}

type workspaceClients struct {
	base *rest.Config

	scheme *kruntime.Scheme

	mu sync.Mutex

	clients map[string]client.Client
}

var (
	_ childapplication.Lister     = (*workspaceClients)(nil)
	_ childapplication.Workspaces = workspaceTree{}
	_ openbaoserver.Grants        = guestGrants{}
	_ marketpresence.Bidders      = bidders{}
)

func newWorkspaceClients(base *rest.Config) (*workspaceClients, error) {
	if base == nil {
		return nil, errors.New("componentcore: the workspace clients need a rest config")
	}
	scheme := kruntime.NewScheme()
	for _, add := range append([]func(*kruntime.Scheme) error{clientgoscheme.AddToScheme, tenancyv1alpha1.AddToScheme}, schemes...) {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return &workspaceClients{base: base, scheme: scheme, clients: map[string]client.Client{}}, nil
}

func (w *workspaceClients) client(workspace string) (client.Client, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.clients[workspace]; ok {
		return c, nil
	}
	cfg := rest.CopyConfig(w.base)
	host, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = host + "/clusters/" + workspace
	c, err := client.New(cfg, client.Options{Scheme: w.scheme})
	if err != nil {
		return nil, err
	}
	w.clients[workspace] = c
	return c, nil
}

func (w *workspaceClients) List(ctx context.Context, workspace string, list client.ObjectList) error {
	c, err := w.client(workspace)
	if err != nil {
		return err
	}
	err = c.List(ctx, list)
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || meta.IsNoMatchError(err) {
		return nil
	}
	return err
}

type workspaceTree struct {
	clients *workspaceClients
}

func (t workspaceTree) Get(ctx context.Context, parent, name string) (childapplication.Workspace, bool, error) {
	c, err := t.clients.client(parent)
	if err != nil {
		return childapplication.Workspace{}, false, err
	}
	var ws tenancyv1alpha1.Workspace
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			return childapplication.Workspace{}, false, nil
		}
		return childapplication.Workspace{}, false, err
	}
	return workspaceOf(ws), true, nil
}

func (t workspaceTree) ByLabel(ctx context.Context, parent, key, value string) (childapplication.Workspace, bool, error) {
	c, err := t.clients.client(parent)
	if err != nil {
		return childapplication.Workspace{}, false, err
	}
	var list tenancyv1alpha1.WorkspaceList
	if err := c.List(ctx, &list, client.MatchingLabels{key: value}); err != nil {
		if apierrors.IsNotFound(err) {
			return childapplication.Workspace{}, false, nil
		}
		return childapplication.Workspace{}, false, err
	}
	if len(list.Items) == 0 {
		return childapplication.Workspace{}, false, nil
	}
	return workspaceOf(list.Items[0]), true, nil
}

func workspaceOf(ws tenancyv1alpha1.Workspace) childapplication.Workspace {
	return childapplication.Workspace{Name: ws.Name, Phase: string(ws.Status.Phase), Deleting: !ws.DeletionTimestamp.IsZero()}
}

type guestGrants struct {
	clients *workspaceClients
}

func (g guestGrants) List(ctx context.Context, workspace string) ([]v1alpha1.GuestIdentityGrant, error) {
	var list v1alpha1.GuestIdentityGrantList
	if err := g.clients.List(ctx, workspace, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

type bidders struct {
	clients *workspaceClients
}

func (b bidders) Get(ctx context.Context, workspace, name string) (*v1alpha1.Bidder, error) {
	return get[v1alpha1.Bidder](ctx, b.clients, workspace, name)
}
