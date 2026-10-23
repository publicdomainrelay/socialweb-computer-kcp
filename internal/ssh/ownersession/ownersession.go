package ownersession

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const DefaultListLimit = 50

var ErrNoSession = errors.New("sshserver: no account session is configured")

type Session = sshtransport.OwnerSession

type Repo interface {
	atprotorepo.RecordReader

	atprotorepo.RecordWriter
}

type Open func(did string, session Session) (Repo, error)

type Config struct {
	Sessions map[string]Session
}

type Deps struct {
	Open Open
}

type Owners struct {
	sessions map[string]Session

	open Open

	mu sync.Mutex

	repos map[string]Repo
}

var (
	_ sshtransport.Authenticator  = (*Owners)(nil)
	_ sshtransport.AgentProvider  = (*Owners)(nil)
	_ gatewayhttp.WebAccountAgent = (*agent)(nil)
	_ gatewayhttp.WebAccountAgent = Unavailable{}
)

func New(cfg Config, deps Deps) *Owners {
	sessions := map[string]Session{}
	for did, session := range cfg.Sessions {
		if strings.TrimSpace(session.Service) == "" {
			continue
		}
		sessions[did] = session
	}
	return &Owners{sessions: sessions, open: deps.Open, repos: map[string]Repo{}}
}

func (o *Owners) Len() int {
	return len(o.sessions)
}

func (o *Owners) Authenticate(r *http.Request) (*gatewayhttp.WebCaller, error) {
	if r == nil {
		return nil, nil
	}
	identifier, password, ok := r.BasicAuth()
	if !ok {
		return nil, nil
	}
	for did, session := range o.sessions {
		sameIdentifier := subtle.ConstantTimeCompare([]byte(session.Identifier), []byte(identifier)) == 1
		samePassword := subtle.ConstantTimeCompare([]byte(session.Password), []byte(password)) == 1
		if !sameIdentifier || !samePassword {
			continue
		}
		return &gatewayhttp.WebCaller{WebUser: gatewayhttp.WebUser{DID: did, Handle: session.Identifier, PDS: session.Service}}, nil
	}
	return nil, nil
}

func (o *Owners) AgentFor(caller *gatewayhttp.WebCaller) gatewayhttp.WebAccountAgent {
	if caller == nil {
		return Unavailable{}
	}
	if _, ok := o.sessions[caller.DID]; !ok {
		return Unavailable{}
	}
	return &agent{owners: o, did: caller.DID}
}

func (o *Owners) repo(did string) (Repo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if repo := o.repos[did]; repo != nil {
		return repo, nil
	}
	session, ok := o.sessions[did]
	if !ok || o.open == nil {
		return nil, ErrNoSession
	}
	repo, err := o.open(did, session)
	if err != nil {
		return nil, err
	}
	o.repos[did] = repo
	return repo, nil
}

type agent struct {
	owners *Owners

	did string
}

func (a *agent) bound(did string) (Repo, error) {
	if did != a.did {
		return nil, fmt.Errorf("sshserver: the owner session of %s cannot write the repo %s", a.did, did)
	}
	return a.owners.repo(a.did)
}

func (a *agent) GetServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	return "", errors.New("sshserver: the owner session agent does not mint service auth tokens")
}

func (a *agent) ListRecords(ctx context.Context, did, collection string, limit int) ([]gatewayhttp.Record, error) {
	repo, err := a.owners.repo(a.did)
	if err != nil {
		return nil, err
	}
	page, err := repo.ListRecords(ctx, did, collection, atprotorepo.ListOptions{Limit: opt.Positive(limit, DefaultListLimit)})
	if err != nil {
		return nil, err
	}
	out := make([]gatewayhttp.Record, 0, len(page.Records))
	for _, record := range page.Records {
		var value map[string]any
		if err := json.Unmarshal(record.Value, &value); err != nil {
			continue
		}
		out = append(out, gatewayhttp.Record{URI: record.URI, CID: record.CID, Value: value})
	}
	return out, nil
}

func (a *agent) CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (gatewayhttp.RecordRef, error) {
	repo, err := a.bound(did)
	if err != nil {
		return gatewayhttp.RecordRef{}, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return gatewayhttp.RecordRef{}, err
	}
	ref, err := repo.CreateRecord(ctx, collection, rkey, raw)
	if err != nil {
		return gatewayhttp.RecordRef{}, err
	}
	return gatewayhttp.RecordRef{URI: ref.URI, CID: ref.CID}, nil
}

func (a *agent) ApplyWrites(ctx context.Context, did string, writes []gatewayhttp.Write) error {
	repo, err := a.bound(did)
	if err != nil {
		return err
	}
	for _, write := range writes {
		switch write.Action {
		case "delete":
			if err := repo.DeleteRecord(ctx, write.Collection, write.Rkey); err != nil {
				return err
			}
		case "create", "update":
			raw, err := json.Marshal(write.Record)
			if err != nil {
				return err
			}
			if write.Action == "create" {
				_, err = repo.CreateRecord(ctx, write.Collection, write.Rkey, raw)
			} else {
				_, err = repo.PutRecord(ctx, write.Collection, write.Rkey, raw)
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

type Unavailable struct{}

func (Unavailable) GetServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	return "", ErrNoSession
}

func (Unavailable) ListRecords(ctx context.Context, did, collection string, limit int) ([]gatewayhttp.Record, error) {
	return nil, ErrNoSession
}

func (Unavailable) CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (gatewayhttp.RecordRef, error) {
	return gatewayhttp.RecordRef{}, ErrNoSession
}

func (Unavailable) ApplyWrites(ctx context.Context, did string, writes []gatewayhttp.Write) error {
	return ErrNoSession
}
