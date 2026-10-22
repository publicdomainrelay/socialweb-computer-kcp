package componentcore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

func loadOAuthCLIKey(stateDir string) (atcrypto.PrivateKey, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return nil, errors.New("componentcore: no state dir for the oauth cli key")
	}
	path := filepath.Join(stateDir, "oauth-cli-key")
	raw, err := os.ReadFile(path)
	if err == nil {
		if key, parseErr := atcrypto.ParsePrivateMultibase(strings.TrimSpace(string(raw))); parseErr == nil {
			if _, ok := key.(*atcrypto.PrivateKeyK256); ok {
				return key, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(key.Multibase()+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
