//go:build ios

package embedded

import (
	"context"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	sandboxembedded "github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/embedded"
)

func policySandboxProvider(PolicySandbox) string {
	return policyv1alpha1.PolicySandboxProviderEmbedded
}

func policySandbox(host PolicySandbox) (sandbox.Runner, func(context.Context, *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error)) {
	return nil, func(ctx context.Context, spec *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error) {
		return sandboxembedded.Open(ctx, host, spec)
	}
}
