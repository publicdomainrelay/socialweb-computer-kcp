//go:build release

package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
)

func releaseBuiltVerbs() map[string]builtVerb {
	return map[string]builtVerb{
		"dev verify-assets": {nil, runDevVerifyAssets},
	}
}

func runDevVerifyAssets(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	scratch, err := os.MkdirTemp("", "socialweb-verify-assets-")
	if err != nil {
		return a.fail(exitFailed, "dev verify-assets: %v", err)
	}
	defer assets.RemoveTree(scratch)
	got, err := assets.VerifyEmbedded(filepath.Join(scratch, "runtime"))
	if err != nil {
		return a.fail(exitFailed, "dev verify-assets (%s): %v", assets.Platform(), err)
	}
	a.printJSON(got)
	return exitOK
}
