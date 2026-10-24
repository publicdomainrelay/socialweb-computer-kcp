package atprotoidentity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/clusterhome"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
)

const (
	Name = "atprotoidentity"

	Export = "atprotoidentities"

	ClusterExport = "clusters"

	ConditionServiceAccountsErased = "ServiceAccountsErased"
)

type Clusters interface {
	AccountClusters(ctx context.Context, account string) ([]v1alpha1.Cluster, error)
}

type Controller struct {
	clusters Clusters

	mu sync.Mutex

	revoked map[string]string
}

func Register(m *ctlruntime.Manager, clusters Clusters) (*Controller, error) {
	if clusters == nil {
		return nil, errors.New("atprotoidentity: the controller needs a cluster reader")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	c := &Controller{clusters: clusters, revoked: map[string]string{}}
	if err := ctlruntime.For[*v1alpha1.AtprotoIdentity](m, Name, c, ctlruntime.Export(Export),
		ctlruntime.InWorkspaces(accountOrUnresolved),
		ctlruntime.WatchesIn(ClusterExport, c.clusterOf)); err != nil {
		return nil, fmt.Errorf("atprotoidentity: registering the controller: %w", err)
	}
	return c, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.AtprotoIdentity) (ctlruntime.Result, error) {
	if obj.Spec.Scope != v1alpha1.IdentityScopeAccount || !obj.Spec.Revoked || obj.Spec.DID == "" || !isAccount(req.Workspace) {
		return ctlruntime.Result{}, nil
	}
	c.mu.Lock()
	c.revoked[req.Workspace] = req.Cluster
	c.mu.Unlock()

	clusters, err := c.clusters.AccountClusters(ctx, req.Workspace)
	if err != nil {
		return ctlruntime.Result{}, fmt.Errorf("atprotoidentity: the clusters of %s: %w", req.Workspace, err)
	}
	views := make([]revocation.SettleView, 0, len(clusters))
	for i := range clusters {
		cl := &clusters[i]
		if cl.Spec.Account != obj.Spec.DID {
			continue
		}
		views = append(views, revocation.SettleView{
			Name:    cl.Name,
			Settled: meta.IsStatusConditionTrue(cl.Status.Conditions, clusterprovider.ConditionRevocationSettled) || cl.DeletionTimestamp != nil,
			Serving: cl.Spec.Running && cl.Status.Phase == v1alpha1.ClusterReady,
		})
	}
	effects := revocation.Settle(views)
	state := revocation.AccountState(effects)
	detail := revocation.Detail(effects)
	held := meta.FindStatusCondition(obj.Status.Conditions, ConditionServiceAccountsErased)
	if obj.Status.State == state && held != nil && held.Message == detail {
		return ctlruntime.Result{}, nil
	}
	settled := metav1.ConditionFalse
	if state == v1alpha1.IdentityRevoked {
		settled = metav1.ConditionTrue
	}
	obj.Status.State = state
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               ConditionServiceAccountsErased,
		Status:             settled,
		Reason:             string(state),
		Message:            detail,
		ObservedGeneration: obj.Generation,
	})
	if err := req.ApplyStatus(ctx, obj); err != nil {
		return ctlruntime.Result{}, err
	}
	return ctlruntime.Result{}, nil
}

func (c *Controller) clusterOf(_ context.Context, src ctlruntime.Request, cl *v1alpha1.Cluster) []ctlruntime.Request {
	parts := strings.Split(src.Workspace, ":")
	if len(parts) != 3 || cl.Spec.Account == "" {
		return nil
	}
	account := parts[0] + ":" + parts[1]
	c.mu.Lock()
	logical, revoked := c.revoked[account]
	c.mu.Unlock()
	if !revoked {
		return nil
	}
	return []ctlruntime.Request{{Cluster: logical, Name: clusterhome.AccountHash(cl.Spec.Account)}}
}

func isAccount(workspace string) bool {
	parts := strings.Split(workspace, ":")
	return len(parts) == 2 && parts[0] == clusterhome.RootWorkspace && parts[1] != ""
}

func accountOrUnresolved(workspace string) bool {
	return !strings.Contains(workspace, ":") || isAccount(workspace)
}
