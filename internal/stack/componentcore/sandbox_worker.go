//go:build js && wasm

package componentcore

import (
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/worker"
)

func newSandbox() sandbox.Runner { return worker.New() }
