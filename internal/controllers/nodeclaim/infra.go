package nodeclaim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/nodeclaim"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

const (
	ClusterKind = "Cluster"

	RepoKind = "AtprotoRepo"
)

var errInfraDeleting = errors.New("nodeclaim: the cluster's Infra is being deleted")

type entry struct {
	contract v1alpha1.InfraContract

	node v1alpha1.InfraContractNode

	secrets *commonv1.SecretRef
}

func entryOf(b *contract.Binding, claim contract.Claim) v1alpha1.NodeClaimInfra {
	name, entry := b.Mapper.InfraName(claim), b.Mapper.EntryName(claim)
	return v1alpha1.NodeClaimInfra{Workspace: b.ContractWorkspace, Name: name, Contract: entry,
		ComputeContract: infra.ChildName(infra.KindInfra, name, entry)}
}

func clusterRef(claim contract.Claim) (commonv1.ObjectRef, bool) {
	for _, o := range claim.Object.OwnerReferences {
		if o.Kind == ClusterKind && o.Name != "" {
			return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: ClusterKind, Name: o.Name, Workspace: claim.Workspace}, true
		}
	}
	return commonv1.ObjectRef{}, false
}

func infras(store *kcpstore.Store) *kcpstore.Resource[v1alpha1.Infra] {
	return kcpstore.OfClusterScoped[v1alpha1.Infra](store, v1alpha1.InfraGVR)
}

func readContract(ctx context.Context, store *kcpstore.Store, at v1alpha1.NodeClaimInfra) (*v1alpha1.ComputeContract, error) {
	cc, err := kcpstore.OfClusterScoped[v1alpha1.ComputeContract](store, v1alpha1.ComputeContractGVR).Get(ctx, ref.New(at.Workspace, "", at.ComputeContract))
	if kcpstore.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("nodeclaim: reading the ComputeContract %s of %s: %w", at.ComputeContract, at.Workspace, err)
	}
	return cc, nil
}

func readCluster(ctx context.Context, store *kcpstore.Store, cluster commonv1.ObjectRef) (*v1alpha1.Cluster, error) {
	cl, err := kcpstore.OfClusterScoped[v1alpha1.Cluster](store, v1alpha1.ClusterGVR).Get(ctx, ref.New(cluster.Workspace, "", cluster.Name))
	if kcpstore.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("nodeclaim: reading the Cluster %s of %s: %w", cluster.Name, cluster.Workspace, err)
	}
	return cl, nil
}

func clusterAlive(ctx context.Context, store *kcpstore.Store, cluster commonv1.ObjectRef) (bool, error) {
	cl, err := readCluster(ctx, store, cluster)
	return cl != nil && cl.DeletionTimestamp.IsZero(), err
}

func recordFor(b *contract.Binding, cl *v1alpha1.Cluster) *v1alpha1.InfraRecordBinding {
	if b.Record == nil || cl == nil || !cl.Spec.PublishInfra || !cl.DeletionTimestamp.IsZero() || strings.TrimSpace(cl.Spec.Account) == "" {
		return nil
	}
	return &v1alpha1.InfraRecordBinding{RepoRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: RepoKind,
		Name: atprotorepo.RepoName(strings.TrimSpace(cl.Spec.Account)), Workspace: b.Record.RepoWorkspace},
		Sync: v1alpha1.InfraRecordSyncPublish, EventSourceRefs: slices.Clone(b.Record.EventSourceRefs)}
}

func readInfra(ctx context.Context, store *kcpstore.Store, workspace, name string) (*v1alpha1.Infra, error) {
	got, err := infras(store).Get(ctx, ref.New(workspace, "", name))
	if kcpstore.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("nodeclaim: reading the Infra %s of %s: %w", name, workspace, err)
	}
	return got, nil
}

func (c *Controller) entry(b *contract.Binding, claim contract.Claim, at v1alpha1.NodeClaimInfra, adopt bool, bases contract.Bases) entry {
	ct, node := b.Mapper.Entry(claim, adopt, bases)
	ct.Name, node.Contract = at.Contract, at.Contract
	out := entry{contract: ct, node: node}
	if !adopt {
		secrets := b.Mapper.BundleRef(at.ComputeContract)
		out.secrets = &secrets
	}
	return out
}

func (c *Controller) declare(ctx context.Context, store *kcpstore.Store, claim contract.Claim, at v1alpha1.NodeClaimInfra, e *entry,
	cl *v1alpha1.Cluster) error {
	b, _ := c.current()
	if b == nil {
		return nil
	}
	state := b.Mapper.State(claim)
	c.infraMu.Lock()
	defer c.infraMu.Unlock()
	cur, err := readInfra(ctx, store, at.Workspace, at.Name)
	if err != nil {
		return err
	}
	if cur != nil && !cur.DeletionTimestamp.IsZero() {
		if state == v1alpha1.InfraContractReleased {
			return nil
		}
		return errInfraDeleting
	}
	var spec v1alpha1.InfraSpec
	if cur != nil {
		cur.Spec.DeepCopyInto(&spec)
	}
	i := slices.IndexFunc(spec.Contracts, func(ct v1alpha1.InfraContract) bool { return ct.Name == at.Contract })
	if i < 0 {
		if e == nil {
			return nil
		}
		spec.Contracts = append(spec.Contracts, v1alpha1.InfraContract{})
		i = len(spec.Contracts) - 1
	}
	if e != nil {
		spec.Contracts[i] = e.contract
		spec.Nodes = putNode(spec.Nodes, e.node)
		spec.Secrets = putSecret(spec.Secrets, at.Contract, e.secrets)
	}
	spec.Contracts[i].State = state
	label, name := "", claim.Object.Spec.ClusterRef.Name
	if cluster, ok := clusterRef(claim); ok {
		spec.ClusterRef, label, name = &cluster, cluster.Name, cluster.Name
	}
	spec.Description = "the nodes of the tenant cluster " + name
	spec.Record = recordFor(b, cl)
	if cur != nil && equality.Semantic.DeepEqual(cur.Spec, spec) {
		return nil
	}
	if err := applyInfra(ctx, store, at.Workspace, at.Name, label, spec); err != nil {
		return err
	}
	c.log.Info("node_claim_entry_declared", "claim", claim.Key(), "infra", at.Name, "entry", at.Contract, "state", state, "created", cur == nil)
	return nil
}

func (c *Controller) dropEntry(ctx context.Context, store *kcpstore.Store, workspace, name, entryName string) error {
	c.infraMu.Lock()
	defer c.infraMu.Unlock()
	cur, err := readInfra(ctx, store, workspace, name)
	if err != nil || cur == nil || !cur.DeletionTimestamp.IsZero() {
		return err
	}
	var spec v1alpha1.InfraSpec
	cur.Spec.DeepCopyInto(&spec)
	before := len(spec.Contracts)
	spec.Contracts = slices.DeleteFunc(spec.Contracts, func(ct v1alpha1.InfraContract) bool { return ct.Name == entryName })
	spec.Nodes = slices.DeleteFunc(spec.Nodes, func(n v1alpha1.InfraContractNode) bool { return n.Contract == entryName })
	spec.Secrets = slices.DeleteFunc(spec.Secrets, func(s v1alpha1.InfraContractSecret) bool { return s.Contract == entryName })
	if len(spec.Contracts) == before {
		return nil
	}
	if len(spec.Contracts) == 0 {
		if err := infras(store).Delete(ctx, ref.New(workspace, "", name)); err != nil {
			return err
		}
		c.log.Info("node_claim_infra_deleted", "infra", name, "workspace", workspace, "reason", "no node is left")
		return nil
	}
	cluster := ""
	if spec.ClusterRef != nil {
		cluster = spec.ClusterRef.Name
	}
	if err := applyInfra(ctx, store, workspace, name, cluster, spec); err != nil {
		return err
	}
	c.log.Info("node_claim_entry_dropped", "infra", name, "entry", entryName, "left", len(spec.Contracts))
	return nil
}

func (c *Controller) deleteInfra(ctx context.Context, store *kcpstore.Store, workspace, name, reason string) error {
	c.infraMu.Lock()
	defer c.infraMu.Unlock()
	cur, err := readInfra(ctx, store, workspace, name)
	if err != nil || cur == nil || !cur.DeletionTimestamp.IsZero() {
		return err
	}
	if err := infras(store).Delete(ctx, ref.New(workspace, "", name)); err != nil {
		return err
	}
	c.log.Info("node_claim_infra_deleted", "infra", name, "workspace", workspace, "reason", reason, "entries", len(cur.Spec.Contracts))
	return nil
}

func applyInfra(ctx context.Context, store *kcpstore.Store, workspace, name, cluster string, spec v1alpha1.InfraSpec) error {
	meta := map[string]any{"name": name}
	if len(validation.IsValidLabelValue(cluster)) == 0 {
		meta["labels"] = map[string]string{v1alpha1.InfraClusterLabel: cluster}
	}
	body, err := json.Marshal(map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": string(infra.KindInfra),
		"metadata": meta, "spec": spec})
	if err != nil {
		return fmt.Errorf("nodeclaim: encoding the Infra %s: %w", name, err)
	}
	return infras(store).Apply(ctx, ref.New(workspace, "", name), body, contract.FieldManager)
}

func putNode(nodes []v1alpha1.InfraContractNode, node v1alpha1.InfraContractNode) []v1alpha1.InfraContractNode {
	out := slices.DeleteFunc(nodes, func(n v1alpha1.InfraContractNode) bool { return n.Contract == node.Contract })
	return append(out, node)
}

func putSecret(secrets []v1alpha1.InfraContractSecret, name string, ref *commonv1.SecretRef) []v1alpha1.InfraContractSecret {
	out := slices.DeleteFunc(secrets, func(s v1alpha1.InfraContractSecret) bool { return s.Contract == name })
	if ref == nil {
		return out
	}
	return append(out, v1alpha1.InfraContractSecret{Contract: name, SecretsRef: *ref})
}

func (c *Controller) sweep(ctx context.Context, store *kcpstore.Store, b contract.Binding) {
	items, err := infras(store).List(ctx, b.ContractWorkspace)
	if err != nil {
		if !kcpstore.IsNotFound(err) {
			c.log.Warn("node_claim_infras_unlisted", "workspace", b.ContractWorkspace, "err", err)
		}
		return
	}
	claims := kcpstore.OfClusterScoped[v1alpha1.NodeClaim](store, v1alpha1.NodeClaimGVR)
	for i := range items {
		in := &items[i]
		if len(in.Spec.Nodes) == 0 || !in.DeletionTimestamp.IsZero() {
			continue
		}
		if in.Spec.ClusterRef != nil {
			alive, err := clusterAlive(ctx, store, *in.Spec.ClusterRef)
			if err != nil {
				c.log.Warn("node_claim_cluster_unread", "infra", in.Name, "err", err)
				continue
			}
			if !alive {
				if err := c.deleteInfra(ctx, store, b.ContractWorkspace, in.Name, "the cluster is gone"); err != nil {
					c.log.Warn("node_claim_infra_delete_failed", "infra", in.Name, "err", err)
				}
				continue
			}
		}
		for _, n := range in.Spec.Nodes {
			r := n.NodeClaimRef
			claim, err := claims.Get(ctx, ref.New(r.Workspace, "", r.Name))
			stale := kcpstore.IsNotFound(err)
			if err == nil {
				stale = b.Mapper.EntryName(contract.Claim{Workspace: r.Workspace, Object: *claim}) != n.Contract
			} else if !stale {
				c.log.Warn("node_claim_entry_claim_unread", "infra", in.Name, "entry", n.Contract, "err", err)
				continue
			}
			if !stale {
				continue
			}
			if err := c.dropEntry(ctx, store, b.ContractWorkspace, in.Name, n.Contract); err != nil {
				c.log.Warn("node_claim_entry_drop_failed", "infra", in.Name, "entry", n.Contract, "err", err)
			}
		}
	}
}

func statusCopy(status v1alpha1.NodeClaimStatus) v1alpha1.NodeClaimStatus {
	var out v1alpha1.NodeClaimStatus
	status.DeepCopyInto(&out)
	return out
}
