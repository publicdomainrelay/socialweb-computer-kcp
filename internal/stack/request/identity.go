package request

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	_ "modernc.org/sqlite"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	pdssvc "github.com/johnandersen777/socialweb-computer-kcp/internal/services/pds"
)

const (
	IdentityFile = "identity.json"

	IdentitySchema = 1

	ExposureIdentityDir = "exposure-identity"

	IdentityLockFile = "lock"
)

var (
	ErrIdentityInUse = errors.New("request: the identity is in use by another command")

	ErrIdentityPartial = errors.New("request: the identity store is partial")
)

type IdentityInvocation struct {
	Name string `json:"name"`

	StateDir string `json:"stateDir,omitempty"`
}

type Identity struct {
	Schema int `json:"schema"`

	Name string `json:"name"`

	CreatedAt time.Time `json:"createdAt"`

	UpdatedAt time.Time `json:"updatedAt,omitempty"`

	Mode string `json:"mode,omitempty"`

	DID string `json:"did,omitempty"`

	Handle string `json:"handle,omitempty"`

	Association string `json:"association,omitempty"`

	ApprovedAssociations []string `json:"approvedAssociations,omitempty"`

	PDSPublicURL string `json:"pdsPublicURL,omitempty"`

	PLCDirectoryURL string `json:"plcDirectoryUrl,omitempty"`

	PDSStateDir string `json:"pdsStateDir,omitempty"`

	RotationKeyDID string `json:"rotationKeyDID,omitempty"`

	StateRoot string `json:"stateRoot,omitempty"`

	LastInvocation *IdentityInvocation `json:"lastInvocation,omitempty"`
}

type Account struct {
	DID string

	Handle string
}

type IdentityStore struct {
	stateRoot string

	name string

	dir string

	pds string

	exposure string

	lockPath string

	lock *os.File
}

func IdentityRoot(stateRoot string) string {
	return filepath.Join(stateRoot, "identities")
}

func IdentityDir(stateRoot, name string) string {
	return filepath.Join(IdentityRoot(stateRoot), name)
}

func CheckIdentityName(name string) error {
	if err := CheckName(name); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	return nil
}

func OpenIdentity(stateRoot, name string) (*IdentityStore, error) {
	if err := CheckIdentityName(name); err != nil {
		return nil, err
	}
	stateRoot, err := filepath.Abs(strings.TrimSpace(stateRoot))
	if err != nil {
		return nil, fmt.Errorf("request: the state root: %w", err)
	}
	root := IdentityRoot(stateRoot)
	if err := secureDir(root, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, name)
	if err := secureDir(dir, 0o700); err != nil {
		return nil, err
	}
	store := &IdentityStore{stateRoot: stateRoot, name: name, dir: dir, pds: filepath.Join(dir, "pds"),
		exposure: filepath.Join(dir, ExposureIdentityDir), lockPath: filepath.Join(dir, IdentityLockFile)}
	if err := store.lockDir(); err != nil {
		return nil, err
	}
	return store, nil
}

func secureDir(path string, perm os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("request: %s is not a real directory", path)
		}
		return os.Chmod(path, perm)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	return os.Chmod(path, perm)
}

func (s *IdentityStore) lockDir() error {
	info, err := os.Lstat(s.lockPath)
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("request: the identity lock %s is not a regular file", s.lockPath)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(s.lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("request: the identity lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking); err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: %s", ErrIdentityInUse, s.name)
	}
	s.lock = f
	return nil
}

func (s *IdentityStore) Close() {
	if s == nil || s.lock == nil {
		return
	}
	_ = hostos.Unlock(s.lock)
	_ = s.lock.Close()
	s.lock = nil
}

func (s *IdentityStore) Name() string { return s.name }

func (s *IdentityStore) Dir() string { return s.dir }

func (s *IdentityStore) PDSDir() string { return s.pds }

func (s *IdentityStore) Path() string { return filepath.Join(s.dir, IdentityFile) }

func (s *IdentityStore) Load() (*Identity, error) {
	if info, err := os.Lstat(s.Path()); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("request: the identity %s is a symlink", s.Path())
	}
	var id Identity
	err := fsx.ReadJSON(s.Path(), &id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("request: the identity %s: %w", s.name, err)
	}
	if id.Schema != IdentitySchema || id.Name != s.name {
		return nil, fmt.Errorf("request: the identity %s has schema %d or another name", s.name, id.Schema)
	}
	if id.StateRoot != "" && id.StateRoot != s.stateRoot {
		return nil, fmt.Errorf("request: the identity %s belongs to state root %s, not %s", s.name, id.StateRoot, s.stateRoot)
	}
	if id.PDSStateDir != "" && id.PDSStateDir != s.pds {
		return nil, fmt.Errorf("request: the identity %s names the PDS state dir %s, not %s", s.name, id.PDSStateDir, s.pds)
	}
	return &id, nil
}

func (s *IdentityStore) Save(id *Identity) error {
	if id == nil {
		return errors.New("request: a nil identity")
	}
	if id.Schema == 0 {
		id.Schema = IdentitySchema
	}
	id.Name = s.name
	id.StateRoot = s.stateRoot
	id.PDSStateDir = s.pds
	id.UpdatedAt = time.Now().UTC()
	if id.CreatedAt.IsZero() {
		id.CreatedAt = id.UpdatedAt
	}
	if err := fsx.WriteJSON(s.Path(), id, 0o600); err != nil {
		return fmt.Errorf("request: writing the identity %s: %w", s.name, err)
	}
	return nil
}

func (s *IdentityStore) EnsureDirs() error {
	for _, dir := range []string{s.pds, s.exposure} {
		if err := secureDir(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (s *IdentityStore) Validate(did string, wantOAuth bool) (string, error) {
	rotation, err := pdssvc.ValidateStateDir(s.pds, did, wantOAuth)
	if err != nil {
		return "", err
	}
	accounts, err := ReadAccounts(s.pds)
	if err != nil {
		return "", err
	}
	if len(accounts) != 1 {
		return "", fmt.Errorf("request: the identity %s PDS holds %d accounts; one identity serves one account", s.name, len(accounts))
	}
	if accounts[0].DID != did {
		return "", fmt.Errorf("request: the identity %s PDS holds %s, not %s", s.name, accounts[0].DID, did)
	}
	return rotation, nil
}

func (s *IdentityStore) LinkExposure(instanceState string) error {
	if err := secureDir(instanceState, 0o700); err != nil {
		return err
	}
	if err := secureDir(s.exposure, 0o700); err != nil {
		return err
	}
	link := filepath.Join(instanceState, ExposureIdentityDir)
	rel, err := filepath.Rel(instanceState, s.exposure)
	if err != nil {
		return err
	}
	info, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(rel, link); err != nil {
			return fmt.Errorf("request: linking the identity exposure dir: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("request: %s is a real directory, not the identity exposure link", link)
	}
	have, err := os.Readlink(link)
	if err != nil {
		return err
	}
	if filepath.Clean(have) != filepath.Clean(rel) {
		return fmt.Errorf("request: %s points at %s, not the identity exposure dir", link, have)
	}
	return nil
}

func (s *IdentityStore) PDSLockHeld() (bool, error) {
	path := filepath.Join(s.pds, "lock")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking); err != nil {
		return true, nil
	}
	_ = hostos.Unlock(f)
	return false, nil
}

func (s *IdentityStore) ReadRotationKeyDID() (string, error) {
	path := filepath.Join(s.pds, pdssvc.RotationKeyFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key, err := atcrypto.ParsePrivateMultibase(strings.TrimSpace(string(raw)))
	if err != nil {
		return "", err
	}
	public, err := key.PublicKey()
	if err != nil {
		return "", err
	}
	return public.DIDKey(), nil
}

func ReadAccounts(pdsDir string) ([]Account, error) {
	path := filepath.Join(pdsDir, "accounts.db")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("request: %s is not a regular file", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("request: opening the identity accounts: %w", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT did, handle FROM pds_accounts ORDER BY did`)
	if err != nil {
		return nil, fmt.Errorf("request: reading the identity accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var did, handle string
		if err := rows.Scan(&did, &handle); err != nil {
			return nil, err
		}
		out = append(out, Account{DID: did, Handle: handle})
	}
	return out, rows.Err()
}

func InstanceStateDir(stateRoot, name string) string {
	return filepath.Join(Dir(stateRoot, name), "dev", name)
}

func LinkIdentityExposure(stateRoot, name, instanceState string) error {
	if err := CheckIdentityName(name); err != nil {
		return err
	}
	dir := IdentityDir(stateRoot, name)
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("request: the identity %s: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("request: the identity %s is not a real directory", name)
	}
	s := &IdentityStore{stateRoot: stateRoot, name: name, dir: dir,
		pds: filepath.Join(dir, "pds"), exposure: filepath.Join(dir, ExposureIdentityDir)}
	return s.LinkExposure(instanceState)
}
