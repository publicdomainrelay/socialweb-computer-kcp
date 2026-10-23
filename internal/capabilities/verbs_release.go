//go:build release

package capabilities

var buildVerbs = []Verb{
	{Path: "dev verify-assets", Summary: "extract every embedded asset of this release build into a scratch cache and verify each tree and executable against its pin (release builds only)"},
}
