package pds

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
)

type FileBlobStore struct {
	dir string
}

func NewFileBlobStore(dir string) (*FileBlobStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("pds: the file blob store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pds: creating the blob directory: %w", err)
	}
	return &FileBlobStore{dir: dir}, nil
}

func (s *FileBlobStore) PutBlob(_ context.Context, _ syntax.DID, mimeType string, data []byte) (*atpds.BlobRef, error) {
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if strings.ContainsAny(mimeType, "\r\n") {
		return nil, errors.New("pds: the blob mime type has a newline")
	}
	sum := sha256.Sum256(data)
	mh, err := multihash.Encode(sum[:], multihash.SHA2_256)
	if err != nil {
		return nil, fmt.Errorf("pds: encoding the blob hash: %w", err)
	}
	c := cid.NewCidV1(cid.Raw, mh)
	payload := make([]byte, 0, len(mimeType)+1+len(data))
	payload = append(payload, mimeType...)
	payload = append(payload, '\n')
	payload = append(payload, data...)
	if err := writePrivateFile(s.path(c), payload); err != nil {
		return nil, fmt.Errorf("pds: writing the blob: %w", err)
	}
	return &atpds.BlobRef{CID: c, MimeType: mimeType, Size: int64(len(data))}, nil
}

func (s *FileBlobStore) GetBlob(_ context.Context, _ syntax.DID, c cid.Cid) ([]byte, string, error) {
	payload, err := os.ReadFile(s.path(c))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", atpds.ErrBlobNotFound
		}
		return nil, "", fmt.Errorf("pds: reading the blob: %w", err)
	}
	before, after, ok := bytes.Cut(payload, []byte{'\n'})
	if !ok {
		return nil, "", fmt.Errorf("pds: the blob file is invalid")
	}
	mimeType := string(before)
	data := after
	sum := sha256.Sum256(data)
	mh, err := multihash.Encode(sum[:], multihash.SHA2_256)
	if err != nil {
		return nil, "", fmt.Errorf("pds: encoding the blob hash: %w", err)
	}
	if !cid.NewCidV1(cid.Raw, mh).Equals(c) {
		return nil, "", fmt.Errorf("pds: the blob content does not match its cid")
	}
	return data, mimeType, nil
}

func (s *FileBlobStore) path(c cid.Cid) string {
	return filepath.Join(s.dir, c.String()+".blob")
}
