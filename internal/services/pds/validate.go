package pds

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	pdsfiles "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/files"
)

func ValidateStateDir(dir, did string, wantOAuth bool) (string, error) {
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", fmt.Errorf("pds: the state dir %q is not a clean absolute path", dir)
	}
	parsed, err := syntax.ParseDID(strings.TrimSpace(did))
	if err != nil {
		return "", fmt.Errorf("pds: the state dir identity did %q: %w", did, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("pds: the state dir %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("pds: the state dir %s is not a real directory", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("pds: the state dir %s is mode %04o; it must be 0700 or stricter", dir, perm)
	}
	for _, name := range []string{"accounts.db", "repos.db"} {
		if err := regularFile(filepath.Join(dir, name), 0); err != nil {
			return "", err
		}
	}
	if err := regularFile(filepath.Join(dir, SessionSecretFile), 0o600); err != nil {
		return "", err
	}
	secret, err := os.ReadFile(filepath.Join(dir, SessionSecretFile))
	if err != nil {
		return "", fmt.Errorf("pds: the state dir %s session secret: %w", dir, err)
	}
	if len(strings.TrimSpace(string(secret))) != 64 {
		return "", fmt.Errorf("pds: the state dir %s session secret is not 32 hex-encoded bytes", dir)
	}
	if _, err := hex.DecodeString(strings.TrimSpace(string(secret))); err != nil {
		return "", fmt.Errorf("pds: the state dir %s session secret: %w", dir, err)
	}
	rotation, err := readPrivateKey(filepath.Join(dir, RotationKeyFile))
	if err != nil {
		return "", err
	}
	if _, ok := rotation.(*atcrypto.PrivateKeyK256); !ok {
		return "", fmt.Errorf("pds: the state dir %s rotation key is not a secp256k1 key", dir)
	}
	public, err := rotation.PublicKey()
	if err != nil {
		return "", fmt.Errorf("pds: the state dir %s rotation key: %w", dir, err)
	}
	if wantOAuth {
		if _, err := readPrivateKey(filepath.Join(dir, pdsfiles.OAuthSignerFile)); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256([]byte(parsed.String()))
	repoKey := filepath.Join(dir, RepoKeyDir, hex.EncodeToString(sum[:])+".key")
	if err := regularFile(repoKey, 0o600); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(repoKey)
	if err != nil {
		return "", fmt.Errorf("pds: the state dir %s repo key: %w", dir, err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)
	if len(lines) != 2 || strings.TrimSpace(lines[0]) != parsed.String() {
		return "", fmt.Errorf("pds: the state dir %s repo key does not name %s", dir, parsed)
	}
	if _, err := atcrypto.ParsePrivateMultibase(strings.TrimSpace(lines[1])); err != nil {
		return "", fmt.Errorf("pds: the state dir %s repo key: %w", dir, err)
	}
	return public.DIDKey(), nil
}

func regularFile(path string, perm os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("pds: %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("pds: %s is not a regular file", path)
	}
	if perm != 0 && info.Mode().Perm() != perm {
		return fmt.Errorf("pds: %s is mode %04o; it must be %04o", path, info.Mode().Perm(), perm)
	}
	return nil
}

func readPrivateKey(path string) (atcrypto.PrivateKey, error) {
	if err := regularFile(path, 0o600); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pds: %s: %w", path, err)
	}
	key, err := atcrypto.ParsePrivateMultibase(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("pds: %s: %w", path, err)
	}
	return key, nil
}
