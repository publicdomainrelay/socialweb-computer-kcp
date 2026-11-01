//go:build !ios

package embedded

import (
	"context"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	sandboxdeno "github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/deno"
)

func policySandbox() (sandbox.Runner, func(context.Context, *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error)) {
	path, _ := deno.Resolve("")
	return sandboxdeno.New(path), nil
}
