package gatewayapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/xrpcserver"
)

const (
	NSIDCreateCluster    = xrpcserver.NSIDPrefix + "createCluster"
	NSIDListClusters     = xrpcserver.NSIDPrefix + "listClusters"
	NSIDGetKubeconfig    = xrpcserver.NSIDPrefix + "getKubeconfig"
	NSIDControlPlaneUp   = xrpcserver.NSIDPrefix + "controlPlaneUp"
	NSIDControlPlaneDown = xrpcserver.NSIDPrefix + "controlPlaneDown"
	NSIDDeleteCluster    = xrpcserver.NSIDPrefix + "deleteCluster"

	ClusterNameMax = 63

	HourSec = int64(3600)

	DaySec = int64(86400)

	DefaultMaxBodyBytes = int64(4 * 1024)

	DefaultMaxKubeconfigTTL = 24 * HourSec
)

var (
	clusterNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	ttlChoices = []int64{HourSec, 8 * HourSec, DaySec}
)

type ClusterSummary struct {
	Name            string `json:"name"`
	Phase           string `json:"phase"`
	Running         bool   `json:"running"`
	Instance        string `json:"instance,omitempty"`
	TeardownBlocked bool   `json:"teardownBlocked,omitempty"`
	Server          string `json:"server,omitempty"`
}

type ClusterPhaseResult struct {
	Phase string `json:"phase"`
}

type KubeconfigGrant struct {
	Kubeconfig string `json:"kubeconfig"`
	Instance   string `json:"instance,omitempty"`
}

type ClusterCaller interface {
	DID() string
	ServiceAuth(ctx context.Context, audience, lxm string) (string, error)
}

type caller struct {
	did string
	fn  func(context.Context, string, string) (string, error)
}

func (c caller) DID() string { return c.did }

func (c caller) ServiceAuth(ctx context.Context, audience, lxm string) (string, error) {
	if c.fn == nil {
		return "", errors.New("gatewayapi: no service auth source")
	}
	return c.fn(ctx, audience, lxm)
}

func NewCaller(did string, serviceAuth func(context.Context, string, string) (string, error)) ClusterCaller {
	return caller{did: did, fn: serviceAuth}
}

type ClusterGateway interface {
	List(ctx context.Context, caller ClusterCaller) ([]ClusterSummary, error)
	Create(ctx context.Context, caller ClusterCaller, name string) (ClusterPhaseResult, error)
	Kubeconfig(ctx context.Context, caller ClusterCaller, name string, ttlSeconds *int64, noExpiry bool) (KubeconfigGrant, error)
	Up(ctx context.Context, caller ClusterCaller, name string) (ClusterPhaseResult, error)
	Down(ctx context.Context, caller ClusterCaller, name string) (ClusterPhaseResult, error)
	Delete(ctx context.Context, caller ClusterCaller, name string) (ClusterPhaseResult, error)
}

type ClusterGatewayError struct {
	Status  int
	Code    string
	Message string
}

func (e *ClusterGatewayError) Error() string {
	return fmt.Sprintf("gatewayapi: cluster gateway: %s: %s", e.Code, e.Message)
}

type DirectClusterGateway struct {
	Server *gateway.Server
}

func (d DirectClusterGateway) server() (*gateway.Server, error) {
	if d.Server == nil {
		return nil, errors.New("gatewayapi: no direct gateway server")
	}
	return d.Server, nil
}

func (d DirectClusterGateway) List(ctx context.Context, c ClusterCaller) ([]ClusterSummary, error) {
	server, err := d.server()
	if err != nil {
		return nil, err
	}
	views, err := server.ListClusters(ctx, gateway.Caller(c.DID()))
	if err != nil {
		return nil, err
	}
	out := make([]ClusterSummary, 0, len(views))
	for _, v := range views {
		out = append(out, ClusterSummary{
			Name:            v.Name,
			Phase:           v.Phase,
			Running:         v.Running,
			Instance:        v.Instance,
			TeardownBlocked: v.TeardownBlocked,
			Server:          v.URL,
		})
	}
	return out, nil
}

func (d DirectClusterGateway) Create(ctx context.Context, c ClusterCaller, name string) (ClusterPhaseResult, error) {
	server, err := d.server()
	if err != nil {
		return ClusterPhaseResult{}, err
	}
	view, err := server.CreateCluster(ctx, gateway.Caller(c.DID()), gateway.NameRequest{Name: name})
	return ClusterPhaseResult{Phase: view.Phase}, err
}

func (d DirectClusterGateway) Kubeconfig(ctx context.Context, c ClusterCaller, name string, ttlSeconds *int64, noExpiry bool) (KubeconfigGrant, error) {
	server, err := d.server()
	if err != nil {
		return KubeconfigGrant{}, err
	}
	grant, err := server.GetKubeconfig(ctx, gateway.Caller(c.DID()), gateway.KubeconfigRequest{
		Name: name, TTLSeconds: ttlSeconds, NoExpiry: noExpiry,
	})
	if err != nil {
		return KubeconfigGrant{}, err
	}
	return KubeconfigGrant{Kubeconfig: grant.Kubeconfig, Instance: grant.Instance}, nil
}

func (d DirectClusterGateway) Up(ctx context.Context, c ClusterCaller, name string) (ClusterPhaseResult, error) {
	server, err := d.server()
	if err != nil {
		return ClusterPhaseResult{}, err
	}
	view, err := server.ControlPlaneUp(ctx, gateway.Caller(c.DID()), gateway.NameRequest{Name: name})
	return ClusterPhaseResult{Phase: view.Phase}, err
}

func (d DirectClusterGateway) Down(ctx context.Context, c ClusterCaller, name string) (ClusterPhaseResult, error) {
	server, err := d.server()
	if err != nil {
		return ClusterPhaseResult{}, err
	}
	view, err := server.ControlPlaneDown(ctx, gateway.Caller(c.DID()), gateway.NameRequest{Name: name})
	return ClusterPhaseResult{Phase: view.Phase}, err
}

func (d DirectClusterGateway) Delete(ctx context.Context, c ClusterCaller, name string) (ClusterPhaseResult, error) {
	server, err := d.server()
	if err != nil {
		return ClusterPhaseResult{}, err
	}
	view, err := server.DeleteCluster(ctx, gateway.Caller(c.DID()), gateway.NameRequest{Name: name})
	return ClusterPhaseResult{Phase: view.Phase}, err
}

type XRPCClusterGateway struct {
	BaseURL  func(context.Context) (string, error)
	Audience string
	Client   *http.Client
	Timeout  time.Duration
}

func (x *XRPCClusterGateway) base(ctx context.Context) (string, error) {
	if x.BaseURL == nil {
		return "", errors.New("gatewayapi: the cluster gateway has no URL")
	}
	raw, err := x.BaseURL(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(raw, "/"), nil
}

func (x *XRPCClusterGateway) client() *http.Client {
	if x.Client != nil {
		return x.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (x *XRPCClusterGateway) call(ctx context.Context, c ClusterCaller, nsid string, query bool, body any, out any) error {
	token, err := c.ServiceAuth(ctx, x.Audience, nsid)
	if err != nil {
		return &ClusterGatewayError{Status: 401, Code: "ServiceAuthUnavailable",
			Message: "could not obtain service auth from the account's PDS: " + err.Error()}
	}
	base, err := x.base(ctx)
	if err != nil {
		return &ClusterGatewayError{Status: 502, Code: "GatewayUnreachable",
			Message: "cluster gateway did not answer: " + err.Error()}
	}
	method := http.MethodPost
	var reader io.Reader
	if query {
		method = http.MethodGet
	} else {
		raw, err := json.Marshal(bodyOrEmpty(body))
		if err != nil {
			return fmt.Errorf("gatewayapi: encoding %s: %w", nsid, err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, base+xrpcserver.XRPCPrefix+nsid, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if !query {
		req.Header.Set("Content-Type", "application/json")
	}
	timeout := x.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(cctx)
	resp, err := x.client().Do(req)
	if err != nil {
		return &ClusterGatewayError{Status: 502, Code: "GatewayUnreachable",
			Message: "cluster gateway did not answer: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &ClusterGatewayError{Status: 502, Code: "GatewayUnreachable",
			Message: "cluster gateway answered unreadably: " + err.Error()}
	}
	var data map[string]any
	if len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &data)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		code, _ := data["error"].(string)
		if code == "" {
			code = "HTTP" + strconv.Itoa(resp.StatusCode)
		}
		message, _ := data["message"].(string)
		if message == "" {
			message = strings.TrimSpace(string(raw))
			if len(message) > 500 {
				message = message[:500]
			}
		}
		return &ClusterGatewayError{Status: resp.StatusCode, Code: code, Message: message}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("gatewayapi: decoding %s: %w", nsid, err)
		}
	}
	return nil
}

func bodyOrEmpty(body any) any {
	if body == nil {
		return map[string]any{}
	}
	return body
}

func (x *XRPCClusterGateway) List(ctx context.Context, c ClusterCaller) ([]ClusterSummary, error) {
	var out struct {
		Clusters []ClusterSummary `json:"clusters"`
	}
	if err := x.call(ctx, c, NSIDListClusters, true, nil, &out); err != nil {
		return nil, err
	}
	if out.Clusters == nil {
		return []ClusterSummary{}, nil
	}
	return out.Clusters, nil
}

func (x *XRPCClusterGateway) name(ctx context.Context, c ClusterCaller, nsid, name string) (ClusterPhaseResult, error) {
	var out ClusterPhaseResult
	err := x.call(ctx, c, nsid, false, map[string]any{"name": name}, &out)
	return out, err
}

func (x *XRPCClusterGateway) Create(ctx context.Context, c ClusterCaller, name string) (ClusterPhaseResult, error) {
	return x.name(ctx, c, NSIDCreateCluster, name)
}

func (x *XRPCClusterGateway) Up(ctx context.Context, c ClusterCaller, name string) (ClusterPhaseResult, error) {
	return x.name(ctx, c, NSIDControlPlaneUp, name)
}

func (x *XRPCClusterGateway) Down(ctx context.Context, c ClusterCaller, name string) (ClusterPhaseResult, error) {
	return x.name(ctx, c, NSIDControlPlaneDown, name)
}

func (x *XRPCClusterGateway) Delete(ctx context.Context, c ClusterCaller, name string) (ClusterPhaseResult, error) {
	return x.name(ctx, c, NSIDDeleteCluster, name)
}

func (x *XRPCClusterGateway) Kubeconfig(ctx context.Context, c ClusterCaller, name string, ttlSeconds *int64, noExpiry bool) (KubeconfigGrant, error) {
	body := map[string]any{"name": name}
	if ttlSeconds != nil {
		body["ttlSeconds"] = *ttlSeconds
	}
	if noExpiry {
		body["noExpiry"] = true
	}
	var out KubeconfigGrant
	err := x.call(ctx, c, NSIDGetKubeconfig, false, body, &out)
	return out, err
}

type ClusterOptions struct {
	Gateway ClusterGateway

	Caller func(context.Context, *http.Request) (ClusterCaller, error)

	MaxKubeconfigTTLSec int64

	KCPPublicHost string

	MaxBodyBytes int64

	Log *slog.Logger
}

type ClusterAPI struct {
	opts       ClusterOptions
	maxBody    int64
	maxTTLSec  int64
	ttlChoices []int64
}

func NewClusterAPI(opts ClusterOptions) (*ClusterAPI, error) {
	if opts.Gateway == nil {
		return nil, errors.New("gatewayapi: no cluster gateway")
	}
	if opts.Caller == nil {
		return nil, errors.New("gatewayapi: no caller authenticator")
	}
	if opts.MaxKubeconfigTTLSec <= 0 {
		opts.MaxKubeconfigTTLSec = DefaultMaxKubeconfigTTL
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	choices := make([]int64, 0, len(ttlChoices))
	for _, c := range ttlChoices {
		if c <= opts.MaxKubeconfigTTLSec {
			choices = append(choices, c)
		}
	}
	if len(choices) == 0 {
		choices = []int64{opts.MaxKubeconfigTTLSec}
	}
	return &ClusterAPI{opts: opts, maxBody: opts.MaxBodyBytes, maxTTLSec: opts.MaxKubeconfigTTLSec, ttlChoices: choices}, nil
}

func (a *ClusterAPI) log() *slog.Logger {
	if a.opts.Log != nil {
		return a.opts.Log
	}
	return slog.Default()
}

func (a *ClusterAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/config", a.config)
	mux.HandleFunc("POST /api/clusters/list", a.list)
	mux.HandleFunc("POST /api/clusters/create", a.nameAction("create"))
	mux.HandleFunc("POST /api/clusters/up", a.nameAction("up"))
	mux.HandleFunc("POST /api/clusters/down", a.nameAction("down"))
	mux.HandleFunc("POST /api/clusters/delete", a.nameAction("delete"))
	mux.HandleFunc("POST /api/clusters/kubeconfig", a.kubeconfig)
}

func (a *ClusterAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	a.Register(mux)
	return mux
}

func (a *ClusterAPI) config(w http.ResponseWriter, r *http.Request) {
	var host any
	if a.opts.KCPPublicHost != "" {
		host = a.opts.KCPPublicHost
	}
	writeJSON(w, http.StatusOK, struct {
		TTLChoicesSec       []int64 `json:"ttlChoicesSec"`
		MaxKubeconfigTTLSec int64   `json:"maxKubeconfigTtlSec"`
		KCPPublicHost       any     `json:"kcpPublicHost"`
	}{a.ttlChoices, a.maxTTLSec, host})
}

func (a *ClusterAPI) caller(r *http.Request) (ClusterCaller, *apiFailure) {
	who, err := a.opts.Caller(r.Context(), r)
	if err != nil {
		a.log().Error("cluster_api_auth_unavailable", "err", err)
		return nil, &apiFailure{Status: http.StatusServiceUnavailable, Error: "Unavailable",
			Message: "the vault is not answering; try again"}
	}
	if who == nil {
		return nil, &apiFailure{Status: http.StatusUnauthorized, Error: "Unauthorized", Message: "sign in again"}
	}
	return who, nil
}

func (a *ClusterAPI) body(r *http.Request) (map[string]any, *apiFailure) {
	if declared, err := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64); err == nil && declared > a.maxBody {
		return nil, &apiFailure{Status: http.StatusRequestEntityTooLarge, Error: "BodyTooLarge"}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, a.maxBody+1))
	if err != nil {
		return nil, &apiFailure{Status: http.StatusBadRequest, Error: "InvalidRequest", Message: "expected a JSON object"}
	}
	if int64(len(raw)) > a.maxBody {
		return nil, &apiFailure{Status: http.StatusRequestEntityTooLarge, Error: "BodyTooLarge"}
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil || out == nil {
		return nil, &apiFailure{Status: http.StatusBadRequest, Error: "InvalidRequest", Message: "expected a JSON object"}
	}
	return out, nil
}

type namedInput struct {
	Caller ClusterCaller
	Name   string
	Input  map[string]any
}

func (a *ClusterAPI) named(w http.ResponseWriter, r *http.Request) (*namedInput, bool) {
	who, failure := a.caller(r)
	if failure != nil {
		writeFailure(w, failure)
		return nil, false
	}
	input, failure := a.body(r)
	if failure != nil {
		writeFailure(w, failure)
		return nil, false
	}
	name, _ := input["name"].(string)
	if !IsClusterName(name) {
		writeFailure(w, &apiFailure{Status: http.StatusBadRequest, Error: "InvalidRequest",
			Message: "name must be a lowercase DNS label of at most 63 characters"})
		return nil, false
	}
	return &namedInput{Caller: who, Name: name, Input: input}, true
}

func (a *ClusterAPI) list(w http.ResponseWriter, r *http.Request) {
	who, failure := a.caller(r)
	if failure != nil {
		writeFailure(w, failure)
		return
	}
	clusters, err := a.opts.Gateway.List(r.Context(), who)
	if err != nil {
		a.failure(w, who.DID(), "list", err)
		return
	}
	if clusters == nil {
		clusters = []ClusterSummary{}
	}
	writeJSON(w, http.StatusOK, struct {
		Clusters []ClusterSummary `json:"clusters"`
	}{clusters})
}

func (a *ClusterAPI) nameAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := a.named(w, r)
		if !ok {
			return
		}
		var out ClusterPhaseResult
		var err error
		switch action {
		case "create":
			out, err = a.opts.Gateway.Create(r.Context(), req.Caller, req.Name)
		case "up":
			out, err = a.opts.Gateway.Up(r.Context(), req.Caller, req.Name)
		case "down":
			out, err = a.opts.Gateway.Down(r.Context(), req.Caller, req.Name)
		default:
			out, err = a.opts.Gateway.Delete(r.Context(), req.Caller, req.Name)
		}
		if err != nil {
			a.failure(w, req.Caller.DID(), action, err)
			return
		}
		a.log().Info("cluster_api_ok", "did", req.Caller.DID(), "action", action, "name", req.Name, "phase", out.Phase)
		writeJSON(w, http.StatusOK, struct {
			Name  string `json:"name"`
			Phase string `json:"phase"`
		}{req.Name, out.Phase})
	}
}

func (a *ClusterAPI) kubeconfig(w http.ResponseWriter, r *http.Request) {
	req, ok := a.named(w, r)
	if !ok {
		return
	}
	rawNoExpiry, present := req.Input["noExpiry"]
	if present {
		if _, isBool := rawNoExpiry.(bool); !isBool {
			writeFailure(w, &apiFailure{Status: http.StatusBadRequest, Error: "InvalidRequest",
				Message: "noExpiry must be a boolean"})
			return
		}
	}
	noExpiry, _ := rawNoExpiry.(bool)
	rawTTL, ttlPresent := req.Input["ttlSeconds"]
	if noExpiry && ttlPresent {
		writeFailure(w, &apiFailure{Status: http.StatusBadRequest, Error: "InvalidRequest",
			Message: "noExpiry and ttlSeconds are mutually exclusive"})
		return
	}
	var ttl *int64
	if ttlPresent {
		number, isNumber := rawTTL.(json.Number)
		value, err := number.Int64()
		if !isNumber || err != nil || !a.allowedTTL(value) {
			writeFailure(w, &apiFailure{Status: http.StatusBadRequest, Error: "InvalidRequest",
				Message: "ttlSeconds must be one of " + joinInt64(a.ttlChoices, ", ")})
			return
		}
		ttl = &value
	}
	grant, err := a.opts.Gateway.Kubeconfig(r.Context(), req.Caller, req.Name, ttl, noExpiry)
	if err != nil {
		a.failure(w, req.Caller.DID(), "kubeconfig", err)
		return
	}
	a.log().Info("cluster_api_ok", "did", req.Caller.DID(), "action", "kubeconfig", "name", req.Name,
		"ttlSeconds", ttl, "noExpiry", noExpiry)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		Name       string `json:"name"`
		Filename   string `json:"filename"`
		Kubeconfig string `json:"kubeconfig"`
	}{req.Name, KubeconfigFilename(req.Name), grant.Kubeconfig})
}

func (a *ClusterAPI) allowedTTL(value int64) bool {
	for _, c := range a.ttlChoices {
		if c == value {
			return true
		}
	}
	return false
}

func (a *ClusterAPI) failure(w http.ResponseWriter, did, action string, err error) {
	var ge *ClusterGatewayError
	if errors.As(err, &ge) {
		status := ge.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		a.log().Error("cluster_api_gateway_error", "did", did, "action", action, "status", ge.Status,
			"code", ge.Code, "message", ge.Message)
		writeFailure(w, &apiFailure{Status: status, Error: ge.Code, Message: ge.Message})
		return
	}
	var xe *gateway.XRPCError
	if errors.As(err, &xe) {
		status := xe.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		a.log().Error("cluster_api_gateway_error", "did", did, "action", action, "status", xe.Status,
			"code", xe.Name, "message", xe.Message)
		writeFailure(w, &apiFailure{Status: status, Error: xe.Name, Message: xe.Message})
		return
	}
	a.log().Error("cluster_api_failed", "did", did, "action", action, "err", err)
	writeFailure(w, &apiFailure{Status: http.StatusInternalServerError, Error: "InternalError",
		Message: "the cluster request failed"})
}

type apiFailure struct {
	Status  int
	Error   string
	Message string
}

func writeFailure(w http.ResponseWriter, f *apiFailure) {
	writeJSON(w, f.Status, struct {
		Error   string `json:"error"`
		Message string `json:"message,omitempty"`
	}{f.Error, f.Message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func IsClusterName(name string) bool {
	return len(name) > 0 && len(name) <= ClusterNameMax && clusterNamePattern.MatchString(name)
}

func KubeconfigFilename(cluster string) string {
	return cluster + ".kubeconfig.yaml"
}

func TTLChoicesUpTo(maxSec int64) []int64 {
	allowed := make([]int64, 0, len(ttlChoices))
	for _, c := range ttlChoices {
		if c <= maxSec {
			allowed = append(allowed, c)
		}
	}
	if len(allowed) == 0 {
		return []int64{maxSec}
	}
	return allowed
}

func joinInt64(values []int64, sep string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(parts, sep)
}
