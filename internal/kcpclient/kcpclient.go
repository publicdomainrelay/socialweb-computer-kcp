package kcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	kcpv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

var (
	_ gateway.Store   = (*Client)(nil)
	_ revocation.Deny = (*Client)(nil)
	_ gateway.Revoker = (*Client)(nil)
)

var (
	socialwebResource = v1alpha1.GroupVersion
	tenancyResource   = kcpv1alpha1.SchemeGroupVersion
)

type GrantInput struct {
	Caller gateway.Caller

	Cluster string

	Server string

	TTL time.Duration

	NoExpiry bool
}

type KubeconfigGranter interface {
	Grant(ctx context.Context, in GrantInput) (string, error)
}

type TenantConfig func(ctx context.Context, did, cluster string) (*rest.Config, error)

type Options struct {
	Host string

	PublicHost string

	Kubeconfigs KubeconfigGranter

	Tenant TenantConfig

	NodeClaimAgent *ServiceAccountRef

	HostProcessWorkspace string

	NodeClaimQuota int64

	RestConfig *rest.Config

	Transport http.RoundTripper

	RecordWorkspaceOwner bool
}

type Client struct {
	cfg         *rest.Config
	http        *http.Client
	codecs      runtime.NegotiatedSerializer
	host        string
	publicHost  string
	kubeconfigs KubeconfigGranter
	tenant      TenantConfig
	agent       *agent

	hostProcessWorkspace string

	nodeClaimQuota int64

	certMu      sync.Mutex
	certWatches map[string]*certWatch

	recordOwner bool

	ownerMu sync.Mutex
	owner   *authenticationv1.UserInfo
}

var ErrNoKubeconfigGranter = errors.New(
	"kcpclient: no kubeconfig granter: getKubeconfig has no issuer for the client certificate it" +
		" hands back, so it hands out nothing")

const ShardAdminContext = "system:admin"

func ConfigFromContext(kubeconfigPath, contextName string) (*rest.Config, error) {
	if kubeconfigPath == "" || contextName == "" {
		return nil, errors.New(
			"kcpclient: ConfigFromContext needs both a kubeconfig path and a context name: the " +
				"identity a write is made as comes from exactly those two, and a missing one of " +
				"them would be filled in by whatever the file's current-context happens to be")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kcpclient: the context %q of %s: %w", contextName, kubeconfigPath, err)
	}
	return cfg, nil
}

func New(opts Options) (*Client, error) {
	if opts.Host == "" {
		return nil, errors.New("kcpclient: Host is required")
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
		return nil, fmt.Errorf("kcpclient: %w", err)
	}
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	codecs := serializer.NewCodecFactory(scheme).WithoutConversion()
	c := &Client{
		cfg: cfg, http: httpClient, codecs: codecs, host: cfg.Host,
		publicHost:  strings.TrimSuffix(opts.PublicHost, "/"),
		kubeconfigs: opts.Kubeconfigs,
		tenant:      opts.Tenant,

		hostProcessWorkspace: opts.HostProcessWorkspace,
		nodeClaimQuota:       opts.NodeClaimQuota,
		recordOwner:          opts.RecordWorkspaceOwner,
	}
	if c.hostProcessWorkspace == "" {
		c.hostProcessWorkspace = RootWorkspace
	}
	if opts.NodeClaimAgent != nil {
		c.agent = &agent{ref: opts.NodeClaimAgent}
	}
	return c, nil
}

func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("kcpclient: %w", err)
	}
	if err := kcpv1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("kcpclient: %w", err)
	}
	metav1.AddToGroupVersion(s, schema.GroupVersion{Version: "v1"})
	return s, nil
}

func (c *Client) resource(logicalCluster string, gv schema.GroupVersion) (rest.Interface, error) {
	return c.bind(logicalCluster, gv, "/apis")
}

func (c *Client) coreResource(logicalCluster string) (rest.Interface, error) {
	return c.bind(logicalCluster, corev1.SchemeGroupVersion, "/api")
}

var ErrNoTenantConfig = errors.New(
	"kcpclient: no tenant credential source: a tenant apiserver is reached directly at its own " +
		"front and never through the kcp mount, so without one no tenant call can be made")

func (c *Client) tenantCore(ctx context.Context, did, cluster string) (rest.Interface, error) {
	if c.tenant == nil {
		return nil, ErrNoTenantConfig
	}
	cfg, err := c.tenant(ctx, did, cluster)
	if err != nil {
		return nil, err
	}
	cfg = rest.CopyConfig(cfg)
	cfg.GroupVersion = &corev1.SchemeGroupVersion
	cfg.APIPath = "/api"
	cfg.ContentType = "application/json"
	cfg.AcceptContentTypes = "application/json"
	cfg.NegotiatedSerializer = c.codecs
	return rest.RESTClientFor(cfg)
}

func (c *Client) bind(logicalCluster string, gv schema.GroupVersion, endpoint string) (rest.Interface, error) {
	cfg := rest.CopyConfig(c.cfg)
	cfg.GroupVersion = &gv
	cfg.APIPath = "/clusters/" + logicalCluster + endpoint
	cfg.NegotiatedSerializer = c.codecs
	r, err := rest.RESTClientForConfigAndClient(cfg, c.http)
	if err != nil {
		return nil, fmt.Errorf("kcpclient: %s: %w", logicalCluster, err)
	}
	return r, nil
}

var (
	workspaceReadyTimeout = 5 * time.Minute
	workspaceReadyPoll    = 250 * time.Millisecond

	workspaceNudgeAfter = 5 * time.Second
	workspaceNudgeEvery = 10 * time.Second
)

const workspaceNudgeAnnotation = "socialweb.computer/initializer-nudge"

var errNoWorkspaceType = errors.New(
	"kcpclient: the workspace names no type, so no WorkspaceType exists to nudge; its initializer cannot be re-run")

func (c *Client) homePath(caller gateway.Caller, name string) (string, error) {
	if caller == "" {
		return "", errors.New("kcpclient: no caller: the identity comes from the verified token and never from a request field")
	}
	if name == "" {
		return "", errors.New("kcpclient: no cluster name")
	}
	return HomePath(string(caller), name), nil
}

func (c *Client) Ensure(ctx context.Context, caller gateway.Caller, name string) error {
	permit, err := revocation.Admit(ctx, c, string(caller))
	if err != nil {
		return err
	}
	return c.ensure(ctx, permit, name)
}

func (c *Client) ensure(ctx context.Context, permit revocation.Permit, name string) error {
	if name == "" {
		return errors.New("kcpclient: no cluster name")
	}
	did := permit.Admitted()
	home := HomePath(did, name)

	if err := c.ensureWorkspace(ctx, RootWorkspace, AccountHash(did), kcpv1alpha1.WorkspaceSpec{
		Type: &kcpv1alpha1.WorkspaceTypeReference{Name: AccountWorkspaceType, Path: RootWorkspace},
	}); err != nil {
		return fmt.Errorf("kcpclient: account workspace %s: %w", AccountHash(did), err)
	}
	if err := c.createGrant(ctx, AccountPath(did), IdentityRecordName(did), did); err != nil {
		return fmt.Errorf("kcpclient: account identity record %s: %w", IdentityRecordName(did), err)
	}
	if err := c.ensureWorkspace(ctx, AccountPath(did), name, kcpv1alpha1.WorkspaceSpec{
		Type: &kcpv1alpha1.WorkspaceTypeReference{Name: HomeWorkspaceType, Path: RootWorkspace},
	}); err != nil {
		return fmt.Errorf("kcpclient: home workspace %s: %w", home, err)
	}
	created, err := c.createCluster(ctx, home, name, did)
	if err != nil {
		return fmt.Errorf("kcpclient: cluster %s: %w", home, err)
	}
	if !created {
		if cl, err := c.readCluster(ctx, home, name); err == nil && cl.DeletionTimestamp != nil {
			return fmt.Errorf("kcpclient: cluster %s is being deleted; ask again once it is gone", home)
		}
	}
	if err := c.ensureNodeClaimQuota(ctx, home); err != nil {
		return err
	}
	return c.grantAgentHome(ctx, home)
}

const (
	DefaultNodeClaimQuota = 64
	NodeClaimQuotaName    = "socialweb-bounded"
	nodeClaimQuotaKey     = "count/nodeclaims.socialweb.computer"
)

func (c *Client) ensureNodeClaimQuota(ctx context.Context, home string) error {
	limit := c.nodeClaimQuota
	if limit < 0 {
		return nil
	}
	if limit == 0 {
		limit = DefaultNodeClaimQuota
	}
	r, err := c.coreResource(home)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ResourceQuota",
		"metadata": map[string]any{"name": NodeClaimQuotaName, "namespace": "default"},
		"spec":     map[string]any{"hard": map[string]string{nodeClaimQuotaKey: fmt.Sprint(limit)}}})
	if err != nil {
		return err
	}
	err = r.Post().Namespace("default").Resource("resourcequotas").Body(body).Do(ctx).Error()
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kcpclient: the NodeClaim quota of %s: %w", home, err)
	}
	return nil
}

var ErrNoMountURL = errors.New(
	"kcpclient: the mount child addresses the cluster's apiserver and carries the URL in " +
		"spec.URL, which kcp copies out of the mounted object's status ONCE. A child written " +
		"without a URL keeps an empty one for ever, and the mount then serves kcp itself: an " +
		"admin token is accepted, the placeholder Node the provider wrote is listed, and every " +
		"object written to the tenant apiserver is invisible")

type MountChild struct{ c *Client }

func NewMountChild(opts Options) (*MountChild, error) {
	c, err := New(opts)
	if err != nil {
		return nil, err
	}
	return &MountChild{c: c}, nil
}

func (m *MountChild) EnsureMountChild(ctx context.Context, home, name, url string) error {
	if home == "" || name == "" {
		return errors.New("kcpclient: EnsureMountChild needs the cluster's home workspace and its name")
	}
	if url == "" {
		return ErrNoMountURL
	}
	child, err := m.c.readWorkspace(ctx, home, ChildLabel)
	switch {
	case apierrors.IsNotFound(err):
		return m.c.createWorkspace(ctx, home, ChildLabel, mountChildSpec(name, url))
	case err != nil:
		return err
	case child.Spec.URL == "":
		return m.c.patchWorkspaceURL(ctx, home, ChildLabel, url)
	}
	return nil
}

func mountChildSpec(name, url string) kcpv1alpha1.WorkspaceSpec {
	return kcpv1alpha1.WorkspaceSpec{
		URL: url,
		Mount: &kcpv1alpha1.Mount{Reference: kcpv1alpha1.ObjectReference{
			APIVersion: socialwebResource.String(),
			Kind:       "Cluster",
			Name:       name,
		}},
	}
}

func (c *Client) patchWorkspaceURL(ctx context.Context, parent, name, url string) error {
	r, err := c.resource(parent, tenancyResource)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"URL": url}})
	if err != nil {
		return fmt.Errorf("kcpclient: workspace %s: marshal: %w", name, err)
	}
	if err := r.Patch(types.MergePatchType).Resource("workspaces").Name(name).
		Body(body).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: patch workspace %s spec.URL: %w", name, err)
	}
	return nil
}

func (c *Client) ensureWorkspace(ctx context.Context, parent, name string, spec kcpv1alpha1.WorkspaceSpec) error {
	if err := c.createWorkspace(ctx, parent, name, spec); err != nil {
		return err
	}
	return c.waitForWorkspaceReady(ctx, parent, name, spec)
}

func (c *Client) workspaceOwner(ctx context.Context) (string, error) {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	if c.owner == nil {
		r, err := c.resource(RootWorkspace, authenticationv1.SchemeGroupVersion)
		if err != nil {
			return "", err
		}
		review := []byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`)
		raw, err := r.Post().Resource("selfsubjectreviews").Body(review).Do(ctx).Raw()
		if err != nil {
			return "", fmt.Errorf("kcpclient: the identity the workspace owner is recorded as: %w. "+
				"kcp records a workspace's creator from the request user, the initializing workspace "+
				"virtual workspace impersonates exactly that user to bind the workspace type's "+
				"defaultAPIBindings, and a workspace recorded without one never leaves Initializing", err)
		}
		var got authenticationv1.SelfSubjectReview
		if err := json.Unmarshal(raw, &got); err != nil {
			return "", fmt.Errorf("kcpclient: the self subject review of the workspace owner: %w", err)
		}
		c.owner = &got.Status.UserInfo
	}
	raw, err := json.Marshal(c.owner)
	if err != nil {
		return "", fmt.Errorf("kcpclient: the workspace owner annotation: %w", err)
	}
	return string(raw), nil
}

func (c *Client) waitForWorkspaceReady(ctx context.Context, parent, name string, spec kcpv1alpha1.WorkspaceSpec) error {
	path := parent + ":" + name
	deadline := time.Now().Add(workspaceReadyTimeout)
	last := "never read"
	lastNudge := time.Time{}
	started := time.Now()
	for {
		ws, err := c.readWorkspace(ctx, parent, name)
		switch {
		case err == nil && ws.DeletionTimestamp != nil:
			return fmt.Errorf("kcpclient: workspace %s is being deleted; ask again once it is gone", path)
		case err == nil && ws.Status.Phase == corev1alpha1.LogicalClusterPhaseReady:
			return nil
		case err == nil:
			last = "phase " + string(ws.Status.Phase)
			if ws.Status.Phase == corev1alpha1.LogicalClusterPhaseInitializing &&
				hasInitializer(ws.Status.Initializers, kcpv1alpha1.WorkspaceAPIBindingsInitializer) {
				last = fmt.Sprintf("phase %s with initializers %v", ws.Status.Phase, ws.Status.Initializers)
				if time.Since(started) >= workspaceNudgeAfter && time.Since(lastNudge) >= workspaceNudgeEvery {
					if err := c.nudgeWorkspaceType(ctx, spec.Type); err != nil {
						slog.Warn("kcpclient: the workspace stayed Initializing and its kcp APIBinding initializer could not be re-run",
							"workspace", path, "initializers", ws.Status.Initializers, "err", err.Error())
					} else {
						lastNudge = time.Now()
						slog.Warn("kcpclient: the workspace stayed Initializing with the kcp APIBinding initializer; re-ran it by updating the WorkspaceType so the apibinder queues every initializing workspace again",
							"workspace", path, "type", workspaceTypeName(spec.Type), "initializers", ws.Status.Initializers)
					}
				}
			}
		default:
			last = err.Error()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("kcpclient: workspace %s did not become %s within %s "+
				"(last read: %s). Nothing may be written into a workspace kcp cannot address, "+
				"and the create would be refused with `workspaces.tenancy.kcp.io %q not found`",
				path, corev1alpha1.LogicalClusterPhaseReady, workspaceReadyTimeout, last, path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(workspaceReadyPoll):
		}
	}
}

func hasInitializer(initializers []corev1alpha1.LogicalClusterInitializer, want corev1alpha1.LogicalClusterInitializer) bool {
	return slices.Contains(initializers, want)
}

func workspaceTypeName(ref *kcpv1alpha1.WorkspaceTypeReference) string {
	if ref == nil {
		return ""
	}
	return ref.Path + ":" + string(ref.Name)
}

func (c *Client) nudgeWorkspaceType(ctx context.Context, ref *kcpv1alpha1.WorkspaceTypeReference) error {
	if ref == nil || ref.Name == "" {
		return errNoWorkspaceType
	}
	path := ref.Path
	if path == "" {
		path = RootWorkspace
	}
	r, err := c.resource(path, tenancyResource)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		workspaceNudgeAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
	}}})
	if err != nil {
		return fmt.Errorf("kcpclient: nudge workspace type %s: marshal: %w", workspaceTypeName(ref), err)
	}
	if err := r.Patch(types.MergePatchType).Resource("workspacetypes").Name(string(ref.Name)).
		Body(body).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: nudge workspace type %s: %w", workspaceTypeName(ref), err)
	}
	return nil
}

func (c *Client) createGrant(ctx context.Context, logicalCluster, name, did string) error {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return err
	}
	rec := v1alpha1.AtprotoIdentity{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "AtprotoIdentity",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AtprotoIdentitySpec{
			DID:     did,
			Scope:   v1alpha1.IdentityScopeAccount,
			Revoked: false,
		},
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("kcpclient: grant: marshal: %w", err)
	}
	err = r.Post().Resource("atprotoidentities").Body(body).Do(ctx).Error()
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *Client) createWorkspace(ctx context.Context, parent, name string, spec kcpv1alpha1.WorkspaceSpec) error {
	r, err := c.resource(parent, tenancyResource)
	if err != nil {
		return err
	}
	annotations := map[string]string{}
	if c.recordOwner {
		owner, err := c.workspaceOwner(ctx)
		if err != nil {
			return err
		}
		annotations[kcpv1alpha1.ExperimentalWorkspaceOwnerAnnotationKey] = owner
	}
	obj := &kcpv1alpha1.Workspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: tenancyResource.String(), Kind: "Workspace"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec:       spec,
	}
	var out kcpv1alpha1.Workspace
	err = r.Post().Resource("workspaces").Body(obj).Do(ctx).Into(&out)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *Client) createCluster(ctx context.Context, home, name, did string) (bool, error) {
	r, err := c.resource(home, socialwebResource)
	if err != nil {
		return false, err
	}
	obj := &v1alpha1.Cluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: socialwebResource.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{v1alpha1.FinalizerCluster}},
		Spec:       v1alpha1.ClusterSpec{Account: did, Running: false},
	}
	var out v1alpha1.Cluster
	err = r.Post().Resource("clusters").Body(obj).Do(ctx).Into(&out)
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) Get(ctx context.Context, caller gateway.Caller, name string) (gateway.ClusterView, bool, error) {
	home, err := c.homePath(caller, name)
	if err != nil {
		return gateway.ClusterView{}, false, err
	}
	cl, err := c.readCluster(ctx, home, name)
	if apierrors.IsNotFound(err) {
		return gateway.ClusterView{}, false, nil
	}
	if err != nil {
		return gateway.ClusterView{}, false, err
	}
	n, err := c.liveNodes(ctx, home, name)
	if err != nil {
		return gateway.ClusterView{}, false, err
	}
	return viewOf(cl, n), true, nil
}

func (c *Client) List(ctx context.Context, caller gateway.Caller) ([]gateway.ClusterView, error) {
	if caller == "" {
		return nil, errors.New("kcpclient: no caller: the identity comes from the verified token and never from a request field")
	}
	names, err := c.clusterLabels(ctx, string(caller))
	if err != nil {
		return nil, err
	}
	views := make([]gateway.ClusterView, 0, len(names))
	for _, name := range names {
		view, ok, err := c.Get(ctx, caller, name)
		if err != nil {
			return nil, err
		}
		if ok {
			views = append(views, view)
		}
	}
	return views, nil
}

func (c *Client) clusterLabels(ctx context.Context, did string) ([]string, error) {
	r, err := c.resource(AccountPath(did), tenancyResource)
	if err != nil {
		return nil, err
	}
	var list kcpv1alpha1.WorkspaceList
	if err := r.Get().Resource("workspaces").Do(ctx).Into(&list); err != nil {
		if _, rerr := c.readWorkspace(ctx, RootWorkspace, AccountHash(did)); apierrors.IsNotFound(rerr) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("kcpclient: list clusters for %s: %w", AccountPath(did), err)
	}
	names := make([]string, 0, len(list.Items))
	for _, ws := range list.Items {
		if ws.Spec.Mount == nil {
			names = append(names, ws.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (c *Client) SetRunning(ctx context.Context, caller gateway.Caller, name string, running bool) (gateway.ClusterView, error) {
	home, err := c.homePath(caller, name)
	if err != nil {
		return gateway.ClusterView{}, err
	}
	r, err := c.resource(home, socialwebResource)
	if err != nil {
		return gateway.ClusterView{}, err
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"running": running}})
	if err != nil {
		return gateway.ClusterView{}, fmt.Errorf("kcpclient: set running: %w", err)
	}
	var out v1alpha1.Cluster
	if err := r.Patch(types.MergePatchType).Resource("clusters").Name(name).Body(body).Do(ctx).Into(&out); err != nil {
		return gateway.ClusterView{}, fmt.Errorf("kcpclient: set running %s: %w", name, err)
	}
	n, err := c.liveNodes(ctx, home, name)
	if err != nil {
		return gateway.ClusterView{}, err
	}
	return viewOf(&out, n), nil
}

func (c *Client) Delete(ctx context.Context, caller gateway.Caller, name string) (gateway.ClusterView, error) {
	home, err := c.homePath(caller, name)
	if err != nil {
		return gateway.ClusterView{}, err
	}
	cl, err := c.readCluster(ctx, home, name)
	if err != nil {
		return gateway.ClusterView{}, err
	}
	n, err := c.liveNodes(ctx, home, name)
	if err != nil {
		return gateway.ClusterView{}, err
	}
	r, err := c.resource(home, socialwebResource)
	if err != nil {
		return gateway.ClusterView{}, err
	}
	if err := r.Delete().Resource("clusters").Name(name).Do(ctx).Error(); err != nil && !apierrors.IsNotFound(err) {
		return gateway.ClusterView{}, fmt.Errorf("kcpclient: delete %s: %w", name, err)
	}
	return viewOf(cl, n), nil
}

func (c *Client) Kubeconfig(ctx context.Context, caller gateway.Caller, name string, ttl time.Duration, noExpiry bool) (string, error) {
	if _, err := c.homePath(caller, name); err != nil {
		return "", err
	}
	if c.kubeconfigs == nil {
		return "", ErrNoKubeconfigGranter
	}
	host := c.host
	if c.publicHost != "" {
		host = c.publicHost
	}
	return c.kubeconfigs.Grant(ctx, GrantInput{
		Caller:   caller,
		Cluster:  name,
		Server:   host + "/clusters/" + MountPath(string(caller), name),
		TTL:      ttl,
		NoExpiry: noExpiry,
	})
}

func (c *Client) Lookup(ctx context.Context, did string) (revocation.DenyState, error) {
	if did == "" {
		return "", revocation.ErrNoDID
	}
	ws, err := c.readWorkspace(ctx, RootWorkspace, AccountHash(did))
	switch {
	case apierrors.IsNotFound(err):
		return revocation.DenyFirstLogin, nil
	case err != nil:
		return "", fmt.Errorf("kcpclient: deny: account workspace %s: %w", AccountPath(did), err)
	case ws.DeletionTimestamp != nil:
		return "", fmt.Errorf("kcpclient: deny: account workspace %s is terminating", AccountPath(did))
	}
	rec, err := c.readIdentity(ctx, AccountPath(did), IdentityRecordName(did))
	switch {
	case apierrors.IsNotFound(err):
		return revocation.DenyRecordMissing, nil
	case err != nil:
		return "", fmt.Errorf("kcpclient: deny: %s: %w", IdentityRecordName(did), err)
	}
	if rec.Spec.DID != did || rec.Spec.Scope != v1alpha1.IdentityScopeAccount {
		return revocation.DenyRecordMissing, nil
	}
	if rec.Spec.Revoked {
		return revocation.DenyRevoked, nil
	}
	return revocation.DenyGranted, nil
}

func (c *Client) WriteDeny(ctx context.Context, did string) error {
	if did == "" {
		return revocation.ErrNoDID
	}
	if _, err := c.readWorkspace(ctx, RootWorkspace, AccountHash(did)); err != nil {
		return fmt.Errorf("kcpclient: deny: account workspace %s: %w", AccountPath(did), err)
	}
	r, err := c.resource(AccountPath(did), socialwebResource)
	if err != nil {
		return err
	}

	name := IdentityRecordName(did)
	rec, err := c.readIdentity(ctx, AccountPath(did), name)
	switch {
	case apierrors.IsNotFound(err):
		return c.createDeny(ctx, r, name, did)
	case err != nil:
		return fmt.Errorf("kcpclient: deny: read %s: %w", name, err)
	}
	if rec.Spec.Revoked {
		return nil
	}

	body, err := json.Marshal(map[string]any{"spec": map[string]any{"revoked": true}})
	if err != nil {
		return fmt.Errorf("kcpclient: deny: marshal: %w", err)
	}
	if err := r.Patch(types.MergePatchType).Resource("atprotoidentities").Name(name).
		Body(body).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: deny: patch %s: %w", name, err)
	}
	return nil
}

func (c *Client) createDeny(ctx context.Context, r rest.Interface, name, did string) error {
	rec := v1alpha1.AtprotoIdentity{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "AtprotoIdentity",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AtprotoIdentitySpec{
			DID:     did,
			Scope:   v1alpha1.IdentityScopeAccount,
			Revoked: true,
		},
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("kcpclient: deny: marshal: %w", err)
	}
	if err := r.Post().Resource("atprotoidentities").Body(body).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: deny: create %s: %w", name, err)
	}
	return nil
}

func (c *Client) FanOut(ctx context.Context, did string) ([]revocation.ClusterView, error) {
	if did == "" {
		return nil, revocation.ErrNoDID
	}
	names, err := c.clusterLabels(ctx, did)
	if err != nil {
		return nil, err
	}
	views := make([]revocation.ClusterView, 0, len(names))
	for _, name := range names {
		view, err := c.observeServiceAccount(ctx, did, name)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (c *Client) observeServiceAccount(ctx context.Context, did, cluster string) (revocation.ClusterView, error) {
	view := revocation.ClusterView{Name: cluster}
	r, err := c.tenantCore(ctx, did, cluster)
	if err != nil {
		if errors.Is(err, ErrNoTenantConfig) {
			return view, err
		}
		view.Cold = true
		return view, nil
	}
	err = r.Get().Namespace(TenantNamespace).Resource("serviceaccounts").
		Name(ServiceAccountName(did)).Do(ctx).Error()
	switch {
	case err == nil:
		view.ServiceAccountPresent = true
	case apierrors.IsNotFound(err):
	default:
		view.Cold = true
	}
	return view, nil
}

func (c *Client) DeleteServiceAccount(ctx context.Context, did, cluster string) error {
	if did == "" {
		return revocation.ErrNoDID
	}
	r, err := c.tenantCore(ctx, did, cluster)
	if err != nil {
		return fmt.Errorf("kcpclient: the tenant apiserver of %s: %w", cluster, err)
	}
	name := ServiceAccountName(did)
	err = r.Delete().Namespace(TenantNamespace).Resource("serviceaccounts").
		Name(name).Do(ctx).Error()
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		if found, ok := err.(apierrors.APIStatus); ok {
			d := found.Status().Details
			if d != nil && d.Kind == "serviceaccounts" && d.Name == name {
				return nil
			}
		}
		return fmt.Errorf("kcpclient: %s in %s answered NotFound without naming the "+
			"ServiceAccount, so the tenant apiserver did NOT answer for it and this is not a "+
			"completed delete: %w", name, cluster, err)
	default:
		return fmt.Errorf("kcpclient: delete the ServiceAccount for %s in %s: %w", did, cluster, err)
	}
}

func (c *Client) readWorkspace(ctx context.Context, parent, name string) (*kcpv1alpha1.Workspace, error) {
	r, err := c.resource(parent, tenancyResource)
	if err != nil {
		return nil, err
	}
	var out kcpv1alpha1.Workspace
	if err := r.Get().Resource("workspaces").Name(name).Do(ctx).Into(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) readIdentity(ctx context.Context, logicalCluster, name string) (*v1alpha1.AtprotoIdentity, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return nil, err
	}
	var out v1alpha1.AtprotoIdentity
	if err := r.Get().Resource("atprotoidentities").Name(name).Do(ctx).Into(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

type ClusterGate struct {
	Account string
	Serving bool
}

func (c *Client) ClusterGate(ctx context.Context, home, name string) (ClusterGate, error) {
	cl, err := c.readCluster(ctx, home, name)
	switch {
	case apierrors.IsNotFound(err):
		return ClusterGate{}, nil
	case err != nil:
		return ClusterGate{}, fmt.Errorf("kcpclient: the Cluster %s in %s: %w", name, home, err)
	}
	return ClusterGate{Account: cl.Spec.Account,
		Serving: cl.Status.Phase == v1alpha1.ClusterReady && cl.DeletionTimestamp == nil}, nil
}

func (c *Client) readCluster(ctx context.Context, logicalCluster, name string) (*v1alpha1.Cluster, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return nil, err
	}
	var out v1alpha1.Cluster
	if err := r.Get().Resource("clusters").Name(name).Do(ctx).Into(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) liveNodes(ctx context.Context, logicalCluster, cluster string) (int, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return 0, err
	}
	var list v1alpha1.NodeList
	err = r.Get().Resource("nodes").Do(ctx).Into(&list)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("kcpclient: list nodes in %s: %w", logicalCluster, err)
	}
	n := 0
	for _, node := range list.Items {
		if node.Spec.ClusterRef.Name == cluster && node.Status.Phase != v1alpha1.NodeDraining {
			n++
		}
	}
	return n, nil
}

func viewOf(cl *v1alpha1.Cluster, nodes int) gateway.ClusterView {
	return gateway.ClusterView{
		Name:      cl.Name,
		Phase:     string(cl.Status.Phase),
		URL:       cl.Status.URL,
		Instance:  cl.Status.Instance,
		Running:   cl.Spec.Running,
		NodeCount: nodes,

		TeardownBlocked: conditionTrue(cl.Status.Conditions, v1alpha1.ConditionTeardownBlocked),
	}
}

func conditionTrue(conds []metav1.Condition, t string) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func (c *Client) WriteNode(ctx context.Context, logicalCluster, cluster, nodeName string, status v1alpha1.NodeStatus) error {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return err
	}
	return WriteNodeTo(ctx, r, cluster, nodeName, status)
}

func WriteNodeTo(ctx context.Context, r rest.Interface, cluster, nodeName string, status v1alpha1.NodeStatus) error {
	if nodeName == "" {
		return errors.New("kcpclient: WriteNode needs a node name")
	}
	if cluster == "" {
		return errors.New("kcpclient: WriteNode needs the cluster it belongs to")
	}

	var existing v1alpha1.Node
	err := r.Get().Resource("nodes").Name(nodeName).Do(ctx).Into(&existing)
	switch {
	case apierrors.IsNotFound(err):
		return createNode(ctx, r, cluster, nodeName, status)
	case err != nil:
		return fmt.Errorf("kcpclient: read node %s: %w", nodeName, err)
	}
	if sameNodeStatus(existing.Status, status) {
		return nil
	}
	body, err := nodeStatusPatch(status)
	if err != nil {
		return fmt.Errorf("kcpclient: node %s: marshal: %w", nodeName, err)
	}
	if err := r.Patch(types.MergePatchType).SubResource("status").Resource("nodes").Name(nodeName).
		Body(body).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: patch node %s to phase %q: %w", nodeName, status.Phase, err)
	}
	return nil
}

func sameNodeStatus(have, want v1alpha1.NodeStatus) bool {
	if have.Phase != want.Phase {
		return false
	}
	switch {
	case have.NodeExpiry == nil && want.NodeExpiry == nil:
		return true
	case have.NodeExpiry == nil || want.NodeExpiry == nil:
		return false
	}
	return *have.NodeExpiry == *want.NodeExpiry
}

func nodeStatusPatch(status v1alpha1.NodeStatus) ([]byte, error) {
	fields := map[string]any{}
	if status.Phase != "" {
		fields["phase"] = string(status.Phase)
	}
	if status.NodeExpiry != nil {
		fields["nodeExpiry"] = *status.NodeExpiry
	}
	return json.Marshal(map[string]any{"status": fields})
}

func createNode(ctx context.Context, r rest.Interface, cluster, nodeName string, status v1alpha1.NodeStatus) error {
	node := v1alpha1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec:       v1alpha1.NodeSpec{ClusterRef: commonv1.LocalObjectRef{Name: cluster}},
	}
	body, err := json.Marshal(node)
	if err != nil {
		return fmt.Errorf("kcpclient: node %s: marshal: %w", nodeName, err)
	}
	if err := r.Post().Resource("nodes").Body(body).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: create node %s: %w", nodeName, err)
	}
	if status.Phase == "" && status.NodeExpiry == nil {
		return nil
	}
	written, err := nodeStatusPatch(status)
	if err != nil {
		return fmt.Errorf("kcpclient: node %s: marshal: %w", nodeName, err)
	}
	if err := r.Patch(types.MergePatchType).SubResource("status").Resource("nodes").Name(nodeName).
		Body(written).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: set the status of the node %s it just created: %w -- the "+
			"schema declares a status subresource, so a status posted inline is DISCARDED and the "+
			"node exists with neither a phase nor an expiry at all", nodeName, err)
	}
	return nil
}

func (c *Client) WriteNodeClaim(ctx context.Context, logicalCluster string, claim v1alpha1.NodeClaim) error {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return err
	}
	return WriteNodeClaimTo(ctx, r, claim)
}

func WriteNodeClaimTo(ctx context.Context, r rest.Interface, claim v1alpha1.NodeClaim) error {
	if claim.Name == "" {
		return errors.New("kcpclient: WriteNodeClaim needs a claim name")
	}
	if claim.Spec.ClusterRef.Name == "" {
		return errors.New("kcpclient: WriteNodeClaim needs the cluster the claimed node belongs to")
	}
	if claim.Spec.NodeName == "" {
		return errors.New("kcpclient: WriteNodeClaim needs the node name the claimed node joins as")
	}

	var existing v1alpha1.NodeClaim
	err := r.Get().Resource("nodeclaims").Name(claim.Name).Do(ctx).Into(&existing)
	switch {
	case apierrors.IsNotFound(err):
		return createNodeClaim(ctx, r, claim)
	case err != nil:
		return fmt.Errorf("kcpclient: read nodeclaim %s: %w", claim.Name, err)
	}
	if len(existing.OwnerReferences) == 0 && len(claim.OwnerReferences) > 0 {
		owner, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": string(existing.UID), "ownerReferences": claim.OwnerReferences}})
		if err != nil {
			return fmt.Errorf("kcpclient: nodeclaim %s: marshal: %w", claim.Name, err)
		}
		if err := r.Patch(types.MergePatchType).Resource("nodeclaims").Name(claim.Name).Body(owner).Do(ctx).Error(); err != nil && !apierrors.IsConflict(err) {
			return fmt.Errorf("kcpclient: hang nodeclaim %s from its Cluster: %w", claim.Name, err)
		}
	}
	if sameNodeClaimSpec(existing.Spec, claim.Spec) {
		return nil
	}
	body, err := nodeClaimSpecPatch(claim.Spec)
	if err != nil {
		return fmt.Errorf("kcpclient: nodeclaim %s: marshal: %w", claim.Name, err)
	}
	if err := r.Patch(types.MergePatchType).Resource("nodeclaims").Name(claim.Name).
		Body(body).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: patch nodeclaim %s: %w -- the claim is the only handle on a "+
			"machine the market is being asked to rent, so a spec that did not land is a node "+
			"nobody can account for", claim.Name, err)
	}
	return nil
}

func (c *Client) ReadNodeClaim(ctx context.Context, logicalCluster, name string) (*v1alpha1.NodeClaim, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("kcpclient: ReadNodeClaim needs a claim name")
	}
	var claim v1alpha1.NodeClaim
	err = r.Get().Resource("nodeclaims").Name(name).Do(ctx).Into(&claim)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kcpclient: read nodeclaim %s: %w", name, err)
	}
	return &claim, nil
}

func (c *Client) MarkNodeClaimRunning(ctx context.Context, logicalCluster, name, message string) error {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("kcpclient: MarkNodeClaimRunning needs a claim name")
	}
	var claim v1alpha1.NodeClaim
	if err := r.Get().Resource("nodeclaims").Name(name).Do(ctx).Into(&claim); err != nil {
		return fmt.Errorf("kcpclient: read nodeclaim %s: %w", name, err)
	}
	if claim.Status.Phase != v1alpha1.NodeClaimJoining {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": claim.ResourceVersion},
		"status":   map[string]any{"phase": v1alpha1.NodeClaimRunning, "message": message},
	})
	if err != nil {
		return err
	}
	err = r.Patch(types.MergePatchType).SubResource("status").Resource("nodeclaims").Name(name).Body(body).Do(ctx).Error()
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kcpclient: mark nodeclaim %s Running: %w", name, err)
	}
	return nil
}

func (c *Client) DeleteNodeClaim(ctx context.Context, logicalCluster, name string) error {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("kcpclient: DeleteNodeClaim needs a claim name")
	}
	err = r.Delete().Resource("nodeclaims").Name(name).Do(ctx).Error()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kcpclient: delete nodeclaim %s: %w", name, err)
	}
	return nil
}

func (c *Client) ListNodeClaims(ctx context.Context, logicalCluster string) ([]v1alpha1.NodeClaim, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.NodeClaimList
	if err := r.Get().Resource("nodeclaims").Do(ctx).Into(&list); err != nil {
		return nil, fmt.Errorf("kcpclient: list nodeclaims in %s: %w", logicalCluster, err)
	}
	return list.Items, nil
}

func sameNodeClaimSpec(have, want v1alpha1.NodeClaimSpec) bool {
	if have.ClusterRef.Name != want.ClusterRef.Name || have.NodeName != want.NodeName ||
		have.OwnerDid != want.OwnerDid || have.ApiserverPort != want.ApiserverPort ||
		have.AdvertiseAddress != want.AdvertiseAddress || have.TokenFile != want.TokenFile ||
		have.NodePasswordFile != want.NodePasswordFile || have.ServerCAFile != want.ServerCAFile {
		return false
	}
	if len(have.DenyBidderDids) != len(want.DenyBidderDids) {
		return false
	}
	for i := range have.DenyBidderDids {
		if have.DenyBidderDids[i] != want.DenyBidderDids[i] {
			return false
		}
	}
	return true
}

func nodeClaimSpecPatch(spec v1alpha1.NodeClaimSpec) ([]byte, error) {
	dids := spec.DenyBidderDids
	if dids == nil {
		dids = []string{}
	}
	return json.Marshal(map[string]any{"spec": map[string]any{
		"clusterRef":       map[string]any{"name": spec.ClusterRef.Name},
		"nodeName":         spec.NodeName,
		"ownerDid":         spec.OwnerDid,
		"apiserverPort":    spec.ApiserverPort,
		"advertiseAddress": spec.AdvertiseAddress,
		"tokenFile":        spec.TokenFile,
		"nodePasswordFile": spec.NodePasswordFile,
		"serverCAFile":     spec.ServerCAFile,
		"denyBidderDids":   dids,
	}})
}

func createNodeClaim(ctx context.Context, r rest.Interface, claim v1alpha1.NodeClaim) error {
	created := v1alpha1.NodeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "NodeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Annotations: claim.Annotations, OwnerReferences: claim.OwnerReferences},
		Spec:       claim.Spec,
	}
	body, err := json.Marshal(created)
	if err != nil {
		return fmt.Errorf("kcpclient: nodeclaim %s: marshal: %w", claim.Name, err)
	}
	if err := r.Post().Resource("nodeclaims").Body(body).Do(ctx).Error(); err != nil {
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota") {
			return fmt.Errorf("kcpclient: AtCapacity: the home workspace's NodeClaim quota is used up, so nodeclaim %s is not created and the next pass asks again: %w", claim.Name, err)
		}
		return fmt.Errorf("kcpclient: create nodeclaim %s: %w", claim.Name, err)
	}
	return nil
}
