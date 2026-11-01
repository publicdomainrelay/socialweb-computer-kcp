package policytest

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const DenoEnv = "SOCIALWEB_POLICYTEST_DENO"

var (
	denoOnce sync.Once
	denoPath string
	denoFrom string
	denoErr  error
)

func DenoPath() (string, string, error) {
	denoOnce.Do(func() { denoPath, denoFrom, denoErr = locateDeno() })
	return denoPath, denoFrom, denoErr
}

func locateDeno() (string, string, error) {
	if path := strings.TrimSpace(os.Getenv(DenoEnv)); path != "" {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return "", "", fmt.Errorf("%s=%s is not a deno binary", DenoEnv, path)
		}
		return path, DenoEnv, nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = filepath.Join(os.TempDir(), "policytest-cache")
	}
	cacheDir = filepath.Join(cacheDir, "policy_engine")
	if len(deno.EmbeddedZip()) > 0 {
		if path, err := deno.Resolve(cacheDir); err == nil {
			return path, "the embedded deno (build tag embed_deno)", nil
		}
	}
	if path, from, err := stagedDeno(cacheDir); err == nil {
		return path, from, nil
	}
	if path, err := exec.LookPath("deno"); err == nil {
		return path, "deno on PATH", nil
	}
	return "", "", errors.New("no deno for the policy engine: set " + DenoEnv + ", or run scripts/build-socialweb-computer.sh --dev once " +
		"so the pinned deno zip is staged in ~/.tmp/socialweb-computer-build/cache/deno-embed, or put deno " + deno.Version + " on PATH")
}

func stagedDeno(cacheDir string) (string, string, error) {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	want, err := versions.FetchSHA256("deno", platform)
	if err != nil {
		return "", "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	zipPath := filepath.Join(home, ".tmp", "socialweb-computer-build", "cache", "deno-embed", want+".zip")
	dir := filepath.Join(cacheDir, "deno-embedded", deno.Version)
	path := filepath.Join(dir, "deno")
	from := "the build-staged pinned deno " + zipPath
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path, from, nil
	}
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return "", "", fmt.Errorf("%s does not match its pin", zipPath)
	}
	if err := extractDeno(zipPath, dir); err != nil {
		return "", "", err
	}
	return path, from, nil
}

func extractDeno(zipPath, dir string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, file := range reader.File {
		if filepath.Base(file.Name) != "deno" {
			continue
		}
		in, err := file.Open()
		if err != nil {
			return err
		}
		out, err := os.CreateTemp(dir, "deno.*.tmp")
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		in.Close()
		if err := errors.Join(copyErr, out.Close(), os.Chmod(out.Name(), 0o755)); err != nil {
			os.Remove(out.Name())
			return err
		}
		return os.Rename(out.Name(), filepath.Join(dir, "deno"))
	}
	return fmt.Errorf("%s holds no deno binary", zipPath)
}
