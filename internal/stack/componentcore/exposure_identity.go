package componentcore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	ExposureIngressIdentityDir = "exposure-identity"

	ExposureServiceAuthTTL = 5 * time.Minute
)

type ExposureIngressIdentity struct {
	stateDir string

	mu sync.Mutex

	signers map[string]*exposureIngressSigner
}

type exposureIngressSigner struct {
	signer *market.Signer

	did string
}

var _ ingress.Keypair = (*exposureIngressSigner)(nil)

func NewExposureIngressIdentity(stateDir string) *ExposureIngressIdentity {
	return &ExposureIngressIdentity{stateDir: stateDir, signers: map[string]*exposureIngressSigner{}}
}

func (s *exposureIngressSigner) DID() string {
	return s.did
}

func (s *exposureIngressSigner) Sign(data []byte) ([]byte, error) {
	return s.signer.PrivateKey.HashAndSign(data)
}

func (i *ExposureIngressIdentity) Keypair(ctx context.Context, workspace, name string) (ingress.Keypair, error) {
	signer, err := i.signer(workspace, name)
	if err != nil {
		return nil, err
	}
	return signer, nil
}

func (i *ExposureIngressIdentity) ServiceAuthToken(ctx context.Context, workspace, name, aud, nsid string) (string, error) {
	signer, err := i.signer(workspace, name)
	if err != nil {
		return "", err
	}
	return ingress.SignServiceAuth(signer.signer.PrivateKey, signer.did, aud, nsid, ExposureServiceAuthTTL)
}

func (i *ExposureIngressIdentity) signer(workspace, name string) (*exposureIngressSigner, error) {
	if i == nil || i.stateDir == "" {
		return nil, errors.New("componentcore: the exposure ingress identity has no state dir")
	}
	workspace = cmp.Or(workspace, DefaultWorkspace)
	key := workspace + "/" + name
	i.mu.Lock()
	defer i.mu.Unlock()
	if existing := i.signers[key]; existing != nil {
		return existing, nil
	}
	dir := filepath.Join(i.stateDir, ExposureIngressIdentityDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("componentcore: the exposure ingress identity dir: %w", err)
	}
	sum := sha256.Sum256([]byte(key))
	path := filepath.Join(dir, hex.EncodeToString(sum[:8])+".jwk")
	signer, err := market.LoadOrCreateSignerFile(path)
	if err != nil {
		return nil, fmt.Errorf("componentcore: the exposure ingress identity for %s: %w", key, err)
	}
	did, err := signer.PublicDID()
	if err != nil {
		return nil, fmt.Errorf("componentcore: the exposure ingress DID for %s: %w", key, err)
	}
	out := &exposureIngressSigner{signer: signer, did: did}
	i.signers[key] = out
	return out, nil
}
