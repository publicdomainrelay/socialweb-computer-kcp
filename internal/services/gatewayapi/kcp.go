package gatewayapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	appgateway "github.com/johnandersen777/socialweb-computer-kcp/internal/app/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clientcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

var (
	nodeClaimGVR = v1alpha1.NodeClaimGVR

	clusterLinkGVR = v1alpha1.ClusterLinkGVR

	computeContractGVR = v1alpha1.ComputeContractGVR

	configMapGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	exposureGVR = exposurev1alpha1.ExposureGVR

	exposureServiceGVR = exposurev1alpha1.ServiceGVR
)

func gatewayServerFor(kcpClients component.KCPClients, nodeClaimAgent *kcpclient.ServiceAccountRef, workspace string, certs *clientcert.Issuer, publicHost, clusterStateDir string) (*gateway.Server, error) {
	server := &gateway.Server{}
	tenant, err := newTenantAPI(clusterStateDir)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the tenant API client: %w", err)
	}
	server.Tenant = tenant
	if kcpClients == nil {
		return server, nil
	}
	client, err := kcpClients.Client(workspace)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the gateway kcp client: %w", err)
	}
	restConfig := client.RESTConfig()
	if restConfig == nil || restConfig.Host == "" {
		return server, nil
	}
	agent := nodeClaimAgent
	if agent == nil {
		agent, _ = kcpclient.ParseServiceAccountRef(appgateway.WebAppAccountRef(workspace))
	}
	opts := kcpclient.Options{Host: restConfig.Host, RestConfig: restConfig, PublicHost: publicHost,
		HostProcessWorkspace: workspace, NodeClaimAgent: agent, RecordWorkspaceOwner: true}
	if clusterStateDir != "" {
		opts.Tenant = ownerCredentials(clusterStateDir)
	}
	if certs != nil {
		opts.Kubeconfigs = certs
	}
	store, err := kcpclient.New(opts)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the gateway kcp store: %w", err)
	}
	if certs != nil {
		certs.Recorder = store
	}
	server.Store = store
	server.Deny = store
	server.Revoker = store
	server.Feed = store
	return server, nil
}

func kcpStoreFor(kcpClients component.KCPClients, workspace string) (*kcpstore.Store, error) {
	if kcpClients == nil {
		return nil, errors.New("gatewayapi: no kcp client")
	}
	client, err := kcpClients.Client(workspace)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: kcp client for %s: %w", workspace, err)
	}
	restConfig := client.RESTConfig()
	if restConfig == nil || restConfig.Host == "" {
		return nil, fmt.Errorf("gatewayapi: the kcp client for %s has no rest config", workspace)
	}
	return kcpstore.New(kcpstore.Options{Host: restConfig.Host, RestConfig: restConfig})
}

type homeSource interface {
	Homes(ctx context.Context) ([]string, error)
}

type staticHomes []string

func (s staticHomes) Homes(context.Context) ([]string, error) {
	return append([]string(nil), s...), nil
}

type homeIndex struct {
	configmaps *kcpstore.Resource[corev1.ConfigMap]

	workspace string

	namespace string

	fallback []string

	listed *lease.Source[homeListing]
}

type homeListing struct {
	homes []string

	err error
}

const homeIndexRefresh = 2 * time.Second

func newHomeIndex(store *kcpstore.Store, workspace, namespace string, fallback []string) *homeIndex {
	workspace = cmp.Or(workspace, "root")
	namespace = cmp.Or(namespace, "default")
	i := &homeIndex{configmaps: kcpstore.Of[corev1.ConfigMap](store, configMapGVR), workspace: workspace,
		namespace: namespace, fallback: append([]string(nil), fallback...)}
	i.listed = lease.New(i.list)
	return i
}

func (i *homeIndex) Homes(ctx context.Context) ([]string, error) {
	listed, err := i.listed.Get(ctx)
	if err != nil {
		return append([]string(nil), i.fallback...), err
	}
	return append([]string(nil), listed.homes...), listed.err
}

func (i *homeIndex) list(ctx context.Context) (homeListing, time.Time, error) {
	items, err := i.configmaps.ListIn(ctx, i.workspace, i.namespace)
	homes := []string{}
	if err == nil {
		seen := map[string]bool{}
		for idx := range items {
			cm := &items[idx]
			if cm.Labels[kcpclient.NodeClaimHomeLabel] != "true" {
				continue
			}
			home := cm.Data[kcpclient.NodeClaimHomeKey]
			if home == "" || seen[home] {
				continue
			}
			seen[home] = true
			homes = append(homes, home)
		}
	} else {
		homes = append([]string(nil), i.fallback...)
	}
	return homeListing{homes: homes, err: err}, time.Now().Add(homeIndexRefresh), nil
}

type claimStore struct {
	claims *kcpstore.Resource[v1alpha1.NodeClaim]

	homes homeSource
}

func newClaimStore(store *kcpstore.Store, homes homeSource) *claimStore {
	return &claimStore{claims: kcpstore.OfClusterScoped[v1alpha1.NodeClaim](store, nodeClaimGVR), homes: homes}
}

func (s *claimStore) List(ctx context.Context) ([]gatewayhttp.NodeClaim, error) {
	out := []gatewayhttp.NodeClaim{}
	homes, err := s.homes.Homes(ctx)
	if err != nil {
		return nil, err
	}
	for _, home := range homes {
		items, err := s.claims.List(ctx, home)
		if err != nil {
			if kcpstore.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		for i := range items {
			out = append(out, gatewayhttp.NodeClaim{Workspace: home, Claim: items[i]})
		}
	}
	return out, nil
}

func (s *claimStore) Get(ctx context.Context, claim gatewayhttp.NodeClaim) (*gatewayhttp.NodeClaim, error) {
	obj, err := s.claims.Get(ctx, ref.New(claim.Workspace, "", claim.Name()))
	if err != nil {
		if kcpstore.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}
	return &gatewayhttp.NodeClaim{Workspace: claim.Workspace, Claim: *obj}, nil
}

func (s *claimStore) ReplaceStatus(ctx context.Context, claim gatewayhttp.NodeClaim, status v1alpha1.NodeClaimStatus) error {
	raw, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return fmt.Errorf("gatewayapi: encoding the NodeClaim status: %w", err)
	}
	var lastErr error
	for range 5 {
		target := ref.New(claim.Workspace, "", claim.Claim.Name)
		target.ResourceVersion = claim.Claim.ResourceVersion
		lastErr = s.claims.PatchStatus(ctx, target, raw)
		if lastErr == nil {
			return nil
		}
		if !apierrors.IsConflict(lastErr) {
			return lastErr
		}
		current, getErr := s.claims.Get(ctx, ref.New(claim.Workspace, "", claim.Claim.Name))
		if getErr != nil {
			return lastErr
		}
		claim.Claim.ResourceVersion = current.ResourceVersion
	}
	return lastErr
}

func (s *claimStore) AddFinalizer(ctx context.Context, claim gatewayhttp.NodeClaim) error {
	target := ref.New(claim.Workspace, "", claim.Claim.Name)
	target.ResourceVersion = claim.Claim.ResourceVersion
	seen := map[string]bool{}
	finalizers := make([]string, 0, len(claim.Claim.Finalizers)+1)
	for _, finalizer := range claim.Claim.Finalizers {
		if seen[finalizer] {
			continue
		}
		seen[finalizer] = true
		finalizers = append(finalizers, finalizer)
	}
	if !seen[v1alpha1.FinalizerNodeClaim] {
		finalizers = append(finalizers, v1alpha1.FinalizerNodeClaim)
	}
	return s.claims.AddFinalizer(ctx, target, finalizers)
}

func (s *claimStore) RemoveFinalizer(ctx context.Context, claim gatewayhttp.NodeClaim) error {
	target := ref.New(claim.Workspace, "", claim.Claim.Name)
	target.ResourceVersion = claim.Claim.ResourceVersion
	return s.claims.RemoveKnownFinalizer(ctx, target, claim.Claim.Finalizers, v1alpha1.FinalizerNodeClaim)
}

type clusterLinkReader struct {
	links *kcpstore.Resource[v1alpha1.ClusterLink]
}

func newClusterLinkReader(store *kcpstore.Store) *clusterLinkReader {
	return &clusterLinkReader{links: kcpstore.OfClusterScoped[v1alpha1.ClusterLink](store, clusterLinkGVR)}
}

func (r *clusterLinkReader) Get(ctx context.Context, workspace, name string) (*gatewayhttp.ClusterLinkView, error) {
	link, err := r.links.Get(ctx, ref.New(workspace, "", name))
	if err != nil {
		if kcpstore.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if link == nil {
		return nil, nil
	}
	return &gatewayhttp.ClusterLinkView{Workspace: workspace, Name: link.Name, Status: link.Status}, nil
}

type contractClient struct {
	contracts *kcpstore.Resource[v1alpha1.ComputeContract]

	workspace string

	quota int64
}

func newContractClient(store *kcpstore.Store, workspace string, quota int64) *contractClient {
	return &contractClient{
		contracts: kcpstore.OfClusterScoped[v1alpha1.ComputeContract](store, computeContractGVR),
		workspace: workspace,
		quota:     quota,
	}
}

func (c *contractClient) Get(ctx context.Context, name string) (*v1alpha1.ComputeContract, error) {
	contract, err := c.contracts.Get(ctx, ref.New(c.workspace, "", name))
	if err != nil {
		if kcpstore.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return contract, nil
}

func (c *contractClient) List(ctx context.Context) ([]v1alpha1.ComputeContract, error) {
	return c.contracts.List(ctx, c.workspace)
}

func (c *contractClient) CreateIfAbsent(ctx context.Context, contract *v1alpha1.ComputeContract) (bool, error) {
	if contract == nil {
		return false, errors.New("gatewayapi: no contract to create")
	}
	existing, err := c.Get(ctx, contract.Name)
	if err != nil {
		return false, err
	}
	if existing != nil {
		return false, nil
	}
	if c.quota > 0 {
		current, err := c.List(ctx)
		if err != nil {
			return false, err
		}
		if int64(len(current)) >= c.quota {
			return false, gatewayhttp.ErrQuotaExceeded
		}
	}
	if err := c.contracts.Create(ctx, c.workspace, contract); err != nil {
		return false, err
	}
	return true, nil
}

func (c *contractClient) Patch(ctx context.Context, name string, patch any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("gatewayapi: encoding the contract patch: %w", err)
	}
	return c.contracts.PatchMerge(ctx, ref.New(c.workspace, "", name), raw)
}

func (c *contractClient) Delete(ctx context.Context, name string) error {
	return c.contracts.Delete(ctx, ref.New(c.workspace, "", name))
}
