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

	WIFURLRouteSuffix = "/wid/token"

	WIFSubjectTemplate = "actx:{actx}:plc:{did-plc-key}:role:{role}"
)

type BidConfig struct {
	Type string `json:"$type"`

	AcceptPath string `json:"accept_path"`

	IssuerURI string `json:"issuer_uri"`

	ToIssue string `json:"to_issue"`

	Actx string `json:"actx"`

	TokenPath string `json:"token_path,omitempty"`

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

func BidConfigFor(issuerURL, mount, bidderDID, acceptPath, createdAt string) (BidConfig, error) {
	if !ValidMount(mount) {
		return BidConfig{}, fmt.Errorf("guestidentity: mount %q is not an auth mount name", mount)
	}
	team, err := didKey(bidderDID)
	if err != nil {
		return BidConfig{}, fmt.Errorf("guestidentity: bidder DID: %w", err)
	}
	ingress, err := cleanIngressURL(issuerURL)
	if err != nil {
		return BidConfig{}, fmt.Errorf("guestidentity: the bid config's issuer: %w", err)
	}
	issuer := OIDCIssuer(ingress, mount)
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
		URLPath:    guestseed.OnNetworkBaseURLPath,
		URLRoute:   "/v1/auth/" + mount + WIFURLRouteSuffix,
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
