package apigen

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	TypeObject  = "object"
	TypeString  = "string"
	TypeInteger = "integer"
	TypeBoolean = "boolean"
	TypeArray   = "array"

	RefObjectMeta = "ObjectMeta"
	RefCondition  = "Condition"

	optionalTag = "optional"
)

type Schema struct {
	Type                 string
	Format               string
	Enum                 []string
	Ref                  string
	Properties           []Property
	Closed               bool
	Items                *Schema
	AdditionalProperties *Schema
	PreserveUnknown      bool
	Default              any
	Minimum              *int64
	Maximum              *int64
	MaxLength            *int64
	Pattern              string
	ListType             string
	ListMapKeys          []string
}

type Property struct {
	Name     string
	Required bool
	Schema   *Schema
}

func (s *Schema) Required() []string {
	var out []string
	for _, p := range s.Properties {
		if p.Required {
			out = append(out, p.Name)
		}
	}
	return out
}

type Resource struct {
	Kind           string
	ListKind       string
	Plural         string
	Singular       string
	Revision       int
	Export         string
	Schema         *Schema
	PrinterColumns []v1alpha1.PrinterColumn
	Conditions     bool
	Contracts      []string
}

func (r Resource) SchemaName(group, version string) string {
	return fmt.Sprintf("%s-%d.%s.%s", version, r.Revision, r.Plural, group)
}

type Model struct {
	Dir       string
	Package   string
	Group     string
	Version   string
	Resources []Resource
	Named     map[string]*Schema
	Order     []string
}

var (
	conditionType  = reflect.TypeFor[metav1.Condition]()
	conditionsType = reflect.TypeFor[[]metav1.Condition]()
	timeType       = reflect.TypeFor[metav1.Time]()
	rawJSONType    = reflect.TypeFor[json.RawMessage]()
	objectMetaType = reflect.TypeFor[metav1.ObjectMeta]()
	typeMetaType   = reflect.TypeFor[metav1.TypeMeta]()
	commonPkgPath  = reflect.TypeFor[commonv1.ObjectRef]().PkgPath()
)

func BuildV1alpha1(root string) (*Model, error) {
	groups := Groups()
	return BuildGroup(root, groups[0])
}

func Build(root string) ([]*Model, error) {
	groups := Groups()
	models := make([]*Model, 0, len(groups))
	for _, g := range groups {
		m, err := BuildGroup(root, g)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", g.Group, err)
		}
		models = append(models, m)
	}
	if err := checkContracts(models); err != nil {
		return nil, err
	}
	return models, nil
}

func checkContracts(models []*Model) error {
	abstract := map[string]*Schema{}
	for _, m := range models {
		for _, r := range m.Resources {
			abstract[r.Plural] = r.Schema
		}
	}
	var errs []error
	for _, m := range models {
		for _, r := range m.Resources {
			for _, c := range r.Contracts {
				root, ok := abstract[c]
				if !ok {
					errs = append(errs, fmt.Errorf("%s: contract %q names no resource", r.Kind, c))
					continue
				}
				spec := root.property("spec")
				if spec == nil || spec.Schema == nil || spec.Schema.property("backendRef") == nil {
					errs = append(errs, fmt.Errorf("%s: contract %q is not an abstract kind: it has no spec.backendRef", r.Kind, c))
				}
			}
		}
	}
	return errors.Join(errs...)
}

func BuildGroup(root string, g Group) (*Model, error) {
	enumValues, err := readEnums(filepath.Join(root, filepath.FromSlash(g.Dir)))
	if err != nil {
		return nil, err
	}
	b := &builder{
		model: &Model{
			Dir:     g.Dir,
			Package: path.Base(g.Package),
			Group:   g.Group,
			Version: g.Version,
			Named:   map[string]*Schema{},
		},
		enumValues: enumValues,
		pkgPath:    g.Package,
		types:      map[string]reflect.Type{},
	}
	b.model.Named[RefObjectMeta] = &Schema{Type: TypeObject, Ref: RefObjectMeta}
	b.model.Named[RefCondition] = &Schema{Type: TypeObject, Ref: RefCondition, PreserveUnknown: true}
	seen := map[string]bool{}
	for _, r := range g.Resources {
		if seen[r.Kind] {
			return nil, fmt.Errorf("%s.APIResources lists %s twice", g.Dir, r.Kind)
		}
		seen[r.Kind] = true
		if r.Revision < 1 {
			return nil, fmt.Errorf("%s: Revision must be 1 or more, got %d", r.Kind, r.Revision)
		}
		root, err := b.root(reflect.TypeOf(r.Object), r.Kind)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Kind, err)
		}
		if err := b.checkSpecEndpoints(reflect.TypeOf(r.Object)); err != nil {
			b.errs = append(b.errs, fmt.Errorf("%s: %w", r.Kind, err))
		}
		for _, c := range r.PrinterColumns {
			if err := checkColumn(root, c); err != nil {
				return nil, fmt.Errorf("%s: printer column %s: %w", r.Kind, c.Name, err)
			}
		}
		b.model.Resources = append(b.model.Resources, Resource{
			Kind:           r.Kind,
			ListKind:       r.Kind + "List",
			Plural:         r.Plural,
			Singular:       strings.ToLower(r.Kind),
			Revision:       r.Revision,
			Export:         r.Export,
			Schema:         root,
			PrinterColumns: r.PrinterColumns,
			Conditions:     hasStatusConditions(reflect.TypeOf(r.Object)),
			Contracts:      r.Contracts,
		})
	}
	if err := errors.Join(b.errs...); err != nil {
		return nil, err
	}
	return b.model, nil
}

func hasStatusConditions(t reflect.Type) bool {
	status, ok := t.FieldByName("Status")
	if !ok || status.Type.Kind() != reflect.Struct {
		return false
	}
	conditions, ok := status.Type.FieldByName("Conditions")
	return ok && conditions.Type == conditionsType
}

var columnTypes = map[string]string{
	"string":  TypeString,
	"integer": TypeInteger,
	"boolean": TypeBoolean,
	"date":    TypeString,
}

func checkColumn(root *Schema, c v1alpha1.PrinterColumn) error {
	want, ok := columnTypes[c.Type]
	if !ok {
		return fmt.Errorf("type %q is not one of string, integer, boolean, date", c.Type)
	}
	if !strings.HasPrefix(c.JSONPath, ".") {
		return fmt.Errorf("jsonPath %q does not start with a dot", c.JSONPath)
	}
	s := root
	for name := range strings.SplitSeq(c.JSONPath[1:], ".") {
		if s.Ref == RefObjectMeta {
			return nil
		}
		var next *Schema
		for _, p := range s.Properties {
			if p.Name == name {
				next = p.Schema
			}
		}
		if next == nil {
			return fmt.Errorf("jsonPath %q names no field %s", c.JSONPath, name)
		}
		s = next
	}
	if s.Type != want {
		return fmt.Errorf("jsonPath %q is a %s, the column says %s", c.JSONPath, s.Type, c.Type)
	}
	return nil
}

type builder struct {
	model      *Model
	enumValues map[string][]string
	pkgPath    string
	types      map[string]reflect.Type
	errs       []error
	reported   map[string]bool
}

func (b *builder) name(s *Schema) {
	b.model.Named[s.Ref] = s
	b.model.Order = append(b.model.Order, s.Ref)
}

func (b *builder) root(t reflect.Type, kind string) (*Schema, error) {
	if t.Kind() != reflect.Struct || t.Name() != kind {
		return nil, fmt.Errorf("the registered Object is %v, want the struct %s", t, kind)
	}
	s := &Schema{Type: TypeObject, Ref: kind, Closed: true}
	b.name(s)
	if err := b.fields(s, t); err != nil {
		return nil, err
	}
	return s, nil
}

func (b *builder) fields(s *Schema, t reflect.Type) error {
	for f := range t.Fields() {
		switch {
		case f.Type == typeMetaType:
			s.Properties = append(s.Properties,
				Property{Name: "apiVersion", Schema: &Schema{Type: TypeString}},
				Property{Name: "kind", Schema: &Schema{Type: TypeString}})
		case f.Type == objectMetaType:
			s.Properties = append(s.Properties, Property{Name: "metadata", Schema: b.model.Named[RefObjectMeta]})
		default:
			props, err := b.properties(f)
			if err != nil {
				return err
			}
			s.Properties = append(s.Properties, props...)
		}
	}
	return nil
}

func (b *builder) properties(f reflect.StructField) ([]Property, error) {
	tag := f.Tag.Get("json")
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "-" {
		return nil, nil
	}
	if f.Anonymous && name == "" {
		s, err := b.schema(f.Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Name, err)
		}
		if s.Type != TypeObject || s.AdditionalProperties != nil || !s.Closed {
			return nil, fmt.Errorf("embedded field %s is not a closed object", f.Name)
		}
		return s.Properties, nil
	}
	if name == "" {
		return nil, fmt.Errorf("field %s carries no json name", f.Name)
	}
	omitempty := false
	for _, o := range parts[1:] {
		if o == "omitempty" {
			omitempty = true
		}
	}
	opts, err := parseTag(f.Tag.Get("apigen"))
	if err != nil {
		return nil, fmt.Errorf("field %s: %w", f.Name, err)
	}
	if f.Type.Kind() == reflect.Pointer && !omitempty {
		return nil, fmt.Errorf("field %s is a pointer without omitempty, which writes null", f.Name)
	}
	s, err := b.schema(f.Type)
	if err != nil {
		return nil, fmt.Errorf("field %s: %w", f.Name, err)
	}
	if s, err = opts.apply(s); err != nil {
		return nil, fmt.Errorf("field %s: %w", f.Name, err)
	}
	required := !omitempty && !opts.Optional && opts.Default == nil
	return []Property{{Name: name, Required: required, Schema: s}}, nil
}

func (b *builder) schema(t reflect.Type) (*Schema, error) {
	if t == conditionType {
		return b.model.Named[RefCondition], nil
	}
	if t == timeType {
		return &Schema{Type: TypeString, Format: "date-time"}, nil
	}
	if t == rawJSONType {
		return &Schema{Type: TypeObject, PreserveUnknown: true}, nil
	}
	if t.Kind() == reflect.Interface {
		if t.NumMethod() != 0 {
			return nil, fmt.Errorf("interface %v is not empty and has no mapping", t)
		}
		return &Schema{Type: TypeObject, PreserveUnknown: true}, nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		return b.schema(t.Elem())
	case reflect.String:
		if t.PkgPath() == "" {
			return &Schema{Type: TypeString}, nil
		}
		if t.PkgPath() != b.pkgPath {
			return nil, fmt.Errorf("named string type %v is outside %s", t, b.pkgPath)
		}
		if s, ok := b.model.Named[t.Name()]; ok {
			return s, nil
		}
		values := b.enumValues[t.Name()]
		if len(values) == 0 {
			return nil, fmt.Errorf("named string type %s declares no constants, so it has no enum", t.Name())
		}
		s := &Schema{Type: TypeString, Enum: values, Ref: t.Name()}
		b.name(s)
		return s, nil
	case reflect.Bool:
		return &Schema{Type: TypeBoolean}, nil
	case reflect.Int:
		return &Schema{Type: TypeInteger}, nil
	case reflect.Int32:
		return &Schema{Type: TypeInteger, Format: "int32"}, nil
	case reflect.Int64:
		return &Schema{Type: TypeInteger, Format: "int64"}, nil
	case reflect.Slice:
		items, err := b.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return &Schema{Type: TypeArray, Items: items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map %v does not have string keys", t)
		}
		if t.PkgPath() != "" && t.PkgPath() != b.pkgPath {
			return nil, fmt.Errorf("named map type %v is outside %s", t, b.pkgPath)
		}
		if t.Name() != "" {
			if s, ok := b.model.Named[t.Name()]; ok {
				return s, nil
			}
		}
		item, err := b.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		s := &Schema{Type: TypeObject, AdditionalProperties: item}
		if t.Name() != "" {
			s.Ref = t.Name()
			b.name(s)
		}
		return s, nil
	case reflect.Struct:
		if t.PkgPath() != b.pkgPath && t.PkgPath() != commonPkgPath {
			return nil, fmt.Errorf("struct %v is outside %s and %s and has no mapping", t, b.pkgPath, commonPkgPath)
		}
		if prior, ok := b.types[t.Name()]; ok && prior != t {
			return nil, fmt.Errorf("struct %v and %v share the schema name %s", prior, t, t.Name())
		}
		b.types[t.Name()] = t
		if s, ok := b.model.Named[t.Name()]; ok {
			return s, nil
		}
		if t.PkgPath() == b.pkgPath {
			if err := b.checkCasing(t); err != nil {
				b.errs = append(b.errs, err)
			}
		}
		s := &Schema{Type: TypeObject, Ref: t.Name(), Closed: true}
		b.name(s)
		if err := b.fields(s, t); err != nil {
			return nil, fmt.Errorf("%s: %w", t.Name(), err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("kind %v of %v has no OpenAPI mapping", t.Kind(), t)
	}
}

func readEnums(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	fset := token.NewFileSet()
	enums := map[string][]string{}
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) || !name.IsExported() {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						return nil, fmt.Errorf("%s: %w", fset.Position(lit.Pos()), err)
					}
					if ident, ok := vs.Type.(*ast.Ident); ok {
						enums[ident.Name] = append(enums[ident.Name], value)
					}
				}
			}
		}
	}
	return enums, nil
}
