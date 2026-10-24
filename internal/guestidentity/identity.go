package guestidentity

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const (
	KCPAPI = "guest-kcp"

	EntityPrefix = "guest-"

	VaultTokenHeader = "X-Vault-Token"

	OIDCIssuerPath = "/v1/identity/oidc"

	OIDCSubjectClaim = "wid_sub"

	OnNetworkScope = "on-network"
)

var (
	computeVMNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

	roleNameRE = regexp.MustCompile(`^guest-[A-Za-z0-9][A-Za-z0-9._-]*-[0-9a-f]{16}$`)

	authMountName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

type Audience struct {
	API string

	Actx string
}

func (a Audience) String() string { return openbaoctl.GuestAudience(a.API, a.Actx) }

func GuestAudience(api, actx string) string { return openbaoctl.GuestAudience(api, actx) }

func OIDCIssuer(ingressURL string) string {
	return strings.TrimRight(ingressURL, "/") + OIDCIssuerPath
}

func ParseAudience(aud string) (Audience, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(aud), "api://")
	if !ok {
		return Audience{}, fmt.Errorf("guestidentity: audience %q does not name an api", aud)
	}
	api, query, ok := strings.Cut(rest, "?")
	if !ok || api == "" {
		return Audience{}, fmt.Errorf("guestidentity: audience %q is not api://<api>?actx=<context>", aud)
	}
	if !validAPI(api) {
		return Audience{}, fmt.Errorf("guestidentity: audience %q names an api that is not one", aud)
	}
	values, err := url.ParseQuery(query)
	if err != nil || len(values) != 1 || len(values["actx"]) != 1 {
		return Audience{}, fmt.Errorf("guestidentity: audience %q does not carry exactly an actx", aud)
	}
	actx := values["actx"][0]
	if !validActx(actx) {
		return Audience{}, fmt.Errorf("guestidentity: audience %q names an actx that is not one", aud)
	}
	return Audience{API: api, Actx: actx}, nil
}

func validAPI(api string) bool {
	return api != "" && !strings.ContainsAny(api, "?/ #&=\"{}'")
}

func validActx(actx string) bool {
	return actx != "" && !strings.ContainsAny(actx, "? #&=\"{}'")
}

func ValidComputeVM(name string) bool { return len(name) <= 201 && computeVMNameRE.MatchString(name) }

func ValidRole(role string) bool { return len(role) <= 224 && roleNameRE.MatchString(role) }

func Entity(computevm string) string { return EntityPrefix + computevm }

func Role(computevm, audience string) string {
	return openbaoctl.GuestRoleName(Entity(computevm), audience)
}

func ValidMount(mount string) bool { return len(mount) <= 63 && authMountName.MatchString(mount) }
