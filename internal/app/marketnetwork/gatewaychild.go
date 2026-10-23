package marketnetwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
)

const (
	GatewayChildSpecFile     = "gateway-spec.json"
	GatewayChildSessionsFile = "owner-sessions.json"
	GatewayChildCluster      = "tenant"
	OwnerAccountDomain       = "alice.test"
)

func readOwnerDid(path string) (string, error) {
	sessions, err := compose.ReadOwnerSessions(path)
	if err != nil {
		return "", err
	}
	dids := make([]string, 0, len(sessions))
	for did := range sessions {
		dids = append(dids, did)
	}
	if len(dids) == 0 {
		return "", fmt.Errorf("marketnetwork: the owner sessions file %s names no account", path)
	}
	sort.Strings(dids)
	return dids[0], nil
}

func ownerAccount(id, handle string) compose.PdsAccount {
	return compose.PdsAccount{Vault: NetworkVaultServer, Mount: NetworkOwnerMount, Path: "identity/owner-" + id + "/pds-session",
		Handle: handle, Pds: NetworkAlicePdsName}
}

func ensureOwner(ctx context.Context, c *compose.Composer, r compose.Root, id, path, pdsURL, handle string) (string, error) {
	if did, session, err := compose.FirstOwnerSession(path); err == nil {
		if session.Service == pdsURL {
			return did, nil
		}
		session.Service = pdsURL
		b, err := json.Marshal(map[string]compose.OwnerSession{did: session})
		if err != nil {
			return "", err
		}
		return did, compose.WriteFileAtomic(path, b, 0o600)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	account := ownerAccount(id, handle)
	links := compose.PdsAccountLinks(r, account)
	if err := c.ApplyLinks(ctx, r, "owner-"+id, links); err != nil {
		return "", err
	}
	if ok, msg := c.LinksAccepted(ctx, r, links); !ok {
		return "", errors.New(msg)
	}
	did, err := c.EnsurePdsAccount(ctx, r, account)
	if err != nil {
		return "", err
	}
	kv, err := c.KV(ctx, r, account.Vault, account.Mount+"/identity/*")
	if err != nil {
		return "", err
	}
	data, err := compose.ReadKV(ctx, kv, account.Mount, account.Path)
	if err != nil {
		return "", err
	}
	password, _ := data["password"].(string)
	if password == "" {
		return "", fmt.Errorf("marketnetwork: the owner account %s has no password in the network vault", handle)
	}
	b, err := json.Marshal(map[string]compose.OwnerSession{did: {Service: pdsURL, Identifier: handle, Password: password}})
	if err != nil {
		return "", err
	}
	return did, compose.WriteFileAtomic(path, b, 0o600)
}

func GatewayChildOf(ctx context.Context, c *compose.Composer, r compose.Root, id, stateDir string, app v1alpha1.NetworkApp,
	value v1alpha1.MarketNetworkValue, aliceURL string, bidderDids []string, suspend bool) (*devinstance.GatewayChild, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	sessions := app.OwnerSessionsFile
	var did string
	var err error
	if sessions == "" {
		sessions = filepath.Join(stateDir, GatewayChildSessionsFile)
		did, err = ensureOwner(ctx, c, r, id, sessions, aliceURL, id+"-owner."+OwnerAccountDomain)
	} else {
		did, err = readOwnerDid(sessions)
	}
	if err != nil {
		return nil, fmt.Errorf("the owner account of %s's tenant cluster: %w", id, err)
	}
	spec := v1alpha1.GatewaySpec{Profile: app.Profile, Suspend: suspend, Network: &value, Options: app.Options, BidderDids: append([]string(nil), bidderDids...),
		MarketNetwork: &v1alpha1.GatewayMarketNetwork{OwnerSessionsFile: sessions}, ClusterProvider: app.ClusterProvider, Quota: app.Quota,
		Components: app.Components}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, GatewayChildSpecFile)
	if have, err := os.ReadFile(path); err != nil || !bytes.Equal(have, b) {
		if err := compose.WriteFileAtomic(path, b, 0o600); err != nil {
			return nil, err
		}
	}
	return &devinstance.GatewayChild{SpecFile: path, BootstrapCluster: GatewayChildCluster, BootstrapAccountDID: did}, nil
}
