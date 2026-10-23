package kcpstore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/clientlimit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
)

func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

var ErrNoPath = errors.New("kcpstore: the logical cluster carries no kcp.io/path annotation")

type Options struct {
	Host string

	RestConfig *rest.Config

	Transport http.RoundTripper

	QPS float32

	Burst int
}

func (o Options) tuned(cfg *rest.Config) *rest.Config {
	return clientlimit.Apply(cfg, o.QPS, o.Burst)
}

type Store struct {
	cfg *rest.Config

	http *http.Client

	codecs runtime.NegotiatedSerializer

	mu sync.Mutex

	clients map[string]rest.Interface
}

func New(opts Options) (*Store, error) {
	if opts.Host == "" {
		return nil, errors.New("kcpstore: Host is required")
	}
	cfg := opts.tuned(&rest.Config{})
	if opts.RestConfig != nil {
		cfg = opts.tuned(opts.RestConfig)
	}
	cfg.Host = ref.BaseHost(opts.Host)
	cfg.ContentType = "application/json"
	cfg.AcceptContentTypes = "application/json"
	if opts.Transport != nil {
		cfg.Transport = opts.Transport
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("kcpstore: %w", err)
	}
	return &Store{
		cfg:     cfg,
		http:    httpClient,
		codecs:  serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion(),
		clients: map[string]rest.Interface{},
	}, nil
}

func (s *Store) Config() *rest.Config {
	return s.cfg
}

func (s *Store) ClientFor(logicalCluster string, gv schema.GroupVersion) (rest.Interface, error) {
	key := logicalCluster + "|" + gv.String()
	s.mu.Lock()
	if client, ok := s.clients[key]; ok {
		s.mu.Unlock()
		return client, nil
	}
	s.mu.Unlock()

	cfg := rest.CopyConfig(s.cfg)
	cfg.GroupVersion = &gv
	apiPath := ref.APIPathPrefix + logicalCluster + "/apis"
	if gv.Group == "" {
		apiPath = ref.APIPathPrefix + logicalCluster + "/api"
	}
	cfg.APIPath = apiPath
	cfg.NegotiatedSerializer = s.codecs
	client, err := rest.RESTClientForConfigAndClient(cfg, s.http)
	if err != nil {
		return nil, fmt.Errorf("kcpstore: %s: %w", logicalCluster, err)
	}
	s.mu.Lock()
	s.clients[key] = client
	s.mu.Unlock()
	return client, nil
}

type Resource[T any] struct {
	store *Store

	gvr schema.GroupVersionResource

	clusterScoped bool
}

type ResourceOptions struct {
	ClusterScoped bool
}

func Of[T any](s *Store, gvr schema.GroupVersionResource) *Resource[T] {
	return OfWithOptions[T](s, gvr, ResourceOptions{})
}

func OfClusterScoped[T any](s *Store, gvr schema.GroupVersionResource) *Resource[T] {
	return OfWithOptions[T](s, gvr, ResourceOptions{ClusterScoped: true})
}

func OfWithOptions[T any](s *Store, gvr schema.GroupVersionResource, opts ResourceOptions) *Resource[T] {
	return &Resource[T]{store: s, gvr: gvr, clusterScoped: opts.ClusterScoped}
}

func (r *Resource[T]) ClusterScoped() *Resource[T] {
	if r == nil {
		return nil
	}
	out := *r
	out.clusterScoped = true
	return &out
}

func (r *Resource[T]) withNamespace(req *rest.Request, namespace string) *rest.Request {
	if r.clusterScoped {
		return req
	}
	return req.Namespace(namespace)
}

func (r *Resource[T]) client(logicalCluster string) (rest.Interface, error) {
	return r.store.ClientFor(logicalCluster, r.gvr.GroupVersion())
}

func (r *Resource[T]) GetRaw(ctx context.Context, target ref.Ref) ([]byte, error) {
	c, err := r.client(target.LogicalCluster)
	if err != nil {
		return nil, err
	}
	raw, err := r.withNamespace(c.Get(), target.Namespace).Resource(r.gvr.Resource).Name(target.Name).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("kcpstore: read %s %s in %s: %w", r.gvr.Resource, target.Name, target.LogicalCluster, err)
	}
	return raw, nil
}

func (r *Resource[T]) Get(ctx context.Context, target ref.Ref) (*T, error) {
	raw, err := r.GetRaw(ctx, target)
	if err != nil {
		return nil, err
	}
	return decode[T](raw)
}

func (r *Resource[T]) List(ctx context.Context, logicalCluster string) ([]T, error) {
	c, err := r.client(logicalCluster)
	if err != nil {
		return nil, err
	}
	raw, err := c.Get().Resource(r.gvr.Resource).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("kcpstore: list %s in %s: %w", r.gvr.Resource, logicalCluster, err)
	}
	return decodeList[T](raw)
}

func (r *Resource[T]) ListIn(ctx context.Context, logicalCluster, namespace string) ([]T, error) {
	c, err := r.client(logicalCluster)
	if err != nil {
		return nil, err
	}
	raw, err := c.Get().Namespace(namespace).Resource(r.gvr.Resource).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("kcpstore: list %s in %s/%s: %w", r.gvr.Resource, logicalCluster, namespace, err)
	}
	return decodeList[T](raw)
}

func (r *Resource[T]) Create(ctx context.Context, logicalCluster string, obj *T) error {
	c, err := r.client(logicalCluster)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("kcpstore: encode %s: %w", r.gvr.Resource, err)
	}
	namespace := ""
	if !r.clusterScoped {
		namespace, err = namespaceOf(raw)
		if err != nil {
			return err
		}
	}
	response, err := r.withNamespace(c.Post(), namespace).Resource(r.gvr.Resource).Body(raw).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("kcpstore: create %s %s in %s: %w%s", r.gvr.Resource, NameOf(raw), logicalCluster, err, ResponseDetail(response))
	}
	return nil
}

func ResponseDetail(response []byte) string {
	if len(response) == 0 {
		return ""
	}
	return ": " + strings.TrimSpace(string(response))
}

func (r *Resource[T]) Delete(ctx context.Context, target ref.Ref) error {
	c, err := r.client(target.LogicalCluster)
	if err != nil {
		return err
	}
	err = r.withNamespace(c.Delete(), target.Namespace).Resource(r.gvr.Resource).Name(target.Name).
		Body([]byte(`{"propagationPolicy":"Background"}`)).Do(ctx).Error()
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("kcpstore: delete %s %s in %s: %w", r.gvr.Resource, target.Name, target.LogicalCluster, err)
	}
	return nil
}

func (r *Resource[T]) ApplyStatus(ctx context.Context, target ref.Ref, body []byte, fieldManager string) error {
	c, err := r.client(target.LogicalCluster)
	if err != nil {
		return err
	}
	response, err := r.withNamespace(c.Patch(types.ApplyPatchType), target.Namespace).SubResource("status").Resource(r.gvr.Resource).
		Name(target.Name).Param("fieldManager", fieldManager).Param("force", "true").Body(body).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("kcpstore: apply %s status for %s in %s: %w%s", r.gvr.Resource, target.Name, target.LogicalCluster, err, ResponseDetail(response))
	}
	return nil
}

func (r *Resource[T]) Apply(ctx context.Context, target ref.Ref, body []byte, fieldManager string) error {
	c, err := r.client(target.LogicalCluster)
	if err != nil {
		return err
	}
	response, err := r.withNamespace(c.Patch(types.ApplyPatchType), target.Namespace).Resource(r.gvr.Resource).
		Name(target.Name).Param("fieldManager", fieldManager).Param("force", "true").Body(body).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("kcpstore: apply %s %s in %s: %w%s", r.gvr.Resource, target.Name, target.LogicalCluster, err, ResponseDetail(response))
	}
	return nil
}

func decode[T any](raw []byte) (*T, error) {
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("kcpstore: decode object: %w", err)
	}
	return &out, nil
}

func decodeList[T any](raw []byte) ([]T, error) {
	var list struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("kcpstore: decode list: %w", err)
	}
	return list.Items, nil
}

func namespaceOf(raw []byte) (string, error) {
	var obj struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("kcpstore: encode does not round trip: %w", err)
	}
	return obj.Metadata.Namespace, nil
}

func NameOf(raw []byte) string {
	var obj struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.Metadata.Name
}

func (s *Store) ClusterPath(ctx context.Context, logicalCluster string) (string, error) {
	c, err := s.ClientFor(logicalCluster, schema.GroupVersion{Group: "core.kcp.io", Version: "v1alpha1"})
	if err != nil {
		return "", err
	}
	raw, err := c.Get().Resource("logicalclusters").Name("cluster").Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("kcpstore: read logical cluster %s: %w", logicalCluster, err)
	}
	var obj struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("kcpstore: parse logical cluster %s: %w", logicalCluster, err)
	}
	path := obj.Metadata.Annotations[kcp.PathAnnotation]
	if path == "" {
		return "", fmt.Errorf("%w: %s", ErrNoPath, logicalCluster)
	}
	return path, nil
}

func (s *Store) MintServiceAccountToken(ctx context.Context, logicalCluster, namespace, name string, ttl time.Duration) (string, error) {
	namespace = cmp.Or(namespace, "default")
	c, err := s.ClientFor(logicalCluster, schema.GroupVersion{Group: "", Version: "v1"})
	if err != nil {
		return "", err
	}
	seconds := int64(ttl / time.Second)
	body, err := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenRequest",
		"spec": map[string]any{
			"audiences":         []string{},
			"expirationSeconds": seconds,
		},
	})
	if err != nil {
		return "", fmt.Errorf("kcpstore: encode the token request: %w", err)
	}
	raw, err := c.Post().Namespace(namespace).Resource("serviceaccounts").Name(name).
		SubResource("token").Body(body).Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("kcpstore: mint token for %s/%s in %s: %w", namespace, name, logicalCluster, err)
	}
	var out struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("kcpstore: parse the token request response: %w", err)
	}
	if out.Status.Token == "" {
		return "", fmt.Errorf("kcpstore: the TokenRequest for %s/%s in %s returned no token", namespace, name, logicalCluster)
	}
	return out.Status.Token, nil
}
