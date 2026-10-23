package clusterkcp

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/scale"
)

type NodeObserver struct {
	Client func(ctx context.Context, cluster string) (kubernetes.Interface, error)

	Baseline func(cluster string) (scale.Baseline, error)

	Operators scale.Operators

	CustomResources func(ctx context.Context, cluster string, kind scale.OperatorKind) ([]scale.CustomResource, error)
}

func (o NodeObserver) Observe(ctx context.Context, cluster string) (scale.Observation, int, error) {
	if o.Client == nil {
		return scale.Observation{}, 0, fmt.Errorf(
			"clusterprovider: NodeObserver has no client, so %s cannot be read", cluster)
	}
	cs, err := o.Client(ctx, cluster)
	if err != nil {
		return scale.Observation{}, 0, fmt.Errorf("clusterprovider: the client for %s: %w", cluster, err)
	}

	pods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Observation{}, 0, fmt.Errorf("clusterprovider: listing pods in %s: %w", cluster, err)
	}
	jobs, err := cs.BatchV1().Jobs(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Observation{}, 0, fmt.Errorf("clusterprovider: listing jobs in %s: %w", cluster, err)
	}
	deployments, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Observation{}, 0, fmt.Errorf("clusterprovider: listing deployments in %s: %w", cluster, err)
	}
	statefulsets, err := cs.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Observation{}, 0, fmt.Errorf("clusterprovider: listing statefulsets in %s: %w", cluster, err)
	}
	daemonsets, err := cs.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Observation{}, 0, fmt.Errorf("clusterprovider: listing daemonsets in %s: %w", cluster, err)
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Observation{}, 0, fmt.Errorf("clusterprovider: listing nodes in %s: %w", cluster, err)
	}
	replicasets, err := cs.AppsV1().ReplicaSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Observation{}, 0, fmt.Errorf("clusterprovider: listing replicasets in %s: %w", cluster, err)
	}

	appliedByTheControlPlane := map[string]bool{}
	for i := range replicasets.Items {
		rs := &replicasets.Items[i]
		if scale.AppliedByTheControlPlane(rs.ObjectMeta) {
			appliedByTheControlPlane[rs.Namespace+"/"+rs.Name] = true
		}
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		for _, ref := range p.OwnerReferences {
			if ref.Kind != "ReplicaSet" {
				continue
			}
			if !appliedByTheControlPlane[p.Namespace+"/"+ref.Name] {
				continue
			}
			scale.StampAppliedByTheControlPlane(&p.ObjectMeta)
			break
		}
	}

	var baseline scale.Baseline
	if o.Baseline != nil {
		baseline, err = o.Baseline(cluster)
		if err != nil {
			return scale.Observation{}, 0, fmt.Errorf("clusterprovider: the add-on baseline of %s: %w", cluster, err)
		}
	}

	unreconciled, err := o.unreconciled(ctx, cluster)
	if err != nil {
		return scale.Observation{}, 0, err
	}

	raw := scale.Observation{
		Pods:                        pods.Items,
		Jobs:                        jobs.Items,
		Deployments:                 deployments.Items,
		StatefulSets:                statefulsets.Items,
		DaemonSets:                  daemonsets.Items,
		UnreconciledCustomResources: unreconciled,
	}
	raw.Placement = scale.Loads(nodes.Items, raw, replicasets.Items, baseline)
	return scale.Narrow(raw, replicasets.Items, baseline), len(nodes.Items), nil
}

func (o NodeObserver) unreconciled(ctx context.Context, cluster string) ([]scale.CustomResource, error) {
	inCluster := o.Operators.InCluster()
	if len(inCluster) == 0 {
		return nil, nil
	}
	if o.CustomResources == nil {
		return nil, fmt.Errorf("clusterprovider: %d kind(s) in the operator registry run in the tenant "+
			"cluster and no lister for custom resources was supplied, so their unreconciled CRs could "+
			"never ask for a node: a CR its operator cannot reach at zero nodes would wait for ever", len(inCluster))
	}
	var all []scale.CustomResource
	for _, kind := range inCluster {
		crs, err := o.CustomResources(ctx, cluster, kind)
		if err != nil {
			return nil, fmt.Errorf("clusterprovider: listing %s.%s in %s: %w -- a read that failed is "+
				"not a cluster whose CRs are all reconciled", kind.Kind, kind.Group, cluster, err)
		}
		all = append(all, crs...)
	}
	return o.Operators.Unreconciled(all), nil
}
