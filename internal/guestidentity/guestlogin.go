package guestidentity

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var (
	bindSecretSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

	errNoGuestLogin = errors.New("no guest login")

	errBadGuestLogin = errors.New("spec.guestLogin")
)

type GuestLoginRefs struct {
	Role string `json:"role"`

	Mount string `json:"mount"`

	Scopes []string `json:"scopes,omitempty"`
}

func GuestLoginRef(o v1alpha1.ComputeVM, scopes []string) (*GuestLoginRefs, error) {
	g := o.Spec.GuestLogin
	if g == nil {
		return nil, errNoGuestLogin
	}
	if !ValidMount(g.Mount) {
		return nil, fmt.Errorf("%w: mount %q is not an auth mount name", errBadGuestLogin, g.Mount)
	}
	if !bindSecretSHA256.MatchString(g.BindSecretSha256) {
		return nil, fmt.Errorf("%w: bindSecretSha256 is not a hex sha256", errBadGuestLogin)
	}
	seen := map[string]bool{}
	ref := &GuestLoginRefs{Role: "auth/" + g.Mount + "/role/" + o.Name, Mount: g.Mount}
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		ref.Scopes = append(ref.Scopes, scope)
	}
	sort.Strings(ref.Scopes)
	return ref, nil
}
