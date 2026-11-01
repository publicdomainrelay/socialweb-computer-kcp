package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/workloadoauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
)

const (
	oauthPasswordEnv = "ATPROTO_PASSWORD"

	defaultOAuthTokenCommand = "/usr/local/bin/guest-login.sh token"
)

var oauthRecordOptions = []string{"oauth-pds", "oauth-account", "oauth-role", "json"}

var oauthAllowOptions = append(append([]string{}, oauthRecordOptions...),
	"oauth-issuer", "oauth-subject", "oauth-client", "oauth-scope", "oauth-redirect-uri", "oauth-service", "oauth-print")

var oauthApproveOptions = []string{"oauth-account", "oauth-role", "oauth-login", "oauth-login-handle", "oauth-token-command",
	"oauth-token-file", "oauth-jar", "json"}

func runOAuthAllow(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	role, err := oauthRole(ctx, vals)
	if err != nil {
		return a.fail(exitRefused, "oauth allow: %v", err)
	}
	record, err := workloadoauth.Record(role, time.Now())
	if err != nil {
		return a.fail(exitRefused, "oauth allow: %v", err)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return a.fail(exitFailed, "oauth allow: %v", err)
	}
	if vals.Bool("oauth-print") {
		fmt.Fprintln(a.stdout, string(raw))
		return exitOK
	}
	client, err := oauthSignedIn(ctx, vals)
	if err != nil {
		return a.fail(exitFailed, "oauth allow: %v", err)
	}
	path, err := pdsclient.ParseRepoPath(role.Account, workloadauth.RBACCollection, role.Name)
	if err != nil {
		return a.fail(exitRefused, "oauth allow: %v", err)
	}
	result, err := client.PutRecord(ctx, path, raw, pdsclient.WriteOptions{})
	if err != nil {
		return a.fail(exitFailed, "oauth allow: writing %s/%s/%s: %v", role.Account, workloadauth.RBACCollection, role.Name, err)
	}
	return a.oauthReport(vals, map[string]any{
		"uri": result.URI, "cid": result.CID.String(), "audience": role.Audience(), "service": role.Service,
	})
}

func runOAuthRevoke(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	account, roleName := strings.TrimSpace(vals.String("oauth-account")), strings.TrimSpace(vals.String("oauth-role"))
	path, err := pdsclient.ParseRepoPath(account, workloadauth.RBACCollection, roleName)
	if err != nil {
		return a.fail(exitRefused, "oauth revoke: --oauth-account <did> and --oauth-role <rkey>: %v", err)
	}
	client, err := oauthSignedIn(ctx, vals)
	if err != nil {
		return a.fail(exitFailed, "oauth revoke: %v", err)
	}
	if err := client.DeleteRecord(ctx, path, pdsclient.WriteOptions{}); err != nil {
		return a.fail(exitFailed, "oauth revoke: %v", err)
	}
	return a.oauthReport(vals, map[string]any{"deleted": "at://" + account + "/" + workloadauth.RBACCollection + "/" + roleName})
}

func runOAuthApprove(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	account, roleName := strings.TrimSpace(vals.String("oauth-account")), strings.TrimSpace(vals.String("oauth-role"))
	if _, err := syntax.ParseDID(account); err != nil {
		return a.fail(exitRefused, "oauth approve: --oauth-account must be the account DID: %v", err)
	}
	if _, err := syntax.ParseRecordKey(roleName); err != nil {
		return a.fail(exitRefused, "oauth approve: --oauth-role must be the record key: %v", err)
	}
	login := strings.TrimSpace(vals.String("oauth-login"))
	if login == "" {
		return a.fail(exitRefused, "oauth approve: --oauth-login <the app's login URL> is required")
	}
	handle := strings.TrimSpace(vals.String("oauth-login-handle"))
	if handle == "" {
		handle = account
	}
	token, err := oauthTokenSource(vals)
	if err != nil {
		return a.fail(exitRefused, "oauth approve: %v", err)
	}
	jarPath := strings.TrimSpace(vals.String("oauth-jar"))
	jar, err := cookiejar.New(nil)
	if err != nil {
		return a.fail(exitFailed, "oauth approve: %v", err)
	}
	app := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	session, err := workloadoauth.NewSession(&http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return a.fail(exitFailed, "oauth approve: %v", err)
	}
	approval, err := session.ApproveLogin(ctx, app, login, handle, account, roleName, token)
	if err != nil {
		return a.fail(exitFailed, "oauth approve: %v", err)
	}
	if jarPath != "" {
		if err := writeCookieJar(jarPath, jar, approval.Redirect); err != nil {
			return a.fail(exitFailed, "oauth approve: %v", err)
		}
	}
	return a.oauthReport(vals, map[string]any{
		"account": approval.Account, "role": approval.Role, "pds": approval.PDS, "client": approval.Request.ClientID,
		"jar": jarPath,
	})
}

func (a *app) oauthReport(vals clicfg.Values, out map[string]any) int {
	if vals.Bool("json") {
		raw, _ := json.Marshal(out)
		fmt.Fprintln(a.stdout, string(raw))
		return exitOK
	}
	keys := []string{"uri", "cid", "audience", "service", "deleted", "account", "role", "pds", "client", "jar"}
	for _, k := range keys {
		if v, ok := out[k]; ok && fmt.Sprint(v) != "" {
			fmt.Fprintf(a.stdout, "%s: %v\n", k, v)
		}
	}
	return exitOK
}

func oauthRole(ctx context.Context, vals clicfg.Values) (workloadoauth.Role, error) {
	role := workloadoauth.Role{
		Name:    strings.TrimSpace(vals.String("oauth-role")),
		Account: strings.TrimSpace(vals.String("oauth-account")),
		Service: strings.TrimSpace(vals.String("oauth-service")),
		Issuer:  strings.TrimRight(strings.TrimSpace(vals.String("oauth-issuer")), "/"),
		Subject: strings.TrimSpace(vals.String("oauth-subject")),
	}
	if role.Service == "" {
		pds := strings.TrimSpace(vals.String("oauth-pds"))
		if pds == "" {
			return role, errors.New("--oauth-pds <the account's PDS URL> or --oauth-service <its service DID> is required")
		}
		did, err := pdsServiceDID(ctx, pds)
		if err != nil {
			return role, err
		}
		role.Service = did
	}
	redirect := strings.TrimSpace(vals.String("oauth-redirect-uri"))
	scopes := oauthList(vals.String("oauth-scope"))
	for _, id := range oauthList(vals.String("oauth-client")) {
		client := workloadoauth.Client{ID: id, RedirectURI: redirect, Scopes: scopes}
		if len(client.Scopes) == 0 {
			declared, err := clientMetadataScopes(ctx, id)
			if err != nil {
				return role, err
			}
			client.Scopes = declared
		}
		role.Clients = append(role.Clients, client)
	}
	return role, role.Check()
}

func oauthList(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func pdsServiceDID(ctx context.Context, pds string) (string, error) {
	var out struct {
		DID string `json:"did"`
	}
	if err := getJSONDocument(ctx, strings.TrimRight(pds, "/")+"/xrpc/com.atproto.server.describeServer", &out); err != nil {
		return "", fmt.Errorf("reading the PDS service DID: %w", err)
	}
	if out.DID == "" {
		return "", errors.New("the PDS describeServer names no did")
	}
	return out.DID, nil
}

func clientMetadataScopes(ctx context.Context, clientID string) ([]string, error) {
	var out struct {
		Scope string `json:"scope"`
	}
	if err := getJSONDocument(ctx, clientID, &out); err != nil {
		return nil, fmt.Errorf("reading the client metadata %s: %w", clientID, err)
	}
	var scopes []string
	for _, scope := range strings.Fields(out.Scope) {
		if strings.HasPrefix(scope, "transition:") || strings.HasPrefix(scope, "repo:"+workloadauth.RBACCollection) {
			continue
		}
		scopes = append(scopes, scope)
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("the client metadata %s declares no scope a workload may approve", clientID)
	}
	return scopes, nil
}

func getJSONDocument(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered HTTP %d", target, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func oauthSignedIn(ctx context.Context, vals clicfg.Values) (*pdsclient.RemotePDS, error) {
	pds := strings.TrimSpace(vals.String("oauth-pds"))
	if pds == "" {
		return nil, errors.New("--oauth-pds <the account's PDS URL> is required to write the record")
	}
	password := os.Getenv(oauthPasswordEnv)
	if password == "" {
		return nil, fmt.Errorf("%s must hold the account's password or app password (or use --oauth-print and write the record yourself)", oauthPasswordEnv)
	}
	client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: pds})
	if err != nil {
		return nil, err
	}
	if _, err := client.CreateSession(ctx, strings.TrimSpace(vals.String("oauth-account")), password); err != nil {
		return nil, fmt.Errorf("signing in to %s: %w", pds, err)
	}
	return client, nil
}

func oauthTokenSource(vals clicfg.Values) (func(context.Context, string) (string, error), error) {
	if path := strings.TrimSpace(vals.String("oauth-token-file")); path != "" {
		return func(context.Context, string) (string, error) {
			raw, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(raw)), nil
		}, nil
	}
	command := strings.Fields(vals.String("oauth-token-command"))
	if len(command) == 0 {
		command = strings.Fields(defaultOAuthTokenCommand)
	}
	return func(ctx context.Context, audience string) (string, error) {
		return workloadoauth.CommandToken(ctx, command, audience)
	}, nil
}

type savedCookie struct {
	URL   string `json:"url"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

func writeCookieJar(path string, jar http.CookieJar, origin string) error {
	u, err := url.Parse(origin)
	if err != nil {
		return err
	}
	base := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}
	var out []savedCookie
	for _, c := range jar.Cookies(base) {
		out = append(out, savedCookie{URL: base.String(), Name: c.Name, Value: c.Value})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
