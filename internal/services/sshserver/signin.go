package sshserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway/custody"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
)

const (
	VaultClient = "ssh-gateway"

	atprotoMount = "atproto"

	renewBelow = 30 * time.Minute

	renewIncrement = time.Hour
)

type webSignIn struct {
	signIn *custody.SignIn

	auth *gatewayhttp.RequestAuth

	oauth *gatewayhttp.OAuth

	tasks *supervise.Group
}

func (w *webSignIn) close() {
	if w == nil {
		return
	}
	_ = w.tasks.Stop()
	w.signIn.Close()
}

func (s *Service) publicOrigin() string {
	return strings.TrimRight(strings.TrimSpace(s.spec.PublicOrigin), "/")
}

func (s *Service) openSignIn(ctx context.Context) (*webSignIn, error) {
	origin := s.publicOrigin()
	if s.spec.VaultRef == nil || origin == "" || len(s.ownerSessions) > 0 {
		return nil, nil
	}
	endpoint, err := s.vaultEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	signIn, err := custody.New(ctx, custody.Config{Endpoint: endpoint, Role: custody.DefaultRole, RenewBelow: renewBelow,
		RenewIncrement: renewIncrement, Network: s.spec.Network}, custody.Deps{TLS: s.tls, Log: s.log})
	if err != nil {
		return nil, err
	}
	tasks := supervise.New(ctx)
	web := &webSignIn{signIn: signIn, tasks: tasks}
	if web.auth, err = gatewayhttp.NewRequestAuth(gatewayhttp.RequestAuthOptions{SignIn: signIn, PublicOrigin: origin, Log: s.log}); err != nil {
		web.close()
		return nil, err
	}
	if web.oauth, err = gatewayhttp.NewOAuth(gatewayhttp.OAuthOptions{SignIn: signIn, Auth: web.auth, PublicOrigin: origin,
		PostSignInURL: origin + "/", OnSignedIn: s.signedIn, Tasks: tasks, Log: s.log}); err != nil {
		web.close()
		return nil, err
	}
	return web, nil
}

func (s *Service) vaultEndpoint(ctx context.Context) (custody.Endpoint, error) {
	vref := s.spec.VaultRef
	if s.deps.KCP == nil {
		return custody.Endpoint{}, errors.New("sshserver: no kcp client for the vault")
	}
	workspace := cmp.Or(vref.Workspace, s.workspace())
	client, err := s.deps.KCP.Client(workspace)
	if err != nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: kcp client for %s: %w", workspace, err)
	}
	restConfig := client.RESTConfig()
	if restConfig == nil || restConfig.Host == "" {
		return custody.Endpoint{}, fmt.Errorf("sshserver: the kcp client for %s has no rest config", workspace)
	}
	store, err := kcpstore.New(kcpstore.Options{Host: restConfig.Host, RestConfig: restConfig})
	if err != nil {
		return custody.Endpoint{}, err
	}
	server, err := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](store, v1alpha1.OpenBaoServerGVR).Get(ctx, ref.New(workspace, "", vref.Name))
	if err != nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: reading OpenBaoServer %s/%s: %w", workspace, vref.Name, err)
	}
	status := server.Status
	if status.Phase != v1alpha1.OpenBaoServerPhaseReady || status.Address == "" || status.Refs == nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: OpenBaoServer %s/%s is %s, not Ready", workspace, vref.Name, status.Phase)
	}
	var vclient *v1alpha1.OpenBaoServerClientRefs
	for i := range status.Clients {
		if status.Clients[i].Name == VaultClient {
			vclient = &status.Clients[i]
		}
	}
	if vclient == nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: OpenBaoServer %s/%s reports no client %q", workspace, vref.Name, VaultClient)
	}
	ca, err := readFileRef(status.Refs.CA)
	if err != nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: the OpenBao CA: %w", err)
	}
	cert, err := readFileRef(vclient.ClientCert)
	if err != nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: the OpenBao client certificate: %w", err)
	}
	key, err := readFileRef(vclient.ClientKey)
	if err != nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: the OpenBao client key: %w", err)
	}
	return custody.Endpoint{Address: status.Address, CACert: ca, ClientCert: cert, ClientKey: key, CertMount: vclient.CertAuthMount,
		CertRole: vclient.CertAuthRole, Mount: atprotoMount, Timeout: custody.DefaultTimeout}, nil
}

func readFileRef(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("the ref is empty")
	}
	return os.ReadFile(strings.TrimPrefix(path, "file:"))
}

func (s *Service) signedIn(ctx context.Context, user *gatewayhttp.WebUser) error {
	if user == nil || strings.TrimSpace(user.DID) == "" {
		return errors.New("sshserver: the sign-in names no DID")
	}
	return s.ensureAccount(ctx, user.DID)
}

func (s *Service) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
	_, _ = w.Write([]byte(signInPage))
}

const signInPage = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>socialweb.computer ssh</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem}input,textarea,button{font:inherit}textarea{width:100%;height:5rem}li{margin:.3rem 0}code{word-break:break-all}</style>
</head><body>
<h1>socialweb.computer ssh</h1>
<div id="out"></div>
<form id="signin" hidden><p>Sign in with your atproto handle, then add the ssh public key you connect with.</p>
<input id="handle" placeholder="you.bsky.social" required> <button>Sign in</button></form>
<div id="keys" hidden>
<p>Signed in as <b id="who"></b>. Connect with <code id="cmd"></code> <button id="signout">Sign out</button></p>
<h2>ssh keys</h2><ul id="list"></ul>
<form id="add"><textarea id="key" placeholder="ssh-ed25519 AAAA... comment" required></textarea><br><button>Add key</button></form>
</div>
<script>
const $ = (id) => document.getElementById(id);
const say = (t) => { $("out").textContent = t; };
const call = async (method, path, body) => {
  const r = await fetch(path, {method, credentials: "same-origin", headers: body ? {"Content-Type": "application/json"} : {}, body: body ? JSON.stringify(body) : undefined});
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.message || j.error || r.status);
  return j;
};
const keys = async () => {
  const j = await call("GET", "/keys");
  $("list").replaceChildren(...(j.keys || []).map((k) => {
    const li = document.createElement("li");
    const b = document.createElement("button");
    b.textContent = "Remove";
    b.onclick = async () => { try { await call("DELETE", "/keys/" + encodeURIComponent(k.rkey)); await keys(); } catch (e) { say(e.message); } };
    li.append(k.name + " " + k.keyId.slice(0, 40) + "... ", b);
    return li;
  }));
};
const load = async () => {
  const err = new URLSearchParams(location.search).get("error");
  if (err) say(err);
  try {
    const s = await call("GET", "/session");
    $("who").textContent = s.handle || s.did;
    $("cmd").textContent = "ssh " + (s.handle || s.did) + "@" + location.hostname;
    $("keys").hidden = false;
    await keys();
  } catch (e) { $("signin").hidden = false; }
};
$("signin").onsubmit = async (e) => { e.preventDefault(); try { const j = await call("POST", "/oauth/start", {handle: $("handle").value}); location.href = j.authorizationUrl; } catch (x) { say(x.message); } };
$("add").onsubmit = async (e) => { e.preventDefault(); try { await call("POST", "/keys", {publicKey: $("key").value}); $("key").value = ""; await keys(); } catch (x) { say(x.message); } };
$("signout").onclick = async () => { try { await call("DELETE", "/session"); } catch (x) {} location.reload(); };
load();
</script></body></html>
`
