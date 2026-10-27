//go:build !js

package componentcore

import (
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/deno"
	sandboxdeno "github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/deno"
)

func newSandbox() sandbox.Runner {
	path, _ := deno.Resolve("")
	return sandboxdeno.New(path)
}
