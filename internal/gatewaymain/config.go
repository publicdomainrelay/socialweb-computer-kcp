package gatewaymain

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
)

const (
	envAddr            = "SOCIALWEB_GATEWAY_ADDR"
	envAudiences       = "SOCIALWEB_GATEWAY_AUDIENCES"
	envKCPHost         = "SOCIALWEB_KCP_HOST"
	envKCPKubeconfig   = "SOCIALWEB_KCP_KUBECONFIG"
	envDirectoryTTL    = "SOCIALWEB_GATEWAY_DIRECTORY_CACHE_TTL"
	envShutdownTimeout = "SOCIALWEB_GATEWAY_SHUTDOWN_TIMEOUT"
	envLogLevel        = "SOCIALWEB_GATEWAY_LOG_LEVEL"

	envPLCURL     = "SOCIALWEB_GATEWAY_PLC_URL"
	envPLCTimeout = "SOCIALWEB_GATEWAY_PLC_TIMEOUT"

	envKCPPublicHost   = "SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST"
	envKCPPublicCAFile = "SOCIALWEB_GATEWAY_KCP_PUBLIC_CA_FILE"
	envClusterStateDir = "SOCIALWEB_GATEWAY_CLUSTER_STATE_DIR"

	envNodeClaimAgent = "SOCIALWEB_GATEWAY_NODECLAIM_AGENT"
	envNodeClaimQuota = "SOCIALWEB_GATEWAY_NODECLAIM_QUOTA"

	envHostProcessWorkspace = "SOCIALWEB_CLUSTERPROVIDER_HOSTPROCESS_WORKSPACE"
)

const DefaultDirectoryCacheTTL = 60 * time.Second

const DefaultPLCTimeout = 10 * time.Second

const DefaultShutdownTimeout = 30 * time.Second

type Config struct {
	Addr string

	Audiences []string

	KCPHost string

	KCPKubeconfig string

	DirectoryCacheTTL time.Duration

	PLCURL string

	PLCTimeout time.Duration

	ShutdownTimeout time.Duration

	KCPPublicHost string

	KCPPublicCAFile string

	ClusterStateDir string

	NodeClaimAgent *kcpclient.ServiceAccountRef

	NodeClaimQuota int64

	HostProcessWorkspace string

	LogLevel slog.Level
}

func load(get func(string) string) (Config, error) {
	cfg := Config{
		Addr:          strings.TrimSpace(get(envAddr)),
		KCPHost:       strings.TrimSpace(get(envKCPHost)),
		KCPKubeconfig: strings.TrimSpace(get(envKCPKubeconfig)),
		PLCURL:        strings.TrimSpace(get(envPLCURL)),

		KCPPublicCAFile: strings.TrimSpace(get(envKCPPublicCAFile)),
		ClusterStateDir: strings.TrimSpace(get(envClusterStateDir)),

		DirectoryCacheTTL: DefaultDirectoryCacheTTL,
		PLCTimeout:        DefaultPLCTimeout,
		ShutdownTimeout:   DefaultShutdownTimeout,
		LogLevel:          slog.LevelInfo,
	}

	for _, req := range []struct {
		name  string
		value string
	}{
		{envAddr, cfg.Addr},
		{envKCPHost, cfg.KCPHost},
		{envKCPKubeconfig, cfg.KCPKubeconfig},
		{envPLCURL, cfg.PLCURL},
		{envClusterStateDir, cfg.ClusterStateDir},
	} {
		if req.value == "" {
			return Config{}, fmt.Errorf("%s is not set", req.name)
		}
	}

	if err := requireLoopbackAddr(cfg.Addr); err != nil {
		return Config{}, err
	}

	cfg.Audiences = splitList(get(envAudiences))
	if len(cfg.Audiences) == 0 {
		return Config{}, fmt.Errorf("%s is not set: the endpoint would refuse every token as Unauthorized", envAudiences)
	}

	var err error
	if cfg.KCPPublicHost, err = optionalHTTPSURL(get, envKCPPublicHost); err != nil {
		return Config{}, err
	}
	if cfg.NodeClaimAgent, err = kcpclient.ParseServiceAccountRef(get(envNodeClaimAgent)); err != nil {
		return Config{}, fmt.Errorf("%s: %w", envNodeClaimAgent, err)
	}
	cfg.HostProcessWorkspace = strings.TrimSpace(get(envHostProcessWorkspace))
	if v := strings.TrimSpace(get(envNodeClaimQuota)); v != "" {
		if cfg.NodeClaimQuota, err = strconv.ParseInt(v, 10, 64); err != nil {
			return Config{}, fmt.Errorf("%s: %w", envNodeClaimQuota, err)
		}
	}
	if cfg.DirectoryCacheTTL, err = duration(get, envDirectoryTTL, DefaultDirectoryCacheTTL); err != nil {
		return Config{}, err
	}
	if cfg.PLCTimeout, err = duration(get, envPLCTimeout, DefaultPLCTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = duration(get, envShutdownTimeout, DefaultShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = level(get(envLogLevel)); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ponytail: loopback-only trusts a local proxy to terminate TLS; serve ListenAndServeTLS with
// cert and key paths when this process must terminate TLS itself
func requireLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s is %q: it is not a host:port address, and this listener must bind "+
			"127.0.0.1 or [::1]: %w", envAddr, addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s is %q: the host %q is not a loopback address. This listener carries "+
			"bearer service-auth tokens in and device credentials with cluster access out, so TLS "+
			"must be terminated by a local proxy: bind 127.0.0.1 or [::1] only, never an "+
			"all-interfaces address", envAddr, addr, host)
	}
	return nil
}

func splitList(raw string) []string {
	out := []string{}
	for part := range strings.SplitSeq(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func duration(get func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(get(name))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration", name, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: %q must be positive", name, raw)
	}
	return d, nil
}

func optionalHTTPSURL(get func(string) string, name string) (string, error) {
	raw := strings.TrimSpace(get(name))
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %q is not a URL: %w", name, raw, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("%s: %q must be an absolute https URL. This value is rendered "+
			"into a kubeconfig a user holds, and client-go silently drops the user identity "+
			"block for a server that is not https, so an http one would hand out a document "+
			"that authenticates nothing", name, raw)
	}
	return strings.TrimSuffix(raw, "/"), nil
}

func level(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%s: %q is not one of debug, info, warn, error", envLogLevel, raw)
	}
}
