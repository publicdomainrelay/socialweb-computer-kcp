package request

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
)

const OwnerLockFile = "owner.lock"

type Owner struct {
	f *os.File
}

func openOwnerLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, OwnerLockFile)
	info, err := os.Lstat(path)
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, fmt.Errorf("request: the owner lock %s is not a regular file", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("request: the owner lock: %w", err)
	}
	return f, nil
}

func TakeOwner(stateRoot, name string) (*Owner, bool, error) {
	return TakeOwnerDir(Dir(stateRoot, name))
}

func TakeOwnerDir(dir string) (*Owner, bool, error) {
	f, err := openOwnerLock(dir)
	if err != nil {
		return nil, false, err
	}
	if err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking); err != nil {
		_ = f.Close()
		return nil, false, nil
	}
	return &Owner{f: f}, true, nil
}

func AwaitOwnerGone(ctx context.Context, stateRoot, name string) error {
	f, err := openOwnerLock(Dir(stateRoot, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- hostos.Lock(f, hostos.LockExclusive)
	}()
	select {
	case err := <-done:
		_ = hostos.Unlock(f)
		_ = f.Close()
		return err
	case <-ctx.Done():
		go func() {
			<-done
			_ = f.Close()
		}()
		return ctx.Err()
	}
}

func (o *Owner) Close() {
	if o == nil || o.f == nil {
		return
	}
	_ = hostos.Unlock(o.f)
	_ = o.f.Close()
	o.f = nil
}
