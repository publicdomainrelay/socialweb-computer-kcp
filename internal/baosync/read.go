package baosync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

func mountType(m openbao.Mount) string {
	if m.Type == MountKV && fmt.Sprint(m.Options["version"]) == "2" {
		return MountKV2
	}
	return m.Type
}

func objectMounted(p string, a Actual) bool {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if segs[0] == "auth" {
		if len(segs) < 2 {
			return false
		}
		_, ok := a.AuthMounts[segs[1]]
		return ok
	}
	_, ok := a.SecretMounts[segs[0]]
	return ok
}

func Read(ctx context.Context, c *openbao.Client, d Desired) (Actual, error) {
	a := Actual{Plugins: map[string]PluginEntry{}, AuthSHA256: map[string]string{}, SecretMounts: map[string]string{}, AuthMounts: map[string]string{}, Policies: map[string]string{},
		Collections: map[string][]string{}, Objects: map[string]map[string]any{}, KV: map[string][]string{},
		KVUpdated: map[string]time.Time{}, Now: time.Now()}
	secrets, err := c.SecretMounts(ctx)
	if err != nil {
		return a, fmt.Errorf("baosync: listing the secret mounts: %w; an unread list is not an empty one", err)
	}
	for p, m := range secrets {
		a.SecretMounts[p] = mountType(m)
	}
	auths, err := c.AuthMounts(ctx)
	if err != nil {
		return a, fmt.Errorf("baosync: listing the auth mounts: %w; an unread list is not an empty one", err)
	}
	for p, m := range auths {
		a.AuthMounts[p] = m.Type
		a.AuthSHA256[p] = m.RunningSHA256
	}
	if err := readPlugins(ctx, c, d, &a); err != nil {
		return a, err
	}
	names, err := c.List(ctx, "sys/policies/acl")
	if err != nil {
		return a, fmt.Errorf("baosync: listing the policies: %w; an unread list is not an empty one", err)
	}
	if len(names) == 0 {
		return a, errors.New("baosync: the policy list came back empty; a server always holds the default policy, so the read is wrong")
	}
	want := map[string]bool{}
	for _, p := range d.Policies {
		want[p.Name] = true
	}
	for _, n := range names {
		a.Policies[n] = ""
		if !want[n] {
			continue
		}
		hcl, err := c.GetPolicy(ctx, n)
		if err != nil {
			return a, fmt.Errorf("baosync: reading policy %s: %w", n, err)
		}
		a.Policies[n] = hcl
	}
	for _, coll := range d.Collections() {
		if !objectMounted(coll, a) || !collectionTypeMatches(coll, d, a) {
			continue
		}
		keys, err := c.List(ctx, coll)
		if err != nil {
			return a, fmt.Errorf("baosync: listing %s: %w; an unread list is not an empty one", coll, err)
		}
		a.Collections[coll] = keys
	}
	for _, o := range d.Objects {
		p := strings.Trim(o.Path, "/")
		if !objectMounted(p, a) {
			continue
		}
		data, err := c.Read(ctx, p)
		if err != nil {
			continue
		}
		a.Objects[p] = data
	}
	for _, s := range d.KV {
		if _, ok := a.SecretMounts[strings.Trim(s.Mount, "/")]; !ok {
			continue
		}
		key := KVScopeKey(s)
		keys, err := c.List(ctx, key)
		if err != nil {
			return a, fmt.Errorf("baosync: listing %s: %w; an unread list is not an empty one", key, err)
		}
		keep := KeptKV(s)
		var entries []string
		for _, name := range keys {
			if keep[KVTop(name)] {
				entries = append(entries, name)
				continue
			}
			leaves, err := kvLeaves(ctx, c, key, name)
			if err != nil {
				return a, err
			}
			for _, leaf := range leaves {
				entries = append(entries, leaf)
				if s.Grace <= 0 {
					continue
				}
				target := strings.TrimSuffix(key, "/") + "/" + leaf
				meta, err := c.Read(ctx, target)
				if errors.Is(err, openbao.ErrNotFound) {
					continue
				}
				at, perr := time.Parse(time.RFC3339Nano, fmt.Sprint(meta["updated_time"]))
				if err != nil || perr != nil {
					at = a.Now
				}
				a.KVUpdated[target] = at
			}
		}
		a.KV[key] = entries
	}
	return a, nil
}

func kvLeaves(ctx context.Context, c *openbao.Client, key, name string) ([]string, error) {
	if !strings.HasSuffix(name, "/") {
		return []string{name}, nil
	}
	path := strings.TrimSuffix(key, "/") + "/" + strings.TrimSuffix(name, "/")
	under, err := c.List(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("baosync: listing %s: %w; an unread list is not an empty one", path, err)
	}
	var out []string
	for _, u := range under {
		leaves, err := kvLeaves(ctx, c, path, u)
		if err != nil {
			return nil, err
		}
		for _, l := range leaves {
			out = append(out, name+l)
		}
	}
	return out, nil
}

func collectionTypeMatches(coll string, d Desired, a Actual) bool {
	if coll == IdentityRoleCollection {
		return d.IdentityOIDC
	}
	for _, m := range d.AuthMounts {
		if d.AuthRoleCollection(m) == coll {
			return a.AuthMounts[strings.Trim(m.Path, "/")] == m.Type
		}
	}
	for _, m := range d.SecretMounts {
		if m.Type == MountPKI && PKIRoleCollection(m.Path) == coll {
			return a.SecretMounts[strings.Trim(m.Path, "/")] == MountPKI
		}
	}
	return false
}

func readPlugins(ctx context.Context, c *openbao.Client, d Desired, a *Actual) error {
	if len(d.Plugins) == 0 {
		return nil
	}
	cat, err := c.Read(ctx, "sys/plugins/catalog")
	if err != nil {
		return fmt.Errorf("baosync: reading the plugin catalog: %w; an unread catalog is not an empty one", err)
	}
	detailed, _ := cat["detailed"].([]any)
	for _, x := range detailed {
		e, ok := x.(map[string]any)
		if !ok || e["builtin"] == true {
			continue
		}
		a.External = append(a.External, fmt.Sprint(e["type"])+"/"+fmt.Sprint(e["name"]))
	}
	for _, p := range d.Plugins {
		got, err := c.Read(ctx, "sys/plugins/catalog/"+PluginKey(p))
		if errors.Is(err, openbao.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("baosync: reading plugin %s: %w", PluginKey(p), err)
		}
		if got["builtin"] == true {
			continue
		}
		a.Plugins[PluginKey(p)] = PluginEntry{Command: fmt.Sprint(got["command"]), SHA256: fmt.Sprint(got["sha256"])}
	}
	return nil
}
