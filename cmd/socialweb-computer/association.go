package main

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

type associationVerdict struct {
	Did      string           `json:"did"`
	Service  string           `json:"service"`
	Operator string           `json:"operator,omitempty"`
	Found    bool             `json:"found"`
	Log      []map[string]any `json:"log,omitempty"`
}

func runInternalAssociation(ctx context.Context, a *app, _ clicfg.Values, rest []string) int {
	if len(rest) < 1 || len(rest) > 2 {
		return a.fail(exitRefused, "internal association: want <did> [requester_associate|bidder_associate]")
	}
	out := associationVerdict{Did: rest[0], Service: trust.RequesterAssociate}
	if len(rest) == 2 {
		out.Service = rest[1]
	}
	resolver := trust.NewResolver(trust.ResolverOptions{Log: func(level, msg string, meta map[string]any) {
		out.Log = append(out.Log, map[string]any{"level": level, "msg": msg, "meta": meta})
	}})
	operator, found, err := resolver.ResolveOperatorDidFor(ctx, out.Did, out.Service)
	if err != nil {
		return a.fail(exitFailed, "internal association: %v", err)
	}
	out.Operator, out.Found = operator, found
	a.printJSON(out)
	if !found {
		return exitFailed
	}
	return exitOK
}
