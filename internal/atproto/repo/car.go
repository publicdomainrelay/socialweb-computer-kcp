package repo

import (
	"bytes"
	"context"
	"fmt"
	"io"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	car "github.com/ipld/go-car"
	carutil "github.com/ipld/go-car/util"
)

func ReadCAR(ctx context.Context, r io.Reader) (cid.Cid, []blocks.Block, error) {
	cr, err := car.NewCarReader(r)
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("reading car header: %w", err)
	}
	if cr.Header.Version != 1 {
		return cid.Undef, nil, fmt.Errorf("unsupported car version: %d", cr.Header.Version)
	}
	if len(cr.Header.Roots) == 0 {
		return cid.Undef, nil, fmt.Errorf("car has no roots")
	}
	out := make([]blocks.Block, 0, 16)
	for {
		blk, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return cid.Undef, nil, fmt.Errorf("reading car block: %w", err)
		}
		out = append(out, blk)
	}
	return cr.Header.Roots[0], out, nil
}

func ReadCARBytes(ctx context.Context, data []byte) (cid.Cid, []blocks.Block, error) {
	return ReadCAR(ctx, bytes.NewReader(data))
}

func VerifyCAR(data []byte) (cid.Cid, error) {
	root, _, err := ReadCARBytes(context.Background(), data)
	return root, err
}

func WriteCAR(root cid.Cid, blks []blocks.Block) ([]byte, error) {
	if !root.Defined() {
		return nil, fmt.Errorf("car root is not defined")
	}
	buf := new(bytes.Buffer)
	header := &car.CarHeader{
		Roots:   []cid.Cid{root},
		Version: 1,
	}
	hb, err := cbor.DumpObject(header)
	if err != nil {
		return nil, fmt.Errorf("encoding car header: %w", err)
	}
	if err := carutil.LdWrite(buf, hb); err != nil {
		return nil, fmt.Errorf("writing car header: %w", err)
	}
	seen := make(map[cid.Cid]bool, len(blks))
	for _, blk := range blks {
		c := blk.Cid()
		if seen[c] {
			continue
		}
		seen[c] = true
		if err := carutil.LdWrite(buf, c.Bytes(), blk.RawData()); err != nil {
			return nil, fmt.Errorf("writing car block %s: %w", c, err)
		}
	}
	return buf.Bytes(), nil
}

func CARBlocks(data []byte) (cid.Cid, []blocks.Block, error) {
	return ReadCARBytes(context.Background(), data)
}
