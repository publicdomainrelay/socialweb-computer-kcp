package openbaoctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/baosync"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	DefaultOIDCAlgorithm       = "RS256"
	DefaultOIDCRotationPeriod  = "24h"
	DefaultOIDCVerificationTTL = "48h"
	DefaultGuestTokenTTL       = "5m"
	GuestRoleHashLen           = 16
	grantNotHeldReason         = "the server does not hold its declaration yet"
	roleConflictMarker         = "role-conflict: "
)

var (
	GrantGVR = v1alpha1.GuestIdentityGrantGVR

	grantNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)
	grantClaimPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	reservedOIDCClaims = map[string]bool{"iat": true, "aud": true, "exp": true, "iss": true, "sub": true, "namespace": true, "nbf": true}
	IdentityCoreRules  = []v1alpha1.OpenBaoPolicyRule{
		{Path: "identity/oidc/key/*", Capabilities: []string{"create", "read", "update", "delete", "list"}},
		{Path: "identity/oidc/role/*", Capabilities: []string{"create", "read", "update", "delete", "list"}},
		{Path: "identity/oidc/role", Capabilities: []string{"list"}},
	}
)

func GuestAudience(api, actx string) string { return "api://" + api + "?actx=" + actx }

func GuestRoleName(entity, audience string) string {
	sum := sha256.Sum256([]byte(audience))
	return entity + "-" + hex.EncodeToString(sum[:])[:GuestRoleHashLen]
}

type grantRole struct {
	grant    v1alpha1.GuestIdentityGrant
	role     string
	audience string
	object   baosync.Object
}

func splitGrantRefusal(why string) (string, bool) {
	if s, ok := strings.CutPrefix(why, roleConflictMarker); ok {
		return s, true
	}
	return why, false
}

func keepConditionTimes(next []metav1.Condition, held []metav1.Condition) {
	for i := range next {
		for _, h := range held {
			if h.Type == next[i].Type && h.Status == next[i].Status {
				next[i].LastTransitionTime = h.LastTransitionTime
				break
			}
		}
	}
}

func grantTemplate(g v1alpha1.GuestIdentityGrant) (string, error) {
	claims := map[string]string{"computevm": g.Spec.ComputeVMRef.Name, "api": g.Spec.Api, "actx": g.Spec.Actx}
	for _, c := range g.Spec.Claims {
		if !grantClaimPattern.MatchString(c.Name) || reservedOIDCClaims[c.Name] {
			return "", fmt.Errorf("claim %q is not a claim name a grant may set", c.Name)
		}
		if strings.Contains(c.Value, "{{") {
			return "", fmt.Errorf("claim %q holds a template expression; a grant carries values, never templates", c.Name)
		}
		claims[c.Name] = c.Value
	}
	raw, err := json.Marshal(claims)
	return string(raw), err
}

func checkGrant(g v1alpha1.GuestIdentityGrant) error {
	s := g.Spec
	switch {
	case !grantNamePattern.MatchString(s.ComputeVMRef.Name):
		return fmt.Errorf("spec.computevm %q is not a ComputeVM name", s.ComputeVMRef.Name)
	case s.Entity != "guest-"+s.ComputeVMRef.Name:
		return fmt.Errorf("spec.entity %q is not guest-%s", s.Entity, s.ComputeVMRef.Name)
	case s.Api == "" || strings.ContainsAny(s.Api, "?/ \"{}"):
		return fmt.Errorf("spec.api %q is not an api name", s.Api)
	case s.Actx == "" || strings.ContainsAny(s.Actx, "&# \"{}"):
		return fmt.Errorf("spec.actx %q is not an audience context", s.Actx)
	}
	return nil
}

func grantRoles(grants []v1alpha1.GuestIdentityGrant, key, ttl string) (roles []grantRole, refused map[string]string) {
	refused = map[string]string{}
	seen := map[string]string{}
	for _, g := range grants {
		if g.DeletionTimestamp != nil {
			continue
		}
		if err := checkGrant(g); err != nil {
			refused[g.Name] = err.Error()
			continue
		}
		tmpl, err := grantTemplate(g)
		if err != nil {
			refused[g.Name] = err.Error()
			continue
		}
		aud := GuestAudience(g.Spec.Api, g.Spec.Actx)
		role := GuestRoleName(g.Spec.Entity, aud)
		if other, dup := seen[role]; dup {
			refused[g.Name] = roleConflictMarker + fmt.Sprintf("grant %s already holds role %s", other, role)
			continue
		}
		seen[role] = g.Name
		roles = append(roles, grantRole{grant: g, role: role, audience: aud, object: baosync.Object{
			Path: baosync.IdentityRoleCollection + "/" + role,
			Data: map[string]any{"key": key, "client_id": aud, "template": tmpl, "ttl": ttl},
		}})
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].role < roles[j].role })
	return roles, refused
}

func entityPolicies(roles []grantRole) []baosync.Policy {
	byEntity := map[string][]string{}
	for _, r := range roles {
		byEntity[r.grant.Spec.Entity] = append(byEntity[r.grant.Spec.Entity], r.role)
	}
	names := make([]string, 0, len(byEntity))
	for e := range byEntity {
		names = append(names, e)
	}
	sort.Strings(names)
	var out []baosync.Policy
	for _, e := range names {
		slices.Sort(byEntity[e])
		var rules []v1alpha1.OpenBaoPolicyRule
		for _, role := range byEntity[e] {
			rules = append(rules, v1alpha1.OpenBaoPolicyRule{Path: "identity/oidc/token/" + role, Capabilities: []string{"read"}})
		}
		out = append(out, baosync.Policy{Name: e, HCL: PolicyHCL(rules)})
	}
	return out
}

func identityOIDC(d baosync.Desired, id *v1alpha1.OpenBaoIdentityOIDC, grants []v1alpha1.GuestIdentityGrant) (baosync.Desired, map[string]grantRole, map[string]string) {
	d.IdentityOIDC = true
	key := map[string]any{
		"algorithm":          or(id.Algorithm, DefaultOIDCAlgorithm),
		"rotation_period":    or(id.RotationPeriod, DefaultOIDCRotationPeriod),
		"verification_ttl":   or(id.VerificationTTL, DefaultOIDCVerificationTTL),
		"allowed_client_ids": []string{"*"},
	}
	d.Objects = append(d.Objects, baosync.Object{Path: "identity/oidc/key/" + id.Key, Data: key})
	roles, refused := grantRoles(grants, id.Key, or(id.TokenTTL, DefaultGuestTokenTTL))
	byGrant := map[string]grantRole{}
	for _, r := range roles {
		d.Objects = append(d.Objects, r.object)
		byGrant[r.grant.Name] = r
	}
	d.Policies = append(d.Policies, entityPolicies(roles)...)
	return d, byGrant, refused
}

func grantOutcomes(roles map[string]grantRole, refused map[string]string, grants []v1alpha1.GuestIdentityGrant, synced bool, syncErr string) map[string]v1alpha1.GuestIdentityGrantStatus {
	out := map[string]v1alpha1.GuestIdentityGrantStatus{}
	for _, g := range grants {
		if g.DeletionTimestamp != nil {
			continue
		}
		gen := g.Generation
		st := v1alpha1.GuestIdentityGrantStatus{ObservedGeneration: gen}
		if why, bad := refused[g.Name]; bad {
			msg, conflict := splitGrantRefusal(why)
			st.LastError = msg
			appliedReason := "InvalidGrant"
			appliedMsg := msg
			if conflict {
				appliedReason = "RoleConflict"
				appliedMsg = "another grant already declares this role for the audience"
			}
			conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionApplied, false, appliedReason, appliedMsg)
			if conflict {
				conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionRoleConflict, true, "RoleConflict",
					"another grant already declares this role for the audience; this grant is not applied")

			} else {
				conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionRoleConflict, false, "NotEvaluated",
					"the grant is refused before its role is compared with an existing one")

			}
			conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionDrifted, false, "NotObserved",
				"the grant is not applied, so the live OpenBao state was not compared")

			out[g.Name] = st
			continue
		}
		r, ok := roles[g.Name]
		if !ok {
			continue
		}
		st.Role, st.Audience = r.role, r.audience
		if synced {
			st.Applied = true
			conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionApplied, true, "Applied",
				"the grant's role and policy are applied")

			conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionRoleConflict, false, "NoConflict",
				"the desired role does not conflict with an existing role")

			conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionDrifted, false, "InSync",
				"the live OpenBao role and policy match the grant")

		} else {
			st.LastError = syncErr
			drift := syncErr == grantNotHeldReason
			appliedReason := "SyncFailed"
			msg := syncErr
			if drift {
				appliedReason, msg = "Pending", grantNotHeldReason
			} else if msg == "" {
				msg = "the grant is not applied"
			}
			conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionApplied, false, appliedReason, msg)
			conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionRoleConflict, false, "NoConflict",
				"the desired role does not conflict with an existing role")

			if drift {
				conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionDrifted, true, "CorrectiveWrite",
					"the live OpenBao role or policy differs from the grant; the controller writes the declaration on the next pass")

			} else {
				conds.Set(&st.Conditions, gen, v1alpha1.GuestIdentityGrantConditionDrifted, false, "NotObserved",
					"the live OpenBao state was not compared because the reconcile failed")

			}
		}
		out[g.Name] = st
	}
	return out
}

func MergeGrantStatus(held, next v1alpha1.GuestIdentityGrantStatus, now time.Time) v1alpha1.GuestIdentityGrantStatus {
	if next.Applied && held.Applied && held.Role == next.Role && held.ObservedGeneration == next.ObservedGeneration {
		next.AppliedAt = held.AppliedAt
	}
	if next.Applied && next.AppliedAt == "" {
		next.AppliedAt = now.UTC().Format(time.RFC3339Nano)
	}
	keepConditionTimes(next.Conditions, held.Conditions)
	return next
}

func desiredWithIdentity(ctx context.Context, in layoutInput, ca caSource) (baosync.Desired, []string, map[string]grantRole, map[string]string, error) {
	d, pending, err := desired(ctx, in, ca)
	if err != nil || in.declared == nil || in.declared.IdentityOIDC == nil {
		return d, pending, nil, nil, err
	}
	d, roles, refused := identityOIDC(d, in.declared.IdentityOIDC, in.grants)
	return d, pending, roles, refused, nil
}
