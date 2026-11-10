package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
)

const (
	atircEnvPlaceholder = ""

	maxAgentConfigBytes = 2 << 20
)

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

func guestToolsOf(value string) ([]string, error) {
	var out []string
	for _, tool := range splitList(value) {
		if tool != v1alpha1.GuestToolSocialwebComputer {
			return nil, fmt.Errorf("%q: want %s", tool, v1alpha1.GuestToolSocialwebComputer)
		}
		if !slices.Contains(out, tool) {
			out = append(out, tool)
		}
	}
	return out, nil
}

func withAgentPlaceholder(bundle []byte, envFile string) ([]byte, error) {
	if !strings.HasPrefix(envFile, "/") || strings.Contains(envFile, "/../") {
		return nil, fmt.Errorf("the agent envFile %q is not an absolute path", envFile)
	}
	return withPlaceholder(bundle, envFile, atircEnvPlaceholder)
}

func withAgentConfig(bundle []byte, file string) ([]byte, error) {
	st, err := os.Stat(file)
	switch {
	case err != nil:
		return nil, err
	case !st.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", file)
	case st.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("%s is readable by group or others (mode %o); chmod 600 it", file, st.Mode().Perm())
	case st.Size() == 0 || st.Size() > maxAgentConfigBytes:
		return nil, fmt.Errorf("%s is empty or larger than %d bytes", file, maxAgentConfigBytes)
	}
	text, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	paths, err := bundlePaths(bundle)
	if err != nil {
		return nil, err
	}
	if slices.Contains(paths, cloudinit.AgentConfigFile) {
		return nil, fmt.Errorf("the secrets bundle already names %s", cloudinit.AgentConfigFile)
	}
	return withPlaceholder(bundle, cloudinit.AgentConfigFile, string(text))
}

func bundlePaths(bundle []byte) ([]string, error) {
	if len(bundle) == 0 {
		return nil, nil
	}
	var entries []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(bundle, &entries); err != nil {
		return nil, fmt.Errorf("the secrets bundle is not a JSON array of {path, value}: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out, nil
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

func runRequestR2(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	start := v1alpha1.ContractEventR2Start{Name: strings.TrimSpace(vals.String("r2-name")), Agent: agentOf(vals.String("agent")) != nil, Hold: vals.Bool("hold")}
	if err := market.ValidGuestRequesterName(start.Name); err != nil {
		return a.fail(exitRefused, "run request r2: --r2-name: %v", err)
	}
	r, started, code := a.openRequest(ctx, vals, rest, "r2")
	if r == nil {
		return code
	}
	if started {
		defer r.stop(context.WithoutCancel(ctx), false)
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		return a.fail(exitFailed, "run request r2: %v", err)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run request r2: the contract is %s, not Held: %s", c.Status.Phase, c.Status.Message)
	}
	if c.Spec.Association == nil || !c.Spec.Association.FromGuest {
		return a.fail(exitRefused, "run request r2: the contract %s has no spec.association.fromGuest; ask for one with run request --associate guest", c.Name)
	}
	name, err := r.kcp.Signaling().PublishR2Start(ctx, start)
	if err != nil {
		return a.fail(exitFailed, "run request r2: creating the r2Start ContractEvent: %v", err)
	}
	e, err := r.kcp.WaitContractEvent(ctx, name, r.contractTimeout, agentStartSettled)
	switch {
	case e != nil && e.Status.Phase == v1alpha1.ContractEventFailed:
		return a.fail(exitFailed, "run request r2: the requester refused r2Start: %s", e.Status.Message)
	case e != nil && (e.Status.Phase == v1alpha1.ContractEventSubmitted || e.Status.Phase == v1alpha1.ContractEventPublished):
		r.log("r2Start is out; the guest agent starts socialweb-r2.service", "contractEvent", name, "r2", start.Name, "agent", start.Agent, "hold", start.Hold)
	case err != nil:
		return a.fail(exitFailed, "run request r2: %v", err)
	default:
		return a.fail(exitFailed, "run request r2: the r2Start ContractEvent %s is not published", name)
	}
	return exitOK
}
