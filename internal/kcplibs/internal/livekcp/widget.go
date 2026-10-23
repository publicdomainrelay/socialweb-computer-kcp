//go:build linux

package livekcp

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/store"
)

var WidgetGVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}

var GadgetGVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: SecondResource}

type Metadata struct {
	Name string `json:"name"`

	Namespace string `json:"namespace,omitempty"`

	UID string `json:"uid,omitempty"`

	ResourceVersion string `json:"resourceVersion,omitempty"`

	CreationTimestamp string `json:"creationTimestamp,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`
}

type Object[Spec, Status any] struct {
	APIVersion string `json:"apiVersion,omitempty"`

	Kind string `json:"kind,omitempty"`

	Metadata Metadata `json:"metadata"`

	Spec Spec `json:"spec,omitempty"`

	Status Status `json:"status,omitempty"`
}

func NewObject[Spec, Status any](namespace, name string) *Object[Spec, Status] {
	obj := &Object[Spec, Status]{APIVersion: APIVersion, Kind: Kind}
	obj.Metadata.Name = name
	obj.Metadata.Namespace = namespace
	return obj
}

func Seed[T any](ctx context.Context, cluster string, resource store.Resource[T], namespace, name string, obj *T) error {
	_ = resource.Delete(ctx, ref.New(cluster, namespace, name))
	return resource.Create(ctx, cluster, obj)
}

func WaitFor[T any](ctx context.Context, cluster string, resource store.Reader[T], namespace, name string, reached func(T) bool) (*T, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		obj, err := resource.Get(ctx, ref.New(cluster, namespace, name))
		if err == nil && reached(*obj) {
			return obj, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("livekcp: %s never reached the wanted state", name)
}
