//go:build release

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/pinneddownloads"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/guestcontrolplane"
)

func releaseBuiltVerbs() map[string]builtVerb {
	return map[string]builtVerb{
		"dev verify-assets": {[]string{"asset-cache"}, runDevVerifyAssets},
	}
}

// runDevVerifyAssets extracts every embedded asset under the build's own checks (the tree digest of each against its
// versions.json pin or, for an asset built from this commit, the build record embedded beside its blob), then loads the
// two downloads trees the way the services do (the GuestControlPlane and the web app: each file against its sidecar).
// --asset-cache DIR keeps the extraction in DIR (tests/compositions/0018 tampers a tree there and runs this again: an
// extracted tree that no longer matches its pin is replaced from the embedded blob, never used); without it a scratch
// directory is removed afterwards.
func runDevVerifyAssets(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	cache := ""
	if vals.String("asset-cache") != "" {
		c, err := a.assetCache(vals)
		if err != nil {
			return a.fail(exitFailed, "dev verify-assets: %v", err)
		}
		cache = c
	} else {
		scratch, err := os.MkdirTemp("", "socialweb-verify-assets-")
		if err != nil {
			return a.fail(exitFailed, "dev verify-assets: %v", err)
		}
		defer assets.RemoveTree(scratch)
		cache = filepath.Join(scratch, "runtime")
	}
	got, err := assets.VerifyEmbedded(cache)
	if err != nil {
		return a.fail(exitFailed, "dev verify-assets (%s): %v", assets.Platform(), err)
	}
	for _, v := range got {
		dir, ok := v.Executables[v.Asset]
		if !ok {
			continue
		}
		var n int
		switch v.Asset {
		case "socialweb-guest-downloads":
			set, err := pinneddownloads.LoadRequired(dir, guestcontrolplane.RequiredDownloads)
			if err != nil {
				return a.fail(exitFailed, "dev verify-assets: %s: the GuestControlPlane refuses its downloads dir: %v", v.Asset, err)
			}
			n = len(set.List())
		case "socialweb-computer-k8s-downloads":
			set, err := pinneddownloads.Load(dir)
			if err != nil {
				return a.fail(exitFailed, "dev verify-assets: %s: the web app refuses its downloads dir: %v", v.Asset, err)
			}
			n = len(set.List())
		default:
			continue
		}
		fmt.Fprintf(a.stderr, "dev verify-assets: %s: %d pinned downloads, each file hashes to its sidecar (tree %s)\n", v.Asset, n, v.TreeSHA256)
	}
	a.printJSON(got)
	return exitOK
}
