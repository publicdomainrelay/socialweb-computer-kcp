package guestagent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	tokenRefreshMargin = 60 * time.Second

	tokenDefaultLifetime = 5 * time.Minute

	tokenHelperTimeout = 6 * time.Minute

	helperStderrTail = 512
)

type tokenSource struct {
	helper string

	audience string

	log *slog.Logger

	mu sync.Mutex

	token string

	expires time.Time
}

func newTokenSource(helper, audience string, log *slog.Logger) *tokenSource {
	return &tokenSource{helper: helper, audience: audience, log: log}
}

func (t *tokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Until(t.expires) > tokenRefreshMargin {
		return t.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, tokenHelperTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, t.helper, "token", t.audience)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("guestagent: %s token: %w: %s", t.helper, err, tail(stderr.String(), helperStderrTail))
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" || strings.ContainsAny(token, " \r\n\t") {
		return "", fmt.Errorf("guestagent: %s token printed no single token (%d bytes)", t.helper, len(token))
	}
	t.token, t.expires = token, tokenExpiry(token, time.Now())
	t.log.Info("took a guest token", "audience", t.audience, "bytes", len(token), "expires", t.expires.UTC().Format(time.RFC3339))
	return token, nil
}

func (t *tokenSource) Invalidate(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == token {
		t.token = ""
	}
}

func tokenExpiry(token string, now time.Time) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return now.Add(tokenDefaultLifetime)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return now.Add(tokenDefaultLifetime)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= now.Unix() {
		return now.Add(tokenDefaultLifetime)
	}
	return time.Unix(claims.Exp, 0)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}
