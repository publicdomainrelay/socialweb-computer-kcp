package guestagent

import (
	"context"
	"os/exec"
	"regexp"
	"time"
)

const agentStartTimeout = 30 * time.Second

var agentUnitRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.service$`)

type AgentUnit struct {
	Unit string `json:"unit"`
}

func (a *Agent) startAgent(seq int64) {
	unit := a.cfg.AgentUnit
	if unit == "" {
		a.log.Debug("an agentStart for a guest with no agent unit", "seq", seq)
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, agentStartTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "start", "--no-block", unit).CombinedOutput()
	if err != nil {
		a.log.Warn("guest_agent_start", "seq", seq, "unit", unit, "err", err.Error(), "output", tail(string(out), 256))
		return
	}
	a.log.Info("guest_agent_start", "seq", seq, "unit", unit)
}
