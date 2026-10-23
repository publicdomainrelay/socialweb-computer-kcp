//go:build !release

package capabilities

var buildVerbs = []Verb{
	{Path: "nodeimage guest-rootfs", Summary: "build pipeline: compose the guest rootfs tar (scripts/build-guest-image.sh; dev builds only)"},
	{Path: "nodeimage guest-manifest", Summary: "build pipeline: write image.json for a guest image (scripts/build-guest-image.sh; dev builds only)"},
}
