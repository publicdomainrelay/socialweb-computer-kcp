package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/deploy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
)

const (
	KnotlessOption     = "knotless"
	KnotlessPhase      = "knotless"
	KnotlessDir        = "knotless"
	KnotlessServeHost  = "knotless.localhost"
	KnotlessRunnerHost = "knotless-runner.localhost"
	KnotlessClientEnv  = "client.env"
	KnotlessOwnerEnv   = "owner.env"
	KnotlessTemplate   = "apps/gateway/knotless.yaml"
)

func (p *pass) knotless() (manifests.Knotless, []v1alpha1.ApplicationListener, error) {
	o := p.o
	bin := strings.TrimSpace(compose.OptionValue(o.Spec.Options, KnotlessOption))
	if bin == "" {
		return manifests.Knotless{}, nil, nil
	}
	n, m := o.Spec.Network, o.Spec.MarketNetwork
	if o.Spec.Profile != ProfileGatewayNetwork || n == nil || m == nil || m.OwnerSessionsFile == "" {
		return manifests.Knotless{}, nil, errors.New("gateway: option knotless runs only in a market network's gateway, as its owner (dev up --with-gateway --with-knotless)")
	}
	proxy, relay := os.Getenv("HTTPS_PROXY"), n.RelayURL()
	if proxy == "" || relay == "" {
		return manifests.Knotless{}, nil, errors.New("gateway: option knotless needs the network's relay and this core's egress to the guest TLS front")
	}
	did, owner, err := compose.FirstOwnerSession(m.OwnerSessionsFile)
	if err != nil {
		return manifests.Knotless{}, nil, err
	}
	dir := filepath.Join(p.c.StateRoot(), KnotlessDir)
	for _, d := range []string{"serve", "runner"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return manifests.Knotless{}, nil, err
		}
	}
	ports, err := component.OpenPortRegistry(component.PortRegistryOptions{StateDir: dir})
	if err != nil {
		return manifests.Knotless{}, nil, err
	}
	serve, err := ports.Allocate("knotless-serve")
	if err != nil {
		return manifests.Knotless{}, nil, err
	}
	run, err := ports.Allocate("knotless-run")
	if err != nil {
		return manifests.Knotless{}, nil, err
	}
	ca, err := compose.NetworkCAFile(dir, n)
	if err != nil {
		return manifests.Knotless{}, nil, err
	}
	if err := ensureIrohKey(filepath.Join(dir, "runner", "iroh")); err != nil {
		return manifests.Knotless{}, nil, err
	}
	v := manifests.Knotless{
		ServeUnit: p.c.Config().Layout.Names().HostProcessPrefix() + "knotless-serve.service",
		RunUnit:   p.c.Config().Layout.Names().HostProcessPrefix() + "knotless-run.service",
		Bin:       bin, Dir: dir, ServeListen: serve.Address(), RunListen: run.Address(),
		Appview: "http://" + KnotlessServeHost + ":" + strconv.Itoa(serve.Port), Origin: "http://" + KnotlessRunnerHost + ":" + strconv.Itoa(run.Port),
		Owner: did, Plc: n.PLCDirectoryURL, CA: ca, Proxy: proxy, Firehose: "ws" + strings.TrimPrefix(owner.Service, "http"), Relay: relay,
		EnvFile: filepath.Join(dir, KnotlessOwnerEnv), PasswordFile: filepath.Join(dir, "password"),
	}
	key, err := json.Marshal(v)
	if err != nil {
		return manifests.Knotless{}, nil, err
	}
	template, err := fs.ReadFile(deploy.FS, KnotlessTemplate)
	if err != nil {
		return manifests.Knotless{}, nil, err
	}
	sum := sha256.Sum256(append(key, template...))
	v.RestartKey = hex.EncodeToString(sum[:8])
	secret := fmt.Sprintf("KNOTLESS_PASSWORD=%s\nOWNER_ATPROTO_HANDLE=%s\nOWNER_ATPROTO_PASSWORD=%s\n", owner.Password, owner.Identifier, owner.Password)
	if err := compose.WriteSecret(v.EnvFile, []byte(secret)); err != nil {
		return manifests.Knotless{}, nil, err
	}
	if err := compose.WriteSecret(v.PasswordFile, []byte(owner.Password+"\n")); err != nil {
		return manifests.Knotless{}, nil, err
	}
	client := strings.Join([]string{"KNOTLESS_APPVIEW=" + v.Appview, "KNOTLESS_RUNNER=" + v.Origin, "KNOTLESS_IDENTIFIER=" + did,
		"KNOTLESS_PLC_URL=" + v.Plc, "KNOTLESS_CA_CERTS=" + ca, "KNOTLESS_HANDLE_RESOLVER=well-known:https", "KNOTLESS_DID_WEB_SCHEME=http", "KNOTLESS_NO_UPDATE_CHECK=1",
		"HTTPS_PROXY=" + proxy, "KNOTLESS_RELAY=" + relay, "KNOTLESS_PDS=" + owner.Service, "KNOTLESS_BIN=" + bin}, "\n") + "\n"
	if err := compose.WriteFileAtomic(filepath.Join(dir, KnotlessClientEnv), []byte(client), 0o644); err != nil {
		return manifests.Knotless{}, nil, err
	}
	return v, []v1alpha1.ApplicationListener{
		{Component: KnotlessOption, Name: "appview", Kind: "http", Address: v.Appview},
		{Component: KnotlessOption, Name: "runner", Kind: "http", Address: v.Origin},
		{Component: KnotlessOption, Name: "env", Kind: "file", Address: filepath.Join(dir, KnotlessClientEnv)},
	}, nil
}

func ensureIrohKey(path string) error {
	if _, err := os.Stat(path + ".pub"); err == nil {
		return nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "knotless-runner")
	if err != nil {
		return err
	}
	if err := compose.WriteSecret(path, pem.EncodeToMemory(block)); err != nil {
		return err
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	return compose.WriteFileAtomic(path+".pub", ssh.MarshalAuthorizedKey(key), 0o644)
}
