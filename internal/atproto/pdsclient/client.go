package pdsclient

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	defaultCallTimeout   = 30 * time.Second
	maxJSONResponseBytes = 64 << 20
	maxErrorBodyBytes    = 64 << 10
	maxListRecordsLimit  = 100
)

type authStore struct {
	mu      sync.RWMutex
	session Session
	present bool
}

func (a *authStore) set(session Session) {
	a.mu.Lock()
	a.session = session
	a.present = true
	a.mu.Unlock()
}

func (a *authStore) access() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.present {
		return ""
	}
	return a.session.AccessJWT
}

func (a *authStore) refresh() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.present {
		return ""
	}
	return a.session.RefreshJWT
}

func (a *authStore) snapshot() Session {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.present {
		return Session{}
	}
	return a.session
}

func (a *authStore) clear() {
	a.mu.Lock()
	a.session = Session{}
	a.present = false
	a.mu.Unlock()
}

type xrpcClient struct {
	base        *url.URL
	httpClient  *http.Client
	timeout     time.Duration
	tokens      TokenSource
	dpop        DPoPSource
	auth        *authStore
	expectedDID syntax.DID
}

func newXRPCClient(base *url.URL, httpClient *http.Client, tokens TokenSource, dpop DPoPSource, timeout time.Duration) *xrpcClient {
	timeout = opt.Positive(timeout, defaultCallTimeout)
	if dpop == nil {
		if combined, ok := tokens.(DPoPSource); ok {
			dpop = combined
		}
	}
	return &xrpcClient{
		base:       base,
		httpClient: httpClient,
		timeout:    timeout,
		tokens:     tokens,
		dpop:       dpop,
		auth:       &authStore{},
	}
}

func (c *xrpcClient) accessToken(ctx context.Context) (string, error) {
	if c.tokens != nil {
		token, err := c.tokens.AccessToken(ctx)
		if err != nil {
			return "", fmt.Errorf("pdsclient: access token: %w", err)
		}
		return strings.TrimSpace(token), nil
	}
	if token := c.auth.access(); token != "" {
		return token, nil
	}
	return "", nil
}

func (c *xrpcClient) refreshAccessToken(ctx context.Context) (string, error) {
	source, ok := c.tokens.(TokenRefresher)
	if !ok {
		return "", nil
	}
	token, err := source.RefreshAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("pdsclient: refreshing the access token: %w", err)
	}
	return strings.TrimSpace(token), nil
}

func (c *xrpcClient) deleteToken(ctx context.Context) (string, error) {
	if token := c.auth.refresh(); token != "" {
		return token, nil
	}
	if token := c.auth.access(); token != "" {
		return token, nil
	}
	if c.tokens == nil {
		return "", nil
	}
	token, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("pdsclient: access token: %w", err)
	}
	return strings.TrimSpace(token), nil
}

func (c *xrpcClient) requireToken(ctx context.Context) (string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("%w: no access token is available", ErrAuthenticationRequired)
	}
	return token, nil
}

func (c *xrpcClient) endpoint(nsid string, query url.Values) *url.URL {
	out := *c.base
	out.Path = strings.TrimRight(c.base.Path, "/") + "/xrpc/" + nsid
	out.RawPath = ""
	out.RawQuery = query.Encode()
	out.Fragment = ""
	return &out
}

func (c *xrpcClient) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

func (c *xrpcClient) do(ctx context.Context, method, nsid string, query url.Values, body []byte, token string) (*http.Response, error) {
	if c == nil || c.base == nil {
		return nil, fmt.Errorf("%w: client has no base URL", ErrInvalidConfig)
	}
	if c.httpClient == nil {
		return nil, fmt.Errorf("%w: client has no HTTP client", ErrInvalidConfig)
	}
	if strings.TrimSpace(nsid) == "" {
		return nil, fmt.Errorf("%w: missing xrpc method", ErrInvalidRequest)
	}
	endpoint := c.endpoint(nsid, query)
	var response *http.Response
	refreshed := false
	for range 3 {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
		if err != nil {
			return nil, fmt.Errorf("%w: building request: %v", ErrInvalidRequest, err)
		}
		request.Header.Set("Accept", "application/json")
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			scheme := "Bearer"
			if c.dpop != nil {
				scheme = "DPoP"
			}
			request.Header.Set("Authorization", scheme+" "+token)
			if c.dpop != nil {
				dpopURL := *endpoint
				dpopURL.RawQuery = ""
				dpopURL.Fragment = ""
				proof, err := c.dpop.DPoPProof(ctx, method, dpopURL.String(), token)
				if err != nil {
					return nil, fmt.Errorf("pdsclient: dpop proof: %w", err)
				}
				if proof != "" {
					request.Header.Set("DPoP", proof)
				}
			}
		}
		response, err = c.httpClient.Do(request)
		if err != nil {
			return nil, fmt.Errorf("pdsclient: request %s: %w", nsid, err)
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusBadRequest {
			header := response.Header.Get("WWW-Authenticate")
			nonce := response.Header.Get("DPoP-Nonce")
			if nonce == "" {
				nonce = response.Header.Get("Dpop-Nonce")
			}
			if nonce != "" && !strings.Contains(header, "invalid_token") {
				if setter, ok := c.dpop.(DPoPNonceSetter); ok {
					_ = setter.SetDPoPNonce(ctx, endpoint.Host, nonce)
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBodyBytes))
					_ = response.Body.Close()
					continue
				}
			}
		}
		if !refreshed && token != "" && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			refreshed = true
			fresh, refreshErr := c.refreshAccessToken(ctx)
			if refreshErr == nil && fresh != "" && fresh != token {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBodyBytes))
				_ = response.Body.Close()
				token = fresh
				continue
			}
		}
		if err := checkXRPCResponse(response); err != nil {
			return nil, err
		}
		return response, nil
	}
	return nil, fmt.Errorf("%w: %s kept rejecting the access token", ErrAuthenticationRequired, nsid)
}

func checkXRPCResponse(response *http.Response) error {
	if response == nil {
		return fmt.Errorf("%w: nil response", ErrUnexpectedResponse)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	defer response.Body.Close()
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, maxErrorBodyBytes)).Decode(&body)
	code := strings.TrimSpace(body.Error)
	message := strings.TrimSpace(body.Message)
	code = cmp.Or(code, "HTTPError")
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return &XRPCError{StatusCode: response.StatusCode, Code: code, Message: message}
}

func decodeJSONBody(reader io.Reader, out any) error {
	if err := json.NewDecoder(io.LimitReader(reader, maxJSONResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("%w: decoding response: %v", ErrUnexpectedResponse, err)
	}
	return nil
}

func (c *xrpcClient) getJSON(ctx context.Context, nsid string, query url.Values, token string, out any) error {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	response, err := c.do(callCtx, http.MethodGet, nsid, query, nil, token)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return decodeJSONBody(response.Body, out)
}

func (c *xrpcClient) postJSON(ctx context.Context, nsid string, in any, token string, out any) error {
	var payload []byte
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%w: encoding request: %v", ErrInvalidRequest, err)
		}
		payload = encoded
	}
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	response, err := c.do(callCtx, http.MethodPost, nsid, nil, payload, token)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if out == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxJSONResponseBytes))
		return err
	}
	return decodeJSONBody(response.Body, out)
}

func (c *xrpcClient) getStream(ctx context.Context, nsid string, query url.Values) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, nsid, query, nil, "")
}

func parseBaseURL(raw string, schemes ...string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: base URL is required", ErrInvalidConfig)
	}
	if !strings.Contains(raw, "://") {
		return nil, fmt.Errorf("%w: base URL needs a scheme", ErrInvalidConfig)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: parsing base URL: %v", ErrInvalidConfig, err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: base URL has no host", ErrInvalidConfig)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: base URL must not carry user info", ErrInvalidConfig)
	}
	allowed := false
	for _, scheme := range schemes {
		if strings.EqualFold(parsed.Scheme, scheme) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("%w: unsupported base URL scheme", ErrInvalidConfig)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

func cloneNoRedirect(client *http.Client) *http.Client {
	if client == nil {
		return nil
	}
	out := *client
	out.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &out
}

type basePDS struct {
	client *xrpcClient
	dial   func(ctx context.Context, cursor *int64) (RepoEventStream, error)
}

func (b *basePDS) Describe(ctx context.Context) (Info, error) {
	var out struct {
		DID                  string   `json:"did"`
		AvailableUserDomains []string `json:"availableUserDomains"`
		Version              string   `json:"version"`
	}
	if err := b.client.getJSON(ctx, "com.atproto.server.describeServer", nil, "", &out); err != nil {
		return Info{}, err
	}
	did, err := syntax.ParseDID(strings.TrimSpace(out.DID))
	if err != nil {
		return Info{}, fmt.Errorf("%w: describeServer returned an invalid did", ErrUnexpectedResponse)
	}
	if b.client.expectedDID != "" && did != b.client.expectedDID {
		return Info{}, fmt.Errorf("%w: expected %s, got %s", ErrIdentityMismatch, b.client.expectedDID, did)
	}
	return Info{
		DID:                  did,
		AvailableUserDomains: append([]string(nil), out.AvailableUserDomains...),
		Version:              out.Version,
	}, nil
}

func validatePath(path RepoPath, requireRkey bool) error {
	if strings.TrimSpace(path.Repo.String()) == "" {
		return fmt.Errorf("%w: repo is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(path.Collection.String()) == "" {
		return fmt.Errorf("%w: collection is required", ErrInvalidRequest)
	}
	if requireRkey && strings.TrimSpace(path.Rkey.String()) == "" {
		return fmt.Errorf("%w: rkey is required", ErrInvalidRequest)
	}
	return nil
}

func validateRecord(record []byte) error {
	if len(bytes.TrimSpace(record)) == 0 {
		return fmt.Errorf("%w: record is required", ErrInvalidRequest)
	}
	parsed, err := atdata.UnmarshalJSON(record)
	if err != nil {
		return fmt.Errorf("%w: invalid record: %v", ErrInvalidRequest, err)
	}
	if parsed == nil {
		return fmt.Errorf("%w: record must be a JSON object", ErrInvalidRequest)
	}
	return nil
}

func parseCID(raw string) (cid.Cid, error) {
	parsed, err := cid.Parse(raw)
	if err != nil || !parsed.Defined() {
		return cid.Undef, fmt.Errorf("%w: invalid cid", ErrUnexpectedResponse)
	}
	return parsed, nil
}

func parseRecordView(view recordView) (Record, error) {
	recordCID, err := parseCID(view.CID)
	if err != nil {
		return Record{}, err
	}
	recordURI, err := syntax.ParseATURI(view.URI)
	if err != nil {
		return Record{}, fmt.Errorf("%w: invalid record uri", ErrUnexpectedResponse)
	}
	value := bytes.TrimSpace(view.Value)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return Record{}, fmt.Errorf("%w: record has no value", ErrUnexpectedResponse)
	}
	return Record{URI: recordURI, CID: recordCID, Value: append(json.RawMessage(nil), value...)}, nil
}

type recordView struct {
	URI   string          `json:"uri"`
	CID   string          `json:"cid"`
	Value json.RawMessage `json:"value"`
}

func (b *basePDS) GetRecord(ctx context.Context, path RepoPath, expected *cid.Cid) (Record, error) {
	if err := validatePath(path, true); err != nil {
		return Record{}, err
	}
	query := url.Values{}
	query.Set("repo", path.Repo.String())
	query.Set("collection", path.Collection.String())
	query.Set("rkey", path.Rkey.String())
	if expected != nil {
		if !expected.Defined() {
			return Record{}, fmt.Errorf("%w: undefined cid", ErrInvalidRequest)
		}
		query.Set("cid", expected.String())
	}
	var out recordView
	if err := b.client.getJSON(ctx, "com.atproto.repo.getRecord", query, "", &out); err != nil {
		return Record{}, err
	}
	return parseRecordView(out)
}

type writeRequest struct {
	Repo       string          `json:"repo"`
	Collection string          `json:"collection"`
	Rkey       string          `json:"rkey,omitempty"`
	Record     json.RawMessage `json:"record"`
	SwapCommit *string         `json:"swapCommit,omitempty"`
	SwapRecord *string         `json:"swapRecord,omitempty"`
	Validate   *bool           `json:"validate,omitempty"`
}

type deleteRequest struct {
	Repo       string  `json:"repo"`
	Collection string  `json:"collection"`
	Rkey       string  `json:"rkey"`
	SwapCommit *string `json:"swapCommit,omitempty"`
	SwapRecord *string `json:"swapRecord,omitempty"`
}

type commitView struct {
	CID string `json:"cid"`
	Rev string `json:"rev"`
}

type writeResponse struct {
	URI    string      `json:"uri"`
	CID    string      `json:"cid"`
	Commit *commitView `json:"commit,omitempty"`
}

func swapValue(value *cid.Cid) (*string, error) {
	if value == nil {
		return nil, nil
	}
	if !value.Defined() {
		return nil, fmt.Errorf("%w: undefined swap cid", ErrInvalidRequest)
	}
	raw := value.String()
	return &raw, nil
}

func parseCommitView(view *commitView) (*CommitRef, error) {
	if view == nil {
		return nil, nil
	}
	commitCID, err := parseCID(view.CID)
	if err != nil {
		return nil, err
	}
	rev, err := syntax.ParseTID(view.Rev)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid commit rev", ErrUnexpectedResponse)
	}
	return &CommitRef{CID: commitCID, Rev: rev}, nil
}

func parseWriteResponse(out writeResponse) (WriteResult, error) {
	recordURI, err := syntax.ParseATURI(out.URI)
	if err != nil {
		return WriteResult{}, fmt.Errorf("%w: invalid record uri", ErrUnexpectedResponse)
	}
	recordCID, err := parseCID(out.CID)
	if err != nil {
		return WriteResult{}, err
	}
	commit, err := parseCommitView(out.Commit)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{URI: recordURI, CID: recordCID, Commit: commit}, nil
}

func (b *basePDS) writeRecord(ctx context.Context, nsid string, path RepoPath, record []byte, opts WriteOptions, requireRkey bool) (WriteResult, error) {
	if err := validatePath(path, requireRkey); err != nil {
		return WriteResult{}, err
	}
	if err := validateRecord(record); err != nil {
		return WriteResult{}, err
	}
	swapCommit, err := swapValue(opts.SwapCommit)
	if err != nil {
		return WriteResult{}, err
	}
	swapRecord, err := swapValue(opts.SwapRecord)
	if err != nil {
		return WriteResult{}, err
	}
	token, err := b.client.requireToken(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	request := writeRequest{
		Repo:       path.Repo.String(),
		Collection: path.Collection.String(),
		Rkey:       path.Rkey.String(),
		Record:     append(json.RawMessage(nil), record...),
		SwapCommit: swapCommit,
		SwapRecord: swapRecord,
		Validate:   opts.Validate,
	}
	var out writeResponse
	if err := b.client.postJSON(ctx, nsid, request, token, &out); err != nil {
		return WriteResult{}, err
	}
	return parseWriteResponse(out)
}

func (b *basePDS) CreateRecord(ctx context.Context, path RepoPath, record []byte, opts WriteOptions) (WriteResult, error) {
	return b.writeRecord(ctx, "com.atproto.repo.createRecord", path, record, opts, false)
}

func (b *basePDS) PutRecord(ctx context.Context, path RepoPath, record []byte, opts WriteOptions) (WriteResult, error) {
	return b.writeRecord(ctx, "com.atproto.repo.putRecord", path, record, opts, true)
}

func (b *basePDS) DeleteRecord(ctx context.Context, path RepoPath, opts WriteOptions) error {
	if err := validatePath(path, true); err != nil {
		return err
	}
	swapCommit, err := swapValue(opts.SwapCommit)
	if err != nil {
		return err
	}
	swapRecord, err := swapValue(opts.SwapRecord)
	if err != nil {
		return err
	}
	token, err := b.client.requireToken(ctx)
	if err != nil {
		return err
	}
	request := deleteRequest{
		Repo:       path.Repo.String(),
		Collection: path.Collection.String(),
		Rkey:       path.Rkey.String(),
		SwapCommit: swapCommit,
		SwapRecord: swapRecord,
	}
	return b.client.postJSON(ctx, "com.atproto.repo.deleteRecord", request, token, nil)
}

func (b *basePDS) ListRecords(ctx context.Context, req ListRecordsRequest) (ListRecordsResult, error) {
	if strings.TrimSpace(req.Repo.String()) == "" {
		return ListRecordsResult{}, fmt.Errorf("%w: repo is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Collection.String()) == "" {
		return ListRecordsResult{}, fmt.Errorf("%w: collection is required", ErrInvalidRequest)
	}
	if req.Limit < 0 || req.Limit > maxListRecordsLimit {
		return ListRecordsResult{}, fmt.Errorf("%w: limit must be between 0 and 100", ErrInvalidRequest)
	}
	query := url.Values{}
	query.Set("repo", req.Repo.String())
	query.Set("collection", req.Collection.String())
	if req.Cursor != "" {
		query.Set("cursor", req.Cursor)
	}
	if req.Limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", req.Limit))
	}
	if req.Reverse {
		query.Set("reverse", "true")
	}
	var out struct {
		Records []recordView `json:"records"`
		Cursor  string       `json:"cursor"`
	}
	if err := b.client.getJSON(ctx, "com.atproto.repo.listRecords", query, "", &out); err != nil {
		return ListRecordsResult{}, err
	}
	result := ListRecordsResult{
		Records: make([]Record, 0, len(out.Records)),
		Cursor:  out.Cursor,
	}
	for _, view := range out.Records {
		record, err := parseRecordView(view)
		if err != nil {
			return ListRecordsResult{}, err
		}
		result.Records = append(result.Records, record)
	}
	return result, nil
}

func (b *basePDS) GetRepo(ctx context.Context, did syntax.DID, since *syntax.TID) (io.ReadCloser, error) {
	if strings.TrimSpace(did.String()) == "" {
		return nil, fmt.Errorf("%w: did is required", ErrInvalidRequest)
	}
	query := url.Values{}
	query.Set("did", did.String())
	if since != nil {
		query.Set("since", since.String())
	}
	response, err := b.client.getStream(ctx, "com.atproto.sync.getRepo", query)
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}

func (b *basePDS) GetLatestCommit(ctx context.Context, did syntax.DID) (cid.Cid, syntax.TID, error) {
	if strings.TrimSpace(did.String()) == "" {
		return cid.Undef, "", fmt.Errorf("%w: did is required", ErrInvalidRequest)
	}
	query := url.Values{}
	query.Set("did", did.String())
	var out struct {
		CID string `json:"cid"`
		Rev string `json:"rev"`
	}
	if err := b.client.getJSON(ctx, "com.atproto.sync.getLatestCommit", query, "", &out); err != nil {
		return cid.Undef, "", err
	}
	commitCID, err := parseCID(out.CID)
	if err != nil {
		return cid.Undef, "", err
	}
	rev, err := syntax.ParseTID(out.Rev)
	if err != nil {
		return cid.Undef, "", fmt.Errorf("%w: invalid rev", ErrUnexpectedResponse)
	}
	return commitCID, rev, nil
}

func (b *basePDS) SubscribeRepos(ctx context.Context, cursor *int64) (RepoEventStream, error) {
	if b.dial == nil {
		return nil, fmt.Errorf("%w: subscriptions are not configured", ErrInvalidConfig)
	}
	return newRepoEventStream(ctx, cursor, b.dial)
}

type createSessionRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

type createAccountRequest struct {
	Handle     string `json:"handle"`
	Email      string `json:"email,omitempty"`
	Password   string `json:"password,omitempty"`
	InviteCode string `json:"inviteCode,omitempty"`
}

type sessionResponse struct {
	AccessJWT  string `json:"accessJwt"`
	RefreshJWT string `json:"refreshJwt"`
	Handle     string `json:"handle"`
	DID        string `json:"did"`
	Email      string `json:"email"`
	Active     bool   `json:"active"`
}

type getSessionResponse struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
	Email  string `json:"email"`
	Active bool   `json:"active"`
}

func (b *basePDS) CreateSession(ctx context.Context, identifier, password string) (Session, error) {
	if strings.TrimSpace(identifier) == "" || strings.TrimSpace(password) == "" {
		return Session{}, fmt.Errorf("%w: identifier and password are required", ErrInvalidRequest)
	}
	var out sessionResponse
	request := createSessionRequest{Identifier: identifier, Password: password}
	if err := b.client.postJSON(ctx, "com.atproto.server.createSession", request, "", &out); err != nil {
		return Session{}, err
	}
	session, err := sessionFromResponse(out)
	if err != nil {
		return Session{}, err
	}
	b.client.auth.set(session)
	return session, nil
}

func (b *basePDS) CreateAccount(ctx context.Context, req CreateAccountRequest) (Session, error) {
	identifier := strings.TrimSpace(req.Identifier)
	if identifier == "" || strings.TrimSpace(req.Password) == "" {
		return Session{}, fmt.Errorf("%w: identifier and password are required", ErrInvalidRequest)
	}
	request := createAccountRequest{
		Handle:     identifier,
		Email:      strings.TrimSpace(req.Email),
		Password:   req.Password,
		InviteCode: strings.TrimSpace(req.InviteCode),
	}
	var out sessionResponse
	if err := b.client.postJSON(ctx, "com.atproto.server.createAccount", request, "", &out); err != nil {
		return Session{}, err
	}
	session, err := sessionFromResponse(out)
	if err != nil {
		return Session{}, err
	}
	b.client.auth.set(session)
	return session, nil
}

func sessionFromResponse(out sessionResponse) (Session, error) {
	did, err := syntax.ParseDID(strings.TrimSpace(out.DID))
	if err != nil {
		return Session{}, fmt.Errorf("%w: session response returned an invalid did", ErrUnexpectedResponse)
	}
	if out.AccessJWT == "" {
		return Session{}, fmt.Errorf("%w: session response returned no access token", ErrUnexpectedResponse)
	}
	return Session{
		DID:        did,
		Handle:     out.Handle,
		Email:      out.Email,
		AccessJWT:  out.AccessJWT,
		RefreshJWT: out.RefreshJWT,
		Active:     out.Active,
	}, nil
}

func (b *basePDS) GetSession(ctx context.Context) (Session, error) {
	token, err := b.client.requireToken(ctx)
	if err != nil {
		return Session{}, err
	}
	var out getSessionResponse
	if err := b.client.getJSON(ctx, "com.atproto.server.getSession", nil, token, &out); err != nil {
		return Session{}, err
	}
	did, err := syntax.ParseDID(strings.TrimSpace(out.DID))
	if err != nil {
		return Session{}, fmt.Errorf("%w: getSession returned an invalid did", ErrUnexpectedResponse)
	}
	stored := b.client.auth.snapshot()
	return Session{
		DID:        did,
		Handle:     out.Handle,
		Email:      out.Email,
		AccessJWT:  stored.AccessJWT,
		RefreshJWT: stored.RefreshJWT,
		Active:     out.Active,
	}, nil
}

func (b *basePDS) DeleteSession(ctx context.Context) error {
	token, err := b.client.deleteToken(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("%w: no refresh or access token is available", ErrAuthenticationRequired)
	}
	if err := b.client.postJSON(ctx, "com.atproto.server.deleteSession", nil, token, nil); err != nil {
		return err
	}
	b.client.auth.clear()
	return nil
}
