package kcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
)

const (
	ControllerLeaseNamespace = "kube-system"
	ControllerLeaseName      = "kcp-controllers"
	clusterAnnotation        = "kcp.io/cluster"
	SystemAdminCluster       = "system:admin"
	RootCluster              = "root"
	ProbePoll                = 100 * time.Millisecond
)

type Lease struct {
	Found     bool      `json:"found"`
	Holder    string    `json:"holder"`
	RenewTime time.Time `json:"renewTime,omitzero"`
}

func ReadSharedAdmin(shared string) (Admin, error) {
	return ReadAdmin(filepath.Join(shared, "admin.kubeconfig"))
}

func WriteFrontKubeconfig(path, front string, a Admin) error {
	if a.Token == "" || len(a.CA) == 0 {
		return errors.New("kcpserver: a consumer kubeconfig needs the shard-admin token and the CA")
	}
	c := clientcmdapi.NewConfig()
	c.AuthInfos[ShardAdminUser] = &clientcmdapi.AuthInfo{Token: a.Token}
	for name, path := range map[string]string{RootCluster: "/clusters/root", SystemAdminCluster: "/clusters/system:admin", "base": ""} {
		c.Clusters[name] = &clientcmdapi.Cluster{Server: front + path, CertificateAuthorityData: a.CA, TLSServerName: "127.0.0.1"}
		c.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: ShardAdminUser}
	}
	c.CurrentContext = RootCluster
	return clientcmd.WriteToFile(*c, path)
}

func FrontRestConfig(kubeconfig string) (*rest.Config, error) {
	a, err := ReadAdmin(kubeconfig)
	if err != nil {
		return nil, err
	}
	host, _, _ := strings.Cut(strings.TrimSuffix(a.Server, "/"), "/clusters/")
	return Throughput(&rest.Config{Host: host, BearerToken: a.Token, TLSClientConfig: rest.TLSClientConfig{CAData: a.CA}}), nil
}

const (
	ClientQPS   = 50
	ClientBurst = 100
)

func Throughput(cfg *rest.Config) *rest.Config {
	cfg.QPS, cfg.Burst = ClientQPS, ClientBurst
	return cfg
}

type HTTPAdmin struct {
	Admin Admin
	Front string
	http  *http.Client
}

func NewHTTPAdmin(a Admin, front string) (*HTTPAdmin, error) {
	client, err := loopbackClient(a.CA, 10*time.Second)
	if err != nil {
		return nil, errors.New("kcpserver: the shared CA does not parse")
	}
	return &HTTPAdmin{Admin: a, Front: strings.TrimSuffix(front, "/"), http: client}, nil
}

func loopbackClient(ca []byte, timeout time.Duration) (*http.Client, error) {
	if len(ca) == 0 {
		return nil, httpclient.ErrNoCA
	}
	return httpclient.TLSClient(httpclient.TLSOptions{CAPEM: ca, ServerName: "127.0.0.1", Timeout: timeout})
}

var ErrAwaitTimeout = kube.ErrAwaitTimeout

func (h *HTTPAdmin) clientFor(cluster string) (*kube.Client, error) {
	if len(h.Admin.CA) == 0 {
		return nil, httpclient.ErrNoCA
	}
	return kube.New(Throughput(&rest.Config{Host: h.Front + "/clusters/" + cluster, BearerToken: h.Admin.Token,
		TLSClientConfig: rest.TLSClientConfig{CAData: h.Admin.CA, ServerName: "127.0.0.1"}}))
}

func (h *HTTPAdmin) Readyz(ctx context.Context, port int) (int, error) {
	code, _, err := Readyz(ctx, port, h.Admin)
	return code, err
}

func (h *HTTPAdmin) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.Front+path, r)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Admin.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

const rootShardPath = "/clusters/system:shard/apis/core.kcp.io/v1alpha1/shards/root"

func (h *HTTPAdmin) SyncRootShard(ctx context.Context) (bool, error) {
	code, b, err := h.do(ctx, http.MethodGet, rootShardPath, nil)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("kcpserver: reading the root shard: %d %s", code, b)
	}
	var s struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return false, err
	}
	keys := []string{"baseURL", "externalURL", "virtualWorkspaceURL"}
	same := true
	for _, k := range keys {
		same = same && s.Spec[k] == h.Front
	}
	if same {
		return false, nil
	}
	spec := map[string]string{}
	for _, k := range keys {
		spec[k] = h.Front
	}
	body, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, h.Front+rootShardPath, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Admin.Token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := h.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("kcpserver: pointing the root shard at %s: %d %s", h.Front, resp.StatusCode, out)
	}
	return true, nil
}

const endpointSlicesPath = "/apis/apis.kcp.io/v1alpha1/apiexportendpointslices"

func (h *HTTPAdmin) SyncEndpointSlices(ctx context.Context) (int, error) {
	code, b, err := h.do(ctx, http.MethodGet, "/clusters/*"+endpointSlicesPath, nil)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("kcpserver: listing the APIExportEndpointSlices: %d %s", code, b)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Endpoints []map[string]any `json:"endpoints"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return 0, err
	}
	moved := 0
	for _, it := range list.Items {
		changed := false
		for _, e := range it.Status.Endpoints {
			raw, _ := e["url"].(string)
			u, err := url.Parse(raw)
			if err != nil || raw == "" {
				continue
			}
			if next := h.Front + u.Path; next != raw {
				e["url"], changed = next, true
			}
		}
		cluster := it.Metadata.Annotations[clusterAnnotation]
		if !changed || cluster == "" {
			continue
		}
		path := "/clusters/" + cluster + endpointSlicesPath + "/" + it.Metadata.Name + "/status"
		body := map[string]any{"status": map[string]any{"endpoints": it.Status.Endpoints}}
		if code, out, err := h.patch(ctx, path, body); err != nil || code != http.StatusOK {
			return moved, fmt.Errorf("kcpserver: pointing APIExportEndpointSlice %s|%s at %s: %d %s %v", cluster, it.Metadata.Name, h.Front, code, out, err)
		}
		moved++
	}
	return moved, nil
}

func (h *HTTPAdmin) patch(ctx context.Context, path string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, h.Front+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Admin.Token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := h.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

func leasePath() string {
	return "/clusters/" + SystemAdminCluster + "/apis/coordination.k8s.io/v1/namespaces/" + ControllerLeaseNamespace +
		"/leases/" + ControllerLeaseName
}

func (h *HTTPAdmin) Lease(ctx context.Context) (Lease, error) {
	code, b, err := h.do(ctx, http.MethodGet, leasePath(), nil)
	if err != nil {
		return Lease{}, err
	}
	if code == http.StatusNotFound {
		return Lease{}, nil
	}
	if code != http.StatusOK {
		return Lease{}, fmt.Errorf("kcpserver: reading the controller lease: %d %s", code, b)
	}
	var l struct {
		Spec struct {
			HolderIdentity *string `json:"holderIdentity"`
			RenewTime      *string `json:"renewTime"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return Lease{}, err
	}
	out := Lease{Found: true}
	if l.Spec.HolderIdentity != nil {
		out.Holder = *l.Spec.HolderIdentity
	}
	if l.Spec.RenewTime != nil {
		out.RenewTime, _ = time.Parse(time.RFC3339Nano, *l.Spec.RenewTime)
	}
	return out, nil
}

func leaseOf(l *coordinationv1.Lease) Lease {
	if l == nil {
		return Lease{}
	}
	out := Lease{Found: true}
	if l.Spec.HolderIdentity != nil {
		out.Holder = *l.Spec.HolderIdentity
	}
	if l.Spec.RenewTime != nil {
		out.RenewTime = l.Spec.RenewTime.UTC()
	}
	return out
}

func (h *HTTPAdmin) AwaitLease(ctx context.Context, timeout time.Duration, done func(Lease) bool) (Lease, error) {
	c, err := h.clientFor(SystemAdminCluster)
	if err != nil {
		return Lease{}, err
	}
	l, err := kube.Await(ctx, c.Resource(kcpgvr.Leases).In(ControllerLeaseNamespace), ControllerLeaseName, timeout,
		func(l *coordinationv1.Lease) (bool, error) { return done(leaseOf(l)), nil })
	return leaseOf(l), err
}

func (h *HTTPAdmin) DeleteLease(ctx context.Context) error {
	code, b, err := h.do(ctx, http.MethodDelete, leasePath(), nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusNotFound && code != http.StatusAccepted {
		return fmt.Errorf("kcpserver: deleting the controller lease: %d %s", code, b)
	}
	return nil
}

func workspacesPath(parent string) string {
	return "/clusters/" + parent + "/apis/tenancy.kcp.io/v1alpha1/workspaces"
}

const (
	WorkspaceOwnerAnnotation = "experimental.tenancy.kcp.io/owner"
	ShardAdminOwner          = `{"username":"shard-admin","groups":["system:masters"]}`
)

func (h *HTTPAdmin) CreateWorkspace(ctx context.Context, parent, name string) error {
	obj := map[string]any{"apiVersion": "tenancy.kcp.io/v1alpha1", "kind": "Workspace", "metadata": map[string]any{"name": name,
		"annotations": map[string]string{WorkspaceOwnerAnnotation: ShardAdminOwner}}}
	code, b, err := h.do(ctx, http.MethodPost, workspacesPath(parent), obj)
	if err != nil {
		return err
	}
	if code != http.StatusCreated && code != http.StatusOK && code != http.StatusConflict {
		return fmt.Errorf("kcpserver: creating workspace %s:%s: %d %s", parent, name, code, b)
	}
	return nil
}

func (h *HTTPAdmin) WorkspacePhase(ctx context.Context, parent, name string) (string, error) {
	code, b, err := h.do(ctx, http.MethodGet, workspacesPath(parent)+"/"+name, nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("kcpserver: reading workspace %s:%s: %d %s", parent, name, code, b)
	}
	var ws struct {
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &ws); err != nil {
		return "", err
	}
	return ws.Status.Phase, nil
}

func (h *HTTPAdmin) ListWorkspaces(ctx context.Context, parent string) ([]string, error) {
	code, b, err := h.do(ctx, http.MethodGet, workspacesPath(parent), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("kcpserver: listing workspaces of %s: %d %s", parent, code, b)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	var out []string
	for _, it := range list.Items {
		out = append(out, it.Metadata.Name)
	}
	return out, nil
}

func (h *HTTPAdmin) DeleteWorkspace(ctx context.Context, parent, name string) error {
	code, b, err := h.do(ctx, http.MethodDelete, workspacesPath(parent)+"/"+name, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusAccepted && code != http.StatusNotFound {
		return fmt.Errorf("kcpserver: deleting workspace %s:%s: %d %s", parent, name, code, b)
	}
	return nil
}

func (h *HTTPAdmin) WaitWorkspaceReady(ctx context.Context, parent, name string) error {
	c, err := h.clientFor(parent)
	if err != nil {
		return err
	}
	ws, err := kube.Await(ctx, c.Resource(kcpgvr.Workspaces), name, 0, func(ws *tenancyv1alpha1.Workspace) (bool, error) {
		return workspacePhaseOf(ws) == "Ready", nil
	})
	if err != nil {
		return fmt.Errorf("kcpserver: workspace %s:%s not Ready: phase %q (%w)", parent, name, workspacePhaseOf(ws), err)
	}
	return nil
}

func workspacePhaseOf(ws *tenancyv1alpha1.Workspace) string {
	if ws == nil {
		return ""
	}
	return string(ws.Status.Phase)
}

func (h *HTTPAdmin) ProbeWorkspace(ctx context.Context, name string) error {
	if err := h.CreateWorkspace(ctx, RootCluster, name); err != nil {
		return err
	}
	err := h.WaitWorkspaceReady(ctx, RootCluster, name)
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if derr := h.DeleteWorkspace(dctx, RootCluster, name); derr != nil && err == nil {
		err = derr
	}
	return err
}

func FrontURL(port int) string { return "https://127.0.0.1:" + strconv.Itoa(port) }
