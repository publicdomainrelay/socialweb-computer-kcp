package ingress

import (
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

type EphemeralKeypair struct {
	key atcrypto.PrivateKey

	did string
}

var _ Keypair = (*EphemeralKeypair)(nil)

func NewEphemeralKeypair() (*EphemeralKeypair, error) {
	key, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		return nil, fmt.Errorf("ingress: generating an ephemeral key: %w", err)
	}
	pub, err := key.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("ingress: deriving the ephemeral key: %w", err)
	}
	return &EphemeralKeypair{key: key, did: pub.DIDKey()}, nil
}

func (k *EphemeralKeypair) DID() string { return k.did }

func (k *EphemeralKeypair) Sign(data []byte) ([]byte, error) { return k.key.HashAndSign(data) }

func (k *EphemeralKeypair) ServiceAuthToken(aud, nsid string, ttl time.Duration) (string, error) {
	return SignServiceAuth(k.key, k.did, aud, nsid, ttl)
}
