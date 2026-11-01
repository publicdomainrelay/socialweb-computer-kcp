package market

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/golang-jwt/jwt/v5"
)

const (
	ServiceAuthIssuedAtLeeway = 30 * time.Second

	ServiceAuthExpiryLeeway = 5 * time.Second
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
	leeway, err := serviceAuthLeeway(token, time.Now())
	if err != nil {
		return nil, err
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
		validator := &auth.ServiceAuthValidator{Audience: candidate.aud, Dir: dir, TimestampLeeway: leeway}
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

func ServiceAudienceDIDs(host string) []string {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	candidates := []string{host, strings.ToLower(host)}
	if name, _, err := net.SplitHostPort(host); err == nil && name != "" {
		candidates = append(candidates, name, strings.ToLower(name))
	}
	unique := []string{}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		did := ServiceDIDForHost(candidate)
		if seen[did] {
			continue
		}
		seen[did] = true
		unique = append(unique, did)
	}
	return unique
}

func serviceAuthLeeway(token string, now time.Time) (time.Duration, error) {
	claims := jwt.RegisteredClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, &claims); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidAuth, err)
	}
	if claims.ExpiresAt == nil {
		return 0, fmt.Errorf("%w: the service auth token has no exp", ErrInvalidAuth)
	}
	if late := now.Sub(claims.ExpiresAt.Time); late > ServiceAuthExpiryLeeway {
		return 0, fmt.Errorf("%w: the service auth token expired %s ago", ErrInvalidAuth, late.Round(time.Second))
	}
	leeway := ServiceAuthExpiryLeeway
	for _, at := range []*jwt.NumericDate{claims.IssuedAt, claims.NotBefore} {
		if at == nil {
			continue
		}
		skew := at.Sub(now)
		if skew > ServiceAuthIssuedAtLeeway {
			return 0, fmt.Errorf("%w: the service auth token is issued %s in this host's future (clock skew)", ErrInvalidAuth, skew.Round(time.Second))
		}
		if skew+time.Second > leeway {
			leeway = skew + time.Second
		}
	}
	return leeway, nil
}
