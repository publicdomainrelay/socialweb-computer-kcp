package clusterprovider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/clusterhome"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
)

const tenantProbeTimeout = 5 * time.Second

type Ref struct {
	Account string
	DID     string
	Home    string
	Name    string
}

func (r Ref) MountPath() string { return clusterhome.MountPath(r.DID, r.Name) }

func (r Ref) Key() (string, error) { return clusterhome.Key(r.Home) }

func ProbeTenantFront(ctx context.Context, dataDir string) materialiser.PathProbe {
	addr, err := tenantFrontAddress(dataDir)
	if err != nil {
		return materialiser.PathProbe{Err: err.Error()}
	}
	raw, err := loadServerCA(filepath.Join(clusterstack.TLSDir(dataDir), serverCACertName))
	if err != nil {
		return materialiser.PathProbe{Err: err.Error()}
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(raw)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	pctx, cancel := context.WithTimeout(ctx, tenantProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, "https://"+addr+"/readyz", nil)
	if err != nil {
		return materialiser.PathProbe{Err: err.Error()}
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return materialiser.PathProbe{Err: err.Error()}
	}
	defer resp.Body.Close()
	return materialiser.PathProbe{Answered: true, Status: resp.StatusCode}
}

func tenantFrontAddress(dataDir string) (string, error) {
	raw, err := os.ReadFile(FrontStatePath(dataDir))
	if err != nil {
		return "", err
	}
	var st apiserverfront.State
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("%s: %w", FrontStatePath(dataDir), err)
	}
	if _, _, err := net.SplitHostPort(st.Address); err != nil {
		return "", fmt.Errorf("%s names no front address: %w", FrontStatePath(dataDir), err)
	}
	return st.Address, nil
}
