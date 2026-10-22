package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/atdata"
	indigorepo "github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/syntax"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
)

type repoHandle struct {
	store *SQLiteStore
	did   syntax.DID
	key   atcrypto.PrivateKey
	pub   atcrypto.PublicKey
}

func (h *repoHandle) DID() syntax.DID {
	return h.did
}

func (h *repoHandle) Head(ctx context.Context) (*CommitRecord, error) {
	return h.store.Head(ctx, h.did)
}

func (h *repoHandle) GetRecord(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey) (*Record, error) {
	return h.store.GetRecord(ctx, h.did, collection, rkey)
}

func (h *repoHandle) ListRecords(ctx context.Context, collection syntax.NSID, cursor syntax.RecordKey, limit int) ([]Record, *syntax.RecordKey, error) {
	return h.store.ListRecords(ctx, h.did, collection, cursor, limit)
}

func (h *repoHandle) GetCommit(ctx context.Context, rev syntax.TID) (*CommitRecord, error) {
	return h.store.GetCommit(ctx, h.did, rev)
}

func (h *repoHandle) Commits(ctx context.Context, since *syntax.TID, limit int) ([]CommitRecord, error) {
	return h.store.ListCommits(ctx, h.did, since, limit)
}

func (h *repoHandle) CAR(ctx context.Context, since *syntax.TID) ([]byte, error) {
	return h.store.CAR(ctx, h.did, since)
}

func (h *repoHandle) CommitCAR(ctx context.Context, commit cid.Cid) ([]byte, error) {
	return h.store.CommitCAR(ctx, h.did, commit)
}

func (h *repoHandle) ApplyWrites(ctx context.Context, writes []WriteOp, swapCommit *cid.Cid) (*CommitEvent, error) {
	ev, _, err := h.apply(ctx, writes, swapCommit)
	return ev, err
}

func (h *repoHandle) CreateRecord(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey, record map[string]any) (*Record, *CommitEvent, error) {
	ev, recs, err := h.apply(ctx, []WriteOp{{
		Action:     ActionCreate,
		Collection: collection,
		Rkey:       rkey,
		Record:     record,
	}}, nil)
	if err != nil {
		return nil, ev, err
	}
	if len(recs) == 0 {
		return nil, ev, fmt.Errorf("%w: create produced no record", ErrInvalidRequest)
	}
	return &recs[0], ev, nil
}

func (h *repoHandle) PutRecord(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey, record map[string]any, swapRecord *cid.Cid) (*Record, *CommitEvent, error) {
	ev, recs, err := h.apply(ctx, []WriteOp{{
		Action:     actionPut,
		Collection: collection,
		Rkey:       rkey,
		Record:     record,
		Prev:       swapRecord,
	}}, nil)
	if err != nil {
		return nil, ev, err
	}
	if len(recs) == 0 {
		return nil, ev, fmt.Errorf("%w: put produced no record", ErrInvalidRequest)
	}
	return &recs[0], ev, nil
}

func (h *repoHandle) DeleteRecord(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey, swapRecord *cid.Cid) (*CommitEvent, error) {
	ev, _, err := h.apply(ctx, []WriteOp{{
		Action:     ActionDelete,
		Collection: collection,
		Rkey:       rkey,
		Prev:       swapRecord,
	}}, nil)
	return ev, err
}

func (h *repoHandle) Walk(ctx context.Context, prefix string, fn func(path string, c cid.Cid) error) error {
	head, err := h.store.Head(ctx, h.did)
	if err != nil {
		return err
	}
	br := &sqlBlockReader{db: h.store.db, did: h.did.String()}
	tree, err := loadFullTree(ctx, br, head.Data)
	if err != nil {
		return err
	}
	return tree.Walk(func(key []byte, val cid.Cid) error {
		path := string(key)
		if prefix != "" && !hasPrefix(path, prefix) {
			return nil
		}
		return fn(path, val)
	})
}

func (h *repoHandle) Diff(ctx context.Context, since syntax.TID) ([]CommitOp, error) {
	old, err := h.store.GetCommit(ctx, h.did, since)
	if err != nil {
		return nil, err
	}
	br := &sqlBlockReader{db: h.store.db, did: h.did.String()}
	oldTree, err := loadFullTree(ctx, br, old.Data)
	if err != nil {
		return nil, err
	}
	head, err := h.store.Head(ctx, h.did)
	if err != nil {
		return nil, err
	}
	newTree, err := loadFullTree(ctx, br, head.Data)
	if err != nil {
		return nil, err
	}
	return DiffTrees(oldTree, newTree)
}

func (h *repoHandle) Proof(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey) ([]byte, error) {
	nsid, err := syntax.ParseNSID(string(collection))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	rk, err := syntax.ParseRecordKey(string(rkey))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	head, err := h.store.Head(ctx, h.did)
	if err != nil {
		return nil, err
	}
	br := &sqlBlockReader{db: h.store.db, did: h.did.String()}
	tree, err := loadFullTree(ctx, br, head.Data)
	if err != nil {
		return nil, err
	}
	rc, err := tree.Get([]byte(nsid.String() + "/" + rk.String()))
	if err != nil {
		return nil, err
	}
	if rc == nil {
		return nil, ErrRecordNotFound
	}
	nodeCIDs := make(map[cid.Cid]bool)
	collectNodeCIDs(tree.Root, nodeCIDs)
	cids := make([]cid.Cid, 0, len(nodeCIDs)+2)
	cids = append(cids, head.CID)
	for c := range nodeCIDs {
		if c != head.CID {
			cids = append(cids, c)
		}
	}
	cids = append(cids, *rc)
	sort.Slice(cids, func(i, j int) bool { return cids[i].String() < cids[j].String() })
	headBlk, err := readBlock(ctx, h.store.db, h.did.String(), head.CID)
	if err != nil {
		return nil, err
	}
	ordered := []blocks.Block{headBlk}
	seen := map[cid.Cid]bool{head.CID: true}
	for _, c := range cids {
		if seen[c] {
			continue
		}
		blk, err := readBlock(ctx, h.store.db, h.did.String(), c)
		if err != nil {
			return nil, err
		}
		seen[c] = true
		ordered = append(ordered, blk)
	}
	return WriteCAR(head.CID, ordered)
}

func (h *repoHandle) Verify(ctx context.Context) error {
	return h.store.verifyRepo(ctx, h.did, h.pub)
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func (h *repoHandle) apply(ctx context.Context, writes []WriteOp, swapCommit *cid.Cid) (*CommitEvent, []Record, error) {
	if h.key == nil {
		return nil, nil, ErrReadOnly
	}
	if len(writes) > MaxWrites {
		return nil, nil, fmt.Errorf("%w: too many writes: %d", ErrInvalidRequest, len(writes))
	}
	for _, w := range writes {
		if _, _, err := validateWriteOp(w); err != nil {
			return nil, nil, err
		}
	}
	s := h.store
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("repo: beginning write: %w", err)
	}
	defer tx.Rollback()
	row, err := loadRepoRowTx(ctx, tx, h.did)
	if err != nil {
		return nil, nil, err
	}
	if swapCommit != nil && *swapCommit != row.commit {
		return nil, nil, ErrInvalidSwap
	}
	bs := newTxBlockstore(ctx, tx, h.did.String())
	tree, err := loadFullTree(ctx, bs, row.root)
	if err != nil {
		return nil, nil, err
	}
	ops := make([]CommitOp, 0, len(writes))
	records := make([]Record, 0, len(writes))
	blobs := make([]cid.Cid, 0)
	blobSeen := make(map[cid.Cid]bool)
	seen := make(map[string]bool, len(writes))
	for _, w := range writes {
		nsid, rk, err := validateWriteOp(w)
		if err != nil {
			return nil, nil, err
		}
		action := w.Action
		if action == ActionCreate && rk == "" {
			for i := 0; i < 100; i++ {
				rk = syntax.RecordKey(s.nextTID().String())
				path := nsid.String() + "/" + rk.String()
				existing, err := tree.Get([]byte(path))
				if err != nil {
					return nil, nil, err
				}
				if existing == nil {
					break
				}
				if i == 99 {
					return nil, nil, fmt.Errorf("%w: could not allocate rkey", ErrInvalidRequest)
				}
			}
		}
		path := nsid.String() + "/" + rk.String()
		if seen[path] {
			return nil, nil, fmt.Errorf("%w: duplicate write path %s", ErrInvalidRequest, path)
		}
		seen[path] = true
		existing, err := tree.Get([]byte(path))
		if err != nil {
			return nil, nil, err
		}
		if action == actionPut {
			if existing != nil {
				action = ActionUpdate
			} else {
				action = ActionCreate
			}
		}
		switch action {
		case ActionCreate:
			if w.Prev != nil {
				return nil, nil, fmt.Errorf("%w: create has prev cid", ErrInvalidSwap)
			}
			if existing != nil {
				return nil, nil, fmt.Errorf("%w: %s", ErrRecordExists, path)
			}
			data, c, value, err := prepareRecord(nsid, w.Record)
			if err != nil {
				return nil, nil, err
			}
			blk, err := blocks.NewBlockWithCid(data, c)
			if err != nil {
				return nil, nil, fmt.Errorf("repo: building record block: %w", err)
			}
			if err := bs.Put(ctx, blk); err != nil {
				return nil, nil, err
			}
			prev, err := tree.Insert([]byte(path), c)
			if err != nil {
				return nil, nil, fmt.Errorf("repo: inserting mst key: %w", err)
			}
			if prev != nil {
				return nil, nil, fmt.Errorf("%w: create found existing mst value", ErrCorruptRepo)
			}
			opCID := c
			ops = append(ops, CommitOp{Action: ActionCreate, Path: path, CID: &opCID})
			records = append(records, Record{Collection: nsid, Rkey: rk, CID: c, Bytes: data, Value: value})
			addBlobs(&blobs, blobSeen, value)
		case ActionUpdate:
			if existing == nil {
				return nil, nil, fmt.Errorf("%w: %s", ErrRecordNotFound, path)
			}
			if w.Prev != nil && *w.Prev != *existing {
				return nil, nil, ErrInvalidSwap
			}
			data, c, value, err := prepareRecord(nsid, w.Record)
			if err != nil {
				return nil, nil, err
			}
			if c == *existing {
				records = append(records, Record{Collection: nsid, Rkey: rk, CID: c, Bytes: data, Value: value})
				continue
			}
			blk, err := blocks.NewBlockWithCid(data, c)
			if err != nil {
				return nil, nil, fmt.Errorf("repo: building record block: %w", err)
			}
			if err := bs.Put(ctx, blk); err != nil {
				return nil, nil, err
			}
			prevCID := *existing
			prev, err := tree.Insert([]byte(path), c)
			if err != nil {
				return nil, nil, fmt.Errorf("repo: updating mst key: %w", err)
			}
			if prev == nil || *prev != prevCID {
				return nil, nil, fmt.Errorf("%w: update mst mismatch", ErrCorruptRepo)
			}
			opCID := c
			opPrev := prevCID
			ops = append(ops, CommitOp{Action: ActionUpdate, Path: path, CID: &opCID, Prev: &opPrev})
			records = append(records, Record{Collection: nsid, Rkey: rk, CID: c, Bytes: data, Value: value})
			addBlobs(&blobs, blobSeen, value)
		case ActionDelete:
			if existing == nil {
				return nil, nil, fmt.Errorf("%w: %s", ErrRecordNotFound, path)
			}
			if w.Prev != nil && *w.Prev != *existing {
				return nil, nil, ErrInvalidSwap
			}
			prev, err := tree.Remove([]byte(path))
			if err != nil {
				return nil, nil, fmt.Errorf("repo: removing mst key: %w", err)
			}
			if prev == nil {
				return nil, nil, fmt.Errorf("%w: delete found no mst value", ErrCorruptRepo)
			}
			opPrev := *prev
			ops = append(ops, CommitOp{Action: ActionDelete, Path: path, Prev: &opPrev})
		default:
			return nil, nil, fmt.Errorf("%w: unknown write action %q", ErrInvalidRequest, w.Action)
		}
	}
	newRoot, err := tree.WriteDiffBlocks(ctx, bs)
	if err != nil {
		return nil, nil, fmt.Errorf("repo: persisting mst: %w", err)
	}
	since := row.rev
	clock := syntax.ClockFromTID(since)
	rev := clock.Next()
	prevCommit := row.commit
	commit := &Commit{
		DID:     h.did.String(),
		Version: indigorepo.ATPROTO_REPO_VERSION,
		Prev:    &prevCommit,
		Data:    *newRoot,
		Rev:     rev.String(),
	}
	if err := commit.Sign(h.key); err != nil {
		return nil, nil, fmt.Errorf("repo: signing commit: %w", err)
	}
	cdata, ccid, err := marshalCommit(commit)
	if err != nil {
		return nil, nil, fmt.Errorf("repo: encoding commit: %w", err)
	}
	cblk, err := blocks.NewBlockWithCid(cdata, ccid)
	if err != nil {
		return nil, nil, fmt.Errorf("repo: building commit block: %w", err)
	}
	if err := bs.Put(ctx, cblk); err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	carBytes, err := WriteCAR(ccid, bs.orderedFrom(ccid))
	if err != nil {
		return nil, nil, err
	}
	cr := &CommitRecord{
		DID:    h.did,
		CID:    ccid,
		Rev:    rev,
		Data:   *newRoot,
		Prev:   &prevCommit,
		Signed: commit,
		Time:   now,
	}
	if err := insertCommitRow(ctx, tx, h.did.String(), cr); err != nil {
		return nil, nil, err
	}
	prevData := row.root
	ev := &CommitEvent{
		Repo:     h.did,
		Commit:   ccid,
		Rev:      rev,
		Since:    &since,
		Data:     *newRoot,
		PrevData: &prevData,
		Blocks:   carBytes,
		Ops:      ops,
		Blobs:    blobs,
		Time:     now,
	}
	seq, err := insertEventRow(ctx, tx, ev)
	if err != nil {
		return nil, nil, err
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE atrepo_repos SET root = ?, commit_cid = ?, rev = ? WHERE did = ?",
		newRoot.String(), ccid.String(), rev.String(), h.did.String())
	if err != nil {
		return nil, nil, fmt.Errorf("repo: updating head: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, nil, err
	}
	if n != 1 {
		return nil, nil, fmt.Errorf("%w: head update affected %d rows", ErrCorruptRepo, n)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("repo: committing write: %w", err)
	}
	ev.Seq = seq
	if sink := s.sink; sink != nil {
		if err := sink.OnCommit(ctx, ev); err != nil {
			return ev, records, err
		}
	}
	return ev, records, nil
}

func addBlobs(blobs *[]cid.Cid, seen map[cid.Cid]bool, value map[string]any) {
	for _, blob := range atdata.ExtractBlobs(value) {
		ref := cid.Cid(blob.Ref)
		if !ref.Defined() || seen[ref] {
			continue
		}
		seen[ref] = true
		*blobs = append(*blobs, ref)
	}
}

func validateWriteOp(w WriteOp) (syntax.NSID, syntax.RecordKey, error) {
	switch w.Action {
	case ActionCreate, ActionUpdate, ActionDelete, actionPut:
	default:
		return "", "", fmt.Errorf("%w: unknown write action %q", ErrInvalidRequest, w.Action)
	}
	nsid, err := syntax.ParseNSID(string(w.Collection))
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	rk := w.Rkey
	if rk == "" {
		if w.Action != ActionCreate && w.Action != actionPut {
			return "", "", fmt.Errorf("%w: rkey required", ErrInvalidRequest)
		}
	} else {
		rk, err = syntax.ParseRecordKey(string(rk))
		if err != nil {
			return "", "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	if w.Action == ActionDelete {
		if w.Record != nil {
			return "", "", fmt.Errorf("%w: delete has record", ErrInvalidRequest)
		}
	} else if w.Record == nil {
		return "", "", fmt.Errorf("%w: missing record", ErrInvalidRequest)
	}
	return nsid, rk, nil
}

func loadRepoRowTx(ctx context.Context, tx *sql.Tx, did syntax.DID) (*repoRow, error) {
	var rootRaw, commitRaw, revRaw, pubkey string
	err := tx.QueryRowContext(ctx,
		"SELECT root, commit_cid, rev, pubkey FROM atrepo_repos WHERE did = ?", did.String()).
		Scan(&rootRaw, &commitRaw, &revRaw, &pubkey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRepoNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repo: loading repo: %w", err)
	}
	root, err := parseCID(rootRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: stored root: %v", ErrCorruptRepo, err)
	}
	commit, err := parseCID(commitRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: stored commit: %v", ErrCorruptRepo, err)
	}
	rev, err := syntax.ParseTID(revRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: stored rev: %v", ErrCorruptRepo, err)
	}
	return &repoRow{root: root, commit: commit, rev: rev, pubkey: pubkey}, nil
}

var _ Repo = (*repoHandle)(nil)
