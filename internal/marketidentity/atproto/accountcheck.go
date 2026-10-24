package atproto

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
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

func (a *Accounts) checkAccount(ctx context.Context, id *v1alpha1.MarketIdentity, did, host, claimed string) (string, error) {
	did = strings.TrimSpace(did)
	resolved := a.handleOf(ctx, did)
	got := describeAccount(did, firstNonEmpty(resolved, claimed))
	if want := strings.TrimSpace(id.Spec.Did); want != "" && want != did {
		return "", &accountMismatchError{text: fmt.Sprintf("the oauth session is for %s, not the configured did %s; sign in as %s",
			got, want, want)}
	}
	want := strings.TrimRight(strings.TrimSpace(id.Spec.PdsURL), "/")
	if want != "" && host != "" && want != strings.TrimRight(strings.TrimSpace(host), "/") {
		return "", &accountMismatchError{text: fmt.Sprintf("the oauth session of %s is on the PDS %s, not the configured pdsUrl %s; sign in again",
			got, strings.TrimRight(host, "/"), want)}
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
