package request

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

const (
	RequestersDir      = bootstrap.RequestersDir
	ReadyFile          = bootstrap.RequesterReadyFile
	RequesterFile      = bootstrap.RequesterRecordFile
	KubeconfigFile     = "kubeconfig"
	ReadySchema        = 1
	RequesterSchema    = 1
	kubeconfigName     = "requester"
	readyFileMode      = 0o600
	requesterDirMode   = 0o700
	clustersPathPrefix = "/clusters/"
)

func RequestersRoot(stateRoot string) string { return filepath.Join(stateRoot, RequestersDir) }

func RequesterDir(stateRoot, name string) string {
	return filepath.Join(RequestersRoot(stateRoot), name)
}

type ReadyIngress struct {
	Pds       string `json:"pds,omitempty"`
	Requester string `json:"requester,omitempty"`
}

type ReadyGateway struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	Clusters  string `json:"clusters"`
}

type Ready struct {
	Schema       int           `json:"schema"`
	Name         string        `json:"name"`
	Pid          int           `json:"pid"`
	StartedAt    time.Time     `json:"startedAt"`
	KcpURL       string        `json:"kcpUrl"`
	Kubeconfig   string        `json:"kubeconfig"`
	Workspace    string        `json:"workspace"`
	Did          string        `json:"did,omitempty"`
	Handle       string        `json:"handle,omitempty"`
	IdentityMode string        `json:"identityMode,omitempty"`
	PdsURL       string        `json:"pdsUrl,omitempty"`
	Ingress      ReadyIngress  `json:"ingress"`
	Gateway      *ReadyGateway `json:"gateway,omitempty"`
	SSHPublicKey string        `json:"sshPublicKey,omitempty"`
}

func (r Ready) Same(o Ready) bool { return reflect.DeepEqual(r, o) }

func WriteReady(path string, r Ready) error {
	if err := os.Remove(path + ".tmp"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsx.WriteJSON(path, r, readyFileMode)
}

func ReadReady(path string) (Ready, error) {
	var r Ready
	if err := fsx.ReadJSON(path, &r); err != nil {
		return r, fmt.Errorf("request: no ready file %s (is run requester running?): %w", path, err)
	}
	if r.Schema != ReadySchema {
		return r, fmt.Errorf("request: the ready file %s has schema %d, not %d", path, r.Schema, ReadySchema)
	}
	return r, nil
}

func RemoveReady(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func ReadyOf(st v1alpha1.ApplicationStatus, id *v1alpha1.MarketIdentity) (did, handle, mode, pdsURL string, ingress ReadyIngress) {
	did = st.Did
	if id != nil {
		did = cmp.Or(strings.TrimSpace(id.Status.Did), did)
		handle = strings.TrimSpace(id.Status.Handle)
		mode = string(id.Spec.Mode)
		pdsURL = strings.TrimSpace(id.Status.PdsURL)
	}
	pdsExposure := compose.PdsIngressExposureName(compose.PdsComponent)
	requesterExposure := compose.ServiceNameFor(compose.ComponentKindRequester, marketrequester.RequesterComponent) + compose.ExposureIngressSuffix
	for _, l := range st.Listeners {
		switch l.Component {
		case pdsExposure:
			ingress.Pds = cmp.Or(ingress.Pds, l.Address)
		case requesterExposure:
			ingress.Requester = cmp.Or(ingress.Requester, l.Address)
		}
	}
	return did, handle, mode, pdsURL, ingress
}

type RequesterRecord struct {
	Schema      int                          `json:"schema"`
	Name        string                       `json:"name"`
	Network     string                       `json:"network,omitempty"`
	Identity    string                       `json:"identity,omitempty"`
	Spec        v1alpha1.MarketRequesterSpec `json:"spec"`
	WithGateway bool                         `json:"withGateway,omitempty"`
	CreatedAt   time.Time                    `json:"createdAt"`
}

func (r RequesterRecord) SameShape(o RequesterRecord) bool {
	return r.Name == o.Name && r.Network == o.Network && r.Identity == o.Identity && reflect.DeepEqual(r.Spec, o.Spec)
}

func EnsureRequesterDir(dir string) error {
	if err := os.MkdirAll(dir, requesterDirMode); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("request: %s is not a real directory", dir)
	}
	return os.Chmod(dir, requesterDirMode)
}

func ReadRequesterRecord(dir string) (RequesterRecord, bool, error) {
	var r RequesterRecord
	err := fsx.ReadJSON(filepath.Join(dir, RequesterFile), &r)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if r.Schema != RequesterSchema {
		return r, false, fmt.Errorf("request: %s has schema %d, not %d", filepath.Join(dir, RequesterFile), r.Schema, RequesterSchema)
	}
	return r, true, nil
}

func WriteRequesterRecord(dir string, r RequesterRecord) error {
	return fsx.WriteJSON(filepath.Join(dir, RequesterFile), r, readyFileMode)
}

func WriteWorkspaceKubeconfig(path, admin, workspace string) error {
	cfg, err := clientcmd.LoadFromFile(admin)
	if err != nil {
		return err
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return fmt.Errorf("request: %s has no current context", admin)
	}
	cluster, ok := cfg.Clusters[ctx.Cluster]
	if !ok {
		return fmt.Errorf("request: %s names no cluster %s", admin, ctx.Cluster)
	}
	user, ok := cfg.AuthInfos[ctx.AuthInfo]
	if !ok {
		return fmt.Errorf("request: %s names no user %s", admin, ctx.AuthInfo)
	}
	host, _, _ := strings.Cut(strings.TrimSuffix(cluster.Server, "/"), clustersPathPrefix)
	out := clientcmdapi.NewConfig()
	c := cluster.DeepCopy()
	c.Server = host + clustersPathPrefix + cmp.Or(workspace, compose.DefaultWorkspace)
	out.Clusters[kubeconfigName] = c
	out.AuthInfos[kubeconfigName] = user.DeepCopy()
	out.Contexts[kubeconfigName] = &clientcmdapi.Context{Cluster: kubeconfigName, AuthInfo: kubeconfigName}
	out.CurrentContext = kubeconfigName
	b, err := clientcmd.Write(*out)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(tmp, b, readyFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
