package infra

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/infra"
)

func (c *Controller) owned(ctx context.Context, req ctlruntime.Request, kind infra.Kind) ([]v1alpha1.ComputeContract, error) {
	var list v1alpha1.ComputeContractList
	if err := req.List(ctx, &list, client.MatchingLabels{v1alpha1.InfraKindLabel: string(kind), v1alpha1.InfraNameLabel: req.Name}); err != nil {
		return nil, fmt.Errorf("infra: listing the ComputeContracts of %s %s: %w", kind, req.Name, err)
	}
	return list.Items, nil
}

func ownerRef(v view) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: string(v.kind), Name: v.object.GetName(),
		UID: v.object.GetUID(), Controller: ptr.To(true)}
}

func childLabels(v view, extra ...string) map[string]string {
	out := map[string]string{v1alpha1.InfraKindLabel: string(v.kind), v1alpha1.InfraNameLabel: v.object.GetName()}
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i]] = extra[i+1]
	}
	return out
}

func (r *reconciler[T]) contracts(ctx context.Context, req ctlruntime.Request, v view, existing []v1alpha1.ComputeContract,
	ending bool) ([]v1alpha1.InfraContractStatus, int, error) {
	byName := map[string]*v1alpha1.ComputeContract{}
	for i := range existing {
		byName[existing[i].Labels[v1alpha1.InfraContractLabel]] = &existing[i]
	}
	previous := map[string]v1alpha1.InfraContractPhase{}
	for _, s := range v.status.Contracts {
		previous[s.Name] = s.Phase
	}
	secrets := map[string]*commonv1.SecretRef{}
	for i := range v.spec.Secrets {
		secrets[v.spec.Secrets[i].Contract] = &v.spec.Secrets[i].SecretsRef
	}
	nodes := map[string]*v1alpha1.InfraContractNode{}
	for i := range v.spec.Nodes {
		nodes[v.spec.Nodes[i].Contract] = &v.spec.Nodes[i]
	}
	declared := map[string]bool{}
	left := 0
	var out []v1alpha1.InfraContractStatus
	for _, c := range v.declaration.Contracts {
		declared[c.Name] = true
		cc := byName[c.Name]
		state := c.State
		if ending {
			state = v1alpha1.InfraContractReleased
		}
		step := infra.Plan(state, cc, previous[c.Name])
		if step.Create {
			created, err := r.create(ctx, req, v, c, secrets[c.Name], nodes[c.Name])
			if err != nil {
				return nil, 0, err
			}
			cc = created
		} else if err := r.apply(ctx, req, v, cc, step); err != nil {
			return nil, 0, err
		}
		if cc != nil && !infra.Finished(cc) {
			left++
		}
		out = append(out, contractStatus(c.Name, cc, step.Phase))
	}
	for name, cc := range byName {
		if declared[name] {
			continue
		}
		if err := r.apply(ctx, req, v, cc, infra.Orphan(cc)); err != nil {
			return nil, 0, err
		}
		left++
	}
	return out, left, nil
}

func contractStatus(name string, cc *v1alpha1.ComputeContract, phase v1alpha1.InfraContractPhase) v1alpha1.InfraContractStatus {
	out := v1alpha1.InfraContractStatus{Name: name, Phase: phase}
	if cc == nil {
		return out
	}
	out.ComputeContract = cc.Name
	out.Message = cc.Status.Message
	if state := cc.Status.Contract; state != nil {
		out.ReceiptURI = state.ReceiptURI
		if state.Winner != nil {
			out.Winner = state.Winner.DID
		}
	}
	return out
}

func (r *reconciler[T]) create(ctx context.Context, req ctlruntime.Request, v view, c v1alpha1.InfraContract,
	secrets *commonv1.SecretRef, node *v1alpha1.InfraContractNode) (*v1alpha1.ComputeContract, error) {
	name := infra.ChildName(v.kind, v.object.GetName(), c.Name)
	cc := &v1alpha1.ComputeContract{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: childLabels(v, v1alpha1.InfraContractLabel, c.Name),
		OwnerReferences: []metav1.OwnerReference{ownerRef(v)}}, Spec: infra.ContractSpec(c, secrets, node)}
	if err := req.Apply(ctx, cc); err != nil {
		return nil, fmt.Errorf("infra: applying the ComputeContract %s: %w", name, err)
	}
	r.c.log.Info("infra_contract_requested", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "contract", c.Name,
		"computeContract", name)
	return cc, nil
}

func (r *reconciler[T]) apply(ctx context.Context, req ctlruntime.Request, v view, cc *v1alpha1.ComputeContract, step infra.Step) error {
	switch {
	case cc == nil:
		return nil
	case step.Release:
		next := &v1alpha1.ComputeContract{ObjectMeta: metav1.ObjectMeta{Name: cc.Name, Labels: cc.Labels, OwnerReferences: cc.OwnerReferences}}
		cc.Spec.DeepCopyInto(&next.Spec)
		next.Spec.Release = true
		if err := req.Apply(ctx, next); err != nil {
			return fmt.Errorf("infra: releasing the ComputeContract %s: %w", cc.Name, err)
		}
		r.c.log.Info("infra_contract_release_requested", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "computeContract", cc.Name,
			"phase", cc.Status.Phase)
	case step.Delete:
		if !cc.DeletionTimestamp.IsZero() {
			return nil
		}
		if err := req.Delete(ctx, cc); err != nil {
			return fmt.Errorf("infra: deleting the ComputeContract %s: %w", cc.Name, err)
		}
		r.c.log.Info("infra_contract_deleted", "kind", v.kind, "workspace", req.Workspace, "name", req.Name, "computeContract", cc.Name,
			"phase", cc.Status.Phase)
	}
	return nil
}
