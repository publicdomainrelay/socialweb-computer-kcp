package spec

import (
	"testing"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/policytest"
)

type trustPolicy struct {
	family string
	want   func(policytest.TrustCase) bool
}

var trustPolicies = []trustPolicy{
	{family: "tangled-vouch", want: func(c policytest.TrustCase) bool { return c.Vouch }},
	{family: "only-me", want: func(c policytest.TrustCase) bool { return c.OnlyMe }},
	{family: "mutuals", want: func(c policytest.TrustCase) bool { return c.Mutuals }},
	{family: "open", want: func(policytest.TrustCase) bool { return true }},
}

func workflowOf(family string, perspective side) string {
	if family == "open" {
		return "open"
	}
	return string(perspective) + "-" + family
}

func TestTrustPolicyMatrix(t *testing.T) {
	t.Parallel()
	for _, policy := range trustPolicies {
		for _, perspective := range []side{bidderSide, requesterSide} {
			for _, mode := range []lane{scope, full} {
				for _, tc := range policytest.TrustCases {
					workflow := workflowOf(policy.family, perspective)
					t.Run("matrix/"+workflow+"/"+string(mode)+"/"+string(perspective)+"/"+tc.Name, func(t *testing.T) {
						t.Parallel()
						h := policytest.New(t)
						p := policytest.PartiesFor(string(perspective))
						tc.Build(h.World, p)
						got := evaluate(t, h, workflow, perspective, mode, p.Self, p.CP, nil)
						ran := got.mustRun(t, workflow)
						want := policy.want(tc)
						actionRan(t, ran, workflow, want)
						if want {
							got.mustAllow(t)
						} else {
							got.mustDeny(t, "")
						}
					})
				}
			}
		}
	}
}
