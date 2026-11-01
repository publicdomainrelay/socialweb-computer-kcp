//go:build ios

package embedded

import (
	"context"
	"errors"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

var ErrNoPolicySandbox = errors.New("embedded: iOS has no policy sandbox yet (plan 0038 I6), so every policy decision is a Deny")

func policySandbox() (sandbox.Runner, func(context.Context, *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error)) {
	return nil, func(context.Context, *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error) {
		return nil, ErrNoPolicySandbox
	}
}
