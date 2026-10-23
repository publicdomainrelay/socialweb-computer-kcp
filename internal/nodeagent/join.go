package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/childproc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodemarket"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/systemdunit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/systemdunit/notify"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const openBaoLinkName = "openbao"

const apiserverLinkName = "apiserver"

type claimDocument struct {
	Node string `json:"node"`

	JWTMount string `json:"jwtMount"`

	Role string `json:"role"`

	Audience string `json:"audience"`

	KVMount string `json:"kvMount"`

	KVPath string `json:"kvPath"`
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("nodeagent: no config path was given")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("nodeagent: reading %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("nodeagent: %s is not a join config: %w", path, err)
	}
	cfg = cfg.WithDefaults()
	cfg.ConfigPath = path
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Join(ctx context.Context, cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !executable(cfg.DumbpipePath) {
		return fmt.Errorf("nodeagent: %s is not installed", cfg.DumbpipePath)
	}
	if _, err := openBaoLinkArgs(cfg); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		superviseLink(runCtx, cfg, openBaoLinkName, log)
	})
	if err := prepare(runCtx, cfg, log); err != nil {
		cancel()
		wg.Wait()
		return err
	}
	if _, err := apiserverLinkArgs(ctx, cfg); err != nil {
		cancel()
		wg.Wait()
		return err
	}
	wg.Go(func() {
		superviseLink(runCtx, cfg, apiserverLinkName, log)
	})
	if err := notify.Ready("joined the cluster; supervising the OpenBao and apiserver links"); err != nil {
		log.Warn("sd_notify READY failed", "err", err.Error())
	}
	log.Info("the join is complete; the links run under this service", "config", cfg.ConfigPath)
	<-ctx.Done()
	cancel()
	wg.Wait()
	return nil
}

func prepare(ctx context.Context, cfg Config, log *slog.Logger) error {
	if fileNonEmpty(cfg.DoneStamp) && fileNonEmpty(cfg.APIServerEnvPath) && fileNonEmpty(cfg.MeshdEnvPath) {
		log.Info("the join secrets were already fetched; keeping them and starting the links",
			"stamp", cfg.DoneStamp)
		return nil
	}
	for _, path := range []string{cfg.ClaimDocPath, cfg.ClaimCAPath} {
		if !fileNonEmpty(path) {
			return fmt.Errorf("nodeagent: %s is missing", path)
		}
	}
	raw, err := os.ReadFile(cfg.ClaimDocPath)
	if err != nil {
		return fmt.Errorf("nodeagent: reading %s: %w", cfg.ClaimDocPath, err)
	}
	var doc claimDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("nodeagent: %s is not a claim document: %w", cfg.ClaimDocPath, err)
	}
	for name, value := range map[string]string{
		"node": doc.Node, "jwtMount": doc.JWTMount, "role": doc.Role,
		"kvMount": doc.KVMount, "kvPath": doc.KVPath,
	} {
		if value == "" {
			return fmt.Errorf("nodeagent: %s carries no %s", cfg.ClaimDocPath, name)
		}
		if !validName(value) {
			return fmt.Errorf("nodeagent: %s carries an unsafe name for %s: %q", cfg.ClaimDocPath, name, value)
		}
	}
	if doc.Audience == "" {
		return fmt.Errorf("nodeagent: %s carries no audience", cfg.ClaimDocPath)
	}
	exchanged, err := exchangeWorkloadToken(ctx, cfg, doc.Audience, log)
	if err != nil {
		return err
	}
	caPEM, err := os.ReadFile(cfg.ClaimCAPath)
	if err != nil {
		return fmt.Errorf("nodeagent: reading %s: %w", cfg.ClaimCAPath, err)
	}
	pool, err := openbao.CertPoolFromPEM(caPEM)
	if err != nil {
		return fmt.Errorf("nodeagent: %s: %w", cfg.ClaimCAPath, err)
	}
	client := openbao.New("https://"+cfg.OpenBaoLinkAddress, "", openbao.WithCACertPool(pool))
	logged, err := loginOpenBao(ctx, cfg, client, doc, exchanged, log)
	if err != nil {
		return err
	}
	data, err := readNodeKV(ctx, cfg, logged, doc, log)
	if err != nil {
		return err
	}
	token, ok := firstValue(data, "token")
	if !ok {
		return fmt.Errorf("nodeagent: %s/%s has no token", doc.KVMount, doc.KVPath)
	}
	nodePassword, ok := firstValue(data, "nodePassword")
	if !ok {
		return fmt.Errorf("nodeagent: %s/%s has no nodePassword", doc.KVMount, doc.KVPath)
	}
	serverCA, ok := firstValue(data, "serverCA")
	if !ok {
		return fmt.Errorf("nodeagent: %s/%s has no serverCA", doc.KVMount, doc.KVPath)
	}
	address, port, ticket := "", "", ""
	fromBundle := fileNonEmpty(cfg.APIServerEnvPath)
	if fromBundle {
		env, err := readEnvFile(cfg.APIServerEnvPath)
		if err != nil {
			return fmt.Errorf("nodeagent: reading %s: %w", cfg.APIServerEnvPath, err)
		}
		if env["NODE_NAME"] != doc.Node {
			return fmt.Errorf("nodeagent: the bundled apiserver link names %q, not %q", env["NODE_NAME"], doc.Node)
		}
		address, port, ticket = env["ADVERTISE_ADDRESS"], env["APISERVER_PORT"], env["APISERVER_TICKET"]
		log.Info("the apiserver link came with the bundle", "path", cfg.APIServerEnvPath)
	} else {
		for _, key := range []string{"apiserverTicket", "advertiseAddress", "apiserverPort"} {
			if _, ok := firstValue(data, key); !ok {
				return fmt.Errorf("nodeagent: %s/%s has no %s", doc.KVMount, doc.KVPath, key)
			}
		}
		address, _ = firstValue(data, "advertiseAddress")
		port, _ = firstValue(data, "apiserverPort")
		ticket, _ = firstValue(data, "apiserverTicket")
	}
	if err := guardLinkEnv(address, port, ticket); err != nil {
		return err
	}
	joinToken := stripSpace(token)
	if !strings.HasPrefix(joinToken, "K10") {
		joinToken = nodemarket.PinnedJoinToken(serverCA, joinToken)
	}
	for _, dir := range []string{cfg.NodeDir, filepath.Dir(cfg.K3sNodePasswordPath)} {
		if err := ensureDir(dir, 0o700); err != nil {
			return fmt.Errorf("nodeagent: creating %s: %w", dir, err)
		}
	}
	if err := writeAtomic(cfg.ServerCAPath, []byte(serverCA), 0o600); err != nil {
		return err
	}
	if err := writeAtomic(cfg.TokenPath, []byte(joinToken), 0o600); err != nil {
		return err
	}
	password := strings.ReplaceAll(nodePassword, "\n", "")
	if err := writeAtomic(cfg.NodePasswordPath, []byte(password), 0o600); err != nil {
		return err
	}
	if err := writeAtomic(cfg.K3sNodePasswordPath, []byte(password), 0o600); err != nil {
		return err
	}
	if !fromBundle {
		env := fmt.Sprintf("NODE_NAME=%s\nADVERTISE_ADDRESS=%s\nAPISERVER_PORT=%s\nAPISERVER_TICKET=%s\n",
			doc.Node, address, port, ticket)
		if err := writeAtomic(cfg.APIServerEnvPath, []byte(env), 0o600); err != nil {
			return err
		}
	}
	meshdEnv := fmt.Sprintf("SOCIALWEB_MESHD_NODE_NAME=%s\nSOCIALWEB_MESHD_KUBECONFIG=%s\n", doc.Node, cfg.KubeletKubeconfig)
	if err := writeAtomic(cfg.MeshdEnvPath, []byte(meshdEnv), 0o600); err != nil {
		return err
	}
	if err := writeAtomic(cfg.DoneStamp, nil, 0o600); err != nil {
		return err
	}
	log.Info("fetched the join secrets", "node", doc.Node, "kv", doc.KVMount+"/"+doc.KVPath)
	return nil
}

func exchangeWorkloadToken(ctx context.Context, cfg Config, audience string, log *slog.Logger) (string, error) {
	waited := 0
	for !executable(cfg.GuestLoginHelperPath) {
		if waited >= cfg.GuestLoginWaitAttempts {
			return "", fmt.Errorf("nodeagent: the guest login helper %s never appeared", cfg.GuestLoginHelperPath)
		}
		waited++
		log.Info("waiting for the guest login helper", "path", cfg.GuestLoginHelperPath,
			"attempt", waited, "of", cfg.GuestLoginWaitAttempts)
		if err := clock.SleepErr(ctx, cfg.GuestLoginWaitInterval()); err != nil {
			return "", err
		}
	}
	out, err := run(ctx, cfg.GuestLoginHelperPath, "token", audience)
	if err != nil {
		return "", fmt.Errorf("nodeagent: exchanging the workload identity token for %s: %w", audience, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("nodeagent: no workload identity token from the bidder's OpenBao for %s", audience)
	}
	return token, nil
}

func loginOpenBao(ctx context.Context, cfg Config, client *openbao.Client, doc claimDocument, exchanged string, log *slog.Logger) (*openbao.Client, error) {
	for attempt := 1; attempt <= cfg.LinkWaitAttempts; attempt++ {
		logged, err := client.LoginJWT(ctx, doc.JWTMount, doc.Role, exchanged)
		if err == nil {
			log.Info("logged in to the cluster OpenBao", "mount", doc.JWTMount, "role", doc.Role)
			return logged, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Info("the OpenBao link has not answered yet", "address", cfg.OpenBaoLinkAddress,
			"attempt", attempt, "of", cfg.LinkWaitAttempts, "err", errString(err))
		if err := clock.SleepErr(ctx, cfg.LinkWaitInterval()); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("nodeagent: OpenBao login at auth/%s as %s failed after %d attempts at the link on %s",
		doc.JWTMount, doc.Role, cfg.LinkWaitAttempts, cfg.OpenBaoLinkAddress)
}

func readNodeKV(ctx context.Context, cfg Config, client *openbao.Client, doc claimDocument, log *slog.Logger) (map[string]any, error) {
	path := strings.Trim(doc.KVMount, "/") + "/data/" + strings.Trim(doc.KVPath, "/")
	for attempt := 1; attempt <= cfg.LinkWaitAttempts; attempt++ {
		response, err := client.Read(ctx, path)
		if err == nil {
			data, dataErr := kvData(response)
			if dataErr == nil {
				return data, nil
			}
			err = dataErr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Info("the OpenBao link has not answered with the node secrets yet", "address", cfg.OpenBaoLinkAddress,
			"attempt", attempt, "of", cfg.LinkWaitAttempts, "err", errString(err))
		if err := clock.SleepErr(ctx, cfg.LinkWaitInterval()); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("nodeagent: reading %s/%s failed after %d attempts at the link on %s",
		doc.KVMount, doc.KVPath, cfg.LinkWaitAttempts, cfg.OpenBaoLinkAddress)
}

func kvData(response map[string]any) (map[string]any, error) {
	envelope, ok := response["data"].(map[string]any)
	if !ok {
		return nil, errors.New("the response carries no data object")
	}
	if data, ok := envelope["data"].(map[string]any); ok {
		return data, nil
	}
	return envelope, nil
}

func openBaoLinkArgs(cfg Config) ([]string, error) {
	raw, err := os.ReadFile(cfg.OpenBaoTicketPath)
	if err != nil {
		return nil, fmt.Errorf("nodeagent: reading the OpenBao ticket %s: %w", cfg.OpenBaoTicketPath, err)
	}
	ticket := strings.TrimSpace(string(raw))
	if ticket == "" {
		return nil, fmt.Errorf("nodeagent: the OpenBao ticket %s is empty", cfg.OpenBaoTicketPath)
	}
	return []string{"connect-tcp", "--addr", cfg.OpenBaoLinkAddress, ticket}, nil
}

func apiserverLinkArgs(ctx context.Context, cfg Config) ([]string, error) {
	env, err := readEnvFile(cfg.APIServerEnvPath)
	if err != nil {
		return nil, fmt.Errorf("nodeagent: reading %s: %w", cfg.APIServerEnvPath, err)
	}
	address, port, ticket := env["ADVERTISE_ADDRESS"], env["APISERVER_PORT"], env["APISERVER_TICKET"]
	if err := guardLinkEnv(address, port, ticket); err != nil {
		return nil, err
	}
	if err := ensureLoopbackAlias(ctx, address); err != nil {
		return nil, err
	}
	return []string{"connect-tcp", "--addr", address + ":" + port, ticket}, nil
}

func guardLinkEnv(address, port, ticket string) error {
	if !validIPv4(address) {
		return errors.New("nodeagent: advertiseAddress is not an IPv4 address")
	}
	if !validPort(port) {
		return errors.New("nodeagent: apiserverPort is not a port")
	}
	if !validTicket(ticket) {
		return errors.New("nodeagent: apiserverTicket is not a ticket")
	}
	return nil
}

func ensureLoopbackAlias(ctx context.Context, address string) error {
	iface, err := net.InterfaceByName("lo")
	if err == nil {
		addrs, err := iface.Addrs()
		if err == nil {
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip != nil && ip.String() == address {
					return nil
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := run(ctx, "ip", "addr", "add", address+"/32", "dev", "lo"); err != nil {
		return fmt.Errorf("nodeagent: adding %s/32 to lo: %w", address, err)
	}
	return nil
}

func superviseLink(ctx context.Context, cfg Config, name string, log *slog.Logger) {
	backoff := systemdunit.Backoff{Min: time.Second, Max: 30 * time.Second}
	for {
		if ctx.Err() != nil {
			return
		}
		var args []string
		var err error
		switch name {
		case openBaoLinkName:
			args, err = openBaoLinkArgs(cfg)
		case apiserverLinkName:
			args, err = apiserverLinkArgs(ctx, cfg)
		default:
			err = fmt.Errorf("nodeagent: unknown link %s", name)
		}
		if err != nil {
			log.Error("the link cannot start", "link", name, "err", err.Error())
			if clock.SleepErr(ctx, backoff.Next()) != nil {
				return
			}
			continue
		}
		cmd := exec.CommandContext(ctx, cfg.DumbpipePath, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = childproc.FilterSecrets(os.Stderr)
		started := time.Now()
		err = cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff.Reset()
		}
		delay := backoff.Next()
		log.Warn("the link child exited; restarting it", "link", name, "err", errString(err), "after", delay.String())
		if clock.SleepErr(ctx, delay) != nil {
			return
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
