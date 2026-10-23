package eventstream

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	cid "github.com/ipfs/go-cid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statesync"
)

const (
	marketBidderMarketInbox = "bidder.market-inbox"

	marketRequesterMarketInbox = "requester.market-inbox"

	marketRequesterBidInbox = "requester.bid-inbox"

	marketInboxLabel = "socialweb.computer/market-inbox"

	marketCollectionLabel = "socialweb.computer/market-collection"

	marketNamePrefix = "market-"

	computeEventsPrefix = "com.publicdomainrelay.temp.compute.events."
)

type MarketOptions struct {
	Workspace string

	Role string

	Dynamic DynamicFactory

	Resolver *pdsclient.Resolver

	PLCDirectoryURL string

	Log *slog.Logger
}

func MarketHandlerName(role, workspace string) string {
	return marketNamePrefix + strings.ToLower(strings.TrimSpace(role)) + "@" + strings.TrimSpace(workspace)
}

type marketHandler struct {
	opts MarketOptions

	log *slog.Logger

	directory identity.Directory

	client *http.Client

	mu    sync.Mutex
	pdses map[syntax.DID]pdsclient.PDS
}

type marketPayload struct {
	DID string `json:"did"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Collection string `json:"collection"`

	Record map[string]any `json:"record"`
}

func NewMarketHandler(opts MarketOptions) (Handler, error) {
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	if opts.Workspace == "" {
		return nil, errors.New("eventstream: a market handler needs a workspace")
	}
	opts.Role = strings.ToLower(strings.TrimSpace(opts.Role))
	switch opts.Role {
	case "bidder", "requester":
	default:
		return nil, fmt.Errorf("eventstream: unknown market role %q", opts.Role)
	}
	if opts.Dynamic == nil {
		return nil, errors.New("eventstream: a market handler needs a dynamic client factory")
	}
	if opts.Resolver == nil {
		return nil, errors.New("eventstream: a market handler needs a pdsclient resolver")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	httpClient := opts.Resolver.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	httpClient = httpClientWithFallback(httpClient)
	plcURL := strings.TrimSpace(opts.PLCDirectoryURL)
	if plcURL == "" {
		plcURL = "http://" + kcp.ServiceFQDN("plc", "default", opts.Workspace, kcp.DefaultServiceDomain)
	}
	local := &identity.BaseDirectory{
		PLCURL:                 plcURL,
		HTTPClient:             *httpClient,
		PLCClient:              httpClient,
		SkipHandleVerification: true,
	}
	public := &identity.BaseDirectory{
		PLCURL:                 identity.DefaultPLCURL,
		HTTPClient:             *httpClient,
		PLCClient:              httpClient,
		SkipHandleVerification: true,
	}
	return &marketHandler{
		opts:      opts,
		log:       opts.Log,
		directory: fallbackDirectory{local, public},
		client:    httpClient,
		pdses:     map[syntax.DID]pdsclient.PDS{},
	}, nil
}

func (h *marketHandler) HandleEvent(ctx context.Context, event Event) error {
	for _, op := range event.Ops {
		switch strings.ToLower(strings.TrimSpace(op.Action)) {
		case "create", "update":
		default:
			continue
		}
		collection, rkey, ok := splitPath(op.Path)
		if !ok {
			continue
		}
		adapter := marketAdapter(h.opts.Role, collection)
		if adapter == "" {
			continue
		}
		uri := "at://" + event.Repo.String() + "/" + collection + "/" + rkey
		value, cid, err := h.fetchMarketRecord(ctx, event, collection, rkey, op.CID)
		if err != nil {
			return fmt.Errorf("eventstream: fetching the market record %s: %w", uri, err)
		}
		if err := h.createSync(ctx, adapter, event.Repo.String(), uri, cid, collection, value); err != nil {
			return err
		}
	}
	return nil
}

func (h *marketHandler) createSync(ctx context.Context, adapter, did, uri, cid, collection string, record map[string]any) error {
	payload, err := json.Marshal(marketPayload{DID: did, URI: uri, CID: cid, Collection: collection, Record: record})
	if err != nil {
		return fmt.Errorf("eventstream: encoding the market payload for %s: %w", uri, err)
	}
	object := &v1alpha1.StateSync{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "StateSync"},
		ObjectMeta: metav1.ObjectMeta{
			Name: marketSyncName(uri, cid),
			Labels: map[string]string{
				marketInboxLabel:      "true",
				marketCollectionLabel: collection,
			},
		},
		Spec: v1alpha1.StateSyncSpec{
			Sink:           v1alpha1.StateSyncSink{Adapter: adapter},
			Direction:      v1alpha1.StateSyncDirectionOutbound,
			Payload:        payload,
			IdempotencyKey: "market:" + uri + "#" + cid,
			ConflictPolicy: v1alpha1.StateSyncConflictSourceWins,
			DeletionPolicy: v1alpha1.StateSyncDeletionRetain,
		},
	}
	stored, err := toUnstructured(object)
	if err != nil {
		return fmt.Errorf("eventstream: encoding the market StateSync for %s: %w", uri, err)
	}
	dyn, err := h.opts.Dynamic(h.opts.Workspace)
	if err != nil {
		return fmt.Errorf("eventstream: opening workspace %s: %w", h.opts.Workspace, err)
	}
	if _, err := dyn.Resource(statesync.StateSyncGVR).Create(ctx, &unstructured.Unstructured{Object: stored}, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("eventstream: creating the market StateSync %s: %w", object.Name, err)
	}
	return nil
}

func (h *marketHandler) fetchMarketRecord(ctx context.Context, event Event, collection, rkey string, cidPtr *cid.Cid) (map[string]any, string, error) {
	cidText := ""
	if cidPtr != nil {
		cidText = cidPtr.String()
	}
	if pds, err := h.authorPDS(ctx, event.Repo); err == nil {
		record, err := pds.GetRecord(ctx, pdsclient.RepoPath{Repo: event.Repo, Collection: syntax.NSID(collection), Rkey: syntax.RecordKey(rkey)}, cidPtr)
		if err == nil {
			value := map[string]any{}
			if err := json.Unmarshal(record.Value, &value); err != nil {
				return nil, "", err
			}
			return value, record.CID.String(), nil
		}
	}
	hostname, err := h.relayHost(ctx, event.Source, event.Repo.String())
	if err != nil {
		return nil, "", err
	}
	for _, scheme := range []string{"http", "https"} {
		target := scheme + "://" + hostname + "/xrpc/com.atproto.repo.getRecord?repo=" + url.QueryEscape(event.Repo.String()) +
			"&collection=" + url.QueryEscape(collection) + "&rkey=" + url.QueryEscape(rkey) + "&cid=" + url.QueryEscape(cidText)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := h.httpClient().Do(req)
		if err != nil {
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		var body struct {
			CID   string         `json:"cid"`
			Value map[string]any `json:"value"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			continue
		}
		if body.Value == nil {
			continue
		}
		body.CID = cmp.Or(body.CID, cidText)
		return body.Value, body.CID, nil
	}
	return nil, "", errors.New("the author PDS is not reachable through its identity or the relay host list")
}

func (h *marketHandler) relayHost(ctx context.Context, source, did string) (string, error) {
	base, ok := strings.CutPrefix(strings.TrimSpace(source), "relay/")
	if !ok || base == "" {
		return "", errors.New("no relay source names the author PDS")
	}
	target := strings.TrimRight(base, "/") + "/xrpc/com.atproto.sync.getRepoStatus?did=" + url.QueryEscape(did)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	resp, err := h.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("relay getRepoStatus returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Hostname string `json:"hostname"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if strings.TrimSpace(body.Hostname) == "" {
		return "", fmt.Errorf("relay getRepoStatus names no host for %s", did)
	}
	return strings.TrimSpace(body.Hostname), nil
}

func (h *marketHandler) authorPDS(ctx context.Context, did syntax.DID) (pdsclient.PDS, error) {
	h.mu.Lock()
	cached := h.pdses[did]
	h.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	ident, err := h.directory.LookupDID(ctx, did)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(ident.PDSEndpoint())
	if endpoint == "" {
		return nil, fmt.Errorf("%s declares no atproto_pds service", did)
	}
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL:    endpoint,
		DID:        did,
		HTTPClient: h.httpClient(),
		Timeout:    20 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.pdses[did] = pds
	h.mu.Unlock()
	return pds, nil
}

func (h *marketHandler) httpClient() *http.Client {
	if h.client != nil {
		return h.client
	}
	return httpClientWithFallback(nil)
}

func marketSyncName(uri, cid string) string {
	sum := sha256.Sum256([]byte(uri + "#" + cid))
	return marketNamePrefix + hex.EncodeToString(sum[:16])
}

func marketAdapter(role, collection string) string {
	compute := strings.HasPrefix(collection, computeEventsPrefix)
	switch role {
	case "bidder":
		switch collection {
		case market.RFPNSID, market.AcceptNSID, market.ReceiptNSID, market.EventNSID:
			return marketBidderMarketInbox
		}
		if compute {
			return marketBidderMarketInbox
		}
	case "requester":
		switch collection {
		case market.BidNSID, market.BidsFreeNSID, market.BidsX402NSID:
			return marketRequesterBidInbox
		case market.ReceiptNSID, market.ReceiptsFreeNSID, market.ReceiptsX402NSID, market.EventNSID:
			return marketRequesterMarketInbox
		}
		if compute {
			return marketRequesterMarketInbox
		}
	}
	return ""
}
