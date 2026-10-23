package clusterkcp

import (
	"context"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/scale"
)

func appliedBy(meta metav1.ObjectMeta, manager string) bool {
	for _, f := range meta.ManagedFields {
		if f.Manager == manager && f.Operation == metav1.ManagedFieldsOperationApply {
			return true
		}
	}
	return false
}

func MeasureAddOnBaseline(ctx context.Context, cs kubernetes.Interface, manager string, now time.Time) (scale.Baseline, error) {
	b := scale.Baseline{Manager: manager, MeasuredAt: now.UTC()}
	add := func(kind string, meta metav1.ObjectMeta, spec any) error {
		if !appliedBy(meta, manager) {
			return nil
		}
		h, err := scale.SpecHash(spec)
		if err != nil {
			return err
		}
		b.Workloads = append(b.Workloads, scale.BaselineEntry{
			WorkloadKey: scale.WorkloadKey{Kind: kind, Namespace: meta.Namespace, Name: meta.Name},
			SpecHash:    h,
		})
		return nil
	}
	deployments, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Baseline{}, fmt.Errorf("clusterprovider: listing deployments to measure the add-on baseline: %w", err)
	}
	for i := range deployments.Items {
		if err := add(scale.KindDeployment, deployments.Items[i].ObjectMeta, deployments.Items[i].Spec); err != nil {
			return scale.Baseline{}, err
		}
	}
	statefulsets, err := cs.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Baseline{}, fmt.Errorf("clusterprovider: listing statefulsets to measure the add-on baseline: %w", err)
	}
	for i := range statefulsets.Items {
		if err := add(scale.KindStatefulSet, statefulsets.Items[i].ObjectMeta, statefulsets.Items[i].Spec); err != nil {
			return scale.Baseline{}, err
		}
	}
	daemonsets, err := cs.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Baseline{}, fmt.Errorf("clusterprovider: listing daemonsets to measure the add-on baseline: %w", err)
	}
	for i := range daemonsets.Items {
		if err := add(scale.KindDaemonSet, daemonsets.Items[i].ObjectMeta, daemonsets.Items[i].Spec); err != nil {
			return scale.Baseline{}, err
		}
	}
	jobs, err := cs.BatchV1().Jobs(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return scale.Baseline{}, fmt.Errorf("clusterprovider: listing jobs to measure the add-on baseline: %w", err)
	}
	for i := range jobs.Items {
		if err := add(scale.KindJob, jobs.Items[i].ObjectMeta, jobs.Items[i].Spec); err != nil {
			return scale.Baseline{}, err
		}
	}
	sort.Slice(b.Workloads, func(i, j int) bool {
		return b.Workloads[i].WorkloadKey.String() < b.Workloads[j].WorkloadKey.String()
	})
	return b, nil
}
