//go:build !js

package componentcore

import (
	"os"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	sandboxdeno "github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/deno"
)

func newSandbox() sandbox.Runner {
	path, _ := deno.Resolve("")
	r := sandboxdeno.New(path)
	if exe, err := os.Executable(); err == nil {
		r.Fence = append([]string{exe}, sandboxdeno.FenceVerb...)
	}
	return r
}
