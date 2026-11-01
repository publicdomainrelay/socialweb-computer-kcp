package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/fsnotify/fsnotify"

	"k8s.io/apimachinery/pkg/api/meta"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/requester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
)

const maxAssociationRefFile = 4 << 10

func parseAssociationRef(value string) (*commonv1.StrongRef, error) {
	uri, cid, ok := strings.Cut(value, "#")
	if !ok {
		return nil, fmt.Errorf("%q: want at://<authority>/<collection>/<rkey>#<cid>", value)
	}
	ref, err := market.ParseRecordRef(strings.TrimSpace(uri), strings.TrimSpace(cid))
	if err != nil {
		return nil, err
	}
	return &commonv1.StrongRef{URI: ref.URI, CID: ref.CID}, nil
}

func readAssociationRefFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) > maxAssociationRefFile {
		return "", fmt.Errorf("%s holds more than %d bytes", path, maxAssociationRefFile)
	}
	return strings.TrimSpace(string(raw)), nil
}

func associationRefOf(value string) (*commonv1.StrongRef, string, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "", "false":
		return nil, "", nil
	case "true":
		value = guestPaths().ref
	}
	if !strings.HasPrefix(value, "/") {
		ref, err := parseAssociationRef(value)
		return ref, "", err
	}
	content, err := readAssociationRefFile(value)
	if errors.Is(err, os.ErrNotExist) {
		return nil, value, nil
	}
	if err != nil {
		return nil, "", err
	}
	if content == "" {
		return nil, value, nil
	}
	ref, err := parseAssociationRef(content)
	return ref, "", err
}

func (r *requestRun) associationRef(network bool) error {
	value := cmp.Or(r.vals.String("association-ref"), r.associationRefArg)
	if r.guestRequester && strings.TrimSpace(value) == "" {
		value = "true"
	}
	ref, wait, err := associationRefOf(value)
	if err != nil || (ref == nil && wait == "") {
		return err
	}
	switch {
	case network:
		return errors.New("it is for a standalone invocation; a market network composes its child's identity")
	case r.root.Identity.Association == v1alpha1.AssociationSkip:
		return errors.New("it needs association RequireApproval; drop -- --skip-qr")
	}
	r.root.Identity.Association = v1alpha1.AssociationRequireApproval
	if wait != "" {
		r.awaitRefFile = wait
		r.log("the association ref file is empty; this requester reports its DID to the guest agent and waits for the ref", "file", wait,
			"didFile", guestPaths().did)
		return nil
	}
	r.root.Identity.AssociationRef = ref
	r.log("the requester identity bootstraps its association from a pinned record", "uri", ref.URI, "cid", ref.CID)
	return nil
}

func readAssociationRequest(path string) (*v1alpha1.AssociationRequest, error) {
	raw, err := readAssociationRefFile(path)
	if err != nil {
		return nil, err
	}
	var entry struct {
		Authority string `json:"authority"`
		Service   string `json:"service"`
		Contract  string `json:"contract"`
		Nonce     string `json:"nonce"`
	}
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return nil, fmt.Errorf("%s is not the requester's association request: %w", path, err)
	}
	if entry.Service != trust.RequesterAssociate || entry.Authority == "" || entry.Contract == "" || entry.Nonce == "" {
		return nil, fmt.Errorf("%s names no requester_associate authority, contract and nonce", path)
	}
	return &v1alpha1.AssociationRequest{Authority: entry.Authority, Contract: entry.Contract, Nonce: entry.Nonce}, nil
}

func (r *requestRun) guestHandshake(ctx context.Context, obj *v1alpha1.MarketIdentity) error {
	if r.awaitRefFile == "" || obj == nil || strings.TrimSpace(obj.Status.Did) == "" || r.handshakeStarted {
		return nil
	}
	did := strings.TrimSpace(obj.Status.Did)
	if err := market.ValidGuestRequesterDID(did, ""); err != nil {
		return err
	}
	paths := guestPaths()
	dir := filepath.Dir(paths.did)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("the guest agent state dir %s is missing; --association-ref with an empty ref file needs a guest of a contract with spec.association.fromGuest", dir)
	}
	request, err := readAssociationRequest(paths.request)
	if err != nil {
		return err
	}
	if request.Authority == did {
		return fmt.Errorf("the association request names this requester's own DID %s as its authority", did)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := watcher.Add(filepath.Dir(r.awaitRefFile)); err != nil {
		watcher.Close()
		return fmt.Errorf("watching %s: %w", r.awaitRefFile, err)
	}
	root := r.root
	root.Identity.AssociationRequest = request
	if err := r.kcp.ApplyRoot(ctx, root); err != nil {
		watcher.Close()
		return fmt.Errorf("applying the association request to the MarketRequester: %w", err)
	}
	r.handshakeStarted = true
	r.log("the association request record goes into this requester's repo", "did", did, "authority", request.Authority, "contract", request.Contract)
	go r.reportAndAwaitRef(ctx, watcher, root, did, request.Authority, request.Contract)
	return nil
}

func (r *requestRun) reportAndAwaitRef(ctx context.Context, watcher *fsnotify.Watcher, root v1alpha1.MarketRequesterSpec, did, authority, contract string) {
	defer watcher.Close()
	want := "at://" + did + "/" + market.AssociationRequestNSID + "/" + market.ContractScopedRKey(trust.RequesterAssociate, authority, contract)
	if _, err := r.kcp.AwaitRecord(ctx, compose.AssociationRequestRecordName, r.networkTimeout, func(rec *v1alpha1.AtprotoRecord) (bool, error) {
		if rec.Status.Phase == v1alpha1.AtprotoRecordPhaseFailed && rec.Status.ObservedGeneration == rec.Generation {
			return false, fmt.Errorf("the association request record failed: %s", rec.Status.Message)
		}
		return rec.Status.Phase == v1alpha1.AtprotoRecordPhaseApplied && rec.Status.URI == want && rec.Status.Cid != "", nil
	}); err != nil {
		r.a.log.Error("the association request record is not in this requester's repo; the DID is not reported", "err", err.Error())
		return
	}
	didFile := guestPaths().did
	part := didFile + ".part"
	if err := os.WriteFile(part, []byte(did+"\n"), 0o600); err != nil {
		r.a.log.Error("handing the requester DID to the guest agent", "err", err.Error())
		return
	}
	if err := os.Rename(part, didFile); err != nil {
		r.a.log.Error("handing the requester DID to the guest agent", "err", err.Error())
		return
	}
	r.log("the requester DID is handed to the guest agent; waiting for the association ref", "did", did, "request", want, "refFile", r.awaitRefFile)
	try := func() bool {
		content, err := readAssociationRefFile(r.awaitRefFile)
		if err != nil || content == "" {
			return false
		}
		ref, err := parseAssociationRef(content)
		if err != nil {
			r.a.log.Warn("the association ref file holds no ref", "file", r.awaitRefFile, "err", err.Error())
			return false
		}
		next := root
		next.Identity.AssociationRef = ref
		if parent, err := r.atircParent(ctx); err != nil {
			r.a.log.Warn("the guest requester's atirc token is not usable for its own agents", "file", guestPaths().atirc, "err", err.Error())
		} else if parent != nil {
			next.AtircParentRef = parent
		}
		if err := r.kcp.ApplyRoot(ctx, next); err != nil {
			r.a.log.Warn("applying the association ref to the MarketRequester", "err", err.Error())
			return false
		}
		r.log("the association ref arrived in the secrets bundle; the identity verifies it", "uri", ref.URI, "cid", ref.CID)
		return true
	}
	if try() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-watcher.Errors:
			if !open {
				return
			}
		case ev, open := <-watcher.Events:
			if !open {
				return
			}
			if filepath.Clean(ev.Name) == filepath.Clean(r.awaitRefFile) && (ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) || ev.Has(fsnotify.Rename)) && try() {
				return
			}
		}
	}
}

func associationRefRefused(obj *v1alpha1.MarketIdentity) error {
	if obj == nil || obj.Spec.AssociationRef == nil {
		return nil
	}
	c := meta.FindStatusCondition(obj.Status.Conditions, v1alpha1.ConditionAssociated)
	if c == nil || c.ObservedGeneration != obj.Generation || c.Reason != atproto.ReasonAssociationRefInvalid {
		return nil
	}
	return fmt.Errorf("the association record is refused: %s", c.Message)
}

func associationRefHint(obj *v1alpha1.MarketIdentity, did string) string {
	ref := obj.Spec.AssociationRef
	state := "no Associated condition yet"
	if c := meta.FindStatusCondition(obj.Status.Conditions, v1alpha1.ConditionAssociated); c != nil {
		state = c.Reason + ": " + c.Message
	}
	return strings.Join([]string{
		"The requester's DID " + did + " bootstraps its association from " + ref.URI + " (cid " + ref.CID + ").",
		"The record must be in the authority's repo and name " + did + " for requester_associate; " + state,
	}, "\n")
}

func associateOf(value string) (*v1alpha1.ComputeContractAssociation, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "":
		return nil, nil
	case "guest":
		return &v1alpha1.ComputeContractAssociation{FromGuest: true, RefFile: cloudinit.DefaultAssociationRefFile}, nil
	}
	did, err := syntax.ParseDID(value)
	if err != nil || did.String() != value {
		return nil, fmt.Errorf("%q is not a DID", value)
	}
	return &v1alpha1.ComputeContractAssociation{Subject: value, RefFile: cloudinit.DefaultAssociationRefFile}, nil
}

func associationFor(subject string) func(*v1alpha1.ComputeContract) bool {
	return func(c *v1alpha1.ComputeContract) bool {
		if c.Status.Phase == v1alpha1.ComputeContractReleased || c.Status.Phase == v1alpha1.ComputeContractFailed {
			return true
		}
		ref, state := c.Status.Association, c.Status.Contract
		if ref == nil || ref.CID == "" || state == nil || state.Accept == nil {
			return false
		}
		return strings.HasSuffix(ref.URI, "/"+market.AssociationNSID+"/"+requester.ContractAssociationRKey(subject, *state.Accept))
	}
}

func runRequestAssociate(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	association, err := associateOf(vals.String("associate"))
	if err != nil {
		return a.fail(exitRefused, "run request associate: --associate: %v", err)
	}
	if association == nil || association.Subject == "" {
		return a.fail(exitRefused, "run request associate: --associate <did> names the subject requester's repo DID")
	}
	r, started, code := a.openRequest(ctx, vals, rest, "associate")
	if r == nil {
		return code
	}
	if started {
		defer r.stop(context.WithoutCancel(ctx), false)
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		return a.fail(exitFailed, "run request associate: %v", err)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run request associate: the contract is %s, not Held: %s", c.Status.Phase, c.Status.Message)
	}
	if err := r.kcp.Associate(ctx, association.Subject); err != nil {
		return a.fail(exitFailed, "run request associate: setting spec.association: %v", err)
	}
	r.log("spec.association is set; the requester writes the contract-scoped market.association record", "subject", association.Subject)
	c, err = r.kcp.WaitContract(ctx, r.contractTimeout, associationFor(association.Subject), r.progress)
	if err != nil {
		return a.fail(exitFailed, "run request associate: %v", err)
	}
	if !associationFor(association.Subject)(c) || c.Status.Association == nil {
		return a.fail(exitFailed, "run request associate: the contract is %s before the association record was written: %s", c.Status.Phase, c.Status.Message)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run request associate: the contract is %s: %s", c.Status.Phase, c.Status.Message)
	}
	fmt.Fprintln(a.stdout, c.Status.Association.URI+"#"+c.Status.Association.CID)
	return exitOK
}

func (r *requestRun) atircParent(ctx context.Context) (*commonv1.SecretRef, error) {
	atircFile := guestPaths().atirc
	raw, err := os.ReadFile(atircFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[name] = strings.TrimSpace(value)
		}
	}
	token, subhandle, exp := values[requester.AtircTokenEnv], values[requester.AtircSubhandleEnv], values[requester.AtircExpEnv]
	if token == "" || subhandle == "" || exp == "" {
		return nil, fmt.Errorf("%s names no %s, %s and %s", atircFile, requester.AtircTokenEnv, requester.AtircSubhandleEnv, requester.AtircExpEnv)
	}
	ref, err := r.kcp.PutSecret(ctx, request.AtircParentPath, map[string]any{"token": token, "subhandle": subhandle, "exp": exp},
		"the guest requester's atirc token", r.networkTimeout)
	if err != nil {
		return nil, err
	}
	r.log("this requester mints its own agents' atirc tokens below its token", "subhandle", subhandle, "file", atircFile)
	return &ref, nil
}

type guestRequesterPaths struct {
	request, did, ref, atirc string
}

func guestPaths() guestRequesterPaths {
	if fi, err := os.Stat(cloudinit.GuestRequesterDir); err == nil && fi.IsDir() {
		dir := cloudinit.GuestRequesterDir
		return guestRequesterPaths{request: filepath.Join(dir, filepath.Base(cloudinit.DefaultAssociationRequestFile)),
			did: filepath.Join(dir, filepath.Base(cloudinit.GuestRequesterDIDPath)), ref: filepath.Join(dir, filepath.Base(cloudinit.DefaultAssociationRefFile)),
			atirc: filepath.Join(dir, filepath.Base(cloudinit.GuestRequesterAtircEnvFile))}
	}
	return guestRequesterPaths{request: cloudinit.DefaultAssociationRequestFile, did: cloudinit.GuestRequesterDIDPath,
		ref: cloudinit.DefaultAssociationRefFile, atirc: cloudinit.GuestRequesterAtircEnvFile}
}
