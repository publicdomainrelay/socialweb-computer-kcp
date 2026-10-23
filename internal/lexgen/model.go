package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/bluesky-social/indigo/atproto/lexicon"
)

type kind int

const (
	kindRecord kind = iota
	kindObject
	kindArray
	kindUnion
)

type shape int

const (
	shapeScalar shape = iota
	shapeSlice
	shapeStruct
)

type field struct {
	name      string
	json      string
	goType    string
	omitempty bool
}

type variant struct {
	goType    string
	typeValue string
}

type goType struct {
	name     string
	ref      string
	base     string
	kind     kind
	obj      lexicon.SchemaObject
	items    lexicon.SchemaDef
	union    lexicon.SchemaUnion
	fields   []field
	elem     string
	variants []variant
	open     bool
}

type gen struct {
	catalog  *lexicon.BaseCatalog
	types    map[string]*goType
	names    map[string]bool
	order    []*goType
	variants map[string]bool
}

func load(fsys fs.FS, include, trim []string, overrides map[string]string) (*gen, error) {
	g := &gen{
		catalog:  lexicon.NewBaseCatalog(),
		types:    map[string]*goType{},
		names:    map[string]bool{},
		variants: map[string]bool{},
	}
	files := map[string]lexicon.SchemaFile{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		var sf lexicon.SchemaFile
		if err := json.Unmarshal(b, &sf); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if err := g.catalog.AddSchemaFile(sf); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		files[sf.ID] = sf
		return nil
	})
	if err != nil {
		return nil, err
	}
	ids := slices.Sorted(maps.Keys(files))
	for _, p := range include {
		if !slices.ContainsFunc(ids, func(id string) bool { return included(id, []string{p}) }) {
			return nil, fmt.Errorf("-include %s matches no lexicon", p)
		}
	}
	for _, id := range ids {
		if !included(id, include) {
			continue
		}
		sf := files[id]
		for _, def := range defNames(sf) {
			ref := id + "#" + def
			name := exported(trimName(id, trim))
			if def != "main" {
				name += exported(def)
			}
			if override, ok := overrides[ref]; ok {
				name = override
				delete(overrides, ref)
			}
			switch v := sf.Defs[def].Inner.(type) {
			case lexicon.SchemaRecord:
				err = g.add(&goType{name: name, ref: ref, base: id, kind: kindRecord, obj: v.Record})
			case lexicon.SchemaObject:
				err = g.add(&goType{name: name, ref: ref, base: id, kind: kindObject, obj: v})
			case lexicon.SchemaArray:
				err = g.add(&goType{name: name, ref: ref, base: id, kind: kindArray, items: v.Items})
			}
			if err != nil {
				return nil, err
			}
		}
	}
	for ref := range overrides {
		return nil, fmt.Errorf("-name %s names no generated definition", ref)
	}
	return g, nil
}

func defNames(sf lexicon.SchemaFile) []string {
	names := slices.Sorted(maps.Keys(sf.Defs))
	if i := slices.Index(names, "main"); i > 0 {
		names = append([]string{"main"}, slices.Delete(names, i, i+1)...)
	}
	return names
}

func (g *gen) add(t *goType) error {
	if g.names[t.name] {
		return fmt.Errorf("two lexicon definitions map to the Go type %s", t.name)
	}
	g.names[t.name] = true
	if t.ref != "" {
		g.types[t.ref] = t
	}
	g.order = append(g.order, t)
	return nil
}

func (g *gen) plan() error {
	for i := 0; i < len(g.order); i++ {
		t := g.order[i]
		var err error
		switch t.kind {
		case kindRecord, kindObject:
			err = g.planObject(t)
		case kindArray:
			t.elem, _, err = g.expr(t.items.Inner, t.base, itemName(t.name))
		case kindUnion:
			err = g.planUnion(t)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", cmp.Or(t.ref, t.name), err)
		}
	}
	return nil
}

func (g *gen) check() error {
	for _, t := range g.order {
		if t.kind != kindRecord && t.kind != kindObject {
			continue
		}
		taken := map[string]bool{"Extra": true}
		if t.kind == kindRecord || g.variants[t.ref] {
			taken["Type"] = true
		}
		if t.kind == kindRecord {
			taken["NSID"] = true
		}
		for _, f := range t.fields {
			if taken[f.name] {
				return fmt.Errorf("%s: property %s maps to the Go field %s, which is taken", cmp.Or(t.ref, t.name), f.json, f.name)
			}
			taken[f.name] = true
		}
	}
	return nil
}

func (g *gen) planObject(t *goType) error {
	for _, prop := range slices.Sorted(maps.Keys(t.obj.Properties)) {
		required := slices.Contains(t.obj.Required, prop)
		nullable := slices.Contains(t.obj.Nullable, prop)
		expr, sh, err := g.expr(t.obj.Properties[prop].Inner, t.base, t.name+exported(prop))
		if err != nil {
			return fmt.Errorf("%s: %w", prop, err)
		}
		f := field{name: exported(prop), json: prop, goType: expr}
		switch {
		case sh == shapeSlice:
			f.omitempty = !(required && nullable)
		case required && nullable:
			f.goType = "*" + expr
		case required:
		case sh == shapeStruct:
			f.goType = "*" + expr
			f.omitempty = true
		default:
			f.omitempty = true
		}
		t.fields = append(t.fields, f)
	}
	return nil
}

func (g *gen) planUnion(t *goType) error {
	t.open = t.union.Closed == nil || !*t.union.Closed
	for _, r := range t.union.Refs {
		full := fullRef(t.base, r)
		target, err := g.resolve(full)
		if err != nil {
			return err
		}
		if target.kind != kindRecord && target.kind != kindObject {
			return fmt.Errorf("union variant %s is not an object", full)
		}
		g.variants[full] = true
		t.variants = append(t.variants, variant{goType: target.name, typeValue: typeValue(full)})
	}
	return nil
}

func (g *gen) resolve(full string) (*goType, error) {
	if _, err := g.catalog.Resolve(full); err != nil {
		return nil, err
	}
	target := g.types[full]
	if target == nil {
		return nil, fmt.Errorf("ref %s is outside the generated lexicons; add it to -include", full)
	}
	return target, nil
}

func (g *gen) expr(inner any, base, nested string) (string, shape, error) {
	switch v := inner.(type) {
	case lexicon.SchemaString:
		return "string", shapeScalar, nil
	case lexicon.SchemaInteger:
		return "int64", shapeScalar, nil
	case lexicon.SchemaBoolean:
		return "bool", shapeScalar, nil
	case lexicon.SchemaBytes:
		return "atdata.Bytes", shapeSlice, nil
	case lexicon.SchemaCIDLink:
		return "atdata.CIDLink", shapeStruct, nil
	case lexicon.SchemaBlob:
		return "atdata.Blob", shapeStruct, nil
	case lexicon.SchemaUnknown:
		return "json.RawMessage", shapeSlice, nil
	case lexicon.SchemaRef:
		target, err := g.resolve(fullRef(base, v.Ref))
		if err != nil {
			return "", 0, err
		}
		if target.kind == kindArray {
			return target.name, shapeSlice, nil
		}
		return target.name, shapeStruct, nil
	case lexicon.SchemaObject:
		return nested, shapeStruct, g.add(&goType{name: nested, base: base, kind: kindObject, obj: v})
	case lexicon.SchemaUnion:
		return nested, shapeStruct, g.add(&goType{name: nested, base: base, kind: kindUnion, union: v})
	case lexicon.SchemaArray:
		elem, _, err := g.expr(v.Items.Inner, base, itemName(nested))
		return "[]" + elem, shapeSlice, err
	default:
		return "", 0, fmt.Errorf("lexicon type %T has no Go type", inner)
	}
}

func itemName(name string) string {
	if trimmed, ok := strings.CutSuffix(name, "s"); ok && trimmed != "" {
		return trimmed
	}
	return name + "Item"
}

var initialisms = map[string]string{
	"api":  "API",
	"cid":  "CID",
	"did":  "DID",
	"http": "HTTP",
	"id":   "ID",
	"json": "JSON",
	"nsid": "NSID",
	"pds":  "PDS",
	"rfp":  "RFP",
	"ssh":  "SSH",
	"uri":  "URI",
	"url":  "URL",
	"vm":   "VM",
}

func exported(s string) string {
	var b strings.Builder
	for _, w := range words(s) {
		if up, ok := initialisms[strings.ToLower(w)]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	return b.String()
}

func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	for _, r := range s {
		switch {
		case r == '.' || r == '_' || r == '-':
			flush()
		case unicode.IsUpper(r):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}
