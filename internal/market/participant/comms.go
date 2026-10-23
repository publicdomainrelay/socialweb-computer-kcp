package participant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

const MaxSubmitBody = 2 << 20

var ErrUnsigned = errors.New("record is missing a valid badge.blue signature")

type Verifier struct {
	Directory identity.Directory

	Records market.RecordGetter
}

func (v Verifier) Options() market.VerifyRecordOptions {
	opts := market.VerifyRecordOptions{KeysForDID: market.NewDirectoryKeyLister(v.Directory)}
	if v.Records != nil {
		opts.KeyBoundByBadgeBlueKey = v.BadgeBlueBinding()
	}
	return opts
}

func (v Verifier) BadgeBlueBinding() func(ctx context.Context, did, key string) (bool, error) {
	records := v.Records
	return func(ctx context.Context, did, key string) (bool, error) {
		return market.KeyBoundByBadgeBlueKey(ctx, records, did, key), nil
	}
}

func (v Verifier) VerifyRecord(ctx context.Context, record market.ResolvedRecord) error {
	if !market.VerifyRecordSignatures(ctx, record.Value, market.ATURIAuthority(record.URI), v.Options()) {
		return ErrUnsigned
	}
	return nil
}

type Authorizer struct {
	Directory identity.Directory

	Hostname string

	ServiceIDs []string

	AudienceDIDs []string
}

func (a *Authorizer) Verify(ctx context.Context, header, lxm string) (string, error) {
	if a == nil || a.Directory == nil {
		return "", errors.New("participant: the service has no identity directory")
	}
	if a.Hostname == "" {
		return "", errors.New("participant: the service has no service hostname")
	}
	result, err := market.VerifyServiceAuth(ctx, a.Directory, market.VerifyServiceAuthOptions{
		AuthHeader:        header,
		Hostname:          a.Hostname,
		LXM:               lxm,
		ServiceIDs:        a.ServiceIDs,
		ExtraAudienceDIDs: a.AudienceDIDs,
	})
	if err != nil {
		return "", err
	}
	return result.IssuerDID, nil
}

func (a *Authorizer) Authorize(ctx context.Context, header, lxm, recordURI string) (string, error) {
	issuer, err := a.Verify(ctx, header, lxm)
	if err != nil {
		return "", err
	}
	if err := market.AuthorizeRecord(issuer, recordURI); err != nil {
		return "", err
	}
	return issuer, nil
}

func (a *Authorizer) AuthorizeRequest(r *http.Request, lxm, recordURI string) (string, bool, int) {
	if a == nil {
		return "", false, http.StatusInternalServerError
	}
	issuer, err := a.Authorize(r.Context(), r.Header.Get("Authorization"), lxm, recordURI)
	if err != nil {
		if errors.Is(err, market.ErrForbidden) {
			return "", false, http.StatusForbidden
		}
		return "", false, http.StatusUnauthorized
	}
	return issuer, true, 0
}

func ReadSubmitBody(r *http.Request, out any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxSubmitBody+1))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("the body is not JSON: %w", err)
	}
	return nil
}

func WriteXRPCError(w http.ResponseWriter, status int, code, message string) {
	httpjson.Write(w, status, map[string]any{"error": code, "message": message})
}

func AuthCode(status int) string {
	if status == http.StatusForbidden {
		return "Forbidden"
	}
	return "Unauthorized"
}
