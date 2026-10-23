package bus

import (
	"bufio"
	"context"
	"maps"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/deploy"
)

const SystemKind = "System"

type Caller struct {
	Ref commonv1.ObjectRef

	Labels map[string]string
}

func (c Caller) System() bool {
	return strings.EqualFold(strings.TrimSpace(c.Ref.Kind), SystemKind)
}

func (c Caller) Complete() bool {
	return strings.TrimSpace(c.Ref.Workspace) != "" && strings.TrimSpace(c.Ref.Kind) != "" && strings.TrimSpace(c.Ref.Name) != ""
}

type callerKey struct{}

func WithCaller(ctx context.Context, ref commonv1.ObjectRef) context.Context {
	return WithCallerLabels(ctx, ref, nil)
}

func WithCallerLabels(ctx context.Context, ref commonv1.ObjectRef, labels map[string]string) context.Context {
	return context.WithValue(ctx, callerKey{}, Caller{Ref: ref, Labels: maps.Clone(labels)})
}

func WithSystemCaller(ctx context.Context, name string) context.Context {
	return WithCaller(ctx, commonv1.ObjectRef{Kind: SystemKind, Name: strings.TrimSpace(name)})
}

func CallerFrom(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok
}

var bootstrap = sync.OnceValue(func() map[string]bool {
	return ParseBootstrapCallers(deploy.BusBootstrapCallers)
})

func ParseBootstrapCallers(data string) map[string]bool {
	out := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(data))
	header := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if header {
			header = false
			continue
		}
		name, _, _ := strings.Cut(line, "\t")
		if name = strings.TrimSpace(name); name != "" {
			out[name] = true
		}
	}
	return out
}

func BootstrapCaller(name string) bool {
	return bootstrap()[strings.TrimSpace(name)]
}

type Object interface {
	GetName() string
	GetLabels() map[string]string
	GroupVersionKind() schema.GroupVersionKind
}

func WithObject(ctx context.Context, workspace string, obj Object) context.Context {
	if obj == nil {
		return ctx
	}
	gvk := obj.GroupVersionKind()
	return WithCallerLabels(ctx, commonv1.ObjectRef{Group: gvk.Group, Kind: gvk.Kind, Name: obj.GetName(), Workspace: workspace}, obj.GetLabels())
}
