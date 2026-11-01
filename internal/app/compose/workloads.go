package compose

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
)

type Workload struct {
	Kind string

	Workspace string

	Name string
}

var replaceableLost = map[v1alpha1.HostProcessLostReason]bool{v1alpha1.HostProcessLostUnitGone: true, v1alpha1.HostProcessLostHostRebooted: true,
	v1alpha1.HostProcessLostManagerRestarted: true, v1alpha1.HostProcessLostIdentityMismatch: true}

func (c *Composer) SetWorkloads(setName, profile string, values any, include ...string) ([]Workload, error) {
	set, err := infraSet(setName, profile, include)
	if err != nil {
		return nil, err
	}
	return c.renderWorkloads(set, values)
}

func (c *Composer) PhaseWorkloads(setName string, values any, phases ...string) ([]Workload, error) {
	set, err := phaseSet(setName, phases)
	if err != nil {
		return nil, err
	}
	return c.renderWorkloads(set, values)
}

func (c *Composer) renderWorkloads(set manifests.Set, values any) ([]Workload, error) {
	set.Base = c.Workspace()
	rendered, err := set.Render(values)
	if err != nil {
		return nil, fmt.Errorf("compose: the workloads of set %s: %w", set.Name, err)
	}
	var out []Workload
	for _, phase := range rendered {
		for _, obj := range phase.Objects {
			if u := obj.Unstructured; u != nil && (u.GetKind() == KindHostProcess || u.GetKind() == KindOpenBaoServer) {
				out = append(out, Workload{Kind: u.GetKind(), Workspace: phase.Workspace, Name: u.GetName()})
			}
		}
	}
	return out, nil
}

func (c *Composer) CheckWorkloads(ctx context.Context, r Root) func([]Workload, error) (bool, error) {
	return func(list []Workload, err error) (bool, error) {
		if err != nil {
			return false, err
		}
		intact, why, err := c.workloadsIntact(ctx, r, list)
		if err != nil {
			return false, err
		}
		if !intact {
			c.log.Info("a set workload is missing or Lost; the set is applied again", "root", r.Owner(), "why", why)
			c.Unapply(r)
		}
		return intact, nil
	}
}

func (c *Composer) workloadsIntact(ctx context.Context, r Root, list []Workload) (bool, string, error) {
	for _, w := range list {
		switch w.Kind {
		case KindHostProcess:
			ri, err := c.Client(w.Workspace, HostProcessGVR)
			if err != nil {
				return false, "", err
			}
			hp, err := kube.Get[v1alpha1.HostProcess](ctx, ri, w.Name)
			if apierrors.IsNotFound(err) {
				return false, "HostProcess " + w.Name + " is missing", nil
			}
			if err != nil {
				return false, "", err
			}
			if hp.DeletionTimestamp != nil {
				return false, "HostProcess " + w.Name + " is being deleted", nil
			}
			if hp.Status.Phase == v1alpha1.HostProcessPhaseLost && replaceableLost[hp.Status.LostReason] {
				if err := ri.Delete(ctx, w.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return false, "", fmt.Errorf("compose: deleting the Lost HostProcess %s: %w", w.Name, err)
				}
				c.log.Info("a Lost set HostProcess is deleted to be placed again", "root", r.Owner(), "hostprocess", w.Name, "reason", hp.Status.LostReason)
				return false, "HostProcess " + w.Name + " was Lost (" + string(hp.Status.LostReason) + ")", nil
			}
		case KindOpenBaoServer:
			ri, err := c.Client(w.Workspace, OpenBaoServerGVR)
			if err != nil {
				return false, "", err
			}
			if _, err := kube.Get[v1alpha1.OpenBaoServer](ctx, ri, w.Name); apierrors.IsNotFound(err) {
				return false, "OpenBaoServer " + w.Name + " is missing", nil
			} else if err != nil {
				return false, "", err
			}
		}
	}
	return true, "", nil
}

func (c *Composer) Unapply(r Root) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.applied, string(r.UID))
}
