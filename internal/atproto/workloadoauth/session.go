package workloadoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
)

const maxReplyBytes = 1 << 20

type Session struct {
	HTTP *http.Client

	Key atcrypto.PrivateKeyExportable
}

type AuthorizeRequest struct {
	Endpoint string

	Origin string

	ClientID string

	RequestURI string
}

type StatusError struct {
	Status int

	Code string

	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("workloadoauth: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

func NewSession(client *http.Client) (*Session, error) {
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Session{HTTP: client, Key: key}, nil
}

func ParseAuthorizeURL(raw string) (AuthorizeRequest, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return AuthorizeRequest{}, fmt.Errorf("workloadoauth: %q is not an authorize URL", raw)
	}
	q := u.Query()
	out := AuthorizeRequest{
		Endpoint:   u.Scheme + "://" + u.Host + u.Path,
		Origin:     u.Scheme + "://" + u.Host,
		ClientID:   q.Get("client_id"),
		RequestURI: q.Get("request_uri"),
	}
	if out.ClientID == "" || out.RequestURI == "" {
		return AuthorizeRequest{}, fmt.Errorf("workloadoauth: the authorize URL carries no client_id and request_uri")
	}
	return out, nil
}

func (s *Session) Exchange(ctx context.Context, pdsURL, audience, subjectToken string) (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	in := map[string]string{
		"subject_token":        subjectToken,
		"subject_token_type":   oauth.TokenTypeJWT,
		"audience":             audience,
		"requested_token_type": oauth.TokenTypeAccessToken,
	}
	if err := s.post(ctx, strings.TrimRight(pdsURL, "/")+oauth.PathExchangeToken, "", in, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" || !strings.EqualFold(out.TokenType, oauth.AuthSchemeDPoP) {
		return "", errors.New("workloadoauth: the exchange answered no DPoP token")
	}
	return out.AccessToken, nil
}

func (s *Session) Approve(ctx context.Context, pdsURL, token, requestURI string) (string, error) {
	var out struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if err := s.post(ctx, strings.TrimRight(pdsURL, "/")+oauth.PathApproveAuthorization, token, map[string]string{"request_uri": requestURI}, &out); err != nil {
		return "", err
	}
	if out.RedirectURI == "" {
		return "", errors.New("workloadoauth: the approval answered no redirect")
	}
	return out.RedirectURI, nil
}

func (s *Session) post(ctx context.Context, target, token string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	nonce := ""
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := oauthclient.DPoPProof(s.Key, http.MethodPost, target, token, nonce)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(oauth.HeaderDPoP, proof)
		if token != "" {
			req.Header.Set("Authorization", oauth.AuthSchemeDPoP+" "+token)
		}
		res, err := s.HTTP.Do(req)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(res.Body, maxReplyBytes))
		res.Body.Close()
		if err != nil {
			return err
		}
		if res.StatusCode == http.StatusOK {
			return json.Unmarshal(raw, out)
		}
		var failure struct {
			Error       string `json:"error"`
			Message     string `json:"message"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &failure)
		if failure.Error == "use_dpop_nonce" && res.Header.Get(oauth.HeaderDPoPNonce) != "" && attempt == 0 {
			nonce = res.Header.Get(oauth.HeaderDPoPNonce)
			continue
		}
		message := failure.Message
		if message == "" {
			message = failure.Description
		}
		return &StatusError{Status: res.StatusCode, Code: failure.Error, Message: message}
	}
	return errors.New("workloadoauth: the PDS kept asking for a new DPoP nonce")
}

func NoRedirects(client *http.Client) *http.Client {
	cloned := *client
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cloned
}

func StartLogin(ctx context.Context, app *http.Client, loginURL, handle string) (AuthorizeRequest, error) {
	u, err := url.Parse(loginURL)
	if err != nil || u.Host == "" {
		return AuthorizeRequest{}, fmt.Errorf("workloadoauth: %q is not a login URL", loginURL)
	}
	q := u.Query()
	q.Set("handle", handle)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return AuthorizeRequest{}, err
	}
	res, err := NoRedirects(app).Do(req)
	if err != nil {
		return AuthorizeRequest{}, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxReplyBytes))
	location := res.Header.Get("Location")
	if res.StatusCode < 300 || res.StatusCode > 399 || location == "" {
		return AuthorizeRequest{}, fmt.Errorf("workloadoauth: the app's login answered HTTP %d with no redirect", res.StatusCode)
	}
	target, err := u.Parse(location)
	if err != nil {
		return AuthorizeRequest{}, err
	}
	return ParseAuthorizeURL(target.String())
}

func Callback(ctx context.Context, app *http.Client, redirect string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, redirect, nil)
	if err != nil {
		return 0, "", err
	}
	res, err := NoRedirects(app).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxReplyBytes))
	if res.StatusCode >= 400 {
		return res.StatusCode, "", fmt.Errorf("workloadoauth: the app's callback answered HTTP %d", res.StatusCode)
	}
	return res.StatusCode, res.Header.Get("Location"), nil
}

func CommandToken(ctx context.Context, command []string, audience string) (string, error) {
	if len(command) == 0 {
		return "", errors.New("workloadoauth: no token command")
	}
	args := append(append([]string(nil), command[1:]...), audience)
	cmd := exec.CommandContext(ctx, command[0], args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("workloadoauth: %s: %v: %s", command[0], err, strings.TrimSpace(stderr.String()))
	}
	token := strings.TrimSpace(string(out))
	if token == "" || strings.Count(token, ".") != 2 {
		return "", fmt.Errorf("workloadoauth: %s printed no JWT", command[0])
	}
	return token, nil
}

type Approval struct {
	Account string

	Role string

	PDS string

	Request AuthorizeRequest

	Redirect string
}

func (s *Session) ApproveLogin(ctx context.Context, app *http.Client, loginURL, handle, account, role string, token func(ctx context.Context, audience string) (string, error)) (Approval, error) {
	request, err := StartLogin(ctx, app, loginURL, handle)
	if err != nil {
		return Approval{}, err
	}
	audience := oauth.WorkloadAudience(account, role)
	subject, err := token(ctx, audience)
	if err != nil {
		return Approval{}, err
	}
	pdsToken, err := s.Exchange(ctx, request.Origin, audience, subject)
	if err != nil {
		return Approval{}, fmt.Errorf("workloadoauth: exchanging the workload token: %w", err)
	}
	redirect, err := s.Approve(ctx, request.Origin, pdsToken, request.RequestURI)
	if err != nil {
		return Approval{}, fmt.Errorf("workloadoauth: approving %s: %w", request.ClientID, err)
	}
	if _, _, err := Callback(ctx, app, redirect); err != nil {
		return Approval{}, err
	}
	return Approval{Account: account, Role: role, PDS: request.Origin, Request: request, Redirect: redirect}, nil
}
