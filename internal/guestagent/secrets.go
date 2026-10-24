package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fsnotify/fsnotify"
)

const (
	maxSecretsCiphertext = 65536

	maxSecretEntries = 256

	defaultSecretMode = 0o600
)

type secretEntry struct {
	Path string `json:"path"`

	Value string `json:"value"`

	Mode string `json:"mode,omitempty"`
}

type secretsRequestedRecord struct {
	Type string `json:"$type"`

	CreatedAt string `json:"createdAt"`
}

type secretsAppliedRecord struct {
	Type string `json:"$type"`

	Via string `json:"via"`

	Count int `json:"count"`

	CreatedAt string `json:"createdAt"`
}

func (a *Agent) readyPath() string { return filepath.Join(a.cfg.StateDir, SecretsReadyFile) }

func (a *Agent) requestPath() string { return filepath.Join(a.cfg.StateDir, SecretsRequestFile) }

func (a *Agent) runSecrets(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		a.log.Error("the secrets watcher cannot start", "err", err.Error())
		return
	}
	defer watcher.Close()
	if err := watcher.Add(a.cfg.StateDir); err != nil {
		a.log.Error("the secrets watcher cannot watch the state dir", "dir", a.cfg.StateDir, "err", err.Error())
		return
	}
	key := publicKeyRecord{Type: PublicKeyNSID, Kind: publicKeyKindAgeX25519, Tag: secretsTag, Key: a.identity.Recipient().String(), CreatedAt: createdAt()}
	a.wg.Go(func() { _ = a.poster.Post(ctx, key.Type, secretsTag, key) })
	a.log.Info("the sealed secrets fallback is enabled", "getSecrets", a.cfg.Secrets.GetSecrets)
	switch via, count, ok := a.readReady(); {
	case ok && via == secretsViaGetSecrets:
		a.postApplied(ctx, via, count)
	case ok:
	case exists(a.requestPath()) || !a.cfg.Secrets.GetSecrets:
		a.postRequest(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err, open := <-watcher.Errors:
			if !open {
				return
			}
			a.log.Warn("the secrets watcher reported an error", "err", err.Error())
		case ev, open := <-watcher.Events:
			if !open {
				return
			}
			if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
				continue
			}
			switch filepath.Base(ev.Name) {
			case SecretsRequestFile:
				a.postRequest(ctx)
			case SecretsReadyFile:
				if via, count, ok := a.readReady(); ok && via == secretsViaGetSecrets {
					a.postApplied(ctx, via, count)
				}
			}
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (a *Agent) readReady() (string, int, bool) {
	raw, err := os.ReadFile(a.readyPath())
	if err != nil {
		return "", 0, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "", 0, false
	}
	count := 0
	if len(fields) > 1 {
		count, _ = strconv.Atoi(fields[1])
	}
	return fields[0], max(count, 0), true
}

func (a *Agent) postRequest(ctx context.Context) {
	a.mu.Lock()
	if a.requested {
		a.mu.Unlock()
		return
	}
	a.requested = true
	a.mu.Unlock()
	record := secretsRequestedRecord{Type: SecretsRequestedNSID, CreatedAt: createdAt()}
	a.log.Info("the guest asks the requester for its sealed secrets bundle")
	a.wg.Go(func() {
		if err := a.poster.Post(ctx, record.Type, "", record); err != nil {
			a.mu.Lock()
			a.requested = false
			a.mu.Unlock()
		}
	})
}

func (a *Agent) postApplied(ctx context.Context, via string, count int) {
	a.mu.Lock()
	if a.applied[via] {
		a.mu.Unlock()
		return
	}
	a.applied[via] = true
	a.mu.Unlock()
	record := secretsAppliedRecord{Type: SecretsAppliedNSID, Via: via, Count: count, CreatedAt: createdAt()}
	a.wg.Go(func() { _ = a.poster.Post(ctx, record.Type, "", record) })
}

func (a *Agent) sealedSecrets(ctx context.Context, seq int64, ciphertext string) {
	if a.cfg.Secrets == nil {
		a.log.Debug("a secrets event for a guest without the sealed fallback", "seq", seq)
		return
	}
	if ciphertext == "" || len(ciphertext) > maxSecretsCiphertext {
		a.log.Debug("a secrets ciphertext is empty or too large", "seq", seq, "bytes", len(ciphertext))
		return
	}
	plaintext, err := open(a.identity, ciphertext)
	if err != nil {
		a.log.Debug("a secrets event does not open with this guest's identity", "seq", seq)
		return
	}
	var entries []secretEntry
	if err := json.Unmarshal(plaintext, &entries); err != nil {
		a.log.Warn("a secrets bundle opened but is not a JSON array of {path, value, mode}", "seq", seq, "bytes", len(plaintext))
		return
	}
	clear(plaintext)
	if err := writeSecrets(entries); err != nil {
		a.log.Error("writing the sealed secrets bundle failed", "seq", seq, "err", err.Error())
		return
	}
	if err := writeReady(a.readyPath(), secretsViaSealed, len(entries)); err != nil {
		a.log.Error("marking the secrets ready failed", "err", err.Error())
		return
	}
	a.log.Info("the sealed secrets bundle is written", "seq", seq, "files", len(entries))
	a.mu.Lock()
	a.applied[secretsViaSealed] = false
	a.mu.Unlock()
	a.postApplied(ctx, secretsViaSealed, len(entries))
}

func secretMode(raw string) (os.FileMode, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultSecretMode, nil
	}
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 8, 32)
	if err != nil || value&^defaultSecretMode != 0 {
		return 0, fmt.Errorf("mode %q is not an octal mode of at most 0600", raw)
	}
	return os.FileMode(value), nil
}

func checkSecretPath(path string) error {
	switch {
	case path == "" || !filepath.IsAbs(path):
		return fmt.Errorf("path %q is not absolute", path)
	case filepath.Clean(path) != path || strings.Contains(path, "/../") || strings.HasSuffix(path, "/.."):
		return fmt.Errorf("path %q is not a clean path", path)
	case path == "/" || strings.ContainsRune(path, 0):
		return fmt.Errorf("path %q names no file", path)
	}
	return nil
}

func writeSecrets(entries []secretEntry) error {
	if len(entries) > maxSecretEntries {
		return fmt.Errorf("the bundle names %d files, more than %d", len(entries), maxSecretEntries)
	}
	modes := make([]os.FileMode, len(entries))
	for i, entry := range entries {
		if err := checkSecretPath(entry.Path); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		mode, err := secretMode(entry.Mode)
		if err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		modes[i] = mode
	}
	for i, entry := range entries {
		if err := writeSecretFile(entry.Path, []byte(entry.Value), modes[i]); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
	}
	return nil
}

func writeSecretFile(path string, value []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s exists and is not a regular file", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chown(0, 0); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(value); err != nil {
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	return nil
}

func writeReady(path, via string, count int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	part := path + ".part"
	if err := os.WriteFile(part, []byte(via+" "+strconv.Itoa(count)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(part, path)
}

func WaitSecrets(ctx context.Context, stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	if err := watcher.Add(stateDir); err != nil {
		return err
	}
	ready := filepath.Join(stateDir, SecretsReadyFile)
	if exists(ready) {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, open := <-watcher.Errors:
			if !open {
				return errors.New("guestagent: the secrets watcher closed")
			}
			return err
		case ev, open := <-watcher.Events:
			if !open {
				return errors.New("guestagent: the secrets watcher closed")
			}
			if ev.Name == ready && exists(ready) {
				return nil
			}
		}
	}
}
