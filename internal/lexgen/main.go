package main

import (
	"flag"
	"fmt"
	"go/format"
	"os"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/lexicons"
)

func main() {
	pkg := flag.String("package", os.Getenv("GOPACKAGE"), "the Go package of the generated file")
	out := flag.String("out", "zz_generated.lexicons.go", "the generated file")
	include := flag.String("include", "", "comma-separated NSIDs, or NSID prefixes ending in a dot, to generate types for")
	trim := flag.String("trim", "", "comma-separated NSID prefixes cut from the front of a type name, first match wins")
	names := flag.String("name", "", "comma-separated nsid#def=GoName overrides of generated type names")
	errVar := flag.String("err", "", "the package error variable that wraps a record decode failure")
	flag.Parse()
	if err := run(*pkg, *out, split(*include), split(*trim), split(*names), *errVar); err != nil {
		fmt.Fprintf(os.Stderr, "lexgen: %v\n", err)
		os.Exit(1)
	}
}

func split(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func run(pkg, out string, include, trim, names []string, errVar string) error {
	if pkg == "" || len(include) == 0 || errVar == "" {
		return fmt.Errorf("-package, -include and -err are required")
	}
	overrides := map[string]string{}
	for _, n := range names {
		ref, name, ok := strings.Cut(n, "=")
		if !ok || name == "" {
			return fmt.Errorf("-name %s is not nsid#def=GoName", n)
		}
		overrides[fullRef("", ref)] = name
	}
	g, err := load(lexicons.FS, include, trim, overrides)
	if err != nil {
		return err
	}
	if err := g.plan(); err != nil {
		return err
	}
	if err := g.check(); err != nil {
		return err
	}
	src := g.emit(pkg, errVar)
	formatted, err := format.Source(src)
	if err != nil {
		return fmt.Errorf("formatting the generated code: %w", err)
	}
	current, err := os.ReadFile(out)
	if err == nil && string(current) == string(formatted) {
		return nil
	}
	return os.WriteFile(out, formatted, 0o644)
}

func trimName(nsid string, trim []string) string {
	for _, p := range trim {
		if rest, ok := strings.CutPrefix(nsid, p); ok && rest != "" {
			return rest
		}
	}
	return nsid
}

func included(nsid string, include []string) bool {
	for _, p := range include {
		if nsid == p || (strings.HasSuffix(p, ".") && strings.HasPrefix(nsid, p)) {
			return true
		}
	}
	return false
}

func fullRef(base, ref string) string {
	switch {
	case strings.HasPrefix(ref, "#"):
		return base + ref
	case strings.Contains(ref, "#"):
		return ref
	default:
		return ref + "#main"
	}
}

func typeValue(ref string) string {
	if nsid, ok := strings.CutSuffix(ref, "#main"); ok {
		return nsid
	}
	return ref
}
