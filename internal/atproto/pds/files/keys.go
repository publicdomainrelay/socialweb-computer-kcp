package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
)

var (
	_ atpds.RepoKeyStore = (*FileRepoKeyStore)(nil)
	_ atpds.RepoKeyStore = (*JSONFileRepoKeyStore)(nil)
)

func Path(raw string) string {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"file://", "file:"} {
		if after, ok := strings.CutPrefix(raw, prefix); ok {
			return after
		}
	}
	return raw
}

func RepoKeyStore(repoKeyPath string) (atpds.RepoKeyStore, error) {
	raw := strings.TrimSpace(repoKeyPath)
	if raw == "" {
		return nil, nil
	}
	path := Path(raw)
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return NewFileRepoKeyStore(path)
		}
		return NewJSONFileRepoKeyStore(path)
	}
	if strings.HasSuffix(raw, string(os.PathSeparator)) || filepath.Ext(path) == "" {
		return NewFileRepoKeyStore(path)
	}
	return NewJSONFileRepoKeyStore(path)
}

func SessionSecret(stateDir string, path string) ([]byte, error) {
	if strings.TrimSpace(path) != "" {
		data, err := pathData(path)
		if err != nil {
			return nil, fmt.Errorf("pds: resolving the session secret: %w", err)
		}
		return sessionSecretFromData(data)
	}
	file := filepath.Join(stateDir, "session-secret")
	data, err := os.ReadFile(file)
	if err == nil {
		secret, decodeErr := hex.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil || len(secret) == 0 {
			return nil, fmt.Errorf("pds: the persisted session secret is invalid")
		}
		return secret, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("pds: reading the session secret: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("pds: generating the session secret: %w", err)
	}
	if err := writePrivateFile(file, []byte(hex.EncodeToString(secret)+"\n")); err != nil {
		return nil, fmt.Errorf("pds: persisting the session secret: %w", err)
	}
	return secret, nil
}

const OAuthSignerFile = "oauth-signing.key"

func OAuthSigner(stateDir string) (atcrypto.PrivateKey, error) {
	return resolvePrivateKey(stateDir, OAuthSignerFile, func() (atcrypto.PrivateKeyExportable, error) {
		return atcrypto.GeneratePrivateKeyP256()
	})
}

func RotationKey(stateDir string, path string) (atcrypto.PrivateKey, error) {
	if strings.TrimSpace(path) != "" {
		data, err := pathData(path)
		if err != nil {
			return nil, fmt.Errorf("pds: resolving the rotation key: %w", err)
		}
		return privateKeyFromData(data)
	}
	return resolvePrivateKey(stateDir, "plc-rotation.key", func() (atcrypto.PrivateKeyExportable, error) {
		return atcrypto.GeneratePrivateKeyK256()
	})
}

func pathData(raw string) (map[string]any, error) {
	data, err := os.ReadFile(Path(raw))
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if json.Unmarshal(data, &out) == nil && out != nil {
		return out, nil
	}
	return map[string]any{"value": string(data)}, nil
}

type FileRepoKeyStore struct {
	dir string
}

func NewFileRepoKeyStore(dir string) (*FileRepoKeyStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("pds: the file repo key store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pds: creating the repo key directory: %w", err)
	}
	return &FileRepoKeyStore{dir: dir}, nil
}

func (s *FileRepoKeyStore) path(did syntax.DID) string {
	sum := sha256.Sum256([]byte(did.String()))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".key")
}

func (s *FileRepoKeyStore) PutRepoKey(_ context.Context, did syntax.DID, key atcrypto.PrivateKeyExportable) error {
	if key == nil {
		return errors.New("pds: the repo signing key is nil")
	}
	payload := did.String() + "\n" + key.Multibase() + "\n"
	if err := writePrivateFile(s.path(did), []byte(payload)); err != nil {
		return fmt.Errorf("pds: writing the repo signing key: %w", err)
	}
	return nil
}

func (s *FileRepoKeyStore) GetRepoKey(_ context.Context, did syntax.DID) (atcrypto.PrivateKeyExportable, error) {
	data, err := os.ReadFile(s.path(did))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("pds: no repo signing key for %s", did)
		}
		return nil, fmt.Errorf("pds: reading the repo signing key: %w", err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) != 2 || strings.TrimSpace(lines[0]) != did.String() {
		return nil, fmt.Errorf("pds: the repo signing key file for %s is invalid", did)
	}
	key, err := atcrypto.ParsePrivateMultibase(strings.TrimSpace(lines[1]))
	if err != nil {
		return nil, fmt.Errorf("pds: parsing the repo signing key: %w", err)
	}
	return key, nil
}

func (s *FileRepoKeyStore) DeleteRepoKey(_ context.Context, did syntax.DID) error {
	if err := os.Remove(s.path(did)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pds: deleting the repo signing key: %w", err)
	}
	return nil
}

type JSONFileRepoKeyStore struct {
	path string
	mu   sync.Mutex
}

func NewJSONFileRepoKeyStore(path string) (*JSONFileRepoKeyStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("pds: the json repo key store needs a file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("pds: creating the repo key directory: %w", err)
	}
	return &JSONFileRepoKeyStore{path: path}, nil
}

func (s *JSONFileRepoKeyStore) load() (map[string]string, error) {
	keys := map[string]string{}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return keys, nil
		}
		return nil, fmt.Errorf("pds: reading the repo key file: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return keys, nil
	}
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("pds: the repo key file is invalid: %w", err)
	}
	return keys, nil
}

func (s *JSONFileRepoKeyStore) save(keys map[string]string) error {
	data, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("pds: encoding the repo key file: %w", err)
	}
	if err := writePrivateFile(s.path, append(data, '\n')); err != nil {
		return fmt.Errorf("pds: writing the repo key file: %w", err)
	}
	return nil
}

func (s *JSONFileRepoKeyStore) PutRepoKey(_ context.Context, did syntax.DID, key atcrypto.PrivateKeyExportable) error {
	if key == nil {
		return errors.New("pds: the repo signing key is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.load()
	if err != nil {
		return err
	}
	keys[did.String()] = key.Multibase()
	return s.save(keys)
}

func (s *JSONFileRepoKeyStore) GetRepoKey(_ context.Context, did syntax.DID) (atcrypto.PrivateKeyExportable, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.load()
	if err != nil {
		return nil, err
	}
	raw, ok := keys[did.String()]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("pds: no repo signing key for %s", did)
	}
	key, err := atcrypto.ParsePrivateMultibase(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("pds: parsing the repo signing key: %w", err)
	}
	return key, nil
}

func (s *JSONFileRepoKeyStore) DeleteRepoKey(_ context.Context, did syntax.DID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := keys[did.String()]; !ok {
		return nil
	}
	delete(keys, did.String())
	return s.save(keys)
}

func sessionSecretFromData(data map[string]any) ([]byte, error) {
	for _, key := range []string{"sessionSecret", "session_secret", "value", "secret"} {
		raw, ok := data[key]
		if !ok {
			continue
		}
		switch value := raw.(type) {
		case []byte:
			if len(value) > 0 {
				return append([]byte(nil), value...), nil
			}
		case string:
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if decoded, err := hex.DecodeString(value); err == nil && len(decoded) > 0 {
				return decoded, nil
			}
			return []byte(value), nil
		}
	}
	return nil, errors.New("pds: the session secret path holds no session secret")
}

func privateKeyFromData(data map[string]any) (atcrypto.PrivateKey, error) {
	for _, key := range []string{"rotationKey", "rotation_key", "signingKey", "value", "key"} {
		raw, ok := data[key]
		if !ok {
			continue
		}
		var text string
		switch value := raw.(type) {
		case []byte:
			text = string(value)
		case string:
			text = value
		default:
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		key, err := atcrypto.ParsePrivateMultibase(text)
		if err != nil {
			return nil, fmt.Errorf("pds: parsing the private key: %w", err)
		}
		return key, nil
	}
	return nil, errors.New("pds: the path holds no private key")
}

func resolvePrivateKey(stateDir, filename string, generate func() (atcrypto.PrivateKeyExportable, error)) (atcrypto.PrivateKey, error) {
	path := filepath.Join(stateDir, filename)
	data, err := os.ReadFile(path)
	if err == nil {
		key, parseErr := atcrypto.ParsePrivateMultibase(strings.TrimSpace(string(data)))
		if parseErr != nil {
			return nil, fmt.Errorf("pds: parsing the persisted private key %s: %w", filename, parseErr)
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("pds: reading the private key %s: %w", filename, err)
	}
	key, err := generate()
	if err != nil {
		return nil, fmt.Errorf("pds: generating the private key %s: %w", filename, err)
	}
	if err := writePrivateFile(path, []byte(key.Multibase()+"\n")); err != nil {
		return nil, fmt.Errorf("pds: persisting the private key %s: %w", filename, err)
	}
	return key, nil
}

func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
