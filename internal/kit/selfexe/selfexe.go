package selfexe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
)

const Path = "/proc/self/exe"

type Pin struct {
	Sha256 string

	Size int64
}

var (
	mu sync.Mutex

	cached *Pin
)

func Sum() (Pin, error) {
	mu.Lock()
	defer mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	f, err := os.Open(Path)
	if err != nil {
		return Pin{}, fmt.Errorf("selfexe: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Pin{}, fmt.Errorf("selfexe: hashing %s: %w", Path, err)
	}
	cached = &Pin{Sha256: hex.EncodeToString(h.Sum(nil)), Size: n}
	return *cached, nil
}
