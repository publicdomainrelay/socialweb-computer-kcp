package atprotoauth

import (
	"context"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const LookupTimeout = 10 * time.Second

type admitPlcAndWeb struct {
	dir identity.Directory
}

var _ identity.Directory = admitPlcAndWeb{}

func (a admitPlcAndWeb) LookupDID(ctx context.Context, did syntax.DID) (*identity.Identity, error) {
	switch did.Method() {
	case "plc", "web":
		ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
		defer cancel()
		return a.dir.LookupDID(ctx, did)
	}
	return nil, fmt.Errorf(
		"%w: %s is a did:%s, and this door admits the methods the directory resolves and a revocation can reach (did:plc, did:web); a did:key is the RENEWAL credential and belongs to the mint",
		ErrBadIssuer, did, did.Method(),
	)
}

func (a admitPlcAndWeb) LookupHandle(ctx context.Context, h syntax.Handle) (*identity.Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	return a.dir.LookupHandle(ctx, h)
}

func (a admitPlcAndWeb) Lookup(ctx context.Context, at syntax.AtIdentifier) (*identity.Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	return a.dir.Lookup(ctx, at)
}

func (a admitPlcAndWeb) Purge(ctx context.Context, at syntax.AtIdentifier) error {
	return a.dir.Purge(ctx, at)
}
