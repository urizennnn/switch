package execution

import (
	"errors"
	"fmt"
	"strings"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/store"
)

// A worker that runs low on context, or hits a usage limit, hands its task
// to another agent instead of failing it: the task goes back to the queue
// assigned to that agent, which continues in the same worktree with the
// worker's note. A worker hands off itself with switch_handoff; when a limit
// stops it first, Switch hands off for it. Each handoff counts as an attempt,
// so agents cannot pass a task back and forth forever.

// handoffPrefix marks task feedback that is a handoff note, not a rejection.
const handoffPrefix = "handoff from "

type handoff struct {
	to   string
	note string
}

// Limits is what an agent knows about its own limits.
type Limits struct {
	Used      int    `json:"context_used,omitempty"`
	Size      int    `json:"context_size,omitempty"`
	HandoffAt int    `json:"handoff_at_percent,omitempty"`
	HandoffTo string `json:"handoff_to,omitempty"`
}

// Percent is the share of the context window in use, or -1 when unknown.
func (l Limits) Percent() int {
	if l.Size <= 0 {
		return -1
	}
	return l.Used * 100 / l.Size
}

// Near reports whether the agent should hand off now.
func (l Limits) Near() bool {
	return l.HandoffAt > 0 && l.Percent() >= l.HandoffAt
}

// useSession records the session an agent's task turn runs in: its context
// use is that session's, unknown for a new one until its adapter reports.
func (e *Engine) useSession(agent, session string) {
	e.stopMu.Lock()
	e.sessions[agent] = session
	e.stopMu.Unlock()
}

func (e *Engine) setUsage(session string, usage acpdriver.UsageInfo) {
	e.stopMu.Lock()
	e.usage[session] = usage
	e.stopMu.Unlock()
}

// Limits reports an agent's context use in its task session and who it hands
// off to.
func (e *Engine) Limits(agent string) Limits {
	e.stopMu.Lock()
	usage := e.usage[e.sessions[agent]]
	e.stopMu.Unlock()
	return Limits{Used: usage.Used, Size: usage.Size, HandoffAt: e.opts.Config.HandoffAt, HandoffTo: e.handoffTarget(agent)}
}

// handoffTarget is the agent configured to continue from, or else the run's
// lead, or else a standby lead; "" when there is none.
func (e *Engine) handoffTarget(from string) string {
	ctx, cancel := saveContext()
	defer cancel()
	agents, _ := e.opts.Store.RunAgents(ctx, e.run.ID)
	if to := e.opts.Handoffs[from]; to != "" && hasAgent(agents, to) {
		return to
	}
	for _, role := range []string{"lead", "standby"} {
		for _, agent := range agents {
			if agent.Role == role && agent.Name != from {
				return agent.Name
			}
		}
	}
	return ""
}

// Handoff records a worker's request to pass its running task on. It takes
// effect when the worker's turn ends.
func (e *Engine) Handoff(from, taskID, to, note string) (string, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return "", errors.New("handoff needs a note: what is done, what is left, and where you were")
	}
	ctx, cancel := saveContext()
	defer cancel()
	task, ok, err := e.opts.Store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if !ok || task.Assignee != from || task.Status != StatusRunning {
		return "", errors.New("you can hand off only your running task")
	}
	if to = strings.TrimSpace(to); to == "" {
		to = e.handoffTarget(from)
	}
	agents, err := e.opts.Store.RunAgents(ctx, e.run.ID)
	if err != nil {
		return "", err
	}
	if to == "" || to == from || !hasAgent(agents, to) {
		return "", fmt.Errorf("no agent to hand off to; name one of this run's agents other than yourself")
	}
	e.stopMu.Lock()
	e.handoffs[taskID] = handoff{to: to, note: note}
	e.stopMu.Unlock()
	return fmt.Sprintf("%s continues this task in this worktree when your turn ends. End your turn now.", to), nil
}

func (e *Engine) takeHandoff(taskID string) (handoff, bool) {
	e.stopMu.Lock()
	defer e.stopMu.Unlock()
	h, ok := e.handoffs[taskID]
	delete(e.handoffs, taskID)
	return h, ok
}

// handOff queues the task for its new agent. On the last attempt it is sent
// back instead, which escalates or blocks it.
func (e *Engine) handOff(task store.Task, from store.RunAgent, h handoff) {
	if task.Attempts >= e.opts.Config.MaxAttempts {
		e.retry(task, from, fmt.Sprintf("%s wanted to hand off to %s:\n%s", from.Name, h.to, h.note))
		return
	}
	e.finish(passOn(task, from.Name, h), from, StatusQueued, "handed off to "+h.to)
}

// passOn assigns the task to the handoff's agent with its note, keeping the
// worktree and starting a fresh session, since sessions belong to one agent.
func passOn(task store.Task, from string, h handoff) store.Task {
	task.Assignee, task.SessionID, task.Feedback = h.to, "", handoffPrefix+from+":\n"+h.note
	return task
}

// escalation is the agent configured to get one more attempt at from's task
// when its attempts run out; "" when there is none in this run.
func (e *Engine) escalation(from string) string {
	to := e.opts.Escalations[from]
	if to == "" || to == from {
		return ""
	}
	ctx, cancel := saveContext()
	defer cancel()
	if agents, _ := e.opts.Store.RunAgents(ctx, e.run.ID); hasAgent(agents, to) {
		return to
	}
	return ""
}

// limitHandoff hands the task on when its worker stopped at a limit.
func (e *Engine) limitHandoff(task store.Task, agent store.RunAgent, stopReason string, err error) bool {
	reason := acpdriver.LimitReason(stopReason, err)
	to := e.handoffTarget(agent.Name)
	if reason == "" || to == "" {
		return false
	}
	e.handOff(task, agent, handoff{to: to, note: fmt.Sprintf(
		"%s stopped at a limit before it finished (%s). Its partial changes are in this worktree: check git status and git diff, then finish the task.", agent.Name, reason)})
	return true
}

// handoffNote tells a worker how to hand off before it runs out.
func handoffNote(limits Limits) string {
	if limits.HandoffTo == "" {
		return ""
	}
	at := "is nearly full"
	if limits.HandoffAt > 0 {
		at = fmt.Sprintf("passes %d%%", limits.HandoffAt)
	}
	return fmt.Sprintf(`
If your context window %s, or you are close to a usage limit, stop and hand off rather than run out mid-change:
call switch_handoff with a note (what is done, what is left, and where you were), then end your turn.
%s continues in this worktree from your note. switch_limits shows how full your context is.
`, at, limits.HandoffTo)
}
