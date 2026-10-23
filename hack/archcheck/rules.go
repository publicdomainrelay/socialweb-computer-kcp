package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apigen"
)

const bootstrapFile = "deploy/bus-bootstrap-callers.txt"

var annotationConventions = map[string]string{
	"kcp.io/path":                       "kcp writes the logical cluster path; read to resolve a cluster name to its workspace path",
	"kcp.io/cluster":                    "kcp writes the logical cluster of every object; read to address the object's workspace",
	"experimental.tenancy.kcp.io/owner": "kcp admission writes the workspace owner; read to keep it when re-applying a Workspace",
	"kubernetes.io/config.mirror":       "the kubelet marks static mirror pods; a drain skips them like kubectl does",
	"objectset.rio.cattle.io/owner-gvk": "k3s marks the objects its addon controller owns; read to leave k3s-owned objects alone",
}

func (c *checker) eachFile(fn func(p *packages.Package, f *ast.File)) {
	for _, p := range c.sortedLocal() {
		for _, f := range p.Syntax {
			fn(p, f)
		}
	}
}

func (c *checker) systemCallers() {
	allowed := map[string]bool{}
	if f, err := os.Open(filepath.Join(c.root, bootstrapFile)); err != nil {
		c.fail("system-caller", bootstrapFile, "%v", err)
	} else {
		scanner := bufio.NewScanner(f)
		header := true
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if header {
				header = false
				continue
			}
			cols := strings.Split(line, "\t")
			if len(cols) >= 2 {
				allowed[strings.TrimSpace(cols[0])+"\t"+strings.TrimSpace(cols[1])] = true
			}
		}
		f.Close()
	}
	bus := c.module + "/internal/bus"
	c.eachFile(func(p *packages.Package, f *ast.File) {
		if p.PkgPath == bus {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				fn := calledFunc(p.TypesInfo, x.Fun)
				if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != bus || fn.Name() != "WithSystemCaller" {
					return true
				}
				name := ""
				if len(x.Args) == 2 {
					if tv, ok := p.TypesInfo.Types[x.Args[1]]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
						name = strings.TrimSpace(constant.StringVal(tv.Value))
					}
				}
				switch {
				case name == "":
					c.fail("system-caller", c.pos(x.Pos()), "bus.WithSystemCaller takes a constant caller name listed in %s", bootstrapFile)
				case !allowed[name+"\t"+c.rel(p.PkgPath)]:
					c.fail("system-caller", c.pos(x.Pos()), "System caller %q in %s is not listed in %s", name, c.rel(p.PkgPath), bootstrapFile)
				}
			case *ast.Ident:
				if obj, ok := p.TypesInfo.Uses[x].(*types.Const); ok && obj.Pkg() != nil && obj.Pkg().Path() == bus && obj.Name() == "SystemKind" {
					c.fail("system-caller", c.pos(x.Pos()), "bus.SystemKind outside internal/bus; a System caller is made only by bus.WithSystemCaller from a listed caller")
				}
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok && k.Name == "Kind" {
					if tv, ok := p.TypesInfo.Types[x.Value]; ok && tv.Value != nil && tv.Value.Kind() == constant.String && strings.EqualFold(constant.StringVal(tv.Value), "System") {
						c.fail("system-caller", c.pos(x.Pos()), "a caller ref of kind System outside internal/bus; use bus.WithSystemCaller from a listed caller")
					}
				}
			}
			return true
		})
	})
}

func calledFunc(info *types.Info, fun ast.Expr) *types.Func {
	switch f := fun.(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := info.Uses[f.Sel].(*types.Func)
		return fn
	case *ast.IndexExpr:
		return calledFunc(info, f.X)
	case *ast.IndexListExpr:
		return calledFunc(info, f.X)
	}
	return nil
}

var dynamicImports = []string{"k8s.io/client-go/dynamic", "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"}

func (c *checker) dynamicClients() {
	for _, top := range []string{"api", "cmd", "hack", "internal", "test"} {
		_ = filepath.WalkDir(filepath.Join(c.root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return nil
			}
			rel, _ := filepath.Rel(c.root, p)
			dir := filepath.ToSlash(filepath.Dir(rel))
			if under(dir, "internal/controllers/runtime", "internal/apigen") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
			if err != nil {
				return nil
			}
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if under(path, dynamicImports...) {
					c.fail("dynamic", fmt.Sprintf("%s:%d", rel, fset.Position(imp.Pos()).Line),
						"imports %s; only internal/controllers/runtime and internal/apigen use a dynamic client or unstructured objects (use kube.Resource with a typed object)", path)
				}
			}
			return nil
		})
	}
}

func isNamed(t types.Type, pkg, name string) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

const schemaPkg = "k8s.io/apimachinery/pkg/runtime/schema"

func (c *checker) gvrLiterals() {
	c.eachFile(func(p *packages.Package, f *ast.File) {
		if under(c.rel(p.PkgPath), "api") {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if len(x.Elts) > 0 && isNamed(p.TypesInfo.TypeOf(x), schemaPkg, "GroupVersionResource") {
					c.fail("gvr", c.pos(x.Pos()), "a schema.GroupVersionResource literal outside api/; use the generated <Kind>GVR var or api/kcpgvr")
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "WithResource" || len(x.Args) != 1 {
					return true
				}
				if fn := calledFunc(p.TypesInfo, x.Fun); fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != schemaPkg {
					return true
				}
				if tv, ok := p.TypesInfo.Types[x.Args[0]]; ok && tv.Value != nil {
					c.fail("gvr", c.pos(x.Pos()), "a GroupVersionResource built from a constant resource outside api/; use the generated <Kind>GVR var or api/kcpgvr")
				}
			}
			return true
		})
	})
}

func annotationSource(info *types.Info, e ast.Expr, marked map[types.Object]bool) bool {
	e = ast.Unparen(e)
	if t := info.TypeOf(e); t == nil || !isStringMap(t) {
		return false
	}
	switch x := e.(type) {
	case *ast.SelectorExpr:
		if s, ok := info.Selections[x]; ok && s.Kind() == types.FieldVal && s.Obj().Name() == "Annotations" {
			return true
		}
	case *ast.CallExpr:
		if fn := calledFunc(info, x.Fun); fn != nil && fn.Name() == "GetAnnotations" {
			return true
		}
	case *ast.Ident:
		if obj := info.Uses[x]; obj != nil && marked[obj] {
			return true
		}
	}
	return false
}

func isStringMap(t types.Type) bool {
	m, ok := t.Underlying().(*types.Map)
	if !ok {
		return false
	}
	k, kok := m.Key().Underlying().(*types.Basic)
	v, vok := m.Elem().Underlying().(*types.Basic)
	return kok && vok && k.Kind() == types.String && v.Kind() == types.String
}

func (c *checker) annotationReads() {
	c.eachFile(func(p *packages.Package, f *ast.File) {
		info := p.TypesInfo
		marked := map[types.Object]bool{}
		writes := map[ast.Expr]bool{}
		mark := func(lhs []ast.Expr, rhs []ast.Expr) {
			if len(lhs) != len(rhs) {
				return
			}
			for i, l := range lhs {
				id, ok := l.(*ast.Ident)
				if !ok || !annotationSource(info, rhs[i], marked) {
					continue
				}
				if obj := info.Defs[id]; obj != nil {
					marked[obj] = true
				} else if obj := info.Uses[id]; obj != nil {
					marked[obj] = true
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					writes[l] = true
				}
				mark(x.Lhs, x.Rhs)
			case *ast.ValueSpec:
				lhs := make([]ast.Expr, len(x.Names))
				for i, id := range x.Names {
					lhs[i] = id
				}
				mark(lhs, x.Values)
			}
			return true
		})
		report := func(at token.Pos, key ast.Expr) {
			value := types.ExprString(key)
			if tv, ok := info.Types[key]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
				value = constant.StringVal(tv.Value)
				if _, ok := annotationConventions[value]; ok {
					return
				}
			}
			c.fail("annotation", c.pos(at), "reads annotation %s as a signal; carry it in a spec or status field, a label or an ownerReference, or list a metadata convention in hack/archcheck", strconv.Quote(value))
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.IndexExpr:
				if !writes[x] && annotationSource(info, x.X, marked) {
					report(x.Pos(), x.Index)
				}
			case *ast.CallExpr:
				fn := calledFunc(info, x.Fun)
				if fn == nil || fn.Pkg() == nil {
					return true
				}
				switch {
				case fn.Name() == "HasAnnotation" && fn.Pkg().Path() == "k8s.io/apimachinery/pkg/apis/meta/v1" && len(x.Args) == 2:
					report(x.Pos(), x.Args[1])
				case strings.HasPrefix(fn.Name(), "Nested") && fn.Pkg().Path() == "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured":
					for i := 1; i+2 < len(x.Args); i++ {
						a, b := constString(info, x.Args[i]), constString(info, x.Args[i+1])
						if a == "metadata" && b == "annotations" {
							report(x.Pos(), x.Args[i+2])
						}
					}
				}
			}
			return true
		})
	})
}

func constString(info *types.Info, e ast.Expr) string {
	if tv, ok := info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value)
	}
	return ""
}

func (c *checker) endpoints() {
	if _, err := apigen.Generate(c.root); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				c.fail("endpoint", "api", "%s", line)
			}
		}
	}
}

func (c *checker) implPackage(path string) bool {
	if !c.isBackend(path) {
		return false
	}
	rel := c.rel(path)
	return !under(rel, hostLibraries...) && strings.Count(rel, "/") == 2
}

func (c *checker) assertions() {
	contract := c.module + "/internal/contract/"
	for _, p := range c.sortedLocal() {
		if !c.implPackage(p.PkgPath) {
			continue
		}
		found := false
		for _, f := range p.Syntax {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, s := range gd.Specs {
					vs := s.(*ast.ValueSpec)
					if vs.Type == nil || len(vs.Names) != 1 || vs.Names[0].Name != "_" {
						continue
					}
					n, ok := types.Unalias(p.TypesInfo.TypeOf(vs.Type)).(*types.Named)
					if !ok || n.Obj().Pkg() == nil || !strings.HasPrefix(n.Obj().Pkg().Path(), contract) {
						continue
					}
					if _, ok := n.Underlying().(*types.Interface); ok {
						found = true
					}
				}
			}
		}
		if !found {
			c.fail("assertion", c.rel(p.PkgPath), "an impl package states its contract at compile time: var _ <contract>.<Interface> = (*Impl)(nil) against an internal/contract interface")
		}
	}
}
