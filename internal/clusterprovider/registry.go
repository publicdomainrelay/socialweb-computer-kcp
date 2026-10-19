package clusterprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	kcpv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
)

var (
	socialwebResource = v1alpha1.GroupVersion
	tenancyResource   = kcpv1alpha1.SchemeGroupVersion
)

type Ref struct {
	Account string
	DID     string
	Home    string
	Name    string
}

func (r Ref) MountPath() string { return kcpclient.MountPath(r.DID, r.Name) }

func (r Ref) Key() (string, error) { return kcpclient.ClusterKey(r.Home) }

type RegistryOptions struct {
	Host string

	RestConfig *rest.Config

	Transport http.RoundTripper
}

type Registry struct {
	cfg    *rest.Config
	http   *http.Client
	codecs runtime.NegotiatedSerializer
	host   string
}

func NewRegistry(opts RegistryOptions) (*Registry, error) {
	if opts.Host == "" {
		return nil, errors.New("clusterprovider: Host is required")
	}
	cfg := &rest.Config{}
	if opts.RestConfig != nil {
		cfg = rest.CopyConfig(opts.RestConfig)
	}
	cfg.Host = strings.TrimSuffix(opts.Host, "/")
	cfg.ContentType = "application/json"
	cfg.AcceptContentTypes = "application/json"
	if opts.Transport != nil {
		cfg.Transport = opts.Transport
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: %w", err)
	}
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	return &Registry{
		cfg:    cfg,
		http:   httpClient,
		codecs: serializer.NewCodecFactory(scheme).WithoutConversion(),
		host:   strings.TrimSuffix(opts.Host, "/"),
	}, nil
}

func (r *Registry) Refs(ctx context.Context) ([]Ref, error) {
	accounts, _, err := r.workspaces(ctx, kcpclient.RootWorkspace)
	if err != nil {
		return nil, err
	}
	var out []Ref
	for _, account := range accounts {
		path := kcpclient.RootWorkspace + ":" + account
		homes, deleting, err := r.workspaces(ctx, path)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, home := range homes {
			ref := Ref{Account: path, Home: path + ":" + home}
			list, err := r.listClusters(ctx, ref.Home)
			if apierrors.IsNotFound(err) || (err != nil && (deleting[home] || r.goneOrDeleting(ctx, path, home))) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				cr := list.Items[i]
				out = append(out, Ref{
					Account: path,
					DID:     cr.Spec.Account,
					Home:    ref.Home,
					Name:    cr.Name,
				})
			}
		}
	}
	return out, nil
}

func (r *Registry) listClusters(ctx context.Context, home string) (*v1alpha1.ClusterList, error) {
	c, err := r.clusters(ctx, home)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.ClusterList
	if err := c.Get().Resource("clusters").Do(ctx).Into(&list); err != nil {
		return nil, fmt.Errorf("clusterprovider: list clusters in %s: %w", home, err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Account == "" {
			return nil, fmt.Errorf(
				"clusterprovider: Cluster %s in %s has no spec.account, so no mount path can be derived for it",
				list.Items[i].Name, home)
		}
	}
	return &list, nil
}

func (r *Registry) Read(ctx context.Context, ref Ref) (*v1alpha1.Cluster, error) {
	c, err := r.clusters(ctx, ref.Home)
	if err != nil {
		return nil, err
	}
	var cl v1alpha1.Cluster
	if err := c.Get().Resource("clusters").Name(ref.Name).Do(ctx).Into(&cl); err != nil {
		return nil, fmt.Errorf("clusterprovider: read cluster %s in %s: %w", ref.Name, ref.Home, err)
	}
	return &cl, nil
}

func (r *Registry) Nodes(ctx context.Context, ref Ref) ([]v1alpha1.Node, error) {
	c, err := r.clusters(ctx, ref.Home)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.NodeList
	if err := c.Get().Resource("nodes").Do(ctx).Into(&list); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("clusterprovider: list nodes in %s: %w", ref.Home, err)
	}
	out := make([]v1alpha1.Node, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.Cluster == ref.Name {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

func (r *Registry) WriteNode(ctx context.Context, ref Ref, nodeName string, status v1alpha1.NodeStatus) error {
	c, err := r.clusters(ctx, ref.Home)
	if err != nil {
		return err
	}
	if err := kcpclient.WriteNodeTo(ctx, c, ref.Name, nodeName, status); err != nil {
		return fmt.Errorf("clusterprovider: node %s in %s: %w", nodeName, ref.Home, err)
	}
	return nil
}

func (r *Registry) WriteStatus(ctx context.Context, ref Ref, st v1alpha1.ClusterStatus) error {
	c, err := r.clusters(ctx, ref.Home)
	if err != nil {
		return err
	}
	body, err := statusPatch(st)
	if err != nil {
		return err
	}
	if err := c.Patch(types.MergePatchType).SubResource("status").Resource("clusters").
		Name(ref.Name).Body(body).Do(ctx).Into(&v1alpha1.Cluster{}); err != nil {
		return fmt.Errorf("clusterprovider: write status for %s in %s: %w", ref.Name, ref.Home, err)
	}
	return nil
}

func (r *Registry) RemoveFinalizer(ctx context.Context, ref Ref) error {
	c, err := r.clusters(ctx, ref.Home)
	if err != nil {
		return err
	}
	var cl v1alpha1.Cluster
	if err := c.Get().Resource("clusters").Name(ref.Name).Do(ctx).Into(&cl); err != nil {
		return fmt.Errorf("clusterprovider: reading %s in %s to release its finalizer: %w", ref.Name, ref.Home, err)
	}
	remaining := make([]string, 0, len(cl.Finalizers))
	for _, f := range cl.Finalizers {
		if f != v1alpha1.FinalizerCluster {
			remaining = append(remaining, f)
		}
	}
	if len(remaining) == len(cl.Finalizers) {
		return nil
	}
	body, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/finalizers", "value": cl.Finalizers},
		{"op": "add", "path": "/metadata/finalizers", "value": remaining},
	})
	if err != nil {
		return fmt.Errorf("clusterprovider: releasing the finalizer of %s: %w", ref.Name, err)
	}
	if err := c.Patch(types.JSONPatchType).Resource("clusters").Name(ref.Name).
		Body(body).Do(ctx).Into(&v1alpha1.Cluster{}); err != nil {
		return fmt.Errorf("clusterprovider: releasing the finalizer of %s in %s: %w", ref.Name, ref.Home, err)
	}
	return nil
}

func (r *Registry) MountStatus(ctx context.Context, ref Ref) int {
	url := fmt.Sprintf("%s/clusters/%s/version", r.host, ref.MountPath())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func (r *Registry) IdentityState(ctx context.Context, ref Ref) (v1alpha1.IdentityState, error) {
	c, err := r.resource(ref.Account, socialwebResource)
	if err != nil {
		return "", err
	}
	var list v1alpha1.AtprotoIdentityList
	if err := c.Get().Resource("atprotoidentities").Do(ctx).Into(&list); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("clusterprovider: list identities in %s: %w", ref.Account, err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Scope == v1alpha1.IdentityScopeAccount {
			return list.Items[i].Status.State, nil
		}
	}
	return "", nil
}

func (r *Registry) workspaces(ctx context.Context, logicalCluster string) ([]string, map[string]bool, error) {
	c, err := r.resource(logicalCluster, tenancyResource)
	if err != nil {
		return nil, nil, err
	}
	var list kcpv1alpha1.WorkspaceList
	if err := c.Get().Resource("workspaces").Do(ctx).Into(&list); err != nil {
		return nil, nil, fmt.Errorf("clusterprovider: list workspaces in %s: %w", logicalCluster, err)
	}
	var out []string
	deleting := map[string]bool{}
	for i := range list.Items {
		if list.Items[i].Spec.Mount == nil {
			out = append(out, list.Items[i].Name)
			deleting[list.Items[i].Name] = list.Items[i].DeletionTimestamp != nil
		}
	}
	return out, deleting, nil
}

func (r *Registry) goneOrDeleting(ctx context.Context, parent, name string) bool {
	c, err := r.resource(parent, tenancyResource)
	if err != nil {
		return false
	}
	var ws kcpv1alpha1.Workspace
	err = c.Get().Resource("workspaces").Name(name).Do(ctx).Into(&ws)
	return apierrors.IsNotFound(err) || (err == nil && ws.DeletionTimestamp != nil)
}

func (r *Registry) clusters(ctx context.Context, logicalCluster string) (rest.Interface, error) {
	return r.resource(logicalCluster, socialwebResource)
}

func (r *Registry) resource(logicalCluster string, gv schema.GroupVersion) (rest.Interface, error) {
	cfg := rest.CopyConfig(r.cfg)
	cfg.GroupVersion = &gv
	cfg.APIPath = "/clusters/" + logicalCluster + "/apis"
	cfg.NegotiatedSerializer = r.codecs
	c, err := rest.RESTClientForConfigAndClient(cfg, r.http)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: %s: %w", logicalCluster, err)
	}
	return c, nil
}

func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("clusterprovider: %w", err)
	}
	if err := kcpv1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("clusterprovider: %w", err)
	}
	metav1.AddToGroupVersion(s, schema.GroupVersion{Version: "v1"})
	return s, nil
}
