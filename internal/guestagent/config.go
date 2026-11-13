package guestagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultConfigDir = "/etc/socialweb-guest-agent"

	DefaultStateDir = "/run/socialweb-guest-agent"

	DefaultLoginHelper = "/usr/local/bin/guest-login.sh"

	TunnelsFile = "tunnels.json"

	ServicesFile = "services.json"

	ShellService = "shell"

	SecretsFile = "secrets.json"

	AgentFile = "agent.json"

	AssociationFile = "association.json"

	MemoryFile = "memory.json"

	RequesterDIDFile = "requester-did"

	AssociationRequestFile = "association-request.json"

	AssociationRefFile = "association-ref"

	RequesterAtircEnvFile = "r2-atirc.env"

	RequesterAgentEnvFile = "r2-agent.env"

	RequesterGroup = "socialweb-r2"

	RequesterDir = "/run/socialweb-r2"

	RequesterStartEnvFile = "r2-start.env"

	RequesterUnit = "socialweb-r2.service"

	RequesterReleaseUnit = "socialweb-r2-release.service"

	DefaultSecretsFetchScript = "/usr/local/bin/setup-secrets.sh"

	SecretsReadyFile = "secrets.ready"

	SecretsRequestFile = "secrets.request"

	secretsTag = "secrets"

	secretsFallbackSealed = "sealed"

	BidderFile = "bidder.json"

	CursorFile = "cursor"

	STUNServer = "stun:stun.cloudflare.com:3478"

	maxTag = 32

	associationRefDir = "/etc/socialweb-computer"
)

var ErrConfig = errors.New("guestagent: invalid config")

type Tunnel struct {
	Tag string `json:"tag"`

	Port int `json:"port"`

	Unix string `json:"unix,omitempty"`
}

type Service struct {
	Unix string `json:"unix"`
}

type Bidder struct {
	Base string `json:"base"`

	Audience string `json:"audience"`

	AcceptPath string `json:"acceptPath"`

	WebRTC *WebRTCPort `json:"webrtc,omitempty"`
}

type WebRTCPort struct {
	UDPPort int `json:"udpPort"`

	HostAddresses []string `json:"hostAddresses"`
}

type Secrets struct {
	Fallback string `json:"fallback"`

	GetSecrets bool `json:"getSecrets"`
}

type Association struct {
	RefFile string `json:"refFile"`
}

type AcceptRef struct {
	URI string

	CID string
}

type Config struct {
	Tunnels []Tunnel

	Shell string

	Secrets *Secrets

	AgentUnit string

	Association *Association

	Memory bool

	Bidder Bidder

	Accept AcceptRef

	StateDir string

	LoginHelper string
}

func (c Config) Tunnel(tag string) (Tunnel, bool) {
	for _, t := range c.Tunnels {
		if t.Tag == tag {
			return t, true
		}
	}
	return Tunnel{}, false
}

func (t Tunnel) Endpoint() (network, address string) {
	if t.Unix != "" {
		return "unix", t.Unix
	}
	return "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(t.Port))
}

func LoadConfig(configDir, stateDir, loginHelper string) (Config, error) {
	cfg := Config{StateDir: stateDir, LoginHelper: loginHelper}
	var tunnels struct {
		Tunnels []Tunnel `json:"tunnels"`
	}
	if err := readOptionalJSON(filepath.Join(configDir, TunnelsFile), &tunnels); err != nil {
		return cfg, err
	}
	var services map[string]Service
	if err := readOptionalJSON(filepath.Join(configDir, ServicesFile), &services); err != nil {
		return cfg, err
	}
	for name, svc := range services {
		if name != ShellService {
			return cfg, fmt.Errorf("%w: %s names service %q; the one service is %s", ErrConfig, ServicesFile, name, ShellService)
		}
		if !cleanAbsolute(svc.Unix) || filepath.Dir(svc.Unix) != filepath.Clean(stateDir) {
			return cfg, fmt.Errorf("%w: %s %s unix %q is not a socket path directly in the state dir %s", ErrConfig, ServicesFile, name, svc.Unix, stateDir)
		}
		cfg.Shell = svc.Unix
	}
	var secrets Secrets
	switch err := readOptionalJSON(filepath.Join(configDir, SecretsFile), &secrets); {
	case err != nil:
		return cfg, err
	case secrets.Fallback == secretsFallbackSealed:
		cfg.Secrets = &secrets
	case secrets.Fallback != "":
		return cfg, fmt.Errorf("%w: %s fallback %q is not %s", ErrConfig, SecretsFile, secrets.Fallback, secretsFallbackSealed)
	}
	var agent AgentUnit
	if err := readOptionalJSON(filepath.Join(configDir, AgentFile), &agent); err != nil {
		return cfg, err
	}
	if agent.Unit != "" && !agentUnitRE.MatchString(agent.Unit) {
		return cfg, fmt.Errorf("%w: %s unit %q is not a systemd service name of [a-z0-9-]", ErrConfig, AgentFile, agent.Unit)
	}
	cfg.AgentUnit = agent.Unit
	var association Association
	switch err := readOptionalJSON(filepath.Join(configDir, AssociationFile), &association); {
	case err != nil:
		return cfg, err
	case association.RefFile != "":
		clean := filepath.Clean(association.RefFile)
		if !strings.HasPrefix(association.RefFile, "/") || clean != association.RefFile || !strings.HasPrefix(clean, associationRefDir+"/") {
			return cfg, fmt.Errorf("%w: %s refFile %q is not a clean path under %s/", ErrConfig, AssociationFile, association.RefFile, associationRefDir)
		}
		cfg.Association = &association
	}
	seen := map[string]bool{}
	for _, t := range tunnels.Tunnels {
		switch {
		case t.Tag == "" || len(t.Tag) > maxTag || !printable(t.Tag):
			return cfg, fmt.Errorf("%w: tunnel tag %q is not 1 to %d printable ASCII bytes", ErrConfig, t.Tag, maxTag)
		case t.Unix != "" && (t.Port != 0 || !cleanAbsolute(t.Unix)):
			return cfg, fmt.Errorf("%w: tunnel %s unix %q is not a clean absolute path, or it names a port too", ErrConfig, t.Tag, t.Unix)
		case t.Unix == "" && (t.Port < 1 || t.Port > 65535):
			return cfg, fmt.Errorf("%w: tunnel %s port %d is not a TCP port", ErrConfig, t.Tag, t.Port)
		case seen[t.Tag]:
			return cfg, fmt.Errorf("%w: tunnel tag %s appears twice", ErrConfig, t.Tag)
		case t.Tag == secretsTag:
			return cfg, fmt.Errorf("%w: tunnel tag %s names the secrets key", ErrConfig, t.Tag)
		}
		seen[t.Tag] = true
		cfg.Tunnels = append(cfg.Tunnels, t)
	}
	if _, err := os.Stat(filepath.Join(configDir, MemoryFile)); err == nil {
		cfg.Memory = true
	}
	if len(cfg.Tunnels) == 0 && cfg.Shell == "" && cfg.Secrets == nil && cfg.AgentUnit == "" && cfg.Association == nil && !cfg.Memory {
		return cfg, fmt.Errorf("%w: %s names no tunnel, %s no service, %s asks for no sealed secrets fallback, %s names no unit, %s names no ref file and there is no %s", ErrConfig,
			TunnelsFile, ServicesFile, SecretsFile, AgentFile, AssociationFile, MemoryFile)
	}
	if err := readJSON(filepath.Join(configDir, BidderFile), &cfg.Bidder); err != nil {
		return cfg, err
	}
	base, err := url.Parse(strings.TrimSpace(cfg.Bidder.Base))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return cfg, fmt.Errorf("%w: %s base %q is not an http or https URL", ErrConfig, BidderFile, cfg.Bidder.Base)
	}
	cfg.Bidder.Base = strings.TrimRight(base.String(), "/")
	cfg.Bidder.Audience = strings.TrimSpace(cfg.Bidder.Audience)
	if cfg.Bidder.Audience == "" {
		return cfg, fmt.Errorf("%w: %s has no audience", ErrConfig, BidderFile)
	}
	if strings.TrimSpace(cfg.Bidder.AcceptPath) == "" {
		return cfg, fmt.Errorf("%w: %s has no acceptPath", ErrConfig, BidderFile)
	}
	if w := cfg.Bidder.WebRTC; w != nil {
		if w.UDPPort < 1 || w.UDPPort > 65535 || len(w.HostAddresses) == 0 {
			return cfg, fmt.Errorf("%w: %s webrtc names port %d and %d host addresses", ErrConfig, BidderFile, w.UDPPort, len(w.HostAddresses))
		}
		for _, a := range w.HostAddresses {
			if _, err := netip.ParseAddr(strings.TrimSpace(a)); err != nil {
				return cfg, fmt.Errorf("%w: %s webrtc host address %q is not an IP address", ErrConfig, BidderFile, a)
			}
		}
	}
	var accept struct {
		Accept struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		} `json:"accept"`
	}
	if err := readJSON(cfg.Bidder.AcceptPath, &accept); err != nil {
		return cfg, err
	}
	cfg.Accept = AcceptRef{URI: strings.TrimSpace(accept.Accept.URI), CID: strings.TrimSpace(accept.Accept.CID)}
	if cfg.Accept.URI == "" || cfg.Accept.CID == "" {
		return cfg, fmt.Errorf("%w: %s has no accept ref", ErrConfig, cfg.Bidder.AcceptPath)
	}
	if strings.TrimSpace(cfg.StateDir) == "" || strings.TrimSpace(cfg.LoginHelper) == "" {
		return cfg, fmt.Errorf("%w: a state dir and a login helper are required", ErrConfig)
	}
	return cfg, nil
}

func readOptionalJSON(path string, into any) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return readJSON(path, into)
}

func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrConfig, path, err)
	}
	return nil
}

func cleanAbsolute(p string) bool {
	return strings.HasPrefix(p, "/") && filepath.Clean(p) == p
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}
