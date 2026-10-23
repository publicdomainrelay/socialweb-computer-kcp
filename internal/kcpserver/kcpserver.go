package kcpserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

const WorkspaceMountsGate = "--feature-gates=WorkspaceMounts=true"

const LeaderElection = "--enable-leader-election"

const SlotStopTimeout = 30 * time.Second

const EventTTL = "1h"

const EventTTLFlag = "--event-ttl=" + EventTTL

const ShardAdminUser = "shard-admin"

const ShardBaseURLFlag = "--shard-base-url="

const MountProxyCertFlag = "--mount-proxy-client-cert-file="

const (
	MountProxyCertName = "mount-proxy-client.crt"
	MountProxyKeyName  = "mount-proxy-client.key"
)

const AuditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy

omitStages:
  - RequestReceived

rules:
  - level: Metadata
`

type SlotConfig struct {
	Unit        string
	Slice       string
	Exec        string
	Root        string
	EtcdServers string
	FrontURL    string
	Wants       []string
	Shape       Shape
}

func SlotArgv(c SlotConfig) ([]string, error) {
	if !filepath.IsAbs(c.Exec) {
		return nil, fmt.Errorf("kcpserver: the kcp executable must be an absolute path, got %q (dev builds take it from kcp-path, never PATH)", c.Exec)
	}
	if !filepath.IsAbs(c.Root) {
		return nil, errors.New("kcpserver: the slot root directory must be absolute")
	}
	if c.EtcdServers == "" {
		return nil, errors.New("kcpserver: a slot needs --etcd-servers (its kine); there is no embedded etcd")
	}
	if c.Shape != ShapeSingle && c.Shape != ShapeBlueGreen {
		return nil, fmt.Errorf("kcpserver: slot shape %q: want %s (a bidder's one slot) or %s (the gateway's slots)", c.Shape, ShapeSingle, ShapeBlueGreen)
	}
	argv := []string{c.Exec, "start",
		"--root-directory=" + c.Root,
		"--etcd-servers=" + c.EtcdServers,
		"--bind-address=127.0.0.1",
		"--secure-port=0",
		"--experimental-bind-free-port",
		"--shutdown-delay-duration=2s",
		WorkspaceMountsGate,
		EventTTLFlag,
		"--audit-policy-file=" + filepath.Join(c.Root, "audit-policy.yaml"),
		"--audit-log-path=" + filepath.Join(c.Root, "audit.log"),
		"--audit-log-maxsize=100", "--audit-log-maxage=1", "--audit-log-maxbackup=4",
		MountProxyCertFlag + filepath.Join(c.Root, MountProxyCertName),
		"--mount-proxy-client-key-file=" + filepath.Join(c.Root, MountProxyKeyName),
	}
	if c.Shape == ShapeBlueGreen {
		argv = append(argv, LeaderElection)
	}
	if c.FrontURL != "" {
		argv = append(argv, ShardBaseURLFlag+c.FrontURL)
	}
	return argv, CheckArgv(argv, c.Shape)
}

func CheckArgv(argv []string, shape Shape) error {
	has := func(want string) bool {
		return slices.Contains(argv, want)
	}
	hasPrefix := func(want string) bool {
		for _, a := range argv {
			if strings.HasPrefix(a, want) && len(a) > len(want) {
				return true
			}
		}
		return false
	}
	if !has(WorkspaceMountsGate) {
		return fmt.Errorf("kcpserver: kcp refused without %s: every tenant's mounted API silently stops being served (versions.json kcp invariant)", WorkspaceMountsGate)
	}
	if !has(EventTTLFlag) {
		return fmt.Errorf("kcpserver: kcp refused without %s: the Event objects of a busy network would stay in the store for the apiserver's default", EventTTLFlag)
	}
	if !has("--secure-port=0") {
		return errors.New("kcpserver: kcp refused on a fixed port: behind the edge every listener binds port 0")
	}
	if !hasPrefix(MountProxyCertFlag) {
		return fmt.Errorf("kcpserver: kcp refused without %s: kcp then proxies every request to a mounted workspace through a new transport and never closes its connection", MountProxyCertFlag)
	}
	if !hasPrefix(ShardBaseURLFlag) {
		return fmt.Errorf("kcpserver: kcp refused without %s<front>: a slot that advertises its own port is cut by the next swap", ShardBaseURLFlag)
	}
	switch shape {
	case ShapeBlueGreen:
		if !has(LeaderElection) {
			return fmt.Errorf("kcpserver: a blue/green slot refused without %s: two slots would both run every controller", LeaderElection)
		}
	case ShapeSingle:
		if has(LeaderElection) {
			return fmt.Errorf("kcpserver: a single slot refused with %s: no second slot exists and a restart would strand the controllers for the 60 s lease", LeaderElection)
		}
	default:
		return fmt.Errorf("kcpserver: unknown slot shape %q", shape)
	}
	return nil
}

func WriteSlotFiles(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "audit-policy.yaml"), []byte(AuditPolicy), 0o600); err != nil {
		return err
	}
	return writeMountProxyClient(root)
}

func writeMountProxyClient(root string) error {
	certPath, keyPath := filepath.Join(root, MountProxyCertName), filepath.Join(root, MountProxyKeyName)
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: "kcp-mount-proxy"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
}

const SlotMemoryMax = 3 << 30

func SlotSpec(c SlotConfig) (hostsvc.UnitSpec, error) {
	argv, err := SlotArgv(c)
	if err != nil {
		return hostsvc.UnitSpec{}, err
	}
	return hostsvc.UnitSpec{
		Name:        c.Unit,
		Description: "kcp slot of one application instance (" + string(c.Shape) + ")",
		Exec:        argv,
		WorkDir:     c.Root,
		Slice:       c.Slice,
		Restart:     hostsvc.RestartOnFailure,
		KillSignal:  syscall.SIGTERM,
		StopTimeout: SlotStopTimeout,
		Limits:      hostsvc.Limits{MemoryMax: SlotMemoryMax},
		After:       c.Wants,
		Wants:       c.Wants,
	}, nil
}

type Admin struct {
	Token  string
	CA     []byte
	Server string
}

func ReadAdmin(kubeconfig string) (Admin, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return Admin{}, err
	}
	var a Admin
	if u, ok := cfg.AuthInfos[ShardAdminUser]; ok && u.Token != "" {
		a.Token = u.Token
	}
	for name, ctx := range cfg.Contexts {
		if ctx.AuthInfo != ShardAdminUser && !(a.Token == "" && name == cfg.CurrentContext) {
			continue
		}
		if cl, ok := cfg.Clusters[ctx.Cluster]; ok {
			a.CA, a.Server = cl.CertificateAuthorityData, cl.Server
		}
		if a.Token == "" {
			if u, ok := cfg.AuthInfos[ctx.AuthInfo]; ok {
				a.Token = u.Token
			}
		}
		break
	}
	if a.Token == "" || len(a.CA) == 0 {
		return Admin{}, fmt.Errorf("kcpserver: %s holds no %s token and CA yet", kubeconfig, ShardAdminUser)
	}
	return a, nil
}

func PortOf(server string) (int, error) {
	u, err := url.Parse(server)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Port())
}

func Readyz(ctx context.Context, port int, a Admin) (int, string, error) {
	c, err := loopbackClient(a.CA, 5*time.Second)
	if err != nil {
		return 0, "", errors.New("kcpserver: the kubeconfig CA does not parse")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:"+strconv.Itoa(port)+"/readyz", nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

func WaitReady(ctx context.Context, port int, kubeconfig string, notBefore time.Time, every time.Duration,
	progress ...func(attempt int, last error)) (time.Time, error) {
	var last error
	for attempt := 1; ; attempt++ {
		fi, err := os.Stat(kubeconfig)
		switch {
		case err != nil:
			last = err
		case fi.ModTime().Before(notBefore.Add(-time.Second)):
			last = fmt.Errorf("%s predates this start", kubeconfig)
		default:
			a, err := ReadAdmin(kubeconfig)
			if err != nil {
				last = err
				break
			}
			code, body, err := Readyz(ctx, port, a)
			if err == nil && code == http.StatusOK {
				return time.Now().UTC(), nil
			}
			last = fmt.Errorf("readyz on 127.0.0.1:%d: %d %q %v", port, code, body, err)
		}
		report(progress, attempt, last)
		select {
		case <-ctx.Done():
			return time.Time{}, fmt.Errorf("kcpserver: not ready: %v (%w)", last, ctx.Err())
		case <-time.After(every):
		}
	}
}

func report(progress []func(attempt int, last error), attempt int, last error) {
	for _, p := range progress {
		if p != nil {
			p(attempt, last)
		}
	}
}
