package guestagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultConfigDir = "/etc/socialweb-guest-agent"

	DefaultStateDir = "/run/socialweb-guest-agent"

	DefaultLoginHelper = "/usr/local/bin/guest-login.sh"

	TunnelsFile = "tunnels.json"

	BidderFile = "bidder.json"

	CursorFile = "cursor"

	STUNServer = "stun:stun.cloudflare.com:3478"

	maxTag = 32
)

var ErrConfig = errors.New("guestagent: invalid config")

type Tunnel struct {
	Tag string `json:"tag"`

	Port int `json:"port"`
}

type Bidder struct {
	Base string `json:"base"`

	Audience string `json:"audience"`

	AcceptPath string `json:"acceptPath"`
}

type AcceptRef struct {
	URI string

	CID string
}

type Config struct {
	Tunnels []Tunnel

	Bidder Bidder

	Accept AcceptRef

	StateDir string

	LoginHelper string
}

func (c Config) Port(tag string) (int, bool) {
	for _, t := range c.Tunnels {
		if t.Tag == tag {
			return t.Port, true
		}
	}
	return 0, false
}

func LoadConfig(configDir, stateDir, loginHelper string) (Config, error) {
	cfg := Config{StateDir: stateDir, LoginHelper: loginHelper}
	var tunnels struct {
		Tunnels []Tunnel `json:"tunnels"`
	}
	if err := readJSON(filepath.Join(configDir, TunnelsFile), &tunnels); err != nil {
		return cfg, err
	}
	seen := map[string]bool{}
	for _, t := range tunnels.Tunnels {
		switch {
		case t.Tag == "" || len(t.Tag) > maxTag || !printable(t.Tag):
			return cfg, fmt.Errorf("%w: tunnel tag %q is not 1 to %d printable ASCII bytes", ErrConfig, t.Tag, maxTag)
		case t.Port < 1 || t.Port > 65535:
			return cfg, fmt.Errorf("%w: tunnel %s port %d is not a TCP port", ErrConfig, t.Tag, t.Port)
		case seen[t.Tag]:
			return cfg, fmt.Errorf("%w: tunnel tag %s appears twice", ErrConfig, t.Tag)
		}
		seen[t.Tag] = true
		cfg.Tunnels = append(cfg.Tunnels, t)
	}
	if len(cfg.Tunnels) == 0 {
		return cfg, fmt.Errorf("%w: %s names no tunnel", ErrConfig, TunnelsFile)
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

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}
