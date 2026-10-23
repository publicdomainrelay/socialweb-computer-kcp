package cloudprovider

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/scale"
)

type Pressure struct {
	Pending int

	Running int

	Nodes int

	AllocatablePods int

	AllocatableCPUMilli int64

	AllocatableMemoryBytes int64
}

func Measure(pods []corev1.Pod, nodes []corev1.Node) Pressure {
	var p Pressure
	p.Nodes = len(nodes)
	for i := range nodes {
		n := &nodes[i]
		if !nodeReady(n) {
			continue
		}
		podCap := n.Status.Allocatable[corev1.ResourcePods]
		cpuCap := n.Status.Allocatable[corev1.ResourceCPU]
		memCap := n.Status.Allocatable[corev1.ResourceMemory]
		p.AllocatablePods += int(podCap.Value())
		p.AllocatableCPUMilli += cpuCap.MilliValue()
		p.AllocatableMemoryBytes += memCap.Value()
	}
	for i := range pods {
		pod := &pods[i]
		if scale.AppliedByTheControlPlane(pod.ObjectMeta) {
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			continue
		case corev1.PodPending:
			if pod.Spec.NodeName == "" {
				p.Pending++
			}
		case corev1.PodRunning:
			p.Running++
		}
	}
	return p
}

func Target(p Pressure, current, maxNodes, podsPerNode int) int {
	work := p.Pending + p.Running
	if work == 0 {
		return 0
	}
	if podsPerNode <= 0 {
		return current
	}
	if maxNodes < 1 {
		return current
	}
	want := min(max((work+podsPerNode-1)/podsPerNode, 1), maxNodes)
	return want
}

func nodeReady(n *corev1.Node) bool {
	for i := range n.Status.Conditions {
		c := &n.Status.Conditions[i]
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
