package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
)

const atircTokenPlaceholder = "ATIRC_TOKEN=\n"

func agentOf(value string) *v1alpha1.ComputeContractAgent {
	switch envFile := strings.TrimSpace(value); envFile {
	case "", "false":
		return nil
	case "true":
		return &v1alpha1.ComputeContractAgent{EnvFile: cloudinit.DefaultAgentEnvFile}
	default:
		return &v1alpha1.ComputeContractAgent{EnvFile: envFile}
	}
}

func withAgentPlaceholder(bundle []byte, envFile string) ([]byte, error) {
	if !strings.HasPrefix(envFile, "/") || strings.Contains(envFile, "/../") {
		return nil, fmt.Errorf("the agent envFile %q is not an absolute path", envFile)
	}
	return withPlaceholder(bundle, envFile, atircTokenPlaceholder)
}

func withPlaceholder(bundle []byte, path, value string) ([]byte, error) {
	entries := []map[string]any{}
	if len(bundle) > 0 {
		if err := json.Unmarshal(bundle, &entries); err != nil {
			return nil, fmt.Errorf("the secrets bundle is not a JSON array of {path, value}: %w", err)
		}
	}
	for _, e := range entries {
		if have, _ := e["path"].(string); have == path {
			return bundle, nil
		}
	}
	entries = append(entries, map[string]any{"path": path, "value": value, "mode": "0600"})
	return json.Marshal(entries)
}

func agentStartSettled(e *v1alpha1.ContractEvent) bool {
	switch e.Status.Phase {
	case v1alpha1.ContractEventSubmitted, v1alpha1.ContractEventFailed:
		return true
	}
	return false
}

func runRequestAgent(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, started, code := a.openRequest(ctx, vals, rest, "agent")
	if r == nil {
		return code
	}
	if started {
		defer r.stop(context.WithoutCancel(ctx), false)
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		return a.fail(exitFailed, "run request agent: %v", err)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run request agent: the contract is %s, not Held: %s", c.Status.Phase, c.Status.Message)
	}
	if c.Spec.Agent == nil {
		return a.fail(exitRefused, "run request agent: the contract %s has no spec.agent; ask for one with run request --agent", c.Name)
	}
	name, err := r.kcp.Signaling().PublishAgentStart(ctx)
	if err != nil {
		return a.fail(exitFailed, "run request agent: creating the agentStart ContractEvent: %v", err)
	}
	r.log("the agentStart ContractEvent is created; the requester publishes it", "contractEvent", name)
	e, err := r.kcp.WaitContractEvent(ctx, name, r.contractTimeout, agentStartSettled)
	switch {
	case e != nil && e.Status.Phase == v1alpha1.ContractEventFailed:
		return a.fail(exitFailed, "run request agent: the requester refused agentStart: %s", e.Status.Message)
	case e != nil && e.Status.Phase == v1alpha1.ContractEventSubmitted:
		r.log("the bidder took agentStart; the guest agent starts the agent unit", "contractEvent", name, "envFile", c.Spec.Agent.EnvFile)
	case e != nil && e.Status.Phase == v1alpha1.ContractEventPublished:
		r.log("agentStart is published in the requester's repo; the bidder reads it from its requester streams", "contractEvent", name,
			"submit", e.Status.Message)
	case err != nil:
		return a.fail(exitFailed, "run request agent: %v", err)
	default:
		return a.fail(exitFailed, "run request agent: the agentStart ContractEvent %s is not published", name)
	}
	return exitOK
}
