package events

import (
	"context"
	"errors"
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

type ClientFor func(workspace string) (client.Client, error)

var (
	_ atprotorecordwatch.Children = recordWatchChildren{}
	_ atprotorecordwatch.Repos    = recordWatchRepos{}
)

func RecordWatch(clientFor ClientFor) (atprotorecordwatch.Children, atprotorecordwatch.Repos, atprotorecordwatch.ValueReader) {
	return recordWatchChildren{clientFor: clientFor}, recordWatchRepos{clientFor: clientFor}, recordWatchValues(clientFor)
}

func (f ClientFor) client(workspace string) (client.Client, error) {
	if f == nil {
		return nil, errors.New("events: the record watch has no workspace clients")
	}
	return f(workspace)
}

type recordWatchChildren struct {
	clientFor ClientFor
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
	c, err := r.clientFor.client(workspace)
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
	c, err := r.clientFor.client(workspace)
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
	c, err := r.clientFor.client(workspace)
	if err != nil {
		return err
	}
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return err
	}
	if err := kube.ApplyWith(ctx, c, obj, kube.ApplyOptions{FieldManager: atprotorecordwatch.FieldManager, Force: true}); err != nil {
		return fmt.Errorf("events: applying %s %s in %s: %w", gvk.Kind, obj.GetName(), workspace, err)
	}
	return nil
}

type recordWatchRepos struct {
	clientFor ClientFor
}

func (r recordWatchRepos) AtprotoRepo(ctx context.Context, workspace, name string) (*v1alpha1.AtprotoRepo, error) {
	return get[v1alpha1.AtprotoRepo](ctx, r.clientFor, workspace, name)
}

func (r recordWatchRepos) LocalRepo(ctx context.Context, workspace, name string) (*v1alpha1.LocalRepo, error) {
	return get[v1alpha1.LocalRepo](ctx, r.clientFor, workspace, name)
}

func (r recordWatchRepos) RemoteRepo(ctx context.Context, workspace, name string) (*v1alpha1.RemoteRepo, error) {
	return get[v1alpha1.RemoteRepo](ctx, r.clientFor, workspace, name)
}

func get[T any, P interface {
	*T
	client.Object
}](ctx context.Context, clientFor ClientFor, workspace, name string) (*T, error) {
	c, err := clientFor.client(workspace)
	if err != nil {
		return nil, err
	}
	out := P(new(T))
	if err := c.Get(ctx, client.ObjectKey{Name: name}, out); err != nil {
		return nil, err
	}
	return (*T)(out), nil
}

func recordWatchValues(clientFor ClientFor) atprotorecordwatch.ValueReader {
	return func(ctx context.Context, workspace string, from v1alpha1.AtprotoValueFrom) (map[string]any, error) {
		c, err := clientFor.client(workspace)
		if err != nil {
			return nil, err
		}
		gvk, err := c.RESTMapper().KindFor(atprotorecord.ReferenceResource(from))
		if err != nil {
			return nil, err
		}
		mapping, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, err
		}
		key := client.ObjectKey{Name: from.Name}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			key.Namespace = from.Namespace
		}
		return kube.GetWith(ctx, c, gvk, key)
	}
}
