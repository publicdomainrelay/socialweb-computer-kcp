package plcsubmit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
)

const plcMaxResponseBytes = 1 << 20

var _ atpds.PLCSubmitter = (*HTTPPLCSubmitter)(nil)

type HTTPPLCSubmitter struct {
	baseURL     string
	rotationKey atcrypto.PrivateKey
	rotationDID string
	client      *http.Client
}

func New(baseURL string, rotationKey atcrypto.PrivateKey, client *http.Client) (*HTTPPLCSubmitter, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("pds: invalid PLC directory URL %q", baseURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("pds: the PLC directory URL must not have a query or fragment")
	}
	if rotationKey == nil {
		return nil, errors.New("pds: the PLC submitter needs a rotation key")
	}
	pub, err := rotationKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("pds: deriving the PLC rotation key: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &HTTPPLCSubmitter{
		baseURL:     baseURL,
		rotationKey: rotationKey,
		rotationDID: pub.DIDKey(),
		client:      client,
	}, nil
}

func (p *HTTPPLCSubmitter) RotationDID() string {
	return p.rotationDID
}

func (p *HTTPPLCSubmitter) SubmitGenesis(ctx context.Context, req atpds.PLCGenesisRequest) (atpds.PLCGenesisResult, error) {
	if req.SigningKey == "" {
		return atpds.PLCGenesisResult{}, errors.New("pds: PLC genesis needs the account signing key")
	}
	if req.Handle == "" || req.Service == "" {
		return atpds.PLCGenesisResult{}, errors.New("pds: PLC genesis needs a handle and a service endpoint")
	}
	if req.RotationKey != "" && req.RotationKey != p.rotationDID {
		return atpds.PLCGenesisResult{}, fmt.Errorf("pds: PLC genesis rotation key does not match the submitter key")
	}
	rotationKeys := []string{p.rotationDID}
	if req.RecoveryKey != "" {
		if _, err := atcrypto.ParsePublicDIDKey(req.RecoveryKey); err != nil {
			return atpds.PLCGenesisResult{}, fmt.Errorf("pds: the PLC genesis recovery key is not a did:key: %w", err)
		}
		if req.RecoveryKey != p.rotationDID {
			rotationKeys = []string{req.RecoveryKey, p.rotationDID}
		}
	}
	verification := req.VerificationMethods
	if len(verification) == 0 {
		verification = map[string]string{"atproto": req.SigningKey}
	}
	didRaw, op, err := plc.CreateGenesis(plc.GenesisOptions{
		RotationKeys:        rotationKeys,
		VerificationMethods: verification,
		AlsoKnownAs:         append([]string(nil), req.AlsoKnownAs...),
		Services: map[string]plc.Service{
			"atproto_pds": {Type: "AtprotoPersonalDataServer", Endpoint: req.Service},
		},
		Signer: p.rotationKey,
	})
	if err != nil {
		return atpds.PLCGenesisResult{}, fmt.Errorf("pds: creating the PLC genesis operation: %w", err)
	}
	did, err := syntax.ParseDID(didRaw)
	if err != nil {
		return atpds.PLCGenesisResult{}, fmt.Errorf("pds: the PLC genesis DID is invalid: %w", err)
	}
	if err := p.submit(ctx, did, op.SignedObject()); err != nil {
		return atpds.PLCGenesisResult{}, err
	}
	raw, err := json.Marshal(op.SignedObject())
	if err != nil {
		return atpds.PLCGenesisResult{}, fmt.Errorf("pds: encoding the PLC genesis operation: %w", err)
	}
	return atpds.PLCGenesisResult{DID: did, RotationKey: p.rotationDID, Operation: raw}, nil
}

func (p *HTTPPLCSubmitter) head(ctx context.Context, did syntax.DID) (plc.Operation, string, error) {
	var ops []plc.Operation
	if err := p.getJSON(ctx, p.baseURL+"/"+did.String()+"/log", &ops); err != nil {
		return plc.Operation{}, "", err
	}
	if len(ops) == 0 {
		return plc.Operation{}, "", fmt.Errorf("pds: the PLC log for %s is empty", did)
	}
	last := ops[len(ops)-1]
	if last.Type != plc.TypeOperation {
		return plc.Operation{}, "", fmt.Errorf("pds: the PLC operation for %s is not updateable", did)
	}
	if !slices.Contains(last.RotationKeys, p.rotationDID) {
		return plc.Operation{}, "", fmt.Errorf("pds: the rotation key %s is not in the PLC log for %s", p.rotationDID, did)
	}
	prevCID, err := plc.OperationCID(&last)
	if err != nil {
		return plc.Operation{}, "", fmt.Errorf("pds: computing the PLC operation cid: %w", err)
	}
	return last, prevCID, nil
}

func (p *HTTPPLCSubmitter) editAlsoKnownAs(ctx context.Context, did syntax.DID, edit func([]string) []string) (*plc.Operation, string, error) {
	last, prev, err := p.head(ctx, did)
	if err != nil {
		return nil, "", err
	}
	aka := edit(slices.Clone(last.AlsoKnownAs))
	if slices.Equal(aka, last.AlsoKnownAs) {
		return nil, "", nil
	}
	verification := make(map[string]string, len(last.VerificationMethods))
	maps.Copy(verification, last.VerificationMethods)
	services := make(map[string]plc.Service, len(last.Services))
	maps.Copy(services, last.Services)
	op := &plc.Operation{
		Type:                plc.TypeOperation,
		Prev:                &prev,
		RotationKeys:        slices.Clone(last.RotationKeys),
		VerificationMethods: verification,
		AlsoKnownAs:         aka,
		Services:            services,
	}
	if err := plc.SignOperation(op, p.rotationKey); err != nil {
		return nil, "", fmt.Errorf("pds: signing the PLC operation: %w", err)
	}
	opCID, err := plc.OperationCID(op)
	if err != nil {
		return nil, "", fmt.Errorf("pds: computing the PLC operation cid: %w", err)
	}
	if err := p.submit(ctx, did, op.SignedObject()); err != nil {
		return nil, "", err
	}
	return op, opCID, nil
}

func (p *HTTPPLCSubmitter) UpdateHandle(ctx context.Context, did syntax.DID, handle string) error {
	_, _, err := p.editAlsoKnownAs(ctx, did, func(aka []string) []string {
		out := []string{"at://" + handle}
		for _, entry := range aka {
			if !strings.HasPrefix(entry, "at://") {
				out = append(out, entry)
			}
		}
		return out
	})
	return err
}

func (p *HTTPPLCSubmitter) UpdateAlsoKnownAs(ctx context.Context, did syntax.DID, add, remove []string) ([]string, error) {
	var want []string
	op, opCID, err := p.editAlsoKnownAs(ctx, did, func(aka []string) []string {
		want = slices.DeleteFunc(aka, func(entry string) bool { return slices.Contains(remove, entry) })
		for _, entry := range add {
			if !slices.Contains(want, entry) {
				want = append(want, entry)
			}
		}
		return want
	})
	if err != nil || op == nil {
		return want, err
	}
	var ops []plc.Operation
	if err := p.getJSON(ctx, p.baseURL+"/"+did.String()+"/log", &ops); err != nil {
		return nil, fmt.Errorf("pds: verifying the PLC alsoKnownAs update: %w", err)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("pds: verifying the PLC alsoKnownAs update: the PLC log for %s is empty", did)
	}
	if headCID, err := plc.OperationCID(&ops[len(ops)-1]); err != nil || headCID != opCID || ops[len(ops)-1].Sig != op.Sig {
		return nil, fmt.Errorf("pds: the PLC alsoKnownAs update for %s is not the head operation", did)
	}
	var doc plc.DIDDocument
	if err := p.getJSON(ctx, p.baseURL+"/"+did.String(), &doc); err != nil {
		return nil, fmt.Errorf("pds: verifying the DID document: %w", err)
	}
	if !slices.Equal(doc.AlsoKnownAs, want) {
		return nil, fmt.Errorf("pds: the DID document of %s has alsoKnownAs %v, not %v", did, doc.AlsoKnownAs, want)
	}
	return want, nil
}

func (p *HTTPPLCSubmitter) UpdateService(ctx context.Context, did syntax.DID, service string) error {
	endpoint, err := plc.NormalizeServiceEndpoint(service)
	if err != nil {
		return fmt.Errorf("pds: the PLC service endpoint is invalid: %w", err)
	}
	var doc plc.DIDDocument
	if err := p.getJSON(ctx, p.baseURL+"/"+did.String(), &doc); err == nil {
		for _, service := range doc.Service {
			if service.ID == "#atproto_pds" && service.ServiceEndpoint == endpoint {
				return nil
			}
		}
	}
	var ops []plc.Operation
	if err := p.getJSON(ctx, p.baseURL+"/"+did.String()+"/log", &ops); err != nil {
		return err
	}
	if len(ops) == 0 {
		return fmt.Errorf("pds: the PLC log for %s is empty", did)
	}
	last := ops[len(ops)-1]
	if last.Type != plc.TypeOperation {
		return fmt.Errorf("pds: the PLC operation for %s is not updateable", did)
	}
	authorized := slices.Contains(last.RotationKeys, p.rotationDID)
	if !authorized {
		return fmt.Errorf("pds: the rotation key %s is not in the PLC log for %s", p.rotationDID, did)
	}
	prevCID, err := plc.OperationCID(&last)
	if err != nil {
		return fmt.Errorf("pds: computing the PLC operation cid: %w", err)
	}
	op, err := plc.CreateServiceUpdate(&last, prevCID, endpoint)
	if err != nil {
		return fmt.Errorf("pds: creating the PLC service update: %w", err)
	}
	if err := plc.SignOperation(op, p.rotationKey); err != nil {
		return fmt.Errorf("pds: signing the PLC operation: %w", err)
	}
	opCID, err := plc.OperationCID(op)
	if err != nil {
		return fmt.Errorf("pds: computing the PLC service update cid: %w", err)
	}
	if err := p.submit(ctx, did, op.SignedObject()); err != nil {
		return err
	}
	return p.verifyServiceUpdate(ctx, did, &last, op, opCID, endpoint)
}

func (p *HTTPPLCSubmitter) Head(ctx context.Context, did syntax.DID) (plc.Operation, error) {
	var ops []plc.Operation
	if err := p.getJSON(ctx, p.baseURL+"/"+did.String()+"/log", &ops); err != nil {
		return plc.Operation{}, err
	}
	if len(ops) == 0 {
		return plc.Operation{}, fmt.Errorf("pds: the PLC log for %s is empty", did)
	}
	return ops[len(ops)-1], nil
}

func (p *HTTPPLCSubmitter) Tombstone(ctx context.Context, did syntax.DID) error {
	last, err := p.Head(ctx, did)
	if err != nil {
		return err
	}
	if last.Type == plc.TypeTombstone {
		return nil
	}
	if !slices.Contains(last.RotationKeys, p.rotationDID) {
		return fmt.Errorf("pds: the rotation key %s is not in the PLC log for %s", p.rotationDID, did)
	}
	prevCID, err := plc.OperationCID(&last)
	if err != nil {
		return fmt.Errorf("pds: computing the PLC operation cid: %w", err)
	}
	op, err := plc.CreateTombstone(prevCID, p.rotationKey)
	if err != nil {
		return fmt.Errorf("pds: creating the PLC tombstone: %w", err)
	}
	if err := p.submit(ctx, did, op.SignedObject()); err != nil {
		return err
	}
	if head, err := p.Head(ctx, did); err != nil || head.Type != plc.TypeTombstone || head.Sig != op.Sig {
		return fmt.Errorf("pds: the PLC tombstone of %s is not the head operation", did)
	}
	return nil
}

func (p *HTTPPLCSubmitter) verifyServiceUpdate(ctx context.Context, did syntax.DID, prev *plc.Operation, expected *plc.Operation, expectedCID string, endpoint string) error {
	var ops []plc.Operation
	if err := p.getJSON(ctx, p.baseURL+"/"+did.String()+"/log", &ops); err != nil {
		return fmt.Errorf("pds: verifying the PLC service update: %w", err)
	}
	if len(ops) == 0 {
		return fmt.Errorf("pds: verifying the PLC service update: the PLC log for %s is empty", did)
	}
	last := &ops[len(ops)-1]
	lastCID, err := plc.OperationCID(last)
	if err != nil {
		return fmt.Errorf("pds: verifying the PLC service update: %w", err)
	}
	if lastCID != expectedCID {
		return fmt.Errorf("pds: the PLC service update for %s is not the head operation", did)
	}
	if err := plc.VerifyServiceOperation(last, prev, endpoint); err != nil {
		return fmt.Errorf("pds: verifying the PLC service update: %w", err)
	}
	if expected.Sig != last.Sig {
		return fmt.Errorf("pds: the PLC service update for %s has a different signature", did)
	}
	var doc plc.DIDDocument
	if err := p.getJSON(ctx, p.baseURL+"/"+did.String(), &doc); err != nil {
		return fmt.Errorf("pds: verifying the DID document: %w", err)
	}
	if err := plc.VerifyServiceDocument(&doc, did.String(), endpoint); err != nil {
		return fmt.Errorf("pds: verifying the DID document: %w", err)
	}
	return nil
}

func (p *HTTPPLCSubmitter) submit(ctx context.Context, did syntax.DID, operation map[string]any) error {
	body, err := json.Marshal(operation)
	if err != nil {
		return fmt.Errorf("pds: encoding the PLC operation: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/"+did.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pds: building the PLC submit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("pds: submitting the PLC operation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, plcMaxResponseBytes))
		return fmt.Errorf("pds: PLC submit returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (p *HTTPPLCSubmitter) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("pds: building the PLC request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("pds: reading the PLC log: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, plcMaxResponseBytes))
		return fmt.Errorf("pds: PLC log returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, plcMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("pds: reading the PLC log: %w", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("pds: decoding the PLC log: %w", err)
	}
	return nil
}
