package guestidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type RBACRecord struct {
	Roles map[string]RBACRole `json:"roles,omitempty"`
}

type RBACRole struct {
	RoleName string `json:"role_name,omitempty"`

	Definition RBACRoleDefinition `json:"definition"`
}

type RBACRoleDefinition struct {
	Iss string `json:"iss,omitempty"`

	Aud string `json:"aud,omitempty"`

	Sub string `json:"sub"`

	Policies []string `json:"policies,omitempty"`
}

type GrantWish struct {
	API string

	Actx string

	Scopes []string

	Claims []v1alpha1.GuestIdentityClaim
}

func (w GrantWish) Audience() string { return GuestAudience(w.API, w.Actx) }

func OwnWish(computevm string) GrantWish {
	return GrantWish{API: KCPAPI, Actx: computevm, Scopes: []string{OnNetworkScope}}
}

func Wishes(computevm string, records []RBACRecord, issuer, subject string) []GrantWish {
	byAudience := map[string]GrantWish{}
	add := func(w GrantWish) {
		key := w.Audience()
		held, ok := byAudience[key]
		if !ok {
			held = GrantWish{API: w.API, Actx: w.Actx, Claims: append([]v1alpha1.GuestIdentityClaim(nil), w.Claims...)}
		}
		for _, scope := range w.Scopes {
			if !contains(held.Scopes, scope) {
				held.Scopes = append(held.Scopes, scope)
			}
		}
		byAudience[key] = held
	}
	add(OwnWish(computevm))
	for _, wish := range WishesFromRBAC(records, issuer, subject) {
		add(wish)
	}
	out := make([]GrantWish, 0, len(byAudience))
	for _, wish := range byAudience {
		sort.Strings(wish.Scopes)
		sort.Slice(wish.Claims, func(i, j int) bool { return wish.Claims[i].Name < wish.Claims[j].Name })
		out = append(out, wish)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Audience() < out[j].Audience() })
	return out
}

func WishesFromRBAC(records []RBACRecord, issuer, subject string) []GrantWish {
	byAudience := map[string]GrantWish{}
	for _, record := range records {
		for _, role := range record.Roles {
			def := role.Definition
			if def.Sub != subject {
				continue
			}
			if def.Iss != "" && def.Iss != issuer {
				continue
			}
			audience, err := ParseAudience(def.Aud)
			if err != nil {
				continue
			}
			key := audience.String()
			wish, ok := byAudience[key]
			if !ok {
				wish = GrantWish{API: audience.API, Actx: audience.Actx,
					Claims: []v1alpha1.GuestIdentityClaim{{Name: OIDCSubjectClaim, Value: subject}}}
			}
			for _, policy := range def.Policies {
				if !contains(wish.Scopes, policy) {
					wish.Scopes = append(wish.Scopes, policy)
				}
			}
			byAudience[key] = wish
		}
	}
	out := make([]GrantWish, 0, len(byAudience))
	for _, wish := range byAudience {
		sort.Strings(wish.Scopes)
		out = append(out, wish)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Audience() < out[j].Audience() })
	return out
}

func GrantName(computevm string, wish GrantWish) string {
	sum := sha256.Sum256([]byte(wish.Audience()))
	return computevm + "-" + hex.EncodeToString(sum[:])[:12]
}

func WishKey(wish GrantWish) string {
	scopes := append([]string(nil), wish.Scopes...)
	sort.Strings(scopes)
	claims := append([]v1alpha1.GuestIdentityClaim(nil), wish.Claims...)
	sort.Slice(claims, func(i, j int) bool { return claims[i].Name < claims[j].Name })
	raw, _ := json.Marshal(struct {
		API    string                        `json:"api"`
		Actx   string                        `json:"actx"`
		Scopes []string                      `json:"scopes"`
		Claims []v1alpha1.GuestIdentityClaim `json:"claims"`
	}{wish.API, wish.Actx, scopes, claims})
	return string(raw)
}

func HeldKey(g v1alpha1.GuestIdentityGrant) string {
	scopes := append([]string(nil), g.Spec.Scopes...)
	sort.Strings(scopes)
	claims := append([]v1alpha1.GuestIdentityClaim(nil), g.Spec.Claims...)
	sort.Slice(claims, func(i, j int) bool { return claims[i].Name < claims[j].Name })
	raw, _ := json.Marshal(struct {
		API    string                        `json:"api"`
		Actx   string                        `json:"actx"`
		Scopes []string                      `json:"scopes"`
		Claims []v1alpha1.GuestIdentityClaim `json:"claims"`
	}{g.Spec.Api, g.Spec.Actx, scopes, claims})
	return string(raw)
}
