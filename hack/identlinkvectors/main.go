package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

type want struct {
	Ancestors []identlink.Ancestor `json:"ancestors"`
	Roots     []string             `json:"roots"`
	ExpiresAt *time.Time           `json:"expiresAt"`
	Truncated bool                 `json:"truncated"`
	Errors    int                  `json:"errors"`
}

type vectors struct {
	At      time.Time                  `json:"now"`
	DIDs    map[string]json.RawMessage `json:"dids"`
	Records map[string]json.RawMessage `json:"records"`
	Held    map[string]json.RawMessage `json:"held"`
	Cases   []struct {
		Name    string            `json:"name"`
		Agent   string            `json:"agent"`
		Options identlink.Options `json:"options"`
		Want    want              `json:"want"`
	} `json:"cases"`
	Market []struct {
		Name        string              `json:"name"`
		Contract    identlink.StrongRef `json:"contract"`
		Authorities []string            `json:"authorities"`
		Want        json.RawMessage     `json:"want"`
	} `json:"market"`
}

func (v *vectors) Forget(string) {}

func marketCases(v *vectors) int {
	resolver := trust.NewResolver(trust.ResolverOptions{Source: v})
	failed := 0
	for _, c := range v.Market {
		held, err := resolver.ContractHeld(context.Background(), atproto.StrongRef{URI: c.Contract.URI, CID: c.Contract.CID}, c.Authorities)
		got := "false"
		switch {
		case err != nil:
			got = `"error"`
		case held:
			got = "true"
		}
		if got == strings.TrimSpace(string(c.Want)) {
			continue
		}
		failed++
		fmt.Fprintf(os.Stderr, "identlink market vector %q: got %s, want %s (err %v)\n", c.Name, got, c.Want, err)
	}
	return failed
}

func entry(m map[string]json.RawMessage, key string, out any) (bool, error) {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return false, nil
	}
	if string(raw) == `"error"` {
		return false, errors.New(key + ": error")
	}
	return true, json.Unmarshal(raw, out)
}

func (v *vectors) ResolveDID(_ context.Context, did string) (*identlink.Doc, error) {
	var doc identlink.Doc
	ok, err := entry(v.DIDs, did, &doc)
	if !ok {
		return nil, err
	}
	return &doc, err
}

func (v *vectors) GetRecord(ctx context.Context, repo, collection, rkey string) (map[string]any, error) {
	if doc, err := v.ResolveDID(ctx, repo); doc == nil {
		return nil, err
	}
	var value map[string]any
	ok, err := entry(v.Records, "at://"+repo+"/"+collection+"/"+rkey, &value)
	if !ok {
		return nil, err
	}
	return value, err
}

func (v *vectors) ContractHeld(_ context.Context, contract identlink.StrongRef, authorities []string) (bool, error) {
	var held bool
	if _, err := entry(v.Held, contract.URI, &held); err != nil {
		return false, err
	}
	repo, _, _ := strings.Cut(strings.TrimPrefix(contract.URI, "at://"), "/")
	return held && slices.Contains(authorities, repo), nil
}

func (v *vectors) Now() time.Time {
	return v.At
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

func equal(got identlink.Result, w want) bool {
	if !slices.Equal(got.Roots, w.Roots) || got.Truncated != w.Truncated || !sameTime(got.ExpiresAt, w.ExpiresAt) ||
		len(got.Ancestors) != len(w.Ancestors) {
		return false
	}
	for i, a := range got.Ancestors {
		if a.DID != w.Ancestors[i].DID || a.Depth != w.Ancestors[i].Depth || !sameTime(a.ExpiresAt, w.Ancestors[i].ExpiresAt) {
			return false
		}
	}
	return true
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: identlinkvectors <vectors.json>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var v vectors
	if err := json.Unmarshal(data, &v); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	failed := 0
	for _, c := range v.Cases {
		got := identlink.Walk(context.Background(), &v, c.Agent, c.Options)
		if equal(got, c.Want) && len(got.Errors) == c.Want.Errors {
			continue
		}
		failed++
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(c.Want)
		fmt.Fprintf(os.Stderr, "identlink vector %q: got %s, want %s\n", c.Name, gotJSON, wantJSON)
	}
	marketFailed := marketCases(&v)
	if failed+marketFailed > 0 || len(v.Cases) == 0 || len(v.Market) == 0 {
		fmt.Fprintf(os.Stderr, "identlink vectors: %d of %d walk and %d of %d market cases failed\n", failed, len(v.Cases), marketFailed, len(v.Market))
		os.Exit(1)
	}
	fmt.Printf("identlink vectors: %d walk cases and %d market cases ok\n", len(v.Cases), len(v.Market))
}
