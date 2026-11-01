//go:build !ios

package embedded

import (
	"context"
	"fmt"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	sandboxdeno "github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/deno"
	sandboxembedded "github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/embedded"
)

func policySandboxProvider(host PolicySandbox) string {
	if host == nil {
		return ""
	}
	return policyv1alpha1.PolicySandboxProviderEmbedded
}

func policySandbox(host PolicySandbox) (sandbox.Runner, func(context.Context, *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error)) {
	path, _ := deno.Resolve("")
	runner := sandboxdeno.New(path)
	if host == nil {
		return runner, nil
	}
	return runner, func(ctx context.Context, spec *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error) {
		switch name := spec.ProviderName(); name {
		case policyv1alpha1.PolicySandboxProviderEmbedded:
			return sandboxembedded.Open(ctx, host, spec)
		case policyv1alpha1.PolicySandboxProviderDeno:
			return runner, nil
		default:
			return nil, fmt.Errorf("embedded: the %s policy sandbox is not available in an app host", name)
		}
	}
}
