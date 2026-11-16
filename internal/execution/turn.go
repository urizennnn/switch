package execution

import (
	"context"
	"fmt"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

// acpTurn runs one turn in the agent's ACP adapter, resuming the session when
// possible and offering the control plane when one is configured.
func (e *Engine) acpTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	spec := acpdriver.Spec{
		Agent:     req.Agent.Driver,
		Cwd:       req.Cwd,
		Model:     req.Agent.Model,
		SessionID: req.SessionID,
		AgentLogs: e.opts.AgentLogs,
		Ephemeral: req.Ephemeral,
	}
	if e.opts.Control != nil {
		grant, err := e.opts.Control.Grant(req.Agent, req.TaskID)
		if err != nil {
			return TurnResult{}, err
		}
		spec.MCPServers, spec.Env = grant.MCPServers, grant.Env
	}

	session, err := acpdriver.Open(ctx, spec)
	if err != nil && spec.SessionID != "" {
		spec.SessionID = ""
		session, err = acpdriver.Open(ctx, spec)
	}
	if err != nil {
		return TurnResult{}, fmt.Errorf("start %s: %w", req.Agent.Name, err)
	}
	defer session.Close(context.Background())

	result := TurnResult{SessionID: session.ID}
	opts := acpdriver.TurnOptions{
		Permission: req.Permission,
		OnUsage:    func(u acpdriver.UsageInfo) { result.Usage = u },
	}
	if e.opts.OnText != nil {
		name := req.Agent.Name
		opts.OnText = func(text string) { e.opts.OnText(name, text) }
	}
	name := req.Agent.Name
	if e.opts.OnEvent != nil {
		e.opts.OnEvent(name, acpdriver.Event{Type: acpdriver.EventStart, Text: req.Label})
	}
	opts.OnEvent = func(event acpdriver.Event) {
		switch {
		case event.Type == acpdriver.EventUsage && event.Usage != nil && event.Usage.Size > 0:
			e.setUsage(name, *event.Usage)
		case event.Type == acpdriver.EventTurn:
			result.StopReason = event.StopReason
		}
		if e.opts.OnEvent != nil {
			e.opts.OnEvent(name, event)
		}
	}
	if req.Ask && e.opts.OnPermission != nil {
		name := req.Agent.Name
		opts.OnPermission = func(request *acpdriver.PermissionRequest) {
			request.Agent = name // shown as who is asking
			e.opts.OnPermission(request)
		}
	}
	result.Output, err = session.RunTurn(ctx, req.Prompt, opts)
	return result, err
}
