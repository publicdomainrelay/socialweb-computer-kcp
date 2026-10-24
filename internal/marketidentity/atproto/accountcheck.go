package atproto

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
)

type accountMismatchError struct {
	text string
}

func (e *accountMismatchError) Error() string {
	return "marketidentity: " + e.text
}

type accountUnverifiedError struct {
	err error
}

func (e *accountUnverifiedError) Error() string {
	return "marketidentity: the oauth session account is not verified: " + e.err.Error()
}

func (e *accountUnverifiedError) Unwrap() error {
	return e.err
}

func accountKey(id *v1alpha1.MarketIdentity) string {
	return strings.Join([]string{strings.TrimSpace(id.Spec.Did), strings.ToLower(strings.TrimSpace(id.Spec.Handle)),
		strings.TrimRight(strings.TrimSpace(id.Spec.PdsURL), "/")}, "\x00")
}

func (a *Accounts) checkAccount(ctx context.Context, id *v1alpha1.MarketIdentity, did, host, authServer, claimed string) (string, error) {
	did = strings.TrimSpace(did)
	resolved := a.handleOf(ctx, did)
	got := describeAccount(did, firstNonEmpty(resolved, claimed))
	if want := strings.TrimSpace(id.Spec.Did); want != "" && want != did {
		return "", &accountMismatchError{text: fmt.Sprintf("the oauth session is for %s, not the configured did %s; sign in as %s",
			got, want, want)}
	}
	want := strings.TrimRight(strings.TrimSpace(id.Spec.PdsURL), "/")
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if want != "" && host != "" && want != host {
		if want != strings.TrimRight(strings.TrimSpace(authServer), "/") {
			return "", &accountMismatchError{text: fmt.Sprintf("the oauth session of %s is on the PDS %s, neither the configured pdsUrl %s nor its authorization server; sign in again",
				got, host, want)}
		}
		if err := a.entrywayAccount(ctx, did, host, want); err != nil {
			if mismatch := (*accountMismatchError)(nil); errors.As(err, &mismatch) {
				return "", err
			}
			return "", &accountUnverifiedError{err: err}
		}
	}
	if raw := strings.TrimSpace(id.Spec.Handle); raw != "" {
		handle, err := syntax.ParseHandle(raw)
		if err != nil {
			return "", &accountUnverifiedError{err: fmt.Errorf("the configured handle %q: %w", raw, err)}
		}
		if a.oauth.Directory == nil {
			return "", &accountUnverifiedError{err: errors.New("no identity directory resolves the configured handle " + raw)}
		}
		ident, err := a.oauth.Directory.LookupHandle(ctx, handle.Normalize())
		if err != nil {
			return "", &accountUnverifiedError{err: fmt.Errorf("resolving the configured handle %s: %w", raw, err)}
		}
		if ident.DID.String() != did {
			return "", &accountMismatchError{text: fmt.Sprintf("the oauth session is for %s, not %s (%s); sign in as %s",
				got, handle.Normalize(), ident.DID, handle.Normalize())}
		}
		if resolved == "" {
			resolved = ident.Handle.Normalize().String()
		}
	}
	return firstNonEmpty(resolved, claimed), nil
}

func (a *Accounts) entrywayAccount(ctx context.Context, did, host, entryway string) error {
	if a.oauth.Directory == nil {
		return errors.New("no identity directory resolves " + did + " to check its PDS against the entryway " + entryway)
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return err
	}
	ident, err := a.oauth.Directory.LookupDID(ctx, parsed)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", did, err)
	}
	if pds := strings.TrimRight(strings.TrimSpace(ident.PDSEndpoint()), "/"); pds != host {
		return &accountMismatchError{text: fmt.Sprintf("the oauth session of %s is on the PDS %s, but %s names the PDS %q; sign in again", did, host, did, pds)}
	}
	authServer, _, _, err := oauthclient.DiscoverAuthServer(ctx, a.oauthHTTP(), host)
	if err != nil {
		return err
	}
	if authServer != entryway {
		return &accountMismatchError{text: fmt.Sprintf("the PDS %s of %s names the authorization server %s, not the configured pdsUrl %s; sign in again",
			host, did, authServer, entryway)}
	}
	return nil
}

func (a *Accounts) handleOf(ctx context.Context, did string) string {
	if a.oauth.Directory == nil {
		return ""
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return ""
	}
	ident, err := a.oauth.Directory.LookupDID(ctx, parsed)
	if err != nil || ident.Handle == "" || ident.Handle.IsInvalidHandle() {
		return ""
	}
	return ident.Handle.Normalize().String()
}

func describeAccount(did, handle string) string {
	did, handle = strings.TrimSpace(did), strings.TrimSpace(handle)
	switch {
	case did == "":
		return handle
	case handle == "" || handle == did:
		return did
	}
	return handle + " (" + did + ")"
}
