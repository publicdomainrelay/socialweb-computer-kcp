package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	MaxMessageBlocksBytes = 2_000_000
	MaxCommitOps          = 200
	futureRevTolerance    = 5 * time.Minute
)

var (
	ErrFutureRev   = errors.New("relay: commit revision in the future")
	ErrRevSequence = errors.New("relay: commit revision out of order")
)

type verifier struct {
	dir identity.Directory
	log *slog.Logger
}

func (v *verifier) verify(ctx context.Context, evt *comatproto.SyncSubscribeRepos_Commit, prev *AccountInfo, hostname string) error {
	if len(evt.Blocks) > MaxMessageBlocksBytes {
		return fmt.Errorf("blocks size (%d bytes) exceeds protocol limit", len(evt.Blocks))
	}
	if len(evt.Ops) > MaxCommitOps {
		return fmt.Errorf("too many ops in commit: %d", len(evt.Ops))
	}
	if len(evt.Blocks) == 0 {
		return fmt.Errorf("commit message has no blocks")
	}

	commit, _, err := repo.LoadCommitFromCAR(ctx, bytes.NewReader(evt.Blocks))
	if err != nil {
		return err
	}
	if err := commit.VerifyStructure(); err != nil {
		return err
	}
	rev, err := syntax.ParseTID(commit.Rev)
	if err != nil {
		return fmt.Errorf("commit rev syntax: %w", err)
	}
	if rev.Time().Compare(time.Now().Add(futureRevTolerance)) > 0 {
		return fmt.Errorf("%w: %s", ErrFutureRev, rev)
	}

	did, err := syntax.ParseDID(evt.Repo)
	if err != nil {
		return err
	}
	ident, err := v.dir.LookupDID(ctx, did)
	if err != nil {
		return fmt.Errorf("resolve identity %s: %w", did, err)
	}
	pubkey, err := ident.PublicKey()
	if err != nil {
		return fmt.Errorf("commit verification: %w", err)
	}
	if err := commit.VerifySignature(pubkey); err != nil {
		signatureErr := err
		if purgeErr := v.dir.Purge(ctx, did.AtIdentifier()); purgeErr != nil {
			v.log.Warn("failed to purge a DID whose commit signature did not verify",
				"did", did.String(), "err", purgeErr)
			return fmt.Errorf("commit verification: %w", signatureErr)
		}
		ident, err = v.dir.LookupDID(ctx, did)
		if err != nil {
			return fmt.Errorf("commit verification: %w", signatureErr)
		}
		pubkey, err = ident.PublicKey()
		if err != nil {
			return fmt.Errorf("commit verification: %w", signatureErr)
		}
		if err := commit.VerifySignature(pubkey); err != nil {
			return fmt.Errorf("commit verification: %w", err)
		}
	}
	if evt.Repo != commit.DID {
		return fmt.Errorf("mismatched inner commit DID field: %s", commit.DID)
	}
	if evt.Rev != commit.Rev {
		return fmt.Errorf("mismatched inner commit rev field: %s", commit.Rev)
	}

	if prev != nil && prev.Rev != nil {
		if evt.Rev <= *prev.Rev {
			return fmt.Errorf("%w: %s before or equal to %s", ErrRevSequence, evt.Rev, *prev.Rev)
		}
	}
	if evt.TooBig {
		return fmt.Errorf("deprecated tooBig commit flag set")
	}
	if evt.Rebase {
		return fmt.Errorf("deprecated rebase commit flag set")
	}
	if _, err := syntax.ParseDatetime(evt.Time); err != nil {
		return fmt.Errorf("commit timestamp syntax: %w", err)
	}
	if _, err := repo.VerifyCommitMessage(ctx, evt); err != nil {
		v.log.Warn("failed to invert commit MST", "host", hostname, "did", evt.Repo, "rev", evt.Rev, "err", err)
	}
	return nil
}
