package guestagent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	maxRequesterDID = 2048

	refetchTimeout = 10 * time.Minute

	GuestRequesterStopTimeout = 2 * time.Minute

	secretsRefetchEnv = "SECRETS_REFETCH"
)

var requesterDIDRE = regexp.MustCompile(`^did:plc:[a-z2-7]{24}$`)

type requesterDIDRecord struct {
	Type string `json:"$type"`

	DID string `json:"did"`

	CreatedAt string `json:"createdAt"`
}

func (a *Agent) requesterDIDPath() string { return filepath.Join(a.cfg.StateDir, RequesterDIDFile) }

func (a *Agent) bundleDir() string { return filepath.Dir(a.cfg.Association.RefFile) }

type sharedFile struct {
	src, name string
}

func (a *Agent) sharedFiles() []sharedFile {
	dir := a.bundleDir()
	return []sharedFile{
		{src: filepath.Join(dir, AssociationRequestFile), name: AssociationRequestFile},
		{src: filepath.Join(dir, RequesterAtircEnvFile), name: RequesterAtircEnvFile},
		{src: a.cfg.Association.RefFile, name: AssociationRefFile},
	}
}

func (a *Agent) prepareSharedDir() (int, bool) {
	if out, err := exec.Command("groupadd", "-f", RequesterGroup).CombinedOutput(); err != nil {
		a.log.Warn("the guest requester group cannot be made", "group", RequesterGroup, "err", err.Error(), "output", tail(string(out), 256))
	}
	group, err := user.LookupGroup(RequesterGroup)
	if err != nil {
		a.log.Warn("the guest requester group is missing; only a root requester can hand over its DID", "group", RequesterGroup, "err", err.Error())
		return 0, false
	}
	gid, err := strconv.Atoi(group.Gid)
	if err == nil {
		err = os.MkdirAll(RequesterDir, 0o770)
	}
	if err == nil {
		err = os.Chown(RequesterDir, 0, gid)
	}
	if err == nil {
		err = os.Chmod(RequesterDir, 0o770|os.ModeSetgid)
	}
	if err != nil {
		a.log.Warn("the guest requester dir cannot be made", "dir", RequesterDir, "err", err.Error())
		return 0, false
	}
	return gid, true
}

func (a *Agent) mirrorShared(gid int) {
	envReady := false
	for _, f := range a.sharedFiles() {
		raw, err := os.ReadFile(f.src)
		switch {
		case f.name == RequesterAtircEnvFile:
			envReady = err == nil && len(raw) > 0
			if !envReady {
				continue
			}
		case f.name == AssociationRefFile:
			if err != nil || !envReady {
				raw = nil
			}
		case err != nil:
			continue
		}
		dst := filepath.Join(RequesterDir, f.name)
		if current, err := os.ReadFile(dst); err == nil && string(current) == string(raw) {
			continue
		}
		if f.name == AssociationRefFile && len(raw) == 0 {
			if _, err := os.Stat(dst); err == nil {
				continue
			}
		}
		part := dst + ".part"
		if err := os.WriteFile(part, raw, 0o640); err != nil {
			a.log.Warn("mirroring a guest requester file failed", "file", dst, "err", err.Error())
			continue
		}
		if err := os.Chown(part, 0, gid); err != nil {
			a.log.Warn("mirroring a guest requester file failed", "file", dst, "err", err.Error())
			_ = os.Remove(part)
			continue
		}
		if err := os.Rename(part, dst); err != nil {
			a.log.Warn("mirroring a guest requester file failed", "file", dst, "err", err.Error())
			continue
		}
		a.log.Info("guest_requester_file_mirrored", "file", dst, "bytes", len(raw))
	}
}

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
	gid, shared := a.prepareSharedDir()
	if shared {
		if err := os.MkdirAll(a.bundleDir(), 0o700); err != nil {
			a.log.Warn("the bundle dir cannot be made", "dir", a.bundleDir(), "err", err.Error())
		}
		for _, dir := range []string{RequesterDir, a.bundleDir()} {
			if err := watcher.Add(dir); err != nil {
				a.log.Warn("the guest requester watcher cannot watch a dir", "dir", dir, "err", err.Error())
			}
		}
		a.mirrorShared(gid)
	}
	a.mu.Lock()
	a.sharedGID, a.shared = gid, shared
	a.mu.Unlock()
	a.log.Info("the guest reports the DID of the requester it runs", "files", []string{a.requesterDIDPath(), filepath.Join(RequesterDir, RequesterDIDFile)},
		"refFile", a.cfg.Association.RefFile, "shared", shared)
	a.reportRequesterDID(ctx, a.requesterDIDPath())
	if shared {
		a.reportRequesterDID(ctx, filepath.Join(RequesterDir, RequesterDIDFile))
	}
	sources := map[string]bool{}
	for _, f := range a.sharedFiles() {
		sources[filepath.Clean(f.src)] = true
	}
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
			if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
				continue
			}
			name := filepath.Clean(ev.Name)
			switch {
			case filepath.Base(name) == RequesterDIDFile && (filepath.Dir(name) == filepath.Clean(a.cfg.StateDir) || (shared && filepath.Dir(name) == RequesterDir)):
				a.reportRequesterDID(ctx, name)
			case shared && sources[name]:
				a.mirrorShared(gid)
			}
		}
	}
}

func (a *Agent) reportRequesterDID(ctx context.Context, path string) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > maxRequesterDID {
		return
	}
	did := strings.TrimSpace(string(raw))
	if !requesterDIDRE.MatchString(did) {
		a.log.Warn("the requester DID file holds no did:plc; not reported", "file", path)
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
	a.mu.Lock()
	gid, shared := a.sharedGID, a.shared
	a.mu.Unlock()
	if shared {
		a.mirrorShared(gid)
	}
}

var requesterNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type r2StartRecord struct {
	Name string `json:"name"`

	Agent bool `json:"agent"`

	Hold bool `json:"hold"`
}

type r2ReleasedRecord struct {
	Type string `json:"$type"`

	Ok bool `json:"ok"`

	Message string `json:"message,omitempty"`

	CreatedAt string `json:"createdAt"`
}

func flag(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

func (a *Agent) startGuestRequester(seq int64, raw []byte) {
	a.mu.Lock()
	gid, shared := a.sharedGID, a.shared
	a.mu.Unlock()
	var start r2StartRecord
	switch {
	case a.cfg.Association == nil || !shared:
		a.log.Warn("guest_r2_start", "seq", seq, "err", "this guest runs no guest requester")
		return
	case json.Unmarshal(raw, &start) != nil || !requesterNameRE.MatchString(start.Name):
		a.log.Warn("guest_r2_start", "seq", seq, "err", "the r2Start names no DNS label")
		return
	}
	env := "R2_NAME=" + start.Name + "\nR2_AGENT=" + flag(start.Agent) + "\nR2_HOLD=" + flag(start.Hold) + "\n"
	path := filepath.Join(RequesterDir, RequesterStartEnvFile)
	part := path + ".part"
	if err := os.WriteFile(part, []byte(env), 0o640); err != nil {
		a.log.Warn("guest_r2_start", "seq", seq, "err", err.Error())
		return
	}
	if err := os.Chown(part, 0, gid); err != nil {
		_ = os.Remove(part)
		a.log.Warn("guest_r2_start", "seq", seq, "err", err.Error())
		return
	}
	if err := os.Rename(part, path); err != nil {
		a.log.Warn("guest_r2_start", "seq", seq, "err", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, agentStartTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "start", "--no-block", RequesterUnit).CombinedOutput()
	if err != nil {
		a.log.Warn("guest_r2_start", "seq", seq, "unit", RequesterUnit, "err", err.Error(), "output", tail(string(out), 256))
		return
	}
	a.log.Info("guest_r2_start", "seq", seq, "unit", RequesterUnit, "name", start.Name, "agent", start.Agent, "hold", start.Hold)
}

func (a *Agent) releaseGuestRequester(seq int64) {
	if a.cfg.Association == nil {
		return
	}
	stopCtx, cancelStop := context.WithTimeout(a.ctx, GuestRequesterStopTimeout)
	out, err := exec.CommandContext(stopCtx, "systemctl", "stop", RequesterUnit).CombinedOutput()
	cancelStop()
	if err != nil {
		a.log.Warn("guest_r2_release", "seq", seq, "step", "stop "+RequesterUnit, "err", err.Error(), "output", tail(string(out), 256))
	}
	record := r2ReleasedRecord{Type: R2ReleasedNSID, Ok: true, CreatedAt: createdAt()}
	if out, err := exec.CommandContext(a.ctx, "systemctl", "start", RequesterReleaseUnit).CombinedOutput(); err != nil {
		record.Ok, record.Message = false, tail("releasing the guest requester's contracts: "+err.Error()+": "+string(out), 500)
	}
	a.log.Info("guest_r2_release", "seq", seq, "ok", record.Ok, "message", record.Message)
	if err := a.poster.Post(a.ctx, record.Type, "", record); err != nil {
		a.log.Warn("guest_r2_release", "seq", seq, "step", "post r2Released", "err", err.Error())
	}
}
