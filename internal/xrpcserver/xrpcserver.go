package xrpcserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/lexicon"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotoauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
)

const (
	XRPCPrefix = "/xrpc/"

	NSIDPrefix = "com.publicdomainrelay.temp.cluster."

	K8sNSIDPrefix = "computer.socialweb.k8s."

	maxBody = 1 << 16

	maxManifestBody = 1 << 20

	DefaultRetryAfter = 5 * time.Second
)

const (
	nsidGetKubeconfig    = NSIDPrefix + "getKubeconfig"
	nsidCreateCluster    = NSIDPrefix + "createCluster"
	nsidControlPlaneUp   = NSIDPrefix + "controlPlaneUp"
	nsidControlPlaneDown = NSIDPrefix + "controlPlaneDown"
	nsidDeleteCluster    = NSIDPrefix + "deleteCluster"
	nsidListClusters     = NSIDPrefix + "listClusters"
	nsidRevokeIdentity   = NSIDPrefix + "revokeIdentity"

	nsidKubectlApplyServerSide = K8sNSIDPrefix + "kubectlApplyServerSide"
	nsidGetJob                 = K8sNSIDPrefix + "getJob"
)

var retryable = map[string]bool{gateway.ErrClusterInitializing.Name: true}

type Options struct {
	Gateway *gateway.Server

	Audiences []string

	Directory identity.Directory

	RetryAfter time.Duration

	StreamLifetime time.Duration

	StreamsPerCaller int
}

type verb func(ctx context.Context, caller gateway.Caller, in any) (any, error)

type route struct {
	nsid  string
	query bool

	errors []lexicon.SchemaError

	input any

	maxBody int64

	call verb
}

type Server struct {
	gateway *gateway.Server
	opts    Options
	routes  []route
	catalog *lexicon.BaseCatalog
	inputs  map[string]lexicon.SchemaObject
	params  map[string]lexicon.SchemaObject
	streams *streams
}

func New(opts Options) (*Server, error) {
	if opts.Gateway == nil {
		return nil, errors.New("xrpcserver: no gateway")
	}
	if len(opts.Audiences) == 0 {
		return nil, errors.New("xrpcserver: no audiences; every token would be refused as Unauthorized")
	}
	if opts.Directory == nil {
		return nil, errors.New("xrpcserver: no directory; did:plc and did:web issuers could not be resolved")
	}
	if opts.RetryAfter == 0 {
		opts.RetryAfter = DefaultRetryAfter
	}
	if opts.StreamLifetime == 0 {
		opts.StreamLifetime = DefaultStreamLifetime
	}
	if opts.StreamsPerCaller == 0 {
		opts.StreamsPerCaller = DefaultStreamsPerCaller
	}

	cat, err := loadCatalog()
	if err != nil {
		return nil, err
	}

	s := &Server{
		gateway: opts.Gateway,
		opts:    opts,
		catalog: cat,
		inputs:  map[string]lexicon.SchemaObject{},
		params:  map[string]lexicon.SchemaObject{},
		streams: newStreams(opts.StreamsPerCaller),
	}
	if err := s.bindSubscription(nsidSubscribeClusters); err != nil {
		return nil, err
	}

	for _, r := range []route{
		{nsid: nsidGetKubeconfig, input: &gateway.KubeconfigRequest{},
			call: func(ctx context.Context, c gateway.Caller, in any) (any, error) {
				return s.gateway.GetKubeconfig(ctx, c, *(in.(*gateway.KubeconfigRequest)))
			}},
		{nsid: nsidCreateCluster, call: func(ctx context.Context, c gateway.Caller, in any) (any, error) {
			return s.gateway.CreateCluster(ctx, c, *(in.(*gateway.NameRequest)))
		}},
		{nsid: nsidControlPlaneUp, call: func(ctx context.Context, c gateway.Caller, in any) (any, error) {
			return s.gateway.ControlPlaneUp(ctx, c, *(in.(*gateway.NameRequest)))
		}},
		{nsid: nsidControlPlaneDown, call: func(ctx context.Context, c gateway.Caller, in any) (any, error) {
			return s.gateway.ControlPlaneDown(ctx, c, *(in.(*gateway.NameRequest)))
		}},
		{nsid: nsidDeleteCluster, call: func(ctx context.Context, c gateway.Caller, in any) (any, error) {
			return s.gateway.DeleteCluster(ctx, c, *(in.(*gateway.NameRequest)))
		}},
		{nsid: nsidListClusters, call: func(ctx context.Context, c gateway.Caller, _ any) (any, error) {
			return s.gateway.ListClusters(ctx, c)
		}},
		{nsid: nsidRevokeIdentity, input: &gateway.RevokeRequest{},
			call: func(ctx context.Context, c gateway.Caller, in any) (any, error) {
				return s.gateway.RevokeIdentity(ctx, c, *(in.(*gateway.RevokeRequest)))
			}},
		{nsid: nsidKubectlApplyServerSide, input: &gateway.ApplyRequest{}, maxBody: maxManifestBody,
			call: func(ctx context.Context, c gateway.Caller, in any) (any, error) {
				return s.gateway.KubectlApplyServerSide(ctx, c, *(in.(*gateway.ApplyRequest)))
			}},
		{nsid: nsidGetJob, input: &gateway.JobRequest{},
			call: func(ctx context.Context, c gateway.Caller, in any) (any, error) {
				return s.gateway.GetJob(ctx, c, *(in.(*gateway.JobRequest)))
			}},
	} {
		if err := s.bind(&r); err != nil {
			return nil, err
		}
		s.routes = append(s.routes, r)
	}
	return s, nil
}

func (s *Server) bind(r *route) error {
	sch, err := s.catalog.Resolve(r.nsid)
	if err != nil {
		return fmt.Errorf("xrpcserver: %s has no lexicon: %w", r.nsid, err)
	}
	switch def := sch.Def.(type) {
	case lexicon.SchemaProcedure:
		if def.Input == nil {
			return fmt.Errorf("xrpcserver: %s is a procedure with no declared input", r.nsid)
		}
		obj, err := objectOf(r.nsid+" input", def.Input.Schema)
		if err != nil {
			return err
		}
		s.inputs[r.nsid] = obj
		r.errors = def.Errors
	case lexicon.SchemaQuery:
		s.params[r.nsid] = paramsAsObject(def.Parameters)
		r.query = true
		r.errors = def.Errors
	default:
		return fmt.Errorf("xrpcserver: %s is a %T; only procedures and queries are served", r.nsid, sch.Def)
	}
	return nil
}

func (s *Server) bindSubscription(nsid string) error {
	sch, err := s.catalog.Resolve(nsid)
	if err != nil {
		return fmt.Errorf("xrpcserver: %s has no lexicon: %w", nsid, err)
	}
	def, ok := sch.Def.(lexicon.SchemaSubscription)
	if !ok {
		return fmt.Errorf("xrpcserver: %s is a %T, not a subscription", nsid, sch.Def)
	}
	s.params[nsid] = paramsAsObject(def.Parameters)
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routes {
		method := http.MethodPost
		if r.query {
			method = http.MethodGet
		}
		mux.Handle(method+" "+XRPCPrefix+r.nsid, s.xrpc(r))
	}
	mux.HandleFunc(XRPCPrefix+nsidSubscribeClusters, s.subscribeClusters)
	return mux
}

func (s *Server) xrpc(r route) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		caller, ok := s.authorize(w, req, r.nsid)
		if !ok {
			return
		}

		in := newInput(r.input)
		if r.query {
			params, err := queryParams(req.URL.Query(), s.params[r.nsid])
			if err != nil {
				writeError(w, r.nsid, s.opts.RetryAfter, gateway.ErrInvalidRequest)
				return
			}
			if err := validate(r.nsid+" parameters", s.catalog, s.params[r.nsid], params); err != nil {
				writeError(w, r.nsid, s.opts.RetryAfter, gateway.ErrInvalidRequest)
				return
			}
			if err := paramsInto(params, in); err != nil {
				writeError(w, r.nsid, s.opts.RetryAfter, gateway.ErrInvalidRequest)
				return
			}
		} else {
			limit := r.maxBody
			if limit == 0 {
				limit = maxBody
			}
			body, err := decodeBody(req, limit, r.nsid, s.catalog, s.inputs[r.nsid], in)
			if err != nil {
				writeError(w, r.nsid, s.opts.RetryAfter, gateway.ErrInvalidRequest)
				return
			}
			in = body
		}

		out, err := r.call(req.Context(), caller, in)
		if r.nsid == nsidGetKubeconfig {
			w.Header().Set("Cache-Control", "no-store")
		}
		if err != nil {
			writeError(w, r.nsid, s.opts.RetryAfter, err)
			return
		}
		httpjson.Write(w, http.StatusOK, shape(r.nsid, out))
	}
}

func (s *Server) authorize(w http.ResponseWriter, req *http.Request, nsid string) (gateway.Caller, bool) {
	token, ok := bearer(req.Header.Get("Authorization"))
	if !ok {
		writeError(w, nsid, s.opts.RetryAfter, gateway.ErrUnauthorized)
		return "", false
	}

	ctx, cancel := context.WithTimeout(req.Context(), atprotoauth.LookupTimeout)
	defer cancel()

	did, err := atprotoauth.Verify(ctx, token, nsid, s.opts.Audiences, s.opts.Directory)
	if err != nil {
		if errors.Is(err, atprotoauth.ErrNoKey) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeError(w, nsid, s.opts.RetryAfter, gateway.ErrClusterUnavailable)
			return "", false
		}
		writeError(w, nsid, s.opts.RetryAfter, gateway.ErrUnauthorized)
		return "", false
	}

	return gateway.Caller(did), true
}

func newInput(proto any) any {
	if proto == nil {
		return &gateway.NameRequest{}
	}
	return reflect.New(reflect.TypeOf(proto).Elem()).Interface()
}

func paramsInto(params map[string]any, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	if _, ok := out.(*gateway.NameRequest); ok {
		return nil
	}
	return checkClusterName(out)
}

func decodeBody(req *http.Request, limit int64, nsid string, cat *lexicon.BaseCatalog, def lexicon.SchemaObject, out any) (any, error) {
	raw, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("xrpcserver: %s body is over %d bytes", nsid, limit)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	if err := validate(nsid, cat, def, numbersAsInts(body)); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, err
	}
	if err := checkClusterName(out); err != nil {
		return nil, err
	}
	return out, nil
}

func checkClusterName(out any) error {
	switch t := out.(type) {
	case *gateway.NameRequest:
		return labelOrInvalidRequest(t.Name)
	case *gateway.KubeconfigRequest:
		return labelOrInvalidRequest(t.Name)
	case *gateway.ApplyRequest:
		return labelOrInvalidRequest(t.Cluster)
	case *gateway.JobRequest:
		return labelOrInvalidRequest(t.Cluster)
	case *gateway.RevokeRequest:
		if t.Name == "" {
			return nil
		}
		return labelOrInvalidRequest(t.Name)
	}
	return nil
}

func labelOrInvalidRequest(name string) error {
	if len(validation.IsDNS1123Label(name)) > 0 {
		return gateway.ErrInvalidRequest
	}
	return nil
}

func numbersAsInts(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	for k, e := range m {
		m[k] = numberAsInt(e)
	}
	return m
}

func numberAsInt(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case map[string]any:
		for k, e := range t {
			t[k] = numberAsInt(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = numberAsInt(e)
		}
		return t
	default:
		return v
	}
}

func queryParams(q url.Values, def lexicon.SchemaObject) (map[string]any, error) {
	out := map[string]any{}
	for name, sd := range def.Properties {
		vals, ok := q[name]
		if !ok || len(vals) == 0 {
			continue
		}
		switch sd.Inner.(type) {
		case lexicon.SchemaInteger:
			n, err := strconv.ParseInt(vals[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("xrpcserver: %s is not an integer: %w", name, err)
			}
			out[name] = n
		case lexicon.SchemaBoolean:
			b, err := strconv.ParseBool(vals[0])
			if err != nil {
				return nil, fmt.Errorf("xrpcserver: %s is not a boolean: %w", name, err)
			}
			out[name] = b
		default:
			out[name] = vals[0]
		}
	}
	return out, nil
}

func shape(nsid string, out any) any {
	switch v := out.(type) {
	case *gateway.GetKubeconfigResponse:
		return getKubeconfigOutput{Kubeconfig: v.Kubeconfig, Instance: v.Instance}
	case gateway.ClusterView:
		return phaseOutput{Phase: v.Phase}
	case *gateway.RevokeResponse:
		return revokeIdentityOutput{Revoked: v.Revoked}
	case []gateway.ClusterView:
		items := make([]clusterItem, 0, len(v))
		for _, c := range v {
			items = append(items, clusterItem{
				Name:            c.Name,
				Phase:           c.Phase,
				Running:         c.Running,
				Instance:        c.Instance,
				TeardownBlocked: c.TeardownBlocked,
				Server:          c.URL,
			})
		}
		return listClustersOutput{Clusters: items}
	default:
		return out
	}
}

type getKubeconfigOutput struct {
	Kubeconfig string `json:"kubeconfig"`
	Instance   string `json:"instance"`
}

type phaseOutput struct {
	Phase string `json:"phase"`
}

type revokeIdentityOutput struct {
	Revoked []revocation.ReplyEntry `json:"revoked"`
}

type clusterItem struct {
	Name            string `json:"name"`
	Phase           string `json:"phase"`
	Running         bool   `json:"running"`
	Instance        string `json:"instance"`
	TeardownBlocked bool   `json:"teardownBlocked,omitempty"`
	Server          string `json:"server"`
}

type listClustersOutput struct {
	Clusters []clusterItem `json:"clusters"`
}

func writeError(w http.ResponseWriter, nsid string, retryAfter time.Duration, err error) {
	var xe *gateway.XRPCError
	if !errors.As(err, &xe) {
		slog.Error("xrpc: unmapped error", "nsid", nsid, "err", err)
		xe = &gateway.XRPCError{
			Name:    "InternalError",
			Message: "the request could not be completed",
			Status:  500,
		}
	}
	if retryable[xe.Name] {
		if retryAfter <= 0 {
			retryAfter = DefaultRetryAfter
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	}
	httpjson.Write(w, xe.Status, errorBody{Error: xe.Name, Message: xe.Message})
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func bearer(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}
