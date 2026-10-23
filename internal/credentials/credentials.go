package credentials

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

var ErrNoSource = errors.New("credentials: a resolver needs an OpenBaoServer source")

type Options struct {
	Logins *openbao.Logins

	Store *kcpstore.Store

	Workspace string

	Client string

	Server string

	Vars map[string]string
}

type Resolver struct {
	logins *openbao.Logins

	workspace string

	server string

	vars map[string]string
}

var _ component.CredentialResolver = (*Resolver)(nil)

var _ component.CredentialWriter = (*Resolver)(nil)

var _ component.CredentialDeleter = (*Resolver)(nil)

func New(opts Options) (*Resolver, error) {
	logins := opts.Logins
	if logins == nil && opts.Store != nil {
		logins = openbao.NewLogins(&KCPStoreSource{Store: opts.Store, Client: opts.Client})
	}
	if logins == nil {
		return nil, ErrNoSource
	}
	opts.Workspace = cmp.Or(opts.Workspace, "root")
	vars := map[string]string{"workspace": opts.Workspace}
	maps.Copy(vars, opts.Vars)
	return &Resolver{logins: logins, workspace: opts.Workspace, server: opts.Server, vars: vars}, nil
}

func (r *Resolver) With(vars map[string]string) *Resolver {
	if r == nil {
		return nil
	}
	out := *r
	merged := make(map[string]string, len(r.vars)+len(vars))
	maps.Copy(merged, r.vars)
	maps.Copy(merged, vars)
	out.vars = merged
	return &out
}

func NewKCPResolver(store *kcpstore.Store, workspace, client, server string) (*Resolver, error) {
	return New(Options{Store: store, Workspace: workspace, Client: client, Server: server})
}

func (r *Resolver) Resolve(ctx context.Context, ref component.CredentialRef) (component.Credential, error) {
	return r.ResolveVars(ctx, ref, nil)
}

func (r *Resolver) ResolveVars(ctx context.Context, ref component.CredentialRef, vars map[string]string) (component.Credential, error) {
	ref = r.expand(ref, vars)
	if path, ok := fileRef(ref); ok {
		data, err := readFile(path)
		if err != nil {
			return component.Credential{}, fmt.Errorf("credentials: reading %s: %w", path, err)
		}
		return component.Credential{Ref: ref, Data: filterKeys(data, ref.Keys)}, nil
	}
	t, err := r.target(ref)
	if err != nil {
		return component.Credential{}, err
	}
	data, err := r.readKV(ctx, &t, ref.Version)
	if err != nil {
		return component.Credential{}, fmt.Errorf("credentials: reading %s/data/%s: %w", t.mount, t.path, err)
	}
	if len(data) == 0 {
		return component.Credential{}, fmt.Errorf("credentials: %s/data/%s: %w", t.mount, t.path, openbao.ErrNotFound)
	}
	return component.Credential{Ref: ref, Data: filterKeys(data, ref.Keys)}, nil
}

func (r *Resolver) WriteCredential(ctx context.Context, ref component.CredentialRef, data map[string]any) error {
	ref = r.expand(ref, nil)
	if path, ok := fileRef(ref); ok {
		if err := writeFile(path, data); err != nil {
			return fmt.Errorf("credentials: writing %s: %w", path, err)
		}
		return nil
	}
	if data == nil {
		return errors.New("credentials: a credential write needs data")
	}
	t, err := r.target(ref)
	if err != nil {
		return err
	}
	client, err := r.client(ctx, &t)
	if err != nil {
		return err
	}
	if err := client.PutKV(ctx, t.mount+"/data/"+t.path, data); err != nil {
		return fmt.Errorf("credentials: writing %s/data/%s: %w", t.mount, t.path, err)
	}
	return nil
}

func (r *Resolver) DeleteCredential(ctx context.Context, ref component.CredentialRef) error {
	ref = r.expand(ref, nil)
	if path, ok := fileRef(ref); ok {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("credentials: deleting %s: %w", path, err)
		}
		return nil
	}
	t, err := r.target(ref)
	if err != nil {
		return err
	}
	client, err := r.client(ctx, &t)
	if err != nil {
		return err
	}
	if err := client.Delete(ctx, t.mount+"/metadata/"+t.path); err != nil {
		return fmt.Errorf("credentials: deleting %s/metadata/%s: %w", t.mount, t.path, err)
	}
	return nil
}

type target struct {
	workspace string

	name string

	mount string

	path string
}

func (r *Resolver) target(ref component.CredentialRef) (target, error) {
	path := strings.Trim(strings.TrimSpace(ref.Path), "/")
	if path == "" {
		return target{}, errors.New("credentials: the credential ref has no path")
	}
	server := cmp.Or(strings.TrimSpace(ref.Server), r.server)
	if server == "" {
		return target{}, openbao.ErrNoServer
	}
	workspace, name := openbao.ParseServer(server, r.workspace)
	mount := strings.Trim(strings.TrimSpace(ref.Mount), "/")
	if mount == "file" || mount == "file:" {
		mount = ""
	}
	return target{workspace: workspace, name: name, mount: mount, path: path}, nil
}

func (r *Resolver) client(ctx context.Context, t *target) (*openbao.Client, error) {
	client, srv, err := r.logins.Client(ctx, t.workspace, t.name)
	if err != nil {
		return nil, err
	}
	t.mount = cmp.Or(t.mount, strings.Trim(srv.KVMount, "/"))
	if t.mount == "" {
		return nil, errors.New("credentials: the credential ref has no mount and the OpenBaoServer names no KV mount")
	}
	return client, nil
}

func (r *Resolver) readKV(ctx context.Context, t *target, version int) (map[string]any, error) {
	client, err := r.client(ctx, t)
	if err != nil {
		return nil, err
	}
	kvPath := t.mount + "/data/" + t.path
	if version > 0 {
		kvPath = fmt.Sprintf("%s?version=%d", kvPath, version)
	}
	return client.GetKV(ctx, kvPath)
}

func (r *Resolver) expand(ref component.CredentialRef, vars map[string]string) component.CredentialRef {
	merged := make(map[string]string, len(r.vars)+len(vars))
	maps.Copy(merged, r.vars)
	maps.Copy(merged, vars)
	ref.Server = expandTemplate(ref.Server, merged)
	ref.Mount = expandTemplate(ref.Mount, merged)
	ref.Path = expandTemplate(ref.Path, merged)
	return ref
}

func expandTemplate(value string, vars map[string]string) string {
	if value == "" || len(vars) == 0 {
		return value
	}
	for key, replacement := range vars {
		value = strings.ReplaceAll(value, "{"+key+"}", replacement)
	}
	return value
}

func fileRef(ref component.CredentialRef) (string, bool) {
	server := strings.TrimSpace(ref.Server)
	path := strings.TrimSpace(ref.Path)
	mount := strings.TrimSpace(ref.Mount)
	switch {
	case strings.HasPrefix(server, "file:"):
		base := strings.TrimPrefix(server, "file:")
		if strings.HasPrefix(path, "/") {
			return path, true
		}
		return filepath.Join(base, path), true
	case strings.HasPrefix(path, "file:"):
		return strings.TrimPrefix(path, "file:"), true
	case mount == "file" || mount == "file:":
		return path, true
	}
	return "", false
}

func readFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err == nil && out != nil {
		return out, nil
	}
	return map[string]any{"value": string(data)}, nil
}

func writeFile(path string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credential-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func filterKeys(data map[string]any, keys []string) map[string]any {
	if len(keys) == 0 {
		return data
	}
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := data[key]; ok {
			out[key] = value
		}
	}
	return out
}

func ParseRef(value string) (component.CredentialRef, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return component.CredentialRef{}, errors.New("credentials: the ref is empty")
	}
	if path, ok := strings.CutPrefix(value, "file:"); ok {
		if path == "" {
			return component.CredentialRef{}, errors.New("credentials: the file ref has no path")
		}
		return component.CredentialRef{Mount: "file", Path: path}, nil
	}
	rest := value
	switch {
	case strings.HasPrefix(rest, "credential:"):
		rest = strings.TrimPrefix(rest, "credential:")
	case strings.HasPrefix(rest, "kv:"):
		rest = strings.TrimPrefix(rest, "kv:")
	}
	rest, version := cutVersion(rest)
	body, key, hasKey := strings.Cut(rest, "#")
	mount, path, ok := strings.Cut(body, "/")
	if !ok || mount == "" || path == "" {
		return component.CredentialRef{}, fmt.Errorf("credentials: %q is not mount/path", value)
	}
	ref := component.CredentialRef{Mount: mount, Path: path, Version: version}
	if hasKey {
		if key == "" {
			return component.CredentialRef{}, fmt.Errorf("credentials: %q names an empty key", value)
		}
		ref.Keys = []string{key}
	}
	if err := ref.Check(); err != nil {
		return component.CredentialRef{}, err
	}
	return ref, nil
}

func cutVersion(value string) (string, int) {
	path, query, ok := strings.Cut(value, "?")
	if !ok {
		return value, 0
	}
	for pair := range strings.SplitSeq(query, "&") {
		key, raw, ok := strings.Cut(pair, "=")
		if !ok || key != "version" {
			continue
		}
		version, err := strconv.Atoi(raw)
		if err != nil || version < 0 {
			return value, 0
		}
		return path, version
	}
	return value, 0
}
