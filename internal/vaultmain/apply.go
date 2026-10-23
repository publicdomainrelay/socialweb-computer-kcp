package vaultmain

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	pkiRootCommonName = "socialweb.local"
	pkiRootTTL        = 8760 * time.Hour
)

type RootAccess struct {
	Server     string
	Address    string
	CAFile     string
	ClientCert string
	ClientKey  string
	CertMount  string
	CertRole   string
}

func RootAccessOf(o v1alpha1.OpenBaoServer) (RootAccess, error) {
	if o.Status.Phase != v1alpha1.OpenBaoServerPhaseReady || o.Status.Address == "" {
		return RootAccess{}, fmt.Errorf("the OpenBaoServer %s is %s (%s), not Ready", o.Name, o.Status.Phase, o.Status.Message)
	}
	a := RootAccess{Server: o.Name, Address: o.Status.Address, CertMount: openbaoctl.CertAuthMount}
	if o.Status.Refs != nil {
		a.CAFile = openbaoctl.FileRef(o.Status.Refs.CA)
	}
	for _, c := range o.Status.Clients {
		if c.Name != vaultlayout.RootClient {
			continue
		}
		a.ClientCert, a.ClientKey, a.CertRole = openbaoctl.FileRef(c.ClientCert), openbaoctl.FileRef(c.ClientKey), c.CertAuthRole
		if c.CertAuthMount != "" {
			a.CertMount = c.CertAuthMount
		}
	}
	if a.CAFile == "" || a.ClientCert == "" || a.ClientKey == "" || a.CertRole == "" {
		return RootAccess{}, fmt.Errorf("the OpenBaoServer %s reports no CA or no client %s", o.Name, vaultlayout.RootClient)
	}
	return a, nil
}

func ReadRootAccess(ctx context.Context, kubeconfig, workspace, server string) (RootAccess, error) {
	restCfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return RootAccess{}, err
	}
	cfg := rest.CopyConfig(restCfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + workspace
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return RootAccess{}, err
	}
	u, err := dc.Resource(openbaoctl.GVR).Get(ctx, server, metav1.GetOptions{})
	if err != nil {
		return RootAccess{}, fmt.Errorf("the root vault's OpenBaoServer %s in %s: %w", server, workspace, err)
	}
	var o v1alpha1.OpenBaoServer
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o); err != nil {
		return RootAccess{}, err
	}
	return RootAccessOf(o)
}

func (a RootAccess) Client(ctx context.Context) (*openbao.Client, error) {
	raw, err := os.ReadFile(a.CAFile)
	if err != nil {
		return nil, fmt.Errorf("the root vault's TLS CA: %w", err)
	}
	pool, err := openbao.CertPoolFromPEM(raw)
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(a.ClientCert, a.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("the client certificate of %s: %w", vaultlayout.RootClient, err)
	}
	base := openbao.New(a.Address, "", openbao.WithCACertPool(pool), openbao.WithClientCertificate(pair))
	client, err := base.LoginCert(ctx, a.CertMount, a.CertRole)
	if err != nil {
		return nil, fmt.Errorf("logging in to the root vault at %s as %s: %w", a.Address, a.CertRole, err)
	}
	return client, nil
}

func pkiRootPresent(ctx context.Context, c *openbao.Client, mount string) (bool, error) {
	_, err := c.CAPEM(ctx, mount)
	if err == nil {
		return true, nil
	}
	var re *openbao.ResponseError
	if errors.As(err, &re) {
		lower := strings.ToLower(re.Body)
		if re.Status == http.StatusNoContent || re.Status == http.StatusNotFound || strings.Contains(lower, "no ca") ||
			strings.Contains(lower, "not found") || strings.Contains(lower, "no certificate") {
			return false, nil
		}
	}
	if strings.Contains(err.Error(), "holds no certificate") {
		return false, nil
	}
	return false, err
}

func ApplyRoot(ctx context.Context, a RootAccess, cfg Config, log *slog.Logger) error {
	c, err := a.Client(ctx)
	if err != nil {
		return err
	}
	if err := ensurePKIRoot(ctx, c, log); err != nil {
		return err
	}
	return applySecrets(ctx, c, cfg, log)
}

func ensurePKIRoot(ctx context.Context, c *openbao.Client, log *slog.Logger) error {
	for _, m := range vaultlayout.Default().Mounts {
		if m.Vault != vaultlayout.Root || m.Kind != vaultlayout.MountPKI {
			continue
		}
		present, err := pkiRootPresent(ctx, c, m.Path)
		if err != nil {
			return err
		}
		if present {
			log.Info("pki root already generated", "mount", m.Path)
			continue
		}
		ttl := m.MaxLeaseTTL
		ttl = opt.Positive(ttl, pkiRootTTL)
		cert, err := c.GeneratePKIRoot(ctx, m.Path, pkiRootCommonName, ttl.String())
		if err != nil {
			return fmt.Errorf("generating the root CA under %s: %w", m.Path, err)
		}
		log.Info("generated the pki root", "mount", m.Path, "common_name", pkiRootCommonName, "serial", cert.Serial)
	}
	return nil
}

type sourcedSecret struct {
	Path     string
	Data     map[string]any
	Reason   string
	Required bool
}

func planRootSecrets(ctx context.Context, c *openbao.Client, cfg Config) []sourcedSecret {
	var out []sourcedSecret
	for _, s := range vaultlayout.Default().Secrets {
		if s.Vault != vaultlayout.Root {
			continue
		}
		switch s.Class {
		case vaultlayout.ClassKCPAdminKubeconfig:
			raw, err := os.ReadFile(cfg.KCPKubeconfig)
			if err != nil {
				out = append(out, sourcedSecret{Path: s.Path, Required: true, Reason: err.Error()})
				continue
			}
			out = append(out, sourcedSecret{Path: s.Path, Data: map[string]any{"kubeconfig": string(raw)}})
		case vaultlayout.ClassKCPAdminTokenStore:
			data, err := kcpAdminTokenStoreData(cfg)
			if err != nil {
				out = append(out, sourcedSecret{Path: s.Path, Required: true, Reason: err.Error()})
				continue
			}
			out = append(out, sourcedSecret{Path: s.Path, Data: data})
		case vaultlayout.ClassKCPClientCA:
			pem, err := c.CAPEM(ctx, vaultlayout.MountPKIRoot)
			if err != nil {
				out = append(out, sourcedSecret{Path: s.Path, Required: true, Reason: err.Error()})
				continue
			}
			out = append(out, sourcedSecret{Path: s.Path, Data: map[string]any{"ca.crt": strings.TrimSpace(pem)}})
		case vaultlayout.ClassContainerTLS:
			if cfg.TLSCert == "" || cfg.TLSKey == "" {
				out = append(out, sourcedSecret{Path: s.Path, Reason: "the TLS front names no certificate"})
				continue
			}
			cert, cerr := os.ReadFile(cfg.TLSCert)
			key, kerr := os.ReadFile(cfg.TLSKey)
			if err := errors.Join(cerr, kerr); err != nil {
				out = append(out, sourcedSecret{Path: s.Path, Reason: err.Error()})
				continue
			}
			out = append(out, sourcedSecret{Path: s.Path, Data: map[string]any{"tls.crt": string(cert), "tls.key": string(key)}})
		default:
			out = append(out, sourcedSecret{Path: s.Path, Required: true, Reason: fmt.Sprintf("class %s has no producer", s.Class)})
		}
	}
	return out
}

func applySecrets(ctx context.Context, c *openbao.Client, cfg Config, log *slog.Logger) error {
	for _, s := range planRootSecrets(ctx, c, cfg) {
		if s.Data == nil {
			if s.Required {
				return fmt.Errorf("the root secret at %s has no source: %s", s.Path, s.Reason)
			}
			log.Warn("root secret not written", "path", s.Path, "reason", s.Reason)
			continue
		}
		changed, err := putIfChanged(ctx, c, s.Path, s.Data)
		if err != nil {
			return fmt.Errorf("writing %s: %w", s.Path, err)
		}
		if changed {
			log.Info("wrote root secret", "path", s.Path, "keys", keysOf(s.Data))
		} else {
			log.Info("root secret already current", "path", s.Path)
		}
	}
	return nil
}

func putIfChanged(ctx context.Context, c *openbao.Client, path string, data map[string]any) (bool, error) {
	existing, err := c.GetKV(ctx, path)
	if err == nil && reflect.DeepEqual(existing, data) {
		return false, nil
	}
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return false, err
	}
	return true, c.PutKV(ctx, path, data)
}

func keysOf(data map[string]any) []string {
	out := make([]string, 0, len(data))
	for k := range data {
		out = append(out, k)
	}
	return out
}
