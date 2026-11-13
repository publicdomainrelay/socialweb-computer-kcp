package main

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/files"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/plcsubmit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
	pdssvc "github.com/johnandersen777/socialweb-computer-kcp/internal/services/pds"
)

var identityAgentOfOptions = []string{"pds-state-dir", "did", "plc-url", "drop-borrowed-handle"}

var identityTombstoneOptions = []string{"pds-state-dir", "rotation-key", "plc-url"}

type agentOfReport struct {
	DID         string   `json:"did"`
	AlsoKnownAs []string `json:"alsoKnownAs"`
	Dropped     []string `json:"dropped,omitempty"`
}

func runIdentityAgentOf(add bool) func(context.Context, *app, clicfg.Values, []string) int {
	verb := "identity agent-of remove"
	if add {
		verb = "identity agent-of add"
	}
	return func(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
		if len(rest) != 1 || !identlink.IsDID(rest[0]) {
			return a.fail(exitRefused, "%s <principal-did>: one principal DID", verb)
		}
		did, err := syntax.ParseDID(strings.TrimSpace(vals.String("did")))
		if err != nil {
			return a.fail(exitRefused, "%s: --did: the account DID of the local PDS: %v", verb, err)
		}
		stateDir := strings.TrimSpace(vals.String("pds-state-dir"))
		if stateDir == "" {
			return a.fail(exitRefused, "%s: --pds-state-dir: the PDS state dir that holds %s", verb, pdssvc.RotationKeyFile)
		}
		key, err := files.RotationKey(stateDir, filepath.Join(stateDir, pdssvc.RotationKeyFile))
		if err != nil {
			return a.fail(exitRefused, "%s: reading the PLC rotation key: %v", verb, err)
		}
		submitter, err := plcsubmit.New(vals.String("plc-url"), key, nil)
		if err != nil {
			return a.fail(exitRefused, "%s: %v", verb, err)
		}
		entry := identlink.AgentOfPrefix + rest[0]
		var addList, removeList []string
		if add {
			addList = []string{entry}
		} else {
			removeList = []string{entry}
		}
		report := agentOfReport{DID: did.String()}
		if vals.Bool("drop-borrowed-handle") {
			dropped, err := borrowedHandles(ctx, vals.String("plc-url"), did)
			if err != nil {
				return a.fail(exitFailed, "%s: %v", verb, err)
			}
			report.Dropped = dropped
			removeList = append(removeList, dropped...)
		}
		report.AlsoKnownAs, err = submitter.UpdateAlsoKnownAs(ctx, did, addList, removeList)
		if err != nil {
			return a.fail(exitFailed, "%s: %v", verb, err)
		}
		a.printJSON(report)
		return exitOK
	}
}

func borrowedHandles(ctx context.Context, plcURL string, did syntax.DID) ([]string, error) {
	dir := &identity.BaseDirectory{}
	doc, err := identlink.PLCDirectory(plcURL, nil).LookupDID(ctx, did)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range doc.AlsoKnownAs {
		raw, ok := strings.CutPrefix(entry, "at://")
		if !ok {
			continue
		}
		handle, err := syntax.ParseHandle(raw)
		if err != nil {
			out = append(out, entry)
			continue
		}
		owner, err := dir.ResolveHandle(ctx, handle)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || owner != did {
			out = append(out, entry)
		}
	}
	return out, nil
}

func runIdentityTombstone(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	const verb = "identity tombstone"
	if len(rest) != 1 {
		return a.fail(exitRefused, "%s <did:plc>: one DID", verb)
	}
	did, err := syntax.ParseDID(strings.TrimSpace(rest[0]))
	if err != nil || did.Method() != "plc" {
		return a.fail(exitRefused, "%s: %q is not a did:plc", verb, rest[0])
	}
	path := strings.TrimSpace(vals.String("rotation-key"))
	if dir := strings.TrimSpace(vals.String("pds-state-dir")); path == "" && dir != "" {
		path = filepath.Join(dir, pdssvc.RotationKeyFile)
	}
	if path == "" {
		return a.fail(exitRefused, "%s: --rotation-key <file> or --pds-state-dir <dir>: the PLC rotation key the DID was created with", verb)
	}
	key, err := files.RotationKey("", path)
	if err != nil {
		return a.fail(exitRefused, "%s: reading the PLC rotation key: %v", verb, err)
	}
	submitter, err := plcsubmit.New(vals.String("plc-url"), key, nil)
	if err != nil {
		return a.fail(exitRefused, "%s: %v", verb, err)
	}
	if err := submitter.Tombstone(ctx, did); err != nil {
		return a.fail(exitFailed, "%s: %v", verb, err)
	}
	a.printJSON(map[string]any{"did": did.String(), "tombstoned": true})
	return exitOK
}
