package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials/openbaosession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
)

const DefaultLoginFor = 10 * time.Minute

var ErrNoSource = errors.New("credentials: a resolver needs an OpenBaoServer source")

type Session interface {
	ReadKV(ctx context.Context, mount, path string, version int) (map[string]any, error)

	WriteKV(ctx context.Context, mount, path string, data map[string]any) error

	DeleteKV(ctx context.Context, mount, path string) error
}

type Options struct {
	Source openbaosession.Source

	Store *kcpstore.Store

	Workspace string

	Client string

	Server string

	Token string

	Vars map[string]string

	Timeout time.Duration

	LoginFor time.Duration

	Open func(ctx context.Context, srv openbaosession.Server) (Session, error)
}

type Resolver struct {
	source openbaosession.Source

	workspace string

	server string

	token string

	vars map[string]string

	timeout time.Duration

	loginFor time.Duration

	open func(ctx context.Context, srv openbaosession.Server) (Session, error)

	state *resolverState
}

type resolverState struct {
	mu sync.Mutex

	sessions map[string]cachedSession
}

type cachedSession struct {
	session Session

	until time.Time
}

var _ component.CredentialResolver = (*Resolver)(nil)

var _ component.CredentialWriter = (*Resolver)(nil)

var _ component.CredentialDeleter = (*Resolver)(nil)

func New(opts Options) (*Resolver, error) {
	source := opts.Source
	if source == nil && opts.Store != nil {
		source = &openbaosession.KCPStoreSource{Store: opts.Store, Client: opts.Client, Token: opts.Token}
	}
	if source == nil {
		return nil, ErrNoSource
	}
	if opts.Workspace == "" {
		opts.Workspace = "root"
	}
	if opts.LoginFor <= 0 {
		opts.LoginFor = DefaultLoginFor
	}
	if opts.Open == nil {
		opts.Open = func(ctx context.Context, srv openbaosession.Server) (Session, error) {
			return openbaosession.Open(ctx, srv)
		}
	}
	vars := map[string]string{"workspace": opts.Workspace}
	maps.Copy(vars, opts.Vars)
	return &Resolver{
		source:    source,
		workspace: opts.Workspace,
		server:    opts.Server,
		token:     opts.Token,
		vars:      vars,
		timeout:   opts.Timeout,
		loginFor:  opts.LoginFor,
		open:      opts.Open,
		state:     &resolverState{sessions: map[string]cachedSession{}},
	}, nil
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

func NewOpenBaoResolver(store *kcpstore.Store, workspace, client, server string) (*Resolver, error) {
	return NewKCPResolver(store, workspace, client, server)
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
	mount, path, srv, err := r.target(ctx, ref)
	if err != nil {
		return component.Credential{}, err
	}
	data, err := r.readKV(ctx, srv, mount, path, ref.Version)
	if err != nil {
		return component.Credential{}, fmt.Errorf("credentials: reading %s/data/%s: %w", mount, path, err)
	}
	if len(data) == 0 {
		return component.Credential{}, fmt.Errorf("credentials: %s/data/%s: %w", mount, path, openbao.ErrNotFound)
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
	mount, path, srv, err := r.target(ctx, ref)
	if err != nil {
		return err
	}
	session, err := r.session(ctx, srv)
	if err != nil {
		return err
	}
	if err := session.WriteKV(ctx, mount, path, data); err != nil {
		if !openbao.IsAuthError(err) {
			return fmt.Errorf("credentials: writing %s/data/%s: %w", mount, path, err)
		}
		r.forgetSession(srv)
		session, err = r.session(ctx, srv)
		if err != nil {
			return err
		}
		if err := session.WriteKV(ctx, mount, path, data); err != nil {
			return fmt.Errorf("credentials: writing %s/data/%s: %w", mount, path, err)
		}
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
	mount, path, srv, err := r.target(ctx, ref)
	if err != nil {
		return err
	}
	session, err := r.session(ctx, srv)
	if err != nil {
		return err
	}
	if err := session.DeleteKV(ctx, mount, path); err != nil {
		if !openbao.IsAuthError(err) {
			return fmt.Errorf("credentials: deleting %s/metadata/%s: %w", mount, path, err)
		}
		r.forgetSession(srv)
		session, err = r.session(ctx, srv)
		if err != nil {
			return err
		}
		if err := session.DeleteKV(ctx, mount, path); err != nil {
			return fmt.Errorf("credentials: deleting %s/metadata/%s: %w", mount, path, err)
		}
	}
	return nil
}

func (r *Resolver) target(ctx context.Context, ref component.CredentialRef) (string, string, openbaosession.Server, error) {
	path := strings.Trim(strings.TrimSpace(ref.Path), "/")
	if path == "" {
		return "", "", openbaosession.Server{}, errors.New("credentials: the credential ref has no path")
	}
	server := strings.TrimSpace(ref.Server)
	if server == "" {
		server = r.server
	}
	if server == "" {
		return "", "", openbaosession.Server{}, openbaosession.ErrNoServer
	}
	workspace, name := openbaosession.ParseServer(server, r.workspace)
	srv, err := r.source.Server(ctx, workspace, name)
	if err != nil {
		return "", "", openbaosession.Server{}, err
	}
	if srv.Token == "" {
		srv.Token = r.token
	}
	if srv.Timeout == 0 {
		srv.Timeout = r.timeout
	}
	mount := strings.Trim(strings.TrimSpace(ref.Mount), "/")
	if mount == "" || mount == "file" || mount == "file:" {
		mount = strings.Trim(srv.KVMount, "/")
	}
	if mount == "" {
		return "", "", openbaosession.Server{}, errors.New("credentials: the credential ref has no mount and the OpenBaoServer names no KV mount")
	}
	return mount, path, srv, nil
}

func (r *Resolver) readKV(ctx context.Context, srv openbaosession.Server, mount, path string, version int) (map[string]any, error) {
	session, err := r.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	data, err := session.ReadKV(ctx, mount, path, version)
	if !openbao.IsAuthError(err) {
		return data, err
	}
	r.forgetSession(srv)
	session, err = r.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	return session.ReadKV(ctx, mount, path, version)
}

func (r *Resolver) forgetSession(srv openbaosession.Server) {
	key := srv.Workspace + "|" + srv.Name
	r.state.mu.Lock()
	delete(r.state.sessions, key)
	r.state.mu.Unlock()
}

func (r *Resolver) session(ctx context.Context, srv openbaosession.Server) (Session, error) {
	key := srv.Workspace + "|" + srv.Name
	r.state.mu.Lock()
	cached, ok := r.state.sessions[key]
	r.state.mu.Unlock()
	if ok && time.Now().Before(cached.until) {
		return cached.session, nil
	}
	session, err := r.open(ctx, srv)
	if err != nil {
		return nil, err
	}
	until := time.Now().Add(r.loginFor)
	if refresh, ok := session.(interface{ RefreshAt() time.Time }); ok {
		if at := refresh.RefreshAt(); !at.IsZero() && at.Before(until) {
			until = at
		}
	}
	r.state.mu.Lock()
	r.state.sessions[key] = cachedSession{session: session, until: until}
	r.state.mu.Unlock()
	return session, nil
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
