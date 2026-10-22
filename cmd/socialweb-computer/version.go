package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
)

type versionInfo struct {
	Name     string           `json:"name"`
	Version  string           `json:"version"`
	Revision string           `json:"revision,omitempty"`
	Time     string           `json:"time,omitempty"`
	Modified bool             `json:"modified"`
	Go       string           `json:"go"`
	GOOS     string           `json:"goos"`
	GOARCH   string           `json:"goarch"`
	Release  bool             `json:"release"`
	Assets   []assets.Listed  `json:"assets"`
	AssetsBy string           `json:"assetsBy"`
	Dev      *buildstamp.Info `json:"dev,omitempty"`
	Images   []string         `json:"guestImages"`
}

func buildVersion(goos, goarch string) versionInfo {
	v := versionInfo{Name: serviceName, Version: "(devel)", Go: runtime.Version(), GOOS: goos, GOARCH: goarch,
		Release: assets.Release(), Assets: []assets.Listed{}, Dev: buildstamp.Read(), Images: []string{}}
	if names, err := guestimage.Names(); err == nil && names != nil {
		v.Images = names
	}
	if listed, err := assets.ListingFor(goos + "/" + goarch); err == nil {
		v.Assets = listed
	}
	switch {
	case assets.Release():
		v.AssetsBy = "embedded in this release build, extracted to asset-cache/<sha256> and re-hashed before every use"
	default:
		v.AssetsBy = "the pins of versions.json; this dev build embeds none and resolves each executable from its explicit path option, never PATH"
	}
	if version, revision := buildstamp.Identity(); version != "" || revision != "" {
		if version != "" {
			v.Version = version
		}
		if revision != "" {
			v.Revision = revision
		}
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.time":
				v.Time = s.Value
			case "vcs.modified":
				v.Modified = s.Value == "true"
			}
		}
	}
	return v
}

func runVersion(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	v := buildVersion(a.goos, a.goarch)
	if vals.Bool("json") {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintln(a.stdout, string(b))
		return exitOK
	}
	fmt.Fprintf(a.stdout, "%s %s %s %s %s/%s\n", v.Name, v.Version, v.Revision, v.Go, v.GOOS, v.GOARCH)
	if d := v.Dev; d != nil {
		tree := "clean"
		if d.Dirty {
			tree = fmt.Sprintf("dirty (%d changed paths)", d.ChangedPaths)
		}
		fmt.Fprintf(a.stdout, "  dev build, not reproducible: commit %s, tree %s, built %s from %s\n", d.Commit, tree, d.BuiltUTC, d.SourceRoot)
	}
	for _, l := range v.Assets {
		fmt.Fprintf(a.stdout, "  %s %s %s\n", l.Name, l.Version, l.SHA256)
	}
	return exitOK
}
