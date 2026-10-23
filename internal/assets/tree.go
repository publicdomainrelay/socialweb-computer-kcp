package assets

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
)

func TreeDigest(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			fmt.Fprintf(h, "d %s\n", rel)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "l %s %s\n", filepath.ToSlash(target), rel)
		case info.Mode().IsRegular():
			sum, err := sha256sum.File(p)
			if err != nil {
				return err
			}
			x := "-"
			if info.Mode().Perm()&0o111 != 0 {
				x = "x"
			}
			fmt.Fprintf(h, "f %s %s %s\n", x, sum, rel)
		default:
			return fmt.Errorf("assets: %s is neither a file, a directory nor a symlink", p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func WriteTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: rel, Format: tar.FormatPAX}
		switch {
		case info.IsDir():
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, rel+"/", 0o755
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := safeLink(rel, target); err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, filepath.ToSlash(target), 0o777
		case info.Mode().IsRegular():
			hdr.Typeflag, hdr.Size, hdr.Mode = tar.TypeReg, info.Size(), 0o444
			if info.Mode().Perm()&0o111 != 0 {
				hdr.Mode = 0o555
			}
		default:
			return fmt.Errorf("assets: %s is neither a file, a directory nor a symlink", p)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func safeRel(name string) (string, error) {
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("assets: tar entry %q escapes the extraction root", name)
	}
	return clean, nil
}

func safeLink(rel, target string) error {
	if path.IsAbs(target) {
		return fmt.Errorf("assets: symlink %s -> %s is absolute", rel, target)
	}
	joined := path.Clean(path.Join(path.Dir(rel), target))
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return fmt.Errorf("assets: symlink %s -> %s escapes the extraction root", rel, target)
	}
	return nil
}

func ExtractTar(r io.Reader, dir string) error { return extractTar(r, dir) }

func RemoveTree(dir string) error { return removeTree(dir) }

func extractTar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return syncTree(dir)
		}
		if err != nil {
			return err
		}
		rel, err := safeRel(hdr.Name)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := safeLink(rel, hdr.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(filepath.FromSlash(hdr.Linkname), dst); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o444)
			if hdr.Mode&0o111 != 0 {
				mode = 0o555
			}
			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(f, tr)
			if err := errors.Join(cerr, f.Close()); err != nil {
				return err
			}
			if err := os.Chmod(dst, mode); err != nil {
				return err
			}
		default:
			return fmt.Errorf("assets: tar entry %q has type %c; only files, directories and symlinks are assets", hdr.Name, hdr.Typeflag)
		}
	}
}

func removeTree(dir string) error {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
