package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/openbao/openbao/sdk/v2/logical"
	"golang.org/x/crypto/ssh"
)

const (
	testIssuer   = "https://bidder.example/v1/auth/sshsig/wid"
	testRole     = "fc-guest-1"
	testSubject  = "actx:bidder:plc:requester:role:vm1"
	testAudience = "did:plc:abcdefghijklmnopqrstuvwx#role/oauth-vm1"
)

type harness struct {
	t       *testing.T
	b       logical.Backend
	storage logical.Storage
	signer  ssh.Signer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	conf := logical.TestBackendConfig()
	conf.StorageView = &logical.InmemStorage{}
	b, err := factory(context.Background(), conf)
	if err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, b: b, storage: conf.StorageView, signer: signer}
}

func (h *harness) call(op logical.Operation, path string, data map[string]interface{}) (*logical.Response, error) {
	h.t.Helper()
	return h.b.HandleRequest(context.Background(), &logical.Request{Operation: op, Path: path, Data: data, Storage: h.storage})
}

func (h *harness) ok(op logical.Operation, path string, data map[string]interface{}) *logical.Response {
	h.t.Helper()
	res, err := h.call(op, path, data)
	if err != nil || (res != nil && res.IsError()) {
		h.t.Fatalf("%s %s: %v %v", op, path, err, res)
	}
	return res
}

func sshsig(t *testing.T, signer ssh.Signer, namespace string, message []byte) string {
	t.Helper()
	sum := sha512.Sum512(message)
	signed := append([]byte(sshsigMagic), ssh.Marshal(sshsigSignedData{Namespace: namespace, HashAlgorithm: "sha512", Hash: sum[:]})...)
	sig, err := signer.Sign(rand.Reader, signed)
	if err != nil {
		t.Fatal(err)
	}
	blob := append([]byte(sshsigMagic), ssh.Marshal(sshsigEnvelope{
		Version: sshsigVersion, PublicKey: signer.PublicKey().Marshal(), Namespace: namespace,
		HashAlgorithm: "sha512", Signature: ssh.Marshal(sig),
	})...)
	return sshsigArmorBegin + "\n" + base64.StdEncoding.EncodeToString(blob) + "\n" + sshsigArmorEnd
}

func (h *harness) bound() {
	h.t.Helper()
	secret := "bind-secret-for-the-test"
	sum := sha256.Sum256([]byte(secret))
	h.ok(logical.UpdateOperation, "role/"+testRole, map[string]interface{}{"bind_secret_hash": hex.EncodeToString(sum[:])})
	h.ok(logical.UpdateOperation, "bind", map[string]interface{}{
		"role": testRole, "bind_secret": secret,
		"host_key":  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(h.signer.PublicKey()))),
		"signature": sshsig(h.t, h.signer, proveNamespace, []byte(secret)),
	})
	h.ok(logical.UpdateOperation, "wid/config", map[string]interface{}{"issuer": testIssuer})
	h.ok(logical.UpdateOperation, "role/"+testRole+"/wid", map[string]interface{}{"wid_subject": testSubject})
}

func (h *harness) token(signer ssh.Signer, audience string) (*logical.Response, error) {
	h.t.Helper()
	res := h.ok(logical.UpdateOperation, "challenge", map[string]interface{}{"role": testRole})
	challenge := res.Data["challenge"].(string)
	return h.call(logical.UpdateOperation, "wid/token", map[string]interface{}{
		"role": testRole, "challenge": challenge, "audience": audience,
		"signature": sshsig(h.t, signer, proveNamespace, []byte(challenge)),
	})
}

func rawBody(t *testing.T, res *logical.Response) map[string]interface{} {
	t.Helper()
	raw, ok := res.Data[logical.HTTPRawBody].([]byte)
	if !ok {
		t.Fatalf("no raw body in %v", res.Data)
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeSegment(t *testing.T, seg string, out interface{}) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func TestAGuestGetsAnOIDCJWTForAnyAudienceWithItsBoundHostKeyAndNoToken(t *testing.T) {
	h := newHarness(t)
	h.bound()
	res, err := h.token(h.signer, testAudience)
	if err != nil || res.IsError() {
		t.Fatalf("wid/token: %v %v", err, res)
	}
	if res.Auth != nil {
		t.Fatal("wid/token answered an OpenBao token")
	}
	jwt := res.Data["token"].(string)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", jwt)
	}
	var header struct {
		Alg, Kid, Typ string
	}
	decodeSegment(t, parts[0], &header)
	var claims map[string]interface{}
	decodeSegment(t, parts[1], &claims)
	if header.Alg != "ES256" || header.Kid == "" {
		t.Fatalf("header %+v", header)
	}
	if claims["iss"] != testIssuer || claims["sub"] != testSubject || claims["aud"] != testAudience {
		t.Fatalf("claims %v", claims)
	}
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat != 300 {
		t.Fatalf("lifetime %v s, want 300", exp-iat)
	}
	if claims["jti"] == "" || claims["jti"] == nil {
		t.Fatal("no jti")
	}

	discovery := rawBody(t, h.ok(logical.ReadOperation, "wid/.well-known/openid-configuration", nil))
	if discovery["issuer"] != testIssuer || discovery["jwks_uri"] != testIssuer+"/.well-known/keys" {
		t.Fatalf("discovery %v", discovery)
	}
	keys := rawBody(t, h.ok(logical.ReadOperation, "wid/.well-known/keys", nil))
	var pub *ecdsa.PublicKey
	for _, k := range keys["keys"].([]interface{}) {
		m := k.(map[string]interface{})
		if m["kid"] != header.Kid {
			continue
		}
		x, _ := base64.RawURLEncoding.DecodeString(m["x"].(string))
		y, _ := base64.RawURLEncoding.DecodeString(m["y"].(string))
		if m["kty"] != "EC" || m["crv"] != "P-256" {
			t.Fatalf("key %v", m)
		}
		pub = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	}
	if pub == nil {
		t.Fatalf("no key %q in the published set", header.Kid)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the JWT does not verify with the published key")
	}

	for _, aud := range []string{"https://app.example/x", "api://socialweb-node?actx=c/n"} {
		if res, err := h.token(h.signer, aud); err != nil || res.IsError() {
			t.Fatalf("audience %s: %v %v", aud, err, res)
		}
	}
}

func TestATokenNeedsTheBoundKeyAFreshChallengeAndAnAudience(t *testing.T) {
	h := newHarness(t)
	h.bound()
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	stranger, _ := ssh.NewSignerFromKey(other)
	if res, err := h.token(stranger, testAudience); err == nil && !res.IsError() {
		t.Fatal("a signature by another key got a token")
	}
	res := h.ok(logical.UpdateOperation, "challenge", map[string]interface{}{"role": testRole})
	challenge := res.Data["challenge"].(string)
	data := map[string]interface{}{"role": testRole, "challenge": challenge, "audience": testAudience,
		"signature": sshsig(t, h.signer, proveNamespace, []byte(challenge))}
	if res, err := h.call(logical.UpdateOperation, "wid/token", data); err != nil || res.IsError() {
		t.Fatalf("first use: %v %v", err, res)
	}
	if res, err := h.call(logical.UpdateOperation, "wid/token", data); err == nil && !res.IsError() {
		t.Fatal("a challenge was used twice")
	}
	for _, aud := range []string{"", "has space", "ftp://x", "did:", "api://noactx"} {
		if res, err := h.token(h.signer, aud); err == nil && !res.IsError() {
			t.Fatalf("audience %q got a token", aud)
		}
	}
}
