package main

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

type associationVerdict struct {
	identlink.Result
	Log []map[string]any `json:"log,omitempty"`
}

func runInternalAssociation(ctx context.Context, a *app, _ clicfg.Values, rest []string) int {
	if len(rest) != 1 {
		return a.fail(exitRefused, "internal association: want <did>")
	}
	var out associationVerdict
	resolver := trust.NewResolver(trust.ResolverOptions{Log: func(level, msg string, meta map[string]any) {
		out.Log = append(out.Log, map[string]any{"level": level, "msg": msg, "meta": meta})
	}})
	walked, err := resolver.Walk(ctx, rest[0], trust.Association{})
	if err != nil {
		return a.fail(exitFailed, "internal association: %v", err)
	}
	out.Result = walked
	a.printJSON(out)
	if len(walked.Ancestors) == 0 || len(walked.Roots) == 0 {
		return exitFailed
	}
	return exitOK
}
