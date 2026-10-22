package sshserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	coreapi "github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const ownerSessionRequestTimeout = 30 * time.Second

type ownerSessionAuth struct {
	sessions map[string]OwnerSession
}

func (a ownerSessionAuth) Authenticate(r *http.Request) (*coreapi.WebCaller, error) {
	if r == nil {
		return nil, nil
	}
	identifier, password, ok := r.BasicAuth()
	if !ok {
		return nil, nil
	}
	for did, session := range a.sessions {
		sameIdentifier := subtle.ConstantTimeCompare([]byte(session.Identifier), []byte(identifier)) == 1
		samePassword := subtle.ConstantTimeCompare([]byte(session.Password), []byte(password)) == 1
		if !sameIdentifier || !samePassword {
			continue
		}
		return &coreapi.WebCaller{WebUser: coreapi.WebUser{DID: did, Handle: session.Identifier, PDS: session.Service}}, nil
	}
	return nil, nil
}

type multiAuth []sshtransport.Authenticator

func (m multiAuth) Authenticate(r *http.Request) (*coreapi.WebCaller, error) {
	var last error
	for _, auth := range m {
		if auth == nil {
			continue
		}
		caller, err := auth.Authenticate(r)
		if caller != nil {
			return caller, nil
		}
		if err != nil {
			last = err
		}
	}
	return nil, last
}

type ownerSessionAgents struct {
	sessions map[string]OwnerSession

	client *http.Client

	log *slog.Logger
}

func (a *ownerSessionAgents) AgentFor(caller *coreapi.WebCaller) coreapi.WebAccountAgent {
	if caller == nil {
		return nil
	}
	session, ok := a.sessions[caller.DID]
	if !ok {
		return nil
	}
	return &ownerSessionAgent{did: caller.DID, session: session, client: a.client, log: a.log}
}

type combinedAgents []sshtransport.AgentProvider

func (c combinedAgents) AgentFor(caller *coreapi.WebCaller) coreapi.WebAccountAgent {
	for _, provider := range c {
		if provider == nil {
			continue
		}
		if agent := provider.AgentFor(caller); agent != nil {
			return agent
		}
	}
	return unavailableAgent{}
}

type ownerSessionAgent struct {
	did string

	session OwnerSession

	client *http.Client

	log *slog.Logger

	mu sync.Mutex

	pds *pdsclient.RemotePDS
}

func (a *ownerSessionAgent) remote(ctx context.Context) (*pdsclient.RemotePDS, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pds != nil {
		return a.pds, nil
	}
	did, err := syntax.ParseDID(a.did)
	if err != nil {
		return nil, fmt.Errorf("sshserver: the owner DID: %w", err)
	}
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL:    a.session.Service,
		DID:        did,
		HTTPClient: a.client,
		Timeout:    ownerSessionRequestTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("sshserver: the owner PDS %s: %w", a.session.Service, err)
	}
	if _, err := pds.CreateSession(ctx, a.session.Identifier, a.session.Password); err != nil {
		return nil, fmt.Errorf("sshserver: the owner session of %s: %w", a.did, err)
	}
	a.pds = pds
	return pds, nil
}

func (a *ownerSessionAgent) GetServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	return "", errors.New("sshserver: the owner session agent does not mint service auth tokens")
}

func (a *ownerSessionAgent) ListRecords(ctx context.Context, did, collection string, limit int) ([]coreapi.Record, error) {
	pds, err := a.remote(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := syntax.ParseDID(did)
	if err != nil {
		return nil, err
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	page, err := pds.ListRecords(ctx, pdsclient.ListRecordsRequest{Repo: repo, Collection: nsid, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]coreapi.Record, 0, len(page.Records))
	for _, record := range page.Records {
		var value map[string]any
		if err := json.Unmarshal(record.Value, &value); err != nil {
			continue
		}
		out = append(out, coreapi.Record{URI: record.URI.String(), CID: record.CID.String(), Value: value})
	}
	return out, nil
}

func (a *ownerSessionAgent) CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (coreapi.RecordRef, error) {
	pds, err := a.remote(ctx)
	if err != nil {
		return coreapi.RecordRef{}, err
	}
	repo, err := syntax.ParseDID(did)
	if err != nil {
		return coreapi.RecordRef{}, err
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return coreapi.RecordRef{}, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return coreapi.RecordRef{}, err
	}
	result, err := pds.CreateRecord(ctx, pdsclient.RepoPath{Repo: repo, Collection: nsid, Rkey: syntax.RecordKey(rkey)}, raw, pdsclient.WriteOptions{})
	if err != nil {
		return coreapi.RecordRef{}, err
	}
	return coreapi.RecordRef{URI: result.URI.String(), CID: result.CID.String()}, nil
}

func (a *ownerSessionAgent) ApplyWrites(ctx context.Context, did string, writes []coreapi.Write) error {
	pds, err := a.remote(ctx)
	if err != nil {
		return err
	}
	repo, err := syntax.ParseDID(did)
	if err != nil {
		return err
	}
	for _, write := range writes {
		nsid, err := syntax.ParseNSID(write.Collection)
		if err != nil {
			return err
		}
		path := pdsclient.RepoPath{Repo: repo, Collection: nsid, Rkey: syntax.RecordKey(write.Rkey)}
		switch write.Action {
		case "delete":
			if err := pds.DeleteRecord(ctx, path, pdsclient.WriteOptions{}); err != nil {
				return err
			}
		case "create", "update":
			raw, err := json.Marshal(write.Record)
			if err != nil {
				return err
			}
			if write.Action == "create" {
				_, err = pds.CreateRecord(ctx, path, raw, pdsclient.WriteOptions{})
			} else {
				_, err = pds.PutRecord(ctx, path, raw, pdsclient.WriteOptions{})
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("sshserver: unknown write action %q", write.Action)
		}
	}
	return nil
}

func ownerSessionsFrom(sessions map[string]OwnerSession) map[string]OwnerSession {
	if len(sessions) == 0 {
		return nil
	}
	out := make(map[string]OwnerSession, len(sessions))
	for did, session := range sessions {
		if strings.TrimSpace(session.Service) == "" {
			continue
		}
		out[did] = session
	}
	return out
}
