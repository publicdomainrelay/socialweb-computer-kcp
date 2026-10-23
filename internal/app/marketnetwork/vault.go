package marketnetwork

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

const (
	NetworkDir         = "network"
	NetworkVaultServer = "network"
	NetworkVaultClient = "components"
	NetworkPKIMount    = "pki"
	NetworkOwnerMount  = "owner"
)

func NetworkStateDir(instanceState string) string { return filepath.Join(instanceState, NetworkDir) }

func (ctl *Controller) vaultReady(ctx context.Context, ws string) error {
	u, err := ctl.c.Get(ctx, ws, compose.OpenBaoServerGVR, NetworkVaultServer)
	if err != nil {
		return err
	}
	var o v1alpha1.OpenBaoServer
	if err := unstruct.Into(u, &o); err != nil {
		return err
	}
	st := o.Status
	if st.Phase != v1alpha1.OpenBaoServerPhaseReady || st.Address == "" || st.Refs == nil {
		return fmt.Errorf("the OpenBaoServer %s is %s, not Ready", NetworkVaultServer, st.Phase)
	}
	for _, x := range st.Clients {
		if x.Name == NetworkVaultClient && x.ClientCert != "" {
			return nil
		}
	}
	return fmt.Errorf("the OpenBaoServer %s reports no client %s yet", NetworkVaultServer, NetworkVaultClient)
}

func vaultLinks(r compose.Root) []compose.Link {
	return []compose.Link{compose.VaultPKILink(r.Ref(), NetworkVaultServer, NetworkPKIMount)}
}

func (ctl *Controller) pkiCA(ctx context.Context, r compose.Root) ([]byte, error) {
	pki, err := bus.Resolve[contractvault.PKI](ctx, ctl.c.Bus(), r.Ref(), compose.VaultRefIn(NetworkVaultServer, r.Workspace),
		bus.Capability{Name: contractvault.CapabilityPKIIssue, Scope: []string{NetworkPKIMount}})
	if err != nil {
		return nil, err
	}
	pem, err := pki.CA(ctx, NetworkPKIMount)
	if err != nil {
		return nil, fmt.Errorf("the network's PKI root CA: %w", err)
	}
	return []byte(pem), nil
}

func (ctl *Controller) caBundle(ctx context.Context, r compose.Root) (string, error) {
	if len(ctl.pkiPEM) == 0 {
		pem, err := ctl.pkiCA(ctx, r)
		if err != nil {
			return "", err
		}
		ctl.pkiPEM = pem
	}
	guest, err := os.ReadFile(filepath.Join(NetworkStateDir(ctl.c.StateRoot()), GuestTLSDir, guestCAFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ctl.pkiPEM)) + "\n" + strings.TrimSpace(string(guest)) + "\n", nil
}

func (ctl *Controller) oneKcp(ws string, apps []v1alpha1.NetworkAppStatus) []string {
	front := ctl.c.KcpServer()
	workspaces := map[string]string{ws: "the network"}
	var wrong []string
	for _, a := range apps {
		if a.KcpURL == "" {
			continue
		}
		if f, _, _ := strings.Cut(strings.TrimSuffix(a.KcpURL, "/"), "/clusters/"); f != front {
			wrong = append(wrong, fmt.Sprintf("%s reports the kcp %s, not the network's %s", a.Name, f, front))
		}
		if a.Workspace == "" {
			wrong = append(wrong, a.Name+" reports no workspace")
			continue
		}
		if other, ok := workspaces[a.Workspace]; ok {
			wrong = append(wrong, fmt.Sprintf("%s shares the workspace %s with %s", a.Name, a.Workspace, other))
		}
		workspaces[a.Workspace] = a.Name
	}
	return wrong
}

func (ctl *Controller) verifierTransport(req *http.Request) http.RoundTripper {
	handlers := ctl.c.Bus()
	if handlers == nil || req == nil || req.URL == nil {
		return nil
	}
	host := strings.ToLower(req.URL.Hostname())
	if host == "" {
		return nil
	}
	if _, ok := handlers.HandlerForHost(host); ok {
		return handlers.RoundTripper()
	}
	return nil
}

func (ctl *Controller) verify(ctx context.Context, snap compose.Snapshot, apps []v1alpha1.NetworkAppStatus) error {
	for _, check := range []struct{ kind, name, path string }{
		{compose.ComponentKindPlc, compose.NetworkPLCName, "/health"},
		{compose.ComponentKindRelay, compose.NetworkRelayName, "/xrpc/_health"},
		{compose.ComponentKindPds, NetworkAlicePdsName, "/xrpc/_health"},
		{compose.ComponentKindPds, NetworkBobPdsName, "/xrpc/_health"},
		{compose.ComponentKindIngressProxy, compose.NetworkIngressName, "/.well-known/did.json"},
	} {
		v, ok := snap.View(check.kind, check.name)
		if !ok {
			return fmt.Errorf("%s %s is not there", check.kind, check.name)
		}
		if v.Status.Phase == v1alpha1.ComponentPhaseSuspended {
			continue
		}
		if !v.Status.Ready {
			return fmt.Errorf("%s %s is not ready", check.kind, check.name)
		}
		e, ok := compose.ComponentEndpoint(v, "http")
		if !ok {
			return fmt.Errorf("%s %s reports no endpoint", check.kind, check.name)
		}
		base := compose.EndpointURL(e)
		if base == "" {
			return fmt.Errorf("%s %s reports no endpoint URL", check.kind, check.name)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+check.path, nil)
		if err != nil {
			return err
		}
		if check.kind == compose.ComponentKindIngressProxy {
			host := GuestTLSHost
			if ctl.value != nil && ctl.value.IngressProxyHost != "" {
				if h, _, ok := strings.Cut(ctl.value.IngressProxyHost, ":"); ok && h != "" {
					host = h
				} else {
					host = ctl.value.IngressProxyHost
				}
			}
			req.Host = host
		}
		client := &http.Client{Timeout: VerifierProbeTimeout}
		if transport := ctl.verifierTransport(req); transport != nil {
			client.Transport = transport
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s at %s: %w", check.kind, check.name, check.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("%s %s at %s returned HTTP %d", check.kind, check.name, check.path, resp.StatusCode)
		}
	}
	for _, a := range apps {
		if a.Kind == v1alpha1.ApplicationKindMarketRequester || a.Kind == v1alpha1.ApplicationKindSshGateway {
			continue
		}
		if a.Message == StoppedMessage || a.Phase == v1alpha1.ApplicationPhaseSuspended {
			continue
		}
		if a.Phase != v1alpha1.ApplicationPhaseReady {
			return fmt.Errorf("the child application %s is %s: %s", a.Name, a.Phase, a.Message)
		}
	}
	return nil
}
