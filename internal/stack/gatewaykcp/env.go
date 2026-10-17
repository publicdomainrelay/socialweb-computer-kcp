package gatewaykcp

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/deploy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
)

const (
	DefaultAudience = "did:web:socialweb.local"
	DefaultPLC      = "https://plc.directory"
	DefaultIngress  = "xrpc.fedproxy.com"
	DefaultJoin     = "*"
	DeployDir       = "deploy"
	tenantGrantFile = "tenant-grant.yaml"
	auditPolicyFile = "audit-policy.yaml"
	esoDir          = "eso"
)

type env struct{ m map[string]string }

func newEnv(environ []string) *env {
	e := &env{m: map[string]string{}}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			e.m[k] = v
		}
	}
	return e
}

func (e *env) get(k string) string { return e.m[k] }

func (e *env) set(k, v string) { e.m[k] = v }

func (e *env) dflt(k, v string) {
	if strings.TrimSpace(e.m[k]) == "" {
		e.m[k] = v
	}
}

func (e *env) list() []string {
	out := make([]string, 0, len(e.m))
	for k, v := range e.m {
		out = append(out, k+"="+v)
	}
	return out
}

func materialize(dir string) (string, string, string, error) {
	grant, audit, eso := filepath.Join(dir, tenantGrantFile), filepath.Join(dir, auditPolicyFile), filepath.Join(dir, esoDir)
	if err := os.MkdirAll(eso, 0o700); err != nil {
		return "", "", "", err
	}
	copyOut := func(src, dst string) error {
		b, err := fs.ReadFile(deploy.FS, src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	}
	for src, dst := range map[string]string{tenantGrantFile: grant, auditPolicyFile: audit} {
		if err := copyOut(src, dst); err != nil {
			return "", "", "", err
		}
	}
	entries, err := fs.ReadDir(deploy.FS, esoDir)
	if err != nil {
		return "", "", "", err
	}
	for _, en := range entries {
		if err := copyOut(esoDir+"/"+en.Name(), filepath.Join(eso, en.Name())); err != nil {
			return "", "", "", err
		}
	}
	return grant, audit, eso, nil
}

func (g *gateway) defaults(e *env, rec devinstance.Record, cert, key string) error {
	state := g.c.StateDir
	e.set("SOCIALWEB_KCP_HOST", rec.KcpURL)
	e.set("SOCIALWEB_KCP_KUBECONFIG", g.memberKubeconfig())
	e.set("SOCIALWEB_GATEWAY_NODECLAIM_AGENT", appctl.WebAppAccountRef(g.ws))
	e.dflt("SOCIALWEB_GATEWAY_ADDR", DefaultAddress)
	e.dflt("SOCIALWEB_GATEWAY_AUDIENCES", DefaultAudience)
	if n := g.c.Spec.Network; n != nil && n.PlcDirectoryUrl != "" {
		e.dflt("SOCIALWEB_GATEWAY_PLC_URL", n.PlcDirectoryUrl)
	}
	e.dflt("SOCIALWEB_GATEWAY_PLC_URL", DefaultPLC)
	e.set("SOCIALWEB_CLUSTERPROVIDER_UNIT_PREFIX", g.l.Names().HostProcessPrefix())
	e.set("SOCIALWEB_CLUSTERPROVIDER_HOSTPROCESS_WORKSPACE", g.ws)
	e.set("SOCIALWEB_CLUSTERPROVIDER_MOUNT_CONTEXT", kcpclient.ShardAdminContext)
	e.dflt("SOCIALWEB_CLUSTERPROVIDER_STATE_DIR", filepath.Join(state, "clusterprovider"))
	e.dflt("SOCIALWEB_CLUSTERPROVIDER_VAULT_STATE_DIR", state)
	e.dflt("SOCIALWEB_GATEWAY_CLUSTER_STATE_DIR", e.get("SOCIALWEB_CLUSTERPROVIDER_STATE_DIR"))
	e.dflt("SOCIALWEB_CLUSTERPROVIDER_LEASE_PATH", filepath.Join(state, "leases.json"))
	e.dflt("SOCIALWEB_CLUSTERPROVIDER_JOIN", DefaultJoin)
	e.dflt("SOCIALWEB_CLUSTERPROVIDER_DATASTORE_HAS_ROW", "false")
	for k, v := range ClusterProviderEnv(g.c.Spec.ClusterProvider) {
		e.set(k, v)
	}
	if q := g.c.Spec.Quota; q != nil && q.NodeClaims != 0 {
		e.set("SOCIALWEB_GATEWAY_NODECLAIM_QUOTA", strconv.Itoa(int(q.NodeClaims)))
	}
	for env, name := range map[string]string{"SOCIALWEB_CLUSTERPROVIDER_EXTERNAL_SECRETS": ExeExternalSecrets,
		"SOCIALWEB_CLUSTERPROVIDER_ESO_BUNDLE": ExeESOBundle, "SOCIALWEB_CLUSTERPROVIDER_DUMBPIPE": ExeDumbpipe,
		"SOCIALWEB_CLUSTERPROVIDER_K3S": ExeK3s, "SOCIALWEB_CLUSTERPROVIDER_KINE": ExeKine, "SOCIALWEB_CLUSTERPROVIDER_BAO": ExeBao} {
		if p := g.c.Executables[name].Path; p != "" {
			e.dflt(env, p)
		}
	}
	if root := g.c.Executables[ExePostgres].Path; root != "" {
		in, err := pgunit.FromRoot(root)
		if err != nil {
			return err
		}
		e.dflt("SOCIALWEB_CLUSTERPROVIDER_PG_DUMP", in.Path("pg_dump"))
	}
	if h, err := os.Hostname(); err == nil {
		e.dflt("SOCIALWEB_CLUSTERPROVIDER_HOST", h)
	}
	os.MkdirAll(e.get("SOCIALWEB_CLUSTERPROVIDER_STATE_DIR"), 0o700)
	if grant, audit, eso, err := materialize(filepath.Join(state, DeployDir)); err == nil {
		e.dflt("SOCIALWEB_CLUSTERPROVIDER_GRANT", grant)
		e.dflt("SOCIALWEB_CLUSTERPROVIDER_AUDIT_POLICY", audit)
		e.dflt("SOCIALWEB_CLUSTERPROVIDER_ESO_MANIFEST_DIR", eso)
	} else {
		g.c.log().Warn("the embedded deploy files could not be written; the clusterprovider reads its own defaults", "err", err.Error())
	}
	e.set("SOCIALWEB_TLS_CERT_FILE", cert)
	e.set("SOCIALWEB_TLS_KEY_FILE", key)
	return nil
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
