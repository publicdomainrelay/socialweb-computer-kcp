package clusterkcp

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

type Tenant struct {
	core kubernetes.Interface
}

var _ clusterprovider.Tenant = Tenant{}

var _ clusterprovider.OpenTenant = OpenKubeconfig

func NewTenant(core kubernetes.Interface) clusterprovider.Tenant {
	if core == nil {
		return nil
	}
	return Tenant{core: core}
}

func OpenKubeconfig(kubeconfig string, o clusterprovider.OpenOptions) (clusterprovider.Tenant, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	if o.Timeout > 0 {
		cfg.Timeout = o.Timeout
	}
	if o.Throughput {
		cfg = kcpserver.Throughput(cfg)
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return Tenant{core: core}, nil
}

func (t Tenant) Readyz(ctx context.Context) error {
	_, err := t.core.Discovery().RESTClient().Get().AbsPath("/readyz").DoRaw(ctx)
	return err
}

func (t Tenant) Nodes(ctx context.Context) ([]corev1.Node, error) {
	nl, err := t.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return nl.Items, nil
}

const runningPodSelector = "status.phase=Running"

func (t Tenant) RunningPods(ctx context.Context) ([]corev1.Pod, error) {
	pl, err := t.core.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: runningPodSelector})
	if err != nil {
		return nil, err
	}
	return pl.Items, nil
}

func (t Tenant) SystemLeases(ctx context.Context) ([]coordinationv1.Lease, error) {
	ll, err := t.core.CoordinationV1().Leases(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return ll.Items, nil
}

var apiserverSliceSelector = discoveryv1.LabelServiceName + "=kubernetes"

func (t Tenant) APIServerSlices(ctx context.Context) ([]discoveryv1.EndpointSlice, error) {
	list, err := t.core.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault).List(ctx,
		metav1.ListOptions{LabelSelector: apiserverSliceSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (t Tenant) AwaitDeployments(ctx context.Context, namespace string, timeout time.Duration, done func(deployments []*appsv1.Deployment) (bool, error)) error {
	return awaitTyped(ctx, deploymentsIn(t.core, namespace), &appsv1.Deployment{}, timeout, done)
}

func (t Tenant) AwaitAPIServerSlices(ctx context.Context, timeout time.Duration, done func(slices []*discoveryv1.EndpointSlice) (bool, error)) error {
	return awaitTyped(ctx, apiserverSlices(t.core), &discoveryv1.EndpointSlice{}, timeout, done)
}

func (t Tenant) AwaitRunningPods(ctx context.Context, timeout time.Duration, done func(pods []*corev1.Pod) (bool, error)) error {
	return awaitTyped(ctx, runningPods(t.core), &corev1.Pod{}, timeout, done)
}

func (t Tenant) FollowAPIServerSlices(ctx context.Context, notify func()) {
	kube.Follow(ctx, apiserverSlices(t.core), &discoveryv1.EndpointSlice{}, func(any) { notify() })
}

func (t Tenant) FollowSystemLeases(ctx context.Context, notify func(lease *coordinationv1.Lease)) {
	kube.Follow(ctx, systemLeases(t.core), &coordinationv1.Lease{}, func(obj any) {
		l, ok := obj.(*coordinationv1.Lease)
		if !ok {
			return
		}
		notify(l)
	})
}

func (t Tenant) Harden(ctx context.Context) error { return Harden(ctx, t.core) }

func (t Tenant) GrantCloudControllerManager(ctx context.Context, cluster string) error {
	return GrantCloudControllerManager(ctx, t.core, cluster)
}

func awaitTyped[T any](ctx context.Context, lw cache.ListerWatcher, proto kruntime.Object, timeout time.Duration, done func(objs []T) (bool, error)) error {
	return translate(kube.AwaitListWatch(ctx, lw, proto, timeout, func(items []any) (bool, error) {
		objs := make([]T, 0, len(items))
		for _, item := range items {
			if o, ok := item.(T); ok {
				objs = append(objs, o)
			}
		}
		return done(objs)
	}))
}

func deploymentsIn(core kubernetes.Interface, namespace string) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			return core.AppsV1().Deployments(namespace).List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			return core.AppsV1().Deployments(namespace).Watch(ctx, o)
		},
	}
}

func apiserverSlices(core kubernetes.Interface) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			o.LabelSelector = apiserverSliceSelector
			return core.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault).List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.LabelSelector = apiserverSliceSelector
			return core.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault).Watch(ctx, o)
		},
	}
}

func runningPods(core kubernetes.Interface) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			o.FieldSelector = runningPodSelector
			return core.CoreV1().Pods(metav1.NamespaceAll).List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.FieldSelector = runningPodSelector
			return core.CoreV1().Pods(metav1.NamespaceAll).Watch(ctx, o)
		},
	}
}

func systemLeases(core kubernetes.Interface) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			return core.CoordinationV1().Leases(metav1.NamespaceSystem).List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			return core.CoordinationV1().Leases(metav1.NamespaceSystem).Watch(ctx, o)
		},
	}
}
