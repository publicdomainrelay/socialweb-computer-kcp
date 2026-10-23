package componentcore

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecord"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecordwatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
)

var (
	_ atprotorecordwatch.Children = recordWatchChildren{}
	_ atprotorecordwatch.Repos    = recordWatchRepos{}
)

type recordWatchChildren struct {
	clients *workspaceClients
}

func (r recordWatchChildren) ApplyEventStream(ctx context.Context, workspace string, obj *eventsv1alpha1.EventStream) error {
	return r.apply(ctx, workspace, obj)
}

func (r recordWatchChildren) ApplyEventSource(ctx context.Context, workspace string, obj *eventsv1alpha1.EventSource) error {
	return r.apply(ctx, workspace, obj)
}

func (r recordWatchChildren) ApplyPdsEventSource(ctx context.Context, workspace string, obj *eventsv1alpha1.PdsEventSource) error {
	return r.apply(ctx, workspace, obj)
}

func (r recordWatchChildren) EventStreams(ctx context.Context, workspace, watch string) ([]eventsv1alpha1.EventStream, error) {
	c, err := r.clients.client(workspace)
	if err != nil {
		return nil, err
	}
	var list eventsv1alpha1.EventStreamList
	if err := c.List(ctx, &list, client.MatchingLabels{eventstream.RecordWatchLabel: watch}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r recordWatchChildren) DeleteEventStream(ctx context.Context, workspace, name string) error {
	c, err := r.clients.client(workspace)
	if err != nil {
		return err
	}
	obj := &eventsv1alpha1.EventStream{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r recordWatchChildren) apply(ctx context.Context, workspace string, obj client.Object) error {
	c, err := r.clients.client(workspace)
	if err != nil {
		return err
	}
	gvk, err := apiutil.GVKForObject(obj, r.clients.scheme)
	if err != nil {
		return err
	}
	if err := kube.ApplyWith(ctx, c, obj, kube.ApplyOptions{FieldManager: atprotorecordwatch.FieldManager, Force: true}); err != nil {
		return fmt.Errorf("componentcore: applying %s %s in %s: %w", gvk.Kind, obj.GetName(), workspace, err)
	}
	return nil
}

type recordWatchRepos struct {
	clients *workspaceClients
}

func (r recordWatchRepos) AtprotoRepo(ctx context.Context, workspace, name string) (*v1alpha1.AtprotoRepo, error) {
	return get[v1alpha1.AtprotoRepo](ctx, r.clients, workspace, name)
}

func (r recordWatchRepos) LocalRepo(ctx context.Context, workspace, name string) (*v1alpha1.LocalRepo, error) {
	return get[v1alpha1.LocalRepo](ctx, r.clients, workspace, name)
}

func (r recordWatchRepos) RemoteRepo(ctx context.Context, workspace, name string) (*v1alpha1.RemoteRepo, error) {
	return get[v1alpha1.RemoteRepo](ctx, r.clients, workspace, name)
}

func get[T any, P interface {
	*T
	client.Object
}](ctx context.Context, clients *workspaceClients, workspace, name string) (*T, error) {
	c, err := clients.client(workspace)
	if err != nil {
		return nil, err
	}
	out := P(new(T))
	if err := c.Get(ctx, client.ObjectKey{Name: name}, out); err != nil {
		return nil, err
	}
	return (*T)(out), nil
}

func recordWatchValues(clients *workspaceClients) atprotorecordwatch.ValueReader {
	return func(ctx context.Context, workspace string, from v1alpha1.AtprotoValueFrom) (map[string]any, error) {
		c, err := clients.client(workspace)
		if err != nil {
			return nil, err
		}
		gvr := atprotorecord.ReferenceResource(from)
		gvk, err := c.RESTMapper().KindFor(gvr)
		if err != nil {
			return nil, err
		}
		mapping, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, err
		}
		dyn, err := clients.dynamic(workspace)
		if err != nil {
			return nil, err
		}
		r := dyn.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			r = r.In(from.Namespace)
		}
		doc, err := kube.Get[kube.Doc](ctx, r, from.Name)
		if err != nil {
			return nil, err
		}
		return *doc, nil
	}
}
