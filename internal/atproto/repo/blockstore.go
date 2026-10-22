package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	ipld "github.com/ipfs/go-ipld-format"
)

type blockQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func verifyBlock(c cid.Cid, data []byte) error {
	computed, err := c.Prefix().Sum(data)
	if err != nil {
		return fmt.Errorf("hashing block %s: %w", c, err)
	}
	if !computed.Equals(c) {
		return fmt.Errorf("block %s: %w", c, blocks.ErrWrongHash)
	}
	return nil
}

func readBlock(ctx context.Context, q blockQueryer, did string, c cid.Cid) (blocks.Block, error) {
	var data []byte
	err := q.QueryRowContext(ctx, "SELECT data FROM atrepo_blocks WHERE did = ? AND cid = ?", did, c.String()).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ipld.ErrNotFound{Cid: c}
	}
	if err != nil {
		return nil, fmt.Errorf("reading block %s: %w", c, err)
	}
	if err := verifyBlock(c, data); err != nil {
		return nil, err
	}
	return blocks.NewBlockWithCid(data, c)
}

type sqlBlockReader struct {
	db  *sql.DB
	did string
}

func (r *sqlBlockReader) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	return readBlock(ctx, r.db, r.did, c)
}

func (r *sqlBlockReader) HashOnRead(enabled bool) {
}

type txBlockstore struct {
	tx     *sql.Tx
	did    string
	order  []cid.Cid
	blocks map[cid.Cid]blocks.Block
}

func newTxBlockstore(ctx context.Context, tx *sql.Tx, did string) *txBlockstore {
	return &txBlockstore{
		tx:     tx,
		did:    did,
		blocks: make(map[cid.Cid]blocks.Block),
	}
}

func (b *txBlockstore) DeleteBlock(ctx context.Context, c cid.Cid) error {
	return fmt.Errorf("blockstore is append only")
}

func (b *txBlockstore) Has(ctx context.Context, c cid.Cid) (bool, error) {
	var one int
	err := b.tx.QueryRowContext(ctx, "SELECT 1 FROM atrepo_blocks WHERE did = ? AND cid = ?", b.did, c.String()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (b *txBlockstore) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	if blk, ok := b.blocks[c]; ok {
		return blk, nil
	}
	return readBlock(ctx, b.tx, b.did, c)
}

func (b *txBlockstore) GetSize(ctx context.Context, c cid.Cid) (int, error) {
	var size int
	err := b.tx.QueryRowContext(ctx, "SELECT length(data) FROM atrepo_blocks WHERE did = ? AND cid = ?", b.did, c.String()).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ipld.ErrNotFound{Cid: c}
	}
	return size, err
}

func (b *txBlockstore) Put(ctx context.Context, blk blocks.Block) error {
	c := blk.Cid()
	if _, ok := b.blocks[c]; !ok {
		b.order = append(b.order, c)
	}
	b.blocks[c] = blk
	_, err := b.tx.ExecContext(ctx, "INSERT OR IGNORE INTO atrepo_blocks (did, cid, data) VALUES (?, ?, ?)", b.did, c.String(), blk.RawData())
	if err != nil {
		return fmt.Errorf("writing block %s: %w", c, err)
	}
	return nil
}

func (b *txBlockstore) PutMany(ctx context.Context, blks []blocks.Block) error {
	for _, blk := range blks {
		if err := b.Put(ctx, blk); err != nil {
			return err
		}
	}
	return nil
}

func (b *txBlockstore) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	rows, err := b.tx.QueryContext(ctx, "SELECT cid FROM atrepo_blocks WHERE did = ?", b.did)
	if err != nil {
		return nil, err
	}
	out := make(chan cid.Cid)
	go func() {
		defer close(out)
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return
			}
			c, err := cid.Parse(raw)
			if err != nil {
				return
			}
			select {
			case out <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (b *txBlockstore) HashOnRead(enabled bool) {
}

func (b *txBlockstore) orderedFrom(first cid.Cid) []blocks.Block {
	out := make([]blocks.Block, 0, len(b.blocks))
	seen := make(map[cid.Cid]bool, len(b.blocks))
	if blk, ok := b.blocks[first]; ok {
		out = append(out, blk)
		seen[first] = true
	}
	for _, c := range b.order {
		if seen[c] {
			continue
		}
		blk, ok := b.blocks[c]
		if !ok {
			continue
		}
		seen[c] = true
		out = append(out, blk)
	}
	return out
}

var _ blockstore.Blockstore = (*txBlockstore)(nil)
