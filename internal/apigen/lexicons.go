package apigen

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const lexiconMarker = "+socialweb:lexicon="

type lexiconDef struct {
	Type        string                 `json:"type"`
	Ref         string                 `json:"ref"`
	Properties  map[string]*lexiconDef `json:"properties"`
	Items       *lexiconDef            `json:"items"`
	KnownValues []string               `json:"knownValues"`
	Enum        []string               `json:"enum"`
	Minimum     *int64                 `json:"minimum"`
	Maximum     *int64                 `json:"maximum"`
}

type lexiconDoc struct {
	ID   string                 `json:"id"`
	Defs map[string]*lexiconDef `json:"defs"`
}

type lexiconSet struct {
	root string
	docs map[string]*lexiconDoc
}

func readLexiconMarkers(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE || gen.Doc == nil || len(gen.Specs) != 1 {
				continue
			}
			for _, c := range gen.Doc.List {
				if ref, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(c.Text, "//")), lexiconMarker); ok {
					out[gen.Specs[0].(*ast.TypeSpec).Name.Name] = strings.TrimSpace(ref)
				}
			}
		}
	}
	return out, nil
}

func (l *lexiconSet) def(base, ref string) (*lexiconDef, string, error) {
	nsid, name, ok := strings.Cut(ref, "#")
	if !ok {
		name = "main"
	}
	if nsid == "" {
		nsid = base
	}
	doc, ok := l.docs[nsid]
	if !ok {
		data, err := os.ReadFile(filepath.Join(l.root, "lexicons", filepath.FromSlash(strings.ReplaceAll(nsid, ".", "/"))+".json"))
		if err != nil {
			return nil, "", fmt.Errorf("lexicon %s: %w", nsid, err)
		}
		doc = &lexiconDoc{}
		if err := json.Unmarshal(data, doc); err != nil {
			return nil, "", fmt.Errorf("lexicon %s: %w", nsid, err)
		}
		l.docs[nsid] = doc
	}
	d, ok := doc.Defs[name]
	if !ok {
		return nil, "", fmt.Errorf("lexicon %s has no def %s", nsid, name)
	}
	return d, nsid, nil
}

func (l *lexiconSet) compare(s *Schema, d *lexiconDef, base, path string) []error {
	if d.Type == "ref" {
		next, nsid, err := l.def(base, d.Ref)
		if err != nil {
			return []error{fmt.Errorf("%s: %w", path, err)}
		}
		return l.compare(s, next, nsid, path)
	}
	mismatch := func(format string, args ...any) []error {
		return []error{fmt.Errorf("%s: "+format, append([]any{path}, args...)...)}
	}
	if s.Type != d.Type {
		return mismatch("the Go type is %s, the lexicon says %s", s.Type, d.Type)
	}
	switch d.Type {
	case TypeObject:
		var errs []error
		goProps := map[string]*Schema{}
		for _, p := range s.Properties {
			goProps[p.Name] = p.Schema
		}
		names := make([]string, 0, len(goProps)+len(d.Properties))
		for n := range goProps {
			names = append(names, n)
		}
		for n := range d.Properties {
			if _, ok := goProps[n]; !ok {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			gs, lp := goProps[n], d.Properties[n]
			switch {
			case gs == nil:
				errs = append(errs, fmt.Errorf("%s.%s: the lexicon has the field, the Go type does not", path, n))
			case lp == nil:
				errs = append(errs, fmt.Errorf("%s.%s: the Go type has the field, the lexicon does not", path, n))
			default:
				errs = append(errs, l.compare(gs, lp, base, path+"."+n)...)
			}
		}
		return errs
	case TypeArray:
		if s.Items == nil || d.Items == nil {
			return mismatch("an array without items")
		}
		return l.compare(s.Items, d.Items, base, path+"[]")
	case TypeString:
		values := append(slices.Clone(d.KnownValues), d.Enum...)
		if len(s.Enum) == 0 && len(values) == 0 {
			return nil
		}
		if !sameFolded(s.Enum, values) {
			return mismatch("the Go enum is %v, the lexicon values are %v", s.Enum, values)
		}
	case TypeInteger:
		if s.Minimum != nil && d.Minimum != nil && *s.Minimum != *d.Minimum {
			return mismatch("the Go minimum is %d, the lexicon minimum is %d", *s.Minimum, *d.Minimum)
		}
		if s.Maximum != nil && d.Maximum != nil && *s.Maximum != *d.Maximum {
			return mismatch("the Go maximum is %d, the lexicon maximum is %d", *s.Maximum, *d.Maximum)
		}
	case TypeBoolean:
	default:
		return mismatch("the lexicon type %s has no Go mapping", d.Type)
	}
	return nil
}

func sameFolded(a, b []string) bool {
	fold := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, v := range in {
			out = append(out, strings.ToLower(v))
		}
		sort.Strings(out)
		return slices.Compact(out)
	}
	return slices.Equal(fold(a), fold(b))
}

func checkLexicons(root, dir string, named map[string]*Schema) error {
	markers, err := readLexiconMarkers(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return err
	}
	set := &lexiconSet{root: root, docs: map[string]*lexiconDoc{}}
	types := make([]string, 0, len(markers))
	for t := range markers {
		types = append(types, t)
	}
	sort.Strings(types)
	var errs []error
	for _, t := range types {
		ref := markers[t]
		s, ok := named[t]
		if !ok {
			errs = append(errs, fmt.Errorf("%s names the lexicon %s but no resource reaches the type", t, ref))
			continue
		}
		d, nsid, err := set.def("", ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t, err))
			continue
		}
		for _, e := range set.compare(s, d, nsid, t) {
			errs = append(errs, fmt.Errorf("%s and the lexicon %s diverge: %w", t, ref, e))
		}
	}
	return errors.Join(errs...)
}
