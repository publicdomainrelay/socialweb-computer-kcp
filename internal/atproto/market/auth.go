package market

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

var bearerRE = regexp.MustCompile(`^Bearer (.+)$`)

func ExtractBearer(header string) (string, error) {
	m := bearerRE.FindStringSubmatch(strings.TrimSpace(header))
	if m == nil {
		return "", fmt.Errorf("%w: missing or malformed Authorization Bearer header", ErrInvalidAuth)
	}
	return m[1], nil
}

func ServiceDIDForHost(hostname string) string {
	return "did:web:" + hostname
}

type ServiceAuthResult struct {
	IssuerDID string
	Audience  string
	ServiceID string
}

type VerifyServiceAuthOptions struct {
	AuthHeader        string
	Hostname          string
	LXM               string
	ServiceIDs        []string
	ExtraAudienceDIDs []string
}

type serviceAudience struct {
	aud       string
	serviceID string
}

func VerifyServiceAuth(ctx context.Context, dir identity.Directory, opts VerifyServiceAuthOptions) (*ServiceAuthResult, error) {
	token, err := ExtractBearer(opts.AuthHeader)
	if err != nil {
		return nil, err
	}
	if dir == nil {
		return nil, fmt.Errorf("%w: no identity directory", ErrInvalidAuth)
	}
	if opts.Hostname == "" {
		return nil, fmt.Errorf("%w: no service hostname", ErrInvalidAuth)
	}
	if opts.LXM == "" {
		return nil, fmt.Errorf("%w: no lexicon method", ErrInvalidAuth)
	}
	nsid, err := syntax.ParseNSID(opts.LXM)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAuth, err)
	}
	serviceDID := ServiceDIDForHost(strings.ToLower(opts.Hostname))
	dids := append([]string{serviceDID, ServiceDIDForHost(opts.Hostname)}, opts.ExtraAudienceDIDs...)
	if name, _, err := net.SplitHostPort(opts.Hostname); err == nil && name != "" {
		dids = append(dids, ServiceDIDForHost(strings.ToLower(name)))
	}
	audiences := make([]serviceAudience, 0, len(dids)*(len(opts.ServiceIDs)+1))
	for _, did := range dids {
		audiences = append(audiences, serviceAudience{aud: did})
		for _, id := range opts.ServiceIDs {
			if id == "" {
				continue
			}
			audiences = append(audiences, serviceAudience{aud: did + "#" + id, serviceID: id})
		}
	}
	var lastErr error
	for _, candidate := range audiences {
		validator := &auth.ServiceAuthValidator{Audience: candidate.aud, Dir: dir}
		issuer, err := validator.Validate(ctx, token, &nsid)
		if err != nil {
			lastErr = err
			continue
		}
		iss := issuer.String()
		if i := strings.IndexByte(iss, '#'); i >= 0 {
			iss = iss[:i]
		}
		if !strings.HasPrefix(iss, "did:") {
			return nil, fmt.Errorf("%w: service auth token missing DID issuer", ErrInvalidAuth)
		}
		return &ServiceAuthResult{IssuerDID: iss, Audience: candidate.aud, ServiceID: candidate.serviceID}, nil
	}
	return nil, fmt.Errorf("%w: %v", ErrInvalidAuth, lastErr)
}

func AuthorizeRecord(issuerDID, recordURI string) error {
	if issuerDID != ATURIAuthority(recordURI) {
		return fmt.Errorf("%w: service-auth token issuer must author the referenced record", ErrForbidden)
	}
	return nil
}
