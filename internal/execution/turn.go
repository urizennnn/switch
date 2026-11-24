package execution

import (
	"context"
	"fmt"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/policy"
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
		Env:       req.Env,
	}
	// Lead reviews, the only ephemeral turns, are read-only: Switch tools such
	// as switch_delegate would let them queue commands for Switch to run.
	if e.opts.Control != nil && !req.Ephemeral {
		grant, err := e.opts.Control.Grant(req.Agent, req.TaskID)
		if err != nil {
			return TurnResult{}, err
		}
		if grant.Revoke != nil {
			defer grant.Revoke()
		}
		spec.MCPServers, spec.Env = grant.MCPServers, append(spec.Env, grant.Env...)
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
	if !req.Ephemeral {
		e.useSession(req.Agent.Name, session.ID)
		queued := [2]string{req.Agent.Name, req.TaskID}
		e.stopMu.Lock()
		e.live[req.Agent.Name] = liveTurn{session, req.TaskID}
		req.Prompt = e.steers[queued] + req.Prompt // what Steer could not get into its last turn on the task
		delete(e.steers, queued)
		e.stopMu.Unlock()
		defer func() {
			e.stopMu.Lock()
			delete(e.live, req.Agent.Name)
			e.stopMu.Unlock()
		}()
	}

	result := TurnResult{SessionID: session.ID}
	opts := acpdriver.TurnOptions{
		OnUsage: func(u acpdriver.UsageInfo) { result.Usage = u },
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
		e.beat(req.TaskID, name, false)
		switch {
		case event.Type == acpdriver.EventUsage && event.Usage != nil && event.Usage.Size > 0:
			e.setUsage(session.ID, *event.Usage)
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
			e.beat(req.TaskID, name, true)
			request.Agent = name // shown as who is asking
			e.opts.OnPermission(request)
		}
	} else {
		opts.OnPermission = func(request *acpdriver.PermissionRequest) {
			request.Resolve(unattended(req, request))
		}
	}
	result.Output, err = session.RunTurn(ctx, req.Prompt, opts)
	e.beat(req.TaskID, name, false) // a request left unanswered ends with the turn
	return result, err
}

// liveTurn is an agent's running turn and the task it serves.
type liveTurn struct {
	session *acpdriver.Session
	task    string
}

// Steer sends text to an agent's running turn, a worker's or a subagent's.
// When its adapter cannot take it mid-turn, or the turn ends first, the agent
// gets it at the start of its next turn on the same task, if one comes before
// the run stops, as the text is about that work; the reply says which.
func (e *Engine) Steer(ctx context.Context, agent, text string) (string, error) {
	e.stopMu.Lock()
	turn, ok := e.live[agent]
	e.stopMu.Unlock()
	if !ok {
		return "", fmt.Errorf("%s has no turn running", agent)
	}
	err := turn.session.Steer(ctx, text)
	if err == nil {
		return agent + " got it in its running turn", nil
	}
	e.stopMu.Lock()
	e.steers[[2]string{agent, turn.task}] += "Sent to you during your previous turn: " + text + "\n\n"
	e.stopMu.Unlock()
	return fmt.Sprintf("%s could not take it mid-turn (%v); it gets it at the start of its next turn on %s, if one comes before the run stops",
		agent, err, planID(turn.task)), nil
}

// unattended answers a request when no one can be asked, as in a headless or
// detached run. A worker, or its subagent, gets build mode's answer, and the
// turn's configured one for what that would ask the user about; a lead
// review, which only reads, gets the configured one throughout.
func unattended(req TurnRequest, request *acpdriver.PermissionRequest) acpdriver.PermissionMode {
	if !req.Ephemeral && policy.Decide(policy.Build, request.Kind, request.Inside) == policy.Allow {
		return acpdriver.PermissionAllow
	}
	return req.Permission
}
