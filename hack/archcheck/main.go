package main

import (
	"flag"
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

type finding struct {
	rule string
	at   string
	msg  string
}

type checker struct {
	root     string
	module   string
	fset     *token.FileSet
	pkgs     []*packages.Package
	byPath   map[string]*packages.Package
	findings []finding
}

func (c *checker) fail(rule, at, format string, args ...any) {
	c.findings = append(c.findings, finding{rule: rule, at: at, msg: fmt.Sprintf(format, args...)})
}

func (c *checker) rel(path string) string {
	if path == c.module {
		return "."
	}
	return strings.TrimPrefix(path, c.module+"/")
}

func (c *checker) local(path string) bool {
	return path == c.module || strings.HasPrefix(path, c.module+"/")
}

func (c *checker) pos(p token.Pos) string {
	at := c.fset.Position(p)
	if r, err := filepath.Rel(c.root, at.Filename); err == nil {
		at.Filename = r
	}
	return fmt.Sprintf("%s:%d", at.Filename, at.Line)
}

func under(rel string, prefixes ...string) bool {
	for _, p := range prefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

var rules = []struct {
	name string
	run  func(*checker)
}{
	{"layers", (*checker).layers},
	{"backend", (*checker).backends},
	{"obligations", (*checker).obligations},
	{"system-caller", (*checker).systemCallers},
	{"dynamic", (*checker).dynamicClients},
	{"gvr", (*checker).gvrLiterals},
	{"annotation", (*checker).annotationReads},
	{"endpoint", (*checker).endpoints},
	{"assertion", (*checker).assertions},
}

func main() {
	root := flag.String("root", ".", "the repository root")
	only := flag.String("rules", "", "comma-separated rules to run; empty runs every rule")
	flag.Parse()
	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-architecture: %v\n", err)
		os.Exit(2)
	}
	c := &checker{root: abs, fset: token.NewFileSet(), byPath: map[string]*packages.Package{}}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports | packages.NeedDeps | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedModule,
		Dir:  abs,
		Fset: c.fset,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-architecture: loading the packages: %v\n", err)
		os.Exit(2)
	}
	broken := 0
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		c.byPath[p.PkgPath] = p
		if p.Module != nil && p.Module.Main && c.module == "" {
			c.module = p.Module.Path
		}
	})
	for _, p := range pkgs {
		for _, e := range p.Errors {
			fmt.Fprintf(os.Stderr, "check-architecture: %s: %v\n", p.PkgPath, e)
			broken++
		}
	}
	if broken > 0 {
		os.Exit(2)
	}
	c.pkgs = pkgs
	selected := map[string]bool{}
	for _, r := range strings.Split(*only, ",") {
		if r = strings.TrimSpace(r); r != "" {
			selected[r] = true
		}
	}
	ran := 0
	for _, r := range rules {
		if len(selected) > 0 && !selected[r.name] {
			continue
		}
		r.run(c)
		ran++
	}
	sort.SliceStable(c.findings, func(i, j int) bool {
		if c.findings[i].rule != c.findings[j].rule {
			return c.findings[i].rule < c.findings[j].rule
		}
		return c.findings[i].at < c.findings[j].at
	})
	for _, f := range c.findings {
		fmt.Fprintf(os.Stderr, "check-architecture: FAIL [%s] %s: %s\n", f.rule, f.at, f.msg)
	}
	fmt.Printf("check-architecture: %d rules, %d errors\n", ran, len(c.findings))
	if len(c.findings) > 0 {
		os.Exit(1)
	}
}
