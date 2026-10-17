package appctl

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
)

const (
	GatewayChildSpecFile     = "gateway-spec.json"
	GatewayChildSessionsFile = "owner-sessions.json"
	GatewayChildCluster      = "tenant"
	OwnerAccountDomain       = "alice.test"
)

type ownerSession struct {
	Service    string `json:"service"`
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

func readOwnerDid(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var sessions map[string]ownerSession
	if err := json.Unmarshal(b, &sessions); err != nil {
		return "", fmt.Errorf("appctl: the owner sessions file %s: %w", path, err)
	}
	dids := make([]string, 0, len(sessions))
	for did := range sessions {
		dids = append(dids, did)
	}
	if len(dids) == 0 {
		return "", fmt.Errorf("appctl: the owner sessions file %s names no account", path)
	}
	sort.Strings(dids)
	return dids[0], nil
}

func ensureOwner(ctx context.Context, path, pdsURL, caFile, handle string) (string, error) {
	if did, err := readOwnerDid(path); err == nil {
		return did, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	buf := make([]byte, passwordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	password := hex.EncodeToString(buf)
	did, err := EnsurePdsAccount(ctx, pdsURL, caFile, handle, password)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(map[string]ownerSession{did: {Service: pdsURL, Identifier: handle, Password: password}})
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return did, os.Rename(tmp, path)
}

func GatewayChildOf(ctx context.Context, id, stateDir string, app v1alpha1.NetworkApp, value v1alpha1.MarketNetworkValue, aliceURL string) (*devinstance.GatewayChild, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	sessions := app.OwnerSessionsFile
	var did string
	var err error
	if sessions == "" {
		sessions = filepath.Join(stateDir, GatewayChildSessionsFile)
		did, err = ensureOwner(ctx, sessions, aliceURL, value.CaBundleRef, id+"-owner."+OwnerAccountDomain)
	} else {
		did, err = readOwnerDid(sessions)
	}
	if err != nil {
		return nil, fmt.Errorf("the owner account of %s's tenant cluster: %w", id, err)
	}
	spec := v1alpha1.GatewaySpec{Profile: app.Profile, Network: &value, Options: app.Options,
		MarketNetwork: &v1alpha1.GatewayMarketNetwork{OwnerSessionsFile: sessions}, ClusterProvider: app.ClusterProvider, Quota: app.Quota}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, GatewayChildSpecFile)
	if have, err := os.ReadFile(path); err != nil || !bytes.Equal(have, b) {
		if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
			return nil, err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return nil, err
		}
	}
	return &devinstance.GatewayChild{SpecFile: path, BootstrapCluster: GatewayChildCluster, BootstrapAccountDID: did}, nil
}
