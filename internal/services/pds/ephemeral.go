package pds

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/files"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/plcsubmit"
)

const ephemeralTombstoneTimeout = 30 * time.Second

type ephemeralKeys struct {
	dir string

	sessionSecret []byte

	rotation atcrypto.PrivateKey

	oauthSigner atcrypto.PrivateKey

	repoKeys *files.MemoryRepoKeyStore

	plc *plcsubmit.HTTPPLCSubmitter
}

func newEphemeralKeys(stateDir string) (*ephemeralKeys, error) {
	dir, err := os.MkdirTemp(stateDir, "ephemeral-")
	if err != nil {
		return nil, fmt.Errorf("pds: the ephemeral data dir: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("pds: the ephemeral session secret: %w", err)
	}
	rotation, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("pds: the ephemeral rotation key: %w", err)
	}
	signer, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("pds: the ephemeral oauth signer: %w", err)
	}
	return &ephemeralKeys{dir: dir, sessionSecret: secret, rotation: rotation, oauthSigner: signer,
		repoKeys: files.NewMemoryRepoKeyStore()}, nil
}

func (k *ephemeralKeys) tombstone(ctx context.Context, log *slog.Logger) {
	if k.plc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, ephemeralTombstoneTimeout)
	defer cancel()
	for _, did := range k.repoKeys.DIDs() {
		err := k.plc.Tombstone(ctx, did)
		if log == nil {
			continue
		}
		if err != nil {
			log.Warn("pds_ephemeral_tombstone_failed", "did", did.String(), "err", err.Error())
			continue
		}
		log.Info("pds_ephemeral_tombstoned", "did", did.String())
	}
}

func (k *ephemeralKeys) remove() {
	_ = os.RemoveAll(k.dir)
}
