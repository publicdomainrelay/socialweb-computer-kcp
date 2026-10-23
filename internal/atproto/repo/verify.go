package repo

import (
	"bytes"
	"context"
	"fmt"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	indigorepo "github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
)

func VerifyCommitSignature(commit *Commit, pub atcrypto.PublicKey) error {
	if commit == nil {
		return fmt.Errorf("%w: missing commit", ErrInvalidRequest)
	}
	if pub == nil {
		return fmt.Errorf("%w: missing public key", ErrInvalidRequest)
	}
	if err := commit.VerifyStructure(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupportedRepo, err)
	}
	if err := commit.VerifySignature(pub); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptRepo, err)
	}
	return nil
}

func VerifyCommitCAR(ctx context.Context, carBytes []byte, pub atcrypto.PublicKey) (*Commit, *indigorepo.Repo, error) {
	if len(carBytes) == 0 {
		return nil, nil, fmt.Errorf("%w: empty car", ErrInvalidRequest)
	}
	root, err := VerifyCAR(ctx, carBytes)
	if err != nil {
		return nil, nil, err
	}
	commit, atr, err := indigorepo.LoadRepoFromCAR(ctx, bytes.NewReader(carBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("loading repo car: %w", err)
	}
	if commit.Data == cid.Undef || !root.Defined() {
		return nil, nil, fmt.Errorf("%w: car root undefined", ErrCorruptRepo)
	}
	if err := VerifyCommitSignature(commit, pub); err != nil {
		return nil, nil, err
	}
	return commit, atr, nil
}

func VerifyCommitMessage(ctx context.Context, dir identity.Directory, msg *comatproto.SyncSubscribeRepos_Commit) (*Commit, *indigorepo.Repo, error) {
	if msg == nil {
		return nil, nil, fmt.Errorf("%w: missing commit message", ErrInvalidRequest)
	}
	if msg.TooBig {
		return nil, nil, fmt.Errorf("%w: tooBig commit has no blocks", ErrInvalidRequest)
	}
	if len(msg.Blocks) == 0 {
		return nil, nil, fmt.Errorf("%w: empty commit blocks", ErrInvalidRequest)
	}
	root, err := VerifyCAR(ctx, []byte(msg.Blocks))
	if err != nil {
		return nil, nil, err
	}
	if !msg.Commit.Defined() {
		return nil, nil, fmt.Errorf("%w: missing commit cid", ErrInvalidRequest)
	}
	if cid.Cid(msg.Commit) != root {
		return nil, nil, fmt.Errorf("%w: commit cid does not match car root", ErrCorruptRepo)
	}
	atr, err := indigorepo.VerifyCommitMessage(ctx, msg)
	if err != nil {
		return nil, nil, fmt.Errorf("verifying commit message: %w", err)
	}
	commit, _, err := indigorepo.LoadCommitFromCAR(ctx, bytes.NewReader(msg.Blocks))
	if err != nil {
		return nil, nil, fmt.Errorf("loading commit from car: %w", err)
	}
	did, err := syntax.ParseDID(commit.DID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: commit did: %v", ErrCorruptRepo, err)
	}
	ident, err := dir.LookupDID(ctx, did)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving commit key: %w", err)
	}
	pub, err := ident.PublicKey()
	if err != nil {
		return nil, nil, fmt.Errorf("reading commit key: %w", err)
	}
	if err := VerifyCommitSignature(commit, pub); err != nil {
		return nil, nil, err
	}
	return commit, atr, nil
}

func VerifySyncMessage(ctx context.Context, dir identity.Directory, msg *comatproto.SyncSubscribeRepos_Sync) (*Commit, error) {
	if msg == nil {
		return nil, fmt.Errorf("%w: missing sync message", ErrInvalidRequest)
	}
	if len(msg.Blocks) == 0 {
		return nil, fmt.Errorf("%w: empty sync blocks", ErrInvalidRequest)
	}
	if _, err := VerifyCAR(ctx, []byte(msg.Blocks)); err != nil {
		return nil, err
	}
	commit, err := indigorepo.VerifySyncMessage(ctx, dir, msg)
	if err != nil {
		return nil, fmt.Errorf("verifying sync message: %w", err)
	}
	return commit, nil
}

func (s *SQLiteStore) verifyRepo(ctx context.Context, did syntax.DID, pub atcrypto.PublicKey) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.loadRepoRow(ctx, did)
	if err != nil {
		return err
	}
	if pub == nil {
		pub, err = atcrypto.ParsePublicMultibase(row.pubkey)
		if err != nil {
			return fmt.Errorf("%w: stored public key: %v", ErrCorruptRepo, err)
		}
	}
	cur := row.commit
	first := true
	seen := make(map[cid.Cid]bool)
	childRev := ""
	for {
		if seen[cur] {
			return fmt.Errorf("%w: commit chain cycle at %s", ErrCorruptRepo, cur)
		}
		seen[cur] = true
		cr, err := s.loadCommitByCID(ctx, s.db, did.String(), cur)
		if err != nil {
			return err
		}
		if first {
			if cr.Rev != row.rev || cr.Data != row.root {
				return fmt.Errorf("%w: head mismatch", ErrCorruptRepo)
			}
			first = false
		}
		commit := cr.Signed
		if commit.DID != did.String() {
			return fmt.Errorf("%w: commit did mismatch", ErrCorruptRepo)
		}
		if err := VerifyCommitSignature(commit, pub); err != nil {
			return err
		}
		if childRev != "" && cr.Rev >= syntax.TID(childRev) {
			return fmt.Errorf("%w: commit rev is not increasing", ErrCorruptRepo)
		}
		tree, err := loadFullTree(ctx, s.dbReader(did), commit.Data)
		if err != nil {
			return err
		}
		if err := tree.Walk(func(key []byte, val cid.Cid) error {
			_, err := readBlock(ctx, s.db, did.String(), val)
			return err
		}); err != nil {
			return fmt.Errorf("%w: reading records: %v", ErrCorruptRepo, err)
		}
		if commit.Prev == nil {
			break
		}
		childRev = cr.Rev.String()
		cur = *commit.Prev
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM atrepo_commits WHERE did = ?", did.String()).Scan(&count); err != nil {
		return err
	}
	if count != len(seen) {
		return fmt.Errorf("%w: %d unreachable commits", ErrCorruptRepo, count-len(seen))
	}
	return nil
}

func (s *SQLiteStore) dbReader(did syntax.DID) *sqlBlockReader {
	return &sqlBlockReader{db: s.db, did: did.String()}
}
