package clusterprovider

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

type Tenant interface {
	Readyz(ctx context.Context) error

	Nodes(ctx context.Context) ([]corev1.Node, error)

	RunningPods(ctx context.Context) ([]corev1.Pod, error)

	SystemLeases(ctx context.Context) ([]coordinationv1.Lease, error)

	APIServerSlices(ctx context.Context) ([]discoveryv1.EndpointSlice, error)

	AwaitDeployments(ctx context.Context, namespace string, timeout time.Duration, done func(deployments []*appsv1.Deployment) (bool, error)) error

	AwaitAPIServerSlices(ctx context.Context, timeout time.Duration, done func(slices []*discoveryv1.EndpointSlice) (bool, error)) error

	AwaitRunningPods(ctx context.Context, timeout time.Duration, done func(pods []*corev1.Pod) (bool, error)) error

	FollowAPIServerSlices(ctx context.Context, notify func())

	FollowSystemLeases(ctx context.Context, notify func(lease *coordinationv1.Lease))

	Harden(ctx context.Context) error

	GrantCloudControllerManager(ctx context.Context, cluster string) error
}

type AdminClient func(ctx context.Context, cluster, apiserver string) (Tenant, error)

type OpenOptions struct {
	Timeout time.Duration

	Throughput bool
}

type OpenTenant func(kubeconfig string, o OpenOptions) (Tenant, error)

type KubeconfigWriter func(path string, cred *Credential, contextName string, unwritable error) error
