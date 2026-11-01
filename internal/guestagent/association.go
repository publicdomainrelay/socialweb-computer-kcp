package guestagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	maxRequesterDID = 2048

	refetchTimeout = 10 * time.Minute

	secretsRefetchEnv = "SECRETS_REFETCH"
)

var requesterDIDRE = regexp.MustCompile(`^did:(plc:[a-z2-7]{24}|web:[a-zA-Z0-9._%-]+(:[a-zA-Z0-9._%-]+)*)$`)

type requesterDIDRecord struct {
	Type string `json:"$type"`

	DID string `json:"did"`

	CreatedAt string `json:"createdAt"`
}

func (a *Agent) requesterDIDPath() string { return filepath.Join(a.cfg.StateDir, RequesterDIDFile) }

func (a *Agent) runAssociation(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		a.log.Error("the requester DID watcher cannot start", "err", err.Error())
		return
	}
	defer watcher.Close()
	if err := watcher.Add(a.cfg.StateDir); err != nil {
		a.log.Error("the requester DID watcher cannot watch the state dir", "dir", a.cfg.StateDir, "err", err.Error())
		return
	}
	a.log.Info("the guest reports the DID of the requester it runs", "file", a.requesterDIDPath(), "refFile", a.cfg.Association.RefFile)
	a.reportRequesterDID(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case err, open := <-watcher.Errors:
			if !open {
				return
			}
			a.log.Warn("the requester DID watcher reported an error", "err", err.Error())
		case ev, open := <-watcher.Events:
			if !open {
				return
			}
			if (ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write)) && filepath.Base(ev.Name) == RequesterDIDFile {
				a.reportRequesterDID(ctx)
			}
		}
	}
}

func (a *Agent) reportRequesterDID(ctx context.Context) {
	raw, err := os.ReadFile(a.requesterDIDPath())
	if err != nil || len(raw) > maxRequesterDID {
		return
	}
	did := strings.TrimSpace(string(raw))
	if !requesterDIDRE.MatchString(did) {
		a.log.Warn("the requester DID file holds no did:plc or did:web; not reported", "file", a.requesterDIDPath())
		return
	}
	a.mu.Lock()
	if a.reportedDID != "" {
		previous := a.reportedDID
		a.mu.Unlock()
		if previous != did {
			a.log.Warn("the guest already reported another requester DID; the first one stands", "reported", previous, "did", did)
		}
		return
	}
	a.reportedDID = did
	a.mu.Unlock()
	record := requesterDIDRecord{Type: RequesterDIDNSID, DID: did, CreatedAt: createdAt()}
	a.log.Info("the guest reports its requester DID", "did", did)
	a.wg.Go(func() {
		if err := a.poster.Post(ctx, record.Type, "", record); err != nil {
			a.mu.Lock()
			if a.reportedDID == did {
				a.reportedDID = ""
			}
			a.mu.Unlock()
		}
	})
}

func (a *Agent) refetchSecrets(seq int64) {
	if s := a.cfg.Secrets; s != nil && !s.GetSecrets {
		a.mu.Lock()
		a.requested = false
		a.mu.Unlock()
		a.log.Info("the requester updated the secrets bundle; the guest asks for the sealed bundle again", "seq", seq)
		a.postRequest(a.ctx)
		return
	}
	if a.cfg.Secrets != nil {
		a.mu.Lock()
		a.requested = false
		a.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(a.ctx, refetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, DefaultSecretsFetchScript)
	cmd.Env = append(os.Environ(), secretsRefetchEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		a.log.Warn("guest_secrets_refetch", "seq", seq, "err", err.Error(), "output", tail(string(out), 256))
		return
	}
	a.log.Info("guest_secrets_refetch", "seq", seq, "output", tail(string(out), 256))
}
