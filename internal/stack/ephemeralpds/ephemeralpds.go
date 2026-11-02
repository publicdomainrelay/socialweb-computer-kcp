package ephemeralpds

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/gorilla/websocket"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/files"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/plcsubmit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
)

const (
	DefaultReadyTimeout = 60 * time.Second

	serviceAuthTTL = 5 * time.Minute

	tombstoneTimeout = 30 * time.Second
)

type Options struct {
	IngressProxyHost string

	TLS bool

	PublicHost string

	PLCURL string

	PLCClient *http.Client

	HTTPClient *http.Client

	Dialer *websocket.Dialer

	OAuthHTTPClient *http.Client

	StateDir string

	ReadyTimeout time.Duration

	Configure func(*atpds.Config)

	Log *slog.Logger
}

type PDS struct {
	URL string

	Host string

	DID string

	Handle string

	Password string

	IngressDID string

	Server *atpds.Server

	dir        string
	subscriber *ingress.Subscriber
	plc        *plcsubmit.HTTPPLCSubmitter
	repos      *repo.SQLiteStore
	accounts   *atpds.Store
	log        *slog.Logger
	closeOnce  sync.Once
	closeErr   error
}

func Start(ctx context.Context, opts Options) (*PDS, error) {
	if strings.TrimSpace(opts.IngressProxyHost) == "" {
		return nil, errors.New("ephemeralpds: the ingress proxy host is required")
	}
	if strings.TrimSpace(opts.PLCURL) == "" {
		return nil, errors.New("ephemeralpds: the PLC directory URL is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	keypair, err := ingress.NewEphemeralKeypair()
	if err != nil {
		return nil, err
	}
	publicHost := cmp.Or(strings.TrimSpace(opts.PublicHost), hostport.URLHostname(opts.IngressProxyHost))
	host := strings.ToLower(hostport.DIDLabel(keypair.DID()) + "." + publicHost)
	publicURL := "https://" + host
	if _, err := syntax.ParseHandle(host); err != nil {
		return nil, fmt.Errorf("ephemeralpds: the run-time host %q is not a handle: %w", host, err)
	}
	dir, err := os.MkdirTemp(opts.StateDir, "ephemeral-pds-")
	if err != nil {
		return nil, fmt.Errorf("ephemeralpds: the data dir: %w", err)
	}
	p := &PDS{URL: publicURL, Host: host, Handle: host, IngressDID: keypair.DID(), dir: dir, log: log}
	ok := false
	defer func() {
		if !ok {
			_ = p.Close(context.WithoutCancel(ctx))
		}
	}()
	if p.repos, err = repo.NewSQLiteStore(ctx, dir+"/repos.db"); err != nil {
		return nil, fmt.Errorf("ephemeralpds: the repo store: %w", err)
	}
	if p.accounts, err = atpds.NewStore(ctx, dir+"/accounts.db"); err != nil {
		return nil, fmt.Errorf("ephemeralpds: the account store: %w", err)
	}
	blobs, err := files.NewFileBlobStore(dir + "/blobs")
	if err != nil {
		return nil, err
	}
	rotation, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		return nil, err
	}
	rotationPub, err := rotation.PublicKey()
	if err != nil {
		return nil, err
	}
	plcClient := cmp.Or(opts.PLCClient, &http.Client{Timeout: 15 * time.Second})
	if p.plc, err = plcsubmit.New(opts.PLCURL, rotation, plcClient); err != nil {
		return nil, err
	}
	signerKey, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return nil, err
	}
	serviceDID, err := syntax.ParseDID("did:web:" + host)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	resolveClient := cmp.Or(opts.HTTPClient, &http.Client{Timeout: 15 * time.Second})
	cfg := atpds.Config{
		Mode:      atpds.ModeFull,
		Repos:     p.repos,
		Store:     p.accounts,
		KeyStore:  files.NewMemoryRepoKeyStore(),
		Directory: &identity.BaseDirectory{PLCURL: strings.TrimRight(opts.PLCURL, "/"), HTTPClient: *resolveClient, PLCClient: plcClient},
		PLC:       p.plc,
		Blobs:     blobs,
		OAuth: &atpds.OAuthConfig{
			Issuer:     publicURL,
			Signer:     oauth.Signer{DID: serviceDID, Key: signerKey},
			HTTPClient: opts.OAuthHTTPClient,
		},
		ServiceDID:           serviceDID.String(),
		PublicBaseURL:        publicURL,
		RotationKey:          rotationPub.DIDKey(),
		SessionSecret:        secret,
		AvailableUserDomains: []string{"." + publicHost},
	}
	if opts.Configure != nil {
		opts.Configure(&cfg)
	}
	if p.Server, err = atpds.New(cfg); err != nil {
		return nil, err
	}
	registered := make(chan struct{})
	var once sync.Once
	audience := "did:web:" + hostport.URLHostname(opts.IngressProxyHost)
	p.subscriber, err = ingress.NewSubscriber(context.WithoutCancel(ctx), ingress.SubscriberConfig{
		Label:   "ephemeral-pds",
		Keypair: keypair,
		GetServiceAuthToken: func(_ context.Context, nsid string) (string, error) {
			return keypair.ServiceAuthToken(audience, nsid, serviceAuthTTL)
		},
		IngressProxyHost: opts.IngressProxyHost,
		TLS:              opts.TLS,
		HTTPClient:       opts.HTTPClient,
		Dialer:           opts.Dialer,
		HandleRequest:    exposure.IngressHandler(p.Server.Handler(), "http://pds.ephemeral"),
		OnStatus: func(status ingress.SubscriberStatus) {
			log.Info("ephemeral_pds_ingress", "host", host, "state", string(status.State), "err", status.LastError)
			if status.State == ingress.SubscriberRegistered {
				once.Do(func() { close(registered) })
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ephemeralpds: connecting to the ingress %s: %w", opts.IngressProxyHost, err)
	}
	wait, cancel := context.WithTimeout(ctx, cmp.Or(opts.ReadyTimeout, DefaultReadyTimeout))
	defer cancel()
	select {
	case <-registered:
	case <-wait.Done():
		return nil, fmt.Errorf("ephemeralpds: the ingress did not register %s: %w", host, wait.Err())
	}
	if err := p.createAccount(ctx); err != nil {
		return nil, err
	}
	log.Info("ephemeral_pds_ready", "url", publicURL, "did", p.DID, "handle", p.Handle)
	ok = true
	return p, nil
}

func (p *PDS) createAccount(ctx context.Context) error {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	p.Password = hex.EncodeToString(raw)
	body, err := json.Marshal(map[string]string{"handle": p.Handle, "password": p.Password})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/xrpc/com.atproto.server.createAccount", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Host = p.Host
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.Server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("ephemeralpds: createAccount answered HTTP %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var out struct {
		DID string `json:"did"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.DID == "" {
		return fmt.Errorf("ephemeralpds: createAccount answered no DID")
	}
	p.DID = out.DID
	return nil
}

func (p *PDS) Close(ctx context.Context) error {
	p.closeOnce.Do(func() {
		var errs []error
		if p.DID != "" && p.plc != nil {
			tctx, cancel := context.WithTimeout(ctx, tombstoneTimeout)
			if err := p.plc.Tombstone(tctx, syntax.DID(p.DID)); err != nil {
				errs = append(errs, fmt.Errorf("ephemeralpds: tombstoning %s: %w", p.DID, err))
			} else {
				p.log.Info("ephemeral_pds_tombstoned", "did", p.DID)
			}
			cancel()
		}
		if p.subscriber != nil {
			p.subscriber.Close()
		}
		if p.accounts != nil {
			_ = p.accounts.Close()
		}
		if p.repos != nil {
			_ = p.repos.Close()
		}
		if p.dir != "" {
			_ = os.RemoveAll(p.dir)
		}
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}
