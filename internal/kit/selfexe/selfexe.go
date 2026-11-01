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

var pin = sync.OnceValues(func() (Pin, error) {
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
	return Pin{Sha256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
})

func Sum() (Pin, error) {
	return pin()
}
