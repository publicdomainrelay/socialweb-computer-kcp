package manifests

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/johnandersen777/socialweb-computer-kcp/deploy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
)

const IndexPath = "sets.yaml"

var ErrMissingValue = errors.New("manifests: a value the template names is unset")

type Index struct {
	Sets      []Set       `json:"sets"`
	Unapplied []Unapplied `json:"unapplied"`
}

type Unapplied struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

type Set struct {
	Name    string  `json:"name"`
	Summary string  `json:"summary"`
	Values  string  `json:"values"`
	Scope   string  `json:"scope,omitempty"`
	Phases  []Phase `json:"phases"`
	Base    string  `json:"-"`
}

const ScopeKcp = "kcp"

const SharedWorkspaceSuffix = "-provider"

func Rebase(base, ws string) string {
	if base == "" || base == kcp.RootWorkspace {
		return ws
	}
	if ws == kcp.RootWorkspace {
		return base
	}
	rest, ok := strings.CutPrefix(ws, kcp.RootWorkspace+":")
	if !ok {
		return ws
	}
	first, _, _ := strings.Cut(rest, ":")
	if strings.HasSuffix(first, SharedWorkspaceSuffix) {
		return ws
	}
	return base + ":" + rest
}

func ServiceSuffix(ws string) string {
	return strings.TrimPrefix(kcp.ServiceFQDN("x", "default", ws, kcp.DefaultServiceDomain), "x.default.")
}

type Phase struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Files     []File `json:"files,omitempty"`
	Before    []Wait `json:"before,omitempty"`
	After     []Wait `json:"after,omitempty"`
}

type File struct {
	Path        string   `json:"path"`
	Template    string   `json:"template,omitempty"`
	Each        string   `json:"each,omitempty"`
	As          string   `json:"as,omitempty"`
	Kinds       []string `json:"kinds,omitempty"`
	ExceptKinds []string `json:"exceptKinds,omitempty"`
}

type Wait struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name,omitempty"`
	Workspace  string   `json:"workspace,omitempty"`
	For        string   `json:"for"`
	Capture    *Capture `json:"capture,omitempty"`
	BestEffort bool     `json:"bestEffort,omitempty"`
}

type Capture struct {
	Into string `json:"into"`
	From string `json:"from"`
}

func Load() (Index, error) {
	b, err := fs.ReadFile(deploy.FS, IndexPath)
	if err != nil {
		return Index{}, fmt.Errorf("manifests: %w", err)
	}
	return Parse(b)
}

func Parse(b []byte) (Index, error) {
	var ix Index
	if err := yaml.UnmarshalStrict(b, &ix); err != nil {
		return Index{}, fmt.Errorf("manifests: %s: %w", IndexPath, err)
	}
	if err := ix.validate(); err != nil {
		return Index{}, fmt.Errorf("manifests: %s: %w", IndexPath, err)
	}
	return ix, nil
}

func (ix Index) Set(name string) (Set, bool) {
	for _, s := range ix.Sets {
		if s.Name == name {
			return s, true
		}
	}
	return Set{}, false
}

func (ix Index) Names() []string {
	out := make([]string, 0, len(ix.Sets))
	for _, s := range ix.Sets {
		out = append(out, s.Name)
	}
	return out
}

func (ix Index) validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	sets := map[string]bool{}
	for _, s := range ix.Sets {
		if s.Name == "" || sets[s.Name] {
			add("set %q is unnamed or named twice", s.Name)
		}
		sets[s.Name] = true
		if s.Scope != "" && s.Scope != ScopeKcp {
			add("set %s: scope %q is not %s", s.Name, s.Scope, ScopeKcp)
		}
		vt, ok := valueTypes[s.Values]
		if !ok {
			add("set %s: values %q is no values type (%s)", s.Name, s.Values, strings.Join(ValueTypeNames(), ", "))
			continue
		}
		if len(s.Phases) == 0 {
			add("set %s has no phase", s.Name)
		}
		phases := map[string]bool{}
		for _, p := range s.Phases {
			where := "set " + s.Name + " phase " + p.Name
			if p.Name == "" || phases[p.Name] {
				add("%s: unnamed or named twice", where)
			}
			phases[p.Name] = true
			if p.Workspace == "" {
				add("%s names no workspace", where)
			}
			if len(p.Files) == 0 && len(p.Before) == 0 && len(p.After) == 0 {
				add("%s applies nothing and waits for nothing", where)
			}
			for _, f := range p.Files {
				if _, err := fs.Stat(deploy.FS, f.Path); err != nil {
					add("%s: %s is not in deploy.FS", where, f.Path)
				}
				if f.Template != "" {
					if _, err := fs.Stat(deploy.FS, f.Template); err != nil {
						add("%s: %s names template %s, which is not in deploy.FS", where, f.Path, f.Template)
					}
				}
				if len(f.Kinds) > 0 && len(f.ExceptKinds) > 0 {
					add("%s: %s names both kinds and exceptKinds", where, f.Path)
				}
				if (f.Each == "") != (f.As == "") {
					add("%s: %s names each without as, or as without each", where, f.Path)
				}
				if f.Each != "" {
					if fld, ok := vt().Elem().FieldByName(f.Each); !ok || fld.Type.Kind() != reflect.Slice {
						add("%s: %s: each %s is no list field of %s", where, f.Path, f.Each, s.Values)
					}
				}
			}
			for _, w := range append(append([]Wait{}, p.Before...), p.After...) {
				if err := w.check(vt().Elem()); err != nil {
					add("%s: %v", where, err)
				}
			}
		}
	}
	for _, u := range ix.Unapplied {
		if _, err := fs.Stat(deploy.FS, u.Path); err != nil {
			add("unapplied %s is not in deploy.FS", u.Path)
		}
		if strings.TrimSpace(u.Why) == "" {
			add("unapplied %s gives no reason", u.Path)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (w Wait) check(values reflect.Type) error {
	k, ok := WaitKinds[w.Kind]
	if !ok {
		return fmt.Errorf("wait kind %q is not one of %v", w.Kind, waitKindNames())
	}
	from, ok := k.For[w.For]
	if !ok {
		return fmt.Errorf("a %s wait cannot be for %q", w.Kind, w.For)
	}
	if w.Kind == "APIBinding" && w.Name != "" {
		return errors.New("an APIBinding wait is for every binding in its workspace and names none")
	}
	if w.Capture == nil {
		return nil
	}
	if w.Capture.From != from || from == "" {
		return fmt.Errorf("a %s %s wait cannot capture %q", w.Kind, w.For, w.Capture.From)
	}
	fld, ok := values.FieldByName(w.Capture.Into)
	if !ok || fld.Type.Kind() != captureKinds[from] {
		return fmt.Errorf("capture into %s: no %s field of that name", w.Capture.Into, captureKinds[from])
	}
	return nil
}
