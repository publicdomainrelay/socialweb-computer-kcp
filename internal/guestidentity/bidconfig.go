package guestidentity

import (
	"cmp"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
)

const (
	WIFSimpleNSID = "com.publicdomainrelay.temp.compute.config.wif.simple"

	WIFToIssue = "openbao-identity-oidc"

	WIFURLRoute = "/v1/identity/oidc/token"

	WIFSubjectTemplate = "actx:{actx}:plc:{did-plc-key}:role:{role}"
)

type BidConfig struct {
	Type string `json:"$type"`

	AcceptPath string `json:"accept_path"`

	IssuerURI string `json:"issuer_uri"`

	ToIssue string `json:"to_issue"`

	Actx string `json:"actx"`

	TokenPath string `json:"token_path"`

	URLPath string `json:"url_path"`

	URLRoute string `json:"url_route"`

	Subject string `json:"subject"`

	CreatedAt string `json:"createdAt"`
}

func SubjectFor(bidderDID, requesterDID, role string) (string, error) {
	team, err := didKey(bidderDID)
	if err != nil {
		return "", fmt.Errorf("guestidentity: bidder DID: %w", err)
	}
	requester, err := didKey(requesterDID)
	if err != nil {
		return "", fmt.Errorf("guestidentity: requester DID: %w", err)
	}
	if role == "" {
		return "", errors.New("guestidentity: the subject names no role")
	}
	return "actx:" + team + ":plc:" + requester + ":role:" + role, nil
}

func BidConfigFor(issuerURL, bidderDID, acceptPath, createdAt string) (BidConfig, error) {
	team, err := didKey(bidderDID)
	if err != nil {
		return BidConfig{}, fmt.Errorf("guestidentity: bidder DID: %w", err)
	}
	ingress, err := cleanIngressURL(issuerURL)
	if err != nil {
		return BidConfig{}, fmt.Errorf("guestidentity: the bid config's issuer: %w", err)
	}
	issuer := OIDCIssuer(ingress)
	acceptPath = cmp.Or(acceptPath, guestseed.AcceptPathVM)
	if createdAt == "" {
		createdAt = time.Now().UTC().Format(time.RFC3339)
	}
	return BidConfig{
		Type:       WIFSimpleNSID,
		AcceptPath: acceptPath,
		IssuerURI:  issuer,
		ToIssue:    WIFToIssue,
		Actx:       team,
		TokenPath:  guestseed.GuestLoginTokenPath,
		URLPath:    guestseed.OnNetworkBaseURLPath,
		URLRoute:   WIFURLRoute,
		Subject:    WIFSubjectTemplate,
		CreatedAt:  createdAt,
	}, nil
}

func didKey(did string) (string, error) {
	did = strings.TrimSpace(did)
	if !strings.HasPrefix(did, "did:") {
		return "", fmt.Errorf("%q is not a DID", did)
	}
	parts := strings.Split(did, ":")
	if len(parts) < 3 || parts[1] == "" || parts[len(parts)-1] == "" {
		return "", fmt.Errorf("%q names no identifier", did)
	}
	return parts[len(parts)-1], nil
}
