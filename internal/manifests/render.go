package manifests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"text/template/parse"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	swcv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/deploy"
	denov1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tsbundle"
)

const VendorRecordPath = "denokcp/vendor.json"

type Text struct {
	Path string
	Body []byte
}

type Object struct {
	Path         string
	Typed        any
	Unstructured *unstructured.Unstructured
}

type Rendered struct {
	Phase     Phase
	Workspace string
	Objects   []Object
}

var typedKinds = map[schema.GroupVersionKind]func() any{
	{Group: "apis.kcp.io", Version: "v1alpha1", Kind: "APIResourceSchema"}:                           func() any { return &apisv1alpha1.APIResourceSchema{} },
	{Group: "apis.kcp.io", Version: "v1alpha2", Kind: "APIExport"}:                                   func() any { return &apisv1alpha2.APIExport{} },
	{Group: "apis.kcp.io", Version: "v1alpha2", Kind: "APIBinding"}:                                  func() any { return &apisv1alpha2.APIBinding{} },
	{Group: "tenancy.kcp.io", Version: "v1alpha1", Kind: "Workspace"}:                                func() any { return &tenancyv1alpha1.Workspace{} },
	{Group: "tenancy.kcp.io", Version: "v1alpha1", Kind: "WorkspaceType"}:                            func() any { return &tenancyv1alpha1.WorkspaceType{} },
	{Group: "", Version: "v1", Kind: "ServiceAccount"}:                                               func() any { return &corev1.ServiceAccount{} },
	{Group: "", Version: "v1", Kind: "Namespace"}:                                                    func() any { return &corev1.Namespace{} },
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}:                         func() any { return &rbacv1.ClusterRole{} },
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}:                  func() any { return &rbacv1.ClusterRoleBinding{} },
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}:                                func() any { return &rbacv1.Role{} },
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}:                         func() any { return &rbacv1.RoleBinding{} },
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicy"}:        func() any { return &admissionregistrationv1.ValidatingAdmissionPolicy{} },
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicyBinding"}: func() any { return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{} },
	{Group: "deno.computer", Version: "v1alpha1", Kind: "DenoPod"}:                                   func() any { return &denov1alpha1.DenoPod{} },
	{Group: "deno.computer", Version: "v1alpha1", Kind: "OpenBao"}:                                   func() any { return &denov1alpha1.OpenBao{} },
	{Group: "socialweb.computer", Version: "v1alpha1", Kind: "OpenBaoServer"}:                        func() any { return &swcv1alpha1.OpenBaoServer{} },
	{Group: "socialweb.computer", Version: "v1alpha1", Kind: "AtprotoRecord"}:                        func() any { return &swcv1alpha1.AtprotoRecord{} },
	{Group: "socialweb.computer", Version: "v1alpha1", Kind: "AtprotoRecordWatch"}:                   func() any { return &swcv1alpha1.AtprotoRecordWatch{} },
	{Group: "socialweb.computer", Version: "v1alpha1", Kind: "HostProcess"}:                          func() any { return &swcv1alpha1.HostProcess{} },
}

func readFile(path string) ([]byte, error) {
	b, err := fs.ReadFile(deploy.FS, path)
	if err != nil {
		return nil, fmt.Errorf("manifests: %w", err)
	}
	return b, nil
}

func execute(name, text string, data map[string]any) ([]byte, error) {
	trees, err := parse.Parse(name, text, "", "")
	if err != nil {
		return nil, fmt.Errorf("manifests: %s: %w", name, err)
	}
	if len(trees) != 1 {
		return nil, fmt.Errorf("manifests: %s: a manifest defines no templates of its own", name)
	}
	var out bytes.Buffer
	for _, n := range trees[name].Root.Nodes {
		switch n := n.(type) {
		case *parse.TextNode:
			out.Write(n.Text)
		case *parse.ActionNode:
			v, err := field(name, n, data)
			if err != nil {
				return nil, err
			}
			fmt.Fprint(&out, v)
		default:
			return nil, fmt.Errorf("manifests: %s:%d: %s is not a field reference; manifests take {{.Field.Path}} only", name, line(text, n.Position()), n)
		}
	}
	return out.Bytes(), nil
}

func line(text string, pos parse.Pos) int {
	return 1 + strings.Count(text[:int(pos)], "\n")
}

func field(name string, n *parse.ActionNode, data map[string]any) (any, error) {
	where := fmt.Sprintf("%s:%d", name, n.Line)
	if n.Pipe == nil || len(n.Pipe.Decl) > 0 || len(n.Pipe.Cmds) != 1 || len(n.Pipe.Cmds[0].Args) != 1 {
		return nil, fmt.Errorf("manifests: %s: %s is not a field reference; manifests take {{.Field.Path}} only", where, n)
	}
	f, ok := n.Pipe.Cmds[0].Args[0].(*parse.FieldNode)
	if !ok {
		return nil, fmt.Errorf("manifests: %s: %s is not a field reference; manifests take {{.Field.Path}} only", where, n)
	}
	var cur any = data
	for i, key := range f.Ident {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("manifests: %s: .%s is not a map", where, strings.Join(f.Ident[:i], "."))
		}
		v, ok := m[key]
		if !ok || v == nil {
			return nil, fmt.Errorf("%w: %s: .%s (no entry for key %q)", ErrMissingValue, where, strings.Join(f.Ident[:i+1], "."), key)
		}
		cur = v
	}
	switch cur.(type) {
	case map[string]any, []any:
		return nil, fmt.Errorf("manifests: %s: .%s is a structure, not a value", where, strings.Join(f.Ident, "."))
	}
	return cur, nil
}

func (s Set) Workspace(p Phase, w *Wait, values any) (string, error) {
	data, err := dataOf(values)
	if err != nil {
		return "", err
	}
	text, where := p.Workspace, s.Name+"/"+p.Name+" workspace"
	if w != nil && w.Workspace != "" {
		text, where = w.Workspace, s.Name+"/"+p.Name+" wait workspace"
	}
	b, err := execute(where, text, data)
	if err != nil {
		return "", err
	}
	ws := strings.TrimSpace(string(b))
	if ws == "" {
		return "", fmt.Errorf("%w: %s renders empty", ErrMissingValue, where)
	}
	return ws, nil
}

func (s Set) RenderText(p Phase, values any) (string, []Text, error) {
	ws, err := s.Workspace(p, nil, values)
	if err != nil {
		return "", nil, err
	}
	data, err := dataOf(values)
	if err != nil {
		return "", nil, err
	}
	var out []Text
	var entries map[string]tsbundle.EntrySpec
	for _, f := range p.Files {
		src, err := readFile(f.Path)
		if err != nil {
			return "", nil, err
		}
		scopes := []map[string]any{data}
		if f.Each != "" {
			items, _ := data[f.Each].([]any)
			if len(items) == 0 {
				return "", nil, fmt.Errorf("%w: %s: each %s has no element", ErrMissingValue, f.Path, f.Each)
			}
			scopes = nil
			for _, item := range items {
				scope := maps.Clone(data)
				scope[f.As] = item
				scopes = append(scopes, scope)
			}
		}
		if f.Template != "" && entries == nil {
			if entries, err = LoadEntries(); err != nil {
				return "", nil, err
			}
		}
		for _, scope := range scopes {
			body, err := execute(f.Path, string(src), scope)
			if err != nil {
				return "", nil, err
			}
			if f.Template != "" {
				if body, err = renderService(f.Path, f.Template, body, entries); err != nil {
					return "", nil, err
				}
			}
			out = append(out, Text{Path: f.Path, Body: body})
		}
	}
	return ws, out, nil
}

func (s Set) RenderPhase(p Phase, values any) (Rendered, error) {
	ws, texts, err := s.RenderText(p, values)
	if err != nil {
		return Rendered{}, err
	}
	r := Rendered{Phase: p, Workspace: ws}
	for i, tx := range texts {
		objs, err := Decode(tx.Path, tx.Body)
		if err != nil {
			return Rendered{}, err
		}
		f := fileFor(p, tx.Path, i)
		for _, o := range objs {
			if f.selects(o.Unstructured.GetKind()) {
				r.Objects = append(r.Objects, o)
			}
		}
	}
	return r, nil
}

func fileFor(p Phase, path string, _ int) File {
	for _, f := range p.Files {
		if f.Path == path {
			return f
		}
	}
	return File{Path: path}
}

func (f File) selects(kind string) bool {
	if len(f.Kinds) > 0 {
		return slices.Contains(f.Kinds, kind)
	}
	return !slices.Contains(f.ExceptKinds, kind)
}

func (s Set) Render(values any) ([]Rendered, error) {
	var out []Rendered
	for _, p := range s.Phases {
		r, err := s.RenderPhase(p, values)
		if err != nil {
			return nil, fmt.Errorf("set %s phase %s: %w", s.Name, p.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func Decode(path string, body []byte) ([]Object, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(body)))
	var out []Object
	for n := 1; ; n++ {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("manifests: %s document %d: %w", path, n, err)
		}
		js, err := yaml.YAMLToJSONStrict(doc)
		if err != nil {
			return nil, fmt.Errorf("manifests: %s document %d: %w", path, n, err)
		}
		if t := bytes.TrimSpace(js); len(t) == 0 || string(t) == "null" {
			continue
		}
		o, err := decodeOne(js)
		if err != nil {
			return nil, fmt.Errorf("manifests: %s document %d: %w", path, n, err)
		}
		o.Path = path
		out = append(out, o)
	}
	return out, nil
}

func decodeOne(js []byte) (Object, error) {
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(js); err != nil {
		return Object{}, err
	}
	gvk := u.GroupVersionKind()
	newTyped, ok := typedKinds[gvk]
	if !ok {
		return Object{}, fmt.Errorf("%s is no kind this repo applies", gvk)
	}
	if u.GetName() == "" {
		return Object{}, fmt.Errorf("%s has no metadata.name", gvk.Kind)
	}
	typed := newTyped()
	d := json.NewDecoder(bytes.NewReader(js))
	d.DisallowUnknownFields()
	if err := d.Decode(typed); err != nil {
		return Object{}, fmt.Errorf("%s %s does not decode strictly: %w", gvk.Kind, u.GetName(), err)
	}
	return Object{Typed: typed, Unstructured: u}, nil
}

type vendorRecord struct {
	Files []struct {
		Path    string `json:"path"`
		Adapted string `json:"adapted"`
	} `json:"files"`
}

func AdaptedFiles() (map[string]bool, error) {
	b, err := readFile(VendorRecordPath)
	if err != nil {
		return nil, err
	}
	var r vendorRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("manifests: %s: %w", VendorRecordPath, err)
	}
	out := map[string]bool{}
	for _, f := range r.Files {
		out[f.Path] = f.Adapted != ""
	}
	return out, nil
}
