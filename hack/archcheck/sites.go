package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type site struct {
	owner string
	name  string
	class string
	kind  string
	at    string
}

func (c *checker) scanSites() []site {
	var out []site
	for _, top := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(c.root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(c.root, p)
			owner, base := filepath.Dir(rel), filepath.Base(rel)
			for _, decl := range f.Decls {
				name := declName(decl)
				ast.Inspect(decl, func(n ast.Node) bool {
					kind := siteKind(n)
					if kind == "" {
						return true
					}
					class := "requeue"
					if strings.HasPrefix(kind, "time.") {
						class = "ticker"
					}
					out = append(out, site{owner: owner, name: base + ":" + name, class: class, kind: kind,
						at: fmt.Sprintf("%s:%d", rel, fset.Position(n.Pos()).Line)})
					return true
				})
			}
			return nil
		})
	}
	return out
}

func siteKind(n ast.Node) string {
	switch x := n.(type) {
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && (sel.Sel.Name == "NewTicker" || sel.Sel.Name == "Tick") {
				return "time." + sel.Sel.Name
			}
			if sel.Sel.Name == "RefreshAfter" {
				return "RefreshAfter"
			}
		}
	case *ast.KeyValueExpr:
		if k, ok := x.Key.(*ast.Ident); ok && k.Name == "RequeueAfter" && periodic(x.Value) {
			return "RequeueAfter"
		}
	case *ast.AssignStmt:
		for i, l := range x.Lhs {
			if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "RequeueAfter" && i < len(x.Rhs) && periodic(x.Rhs[i]) {
				return "RequeueAfter"
			}
		}
	}
	return ""
}

func periodic(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Value != "0"
	case *ast.SelectorExpr:
		return v.Sel.Name != "RequeueAfter"
	}
	return true
}

func declName(d ast.Decl) string {
	switch x := d.(type) {
	case *ast.FuncDecl:
		if x.Recv != nil && len(x.Recv.List) > 0 {
			t := x.Recv.List[0].Type
			if s, ok := t.(*ast.StarExpr); ok {
				t = s.X
			}
			if ix, ok := t.(*ast.IndexExpr); ok {
				t = ix.X
			}
			if ix, ok := t.(*ast.IndexListExpr); ok {
				t = ix.X
			}
			if id, ok := t.(*ast.Ident); ok {
				return id.Name + "." + x.Name.Name
			}
		}
		return x.Name.Name
	case *ast.GenDecl:
		for _, s := range x.Specs {
			if v, ok := s.(*ast.ValueSpec); ok && len(v.Names) > 0 {
				return v.Names[0].Name
			}
		}
	}
	return "_"
}

var listedClass = map[string]string{"ticker": "ticker", "requeue": "requeue", "deadline": "requeue"}

const obligationsFile = "deploy/obligations.txt"

func (c *checker) obligations() {
	f, err := os.Open(filepath.Join(c.root, obligationsFile))
	if err != nil {
		c.fail("obligations", obligationsFile, "%v", err)
		return
	}
	defer f.Close()
	type key struct{ owner, name, class string }
	listed := map[key][]int{}
	scanner := bufio.NewScanner(f)
	n := 0
	for scanner.Scan() {
		n++
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" || strings.HasPrefix(line, "owner\t") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 5 {
			c.fail("obligations", fmt.Sprintf("%s:%d", obligationsFile, n), "a line has owner, site, kind, period and reason")
			continue
		}
		class, scanned := listedClass[cols[2]]
		if !scanned {
			continue
		}
		k := key{cols[0], cols[1], class}
		listed[k] = append(listed[k], n)
	}
	found := map[key][]site{}
	for _, s := range c.scanSites() {
		k := key{s.owner, s.name, s.class}
		found[k] = append(found[k], s)
	}
	for k, sites := range found {
		for i := len(listed[k]); i < len(sites); i++ {
			s := sites[i]
			c.fail("obligations", s.at, "%s site %s\t%s is not listed in %s as %s", s.kind, s.owner, s.name, obligationsFile, kindsOf(k.class))
		}
	}
	for k, lines := range listed {
		for i := len(found[k]); i < len(lines); i++ {
			c.fail("obligations", fmt.Sprintf("%s:%d", obligationsFile, lines[i]), "lists %s\t%s (%s) but the tree has no such site", k.owner, k.name, kindsOf(k.class))
		}
	}
}

func kindsOf(class string) string {
	var out []string
	for k, v := range listedClass {
		if v == class {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " or ")
}
