package deno

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const Version = "v2.9.3"

func Resolve(cacheDir string) (string, error) {
	return resolve(cacheDir)
}

func resolve(cacheDir string) (string, error) {
	if data := EmbeddedZip(); len(data) > 0 {
		if path, extractErr := extractEmbedded(cacheDir, data); extractErr == nil {
			return path, nil
		}
	}
	if path, lookupErr := exec.LookPath("deno"); lookupErr == nil {
		return path, nil
	}
	return "", errors.New("embedded deno artifact is absent and deno is not in PATH; runtime downloads are disabled")
}

func extractEmbedded(cacheDir string, data []byte) (string, error) {
	if cacheDir == "" {
		cacheDir = defaultCacheDir()
	}
	dir := filepath.Join(cacheDir, "deno-embedded", Version)
	binaryName := "deno"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	path := filepath.Join(dir, binaryName)
	if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
		return path, nil
	}
	if mkdirErr := os.MkdirAll(dir, 0755); mkdirErr != nil {
		return "", mkdirErr
	}
	reader, zipErr := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if zipErr != nil {
		return "", zipErr
	}
	for _, file := range reader.File {
		name := filepath.Base(file.Name)
		if !strings.HasPrefix(name, "deno") {
			continue
		}
		rc, openErr := file.Open()
		if openErr != nil {
			return "", openErr
		}
		out, createErr := os.CreateTemp(dir, name+".*.tmp")
		if createErr != nil {
			rc.Close()
			return "", createErr
		}
		_, copyErr := io.Copy(out, rc)
		closeErr := out.Close()
		rc.Close()
		if err := errors.Join(copyErr, closeErr, os.Chmod(out.Name(), 0755)); err != nil {
			os.Remove(out.Name())
			return "", err
		}
		if err := os.Rename(out.Name(), filepath.Join(dir, name)); err != nil {
			os.Remove(out.Name())
			return "", err
		}
	}
	os.Chmod(path, 0755)
	if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
		return "", fmt.Errorf("deno binary not found after extraction: %s", path)
	}
	return path, nil
}

func defaultCacheDir() string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "policy_engine")
	}
	if dir, cacheErr := os.UserCacheDir(); cacheErr == nil {
		return filepath.Join(dir, "policy_engine")
	}
	return filepath.Join(os.TempDir(), "policy_engine_cache")
}
