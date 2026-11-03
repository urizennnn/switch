package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/planning"
)

// A job is background agent work outside the chat: /plan or /run. Its output
// streams into the log panel. Esc cancels a plan; only /stop ends a run. A plan job holds the input like a chat turn; a run job does
// not, so the user can chat with the lead and approve tasks while it runs.

// maxJobLog bounds the log panel's history.
const maxJobLog = 64 * 1024

type jobText struct {
	agent string
	text  string
}

type jobTextMsg jobText

type jobPermissionMsg struct {
	request *acpdriver.PermissionRequest
}

type jobChangedMsg struct{}

type jobStreamClosedMsg struct{}

type planResultMsg struct {
	result *planning.Result
	err    error
}

type runResultMsg struct {
	outcome string
	err     error
}

func (m *model) startJob(name, state string, holdInput bool) context.Context {
	ctx, cancel := context.WithCancel(m.ctx)
	m.job = name
	m.jobCancel = cancel
	m.jobStream = make(chan jobText, 64)
	m.jobPermissions = make(chan *acpdriver.PermissionRequest)
	m.jobChanges = make(chan struct{}, 1)
	m.jobLog = ""
	m.jobPartial = map[string]string{}
	if holdInput {
		m.busy = true
		m.state = state
	} else if !m.busy {
		m.state = state
	}
	m.layout()
	return ctx
}

func (m *model) endJob() {
	if m.jobCancel != nil {
		m.jobCancel()
	}
	if m.job == "plan" || !m.busy {
		m.busy = false
		m.state = "ready"
	}
	m.job = ""
	m.jobCancel = nil
	m.jobStream = nil
	m.jobPermissions = nil
	m.jobChanges = nil
	m.engine = nil
	m.layout()
	m.refreshDashboard()
}

// stopJob cancels a running job and waits briefly for its adapters to exit,
// so quitting does not leave orphan processes.
func (m *model) stopJob() {
	if m.jobCancel == nil {
		return
	}
	m.jobCancel()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-m.jobStream:
			if !ok {
				return
			}
		case <-timeout:
			return
		}
	}
}

func (m model) waitJob() tea.Cmd {
	stream, permissions, changes := m.jobStream, m.jobPermissions, m.jobChanges
	if stream == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case text, ok := <-stream:
			if !ok {
				return jobStreamClosedMsg{}
			}
			return jobTextMsg(text)
		case request := <-permissions:
			return jobPermissionMsg{request: request}
		case <-changes:
			return jobChangedMsg{}
		}
	}
}

// appendJobText adds streamed text to the log, one prefixed line per agent
// line, so concurrent workers do not interleave mid-line.
func (m *model) appendJobText(agent, text string) {
	pending := m.jobPartial[agent] + text
	lines := strings.Split(pending, "\n")
	m.jobPartial[agent] = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m.jobLog += agent + "  " + line + "\n"
	}
	if len(m.jobLog) > maxJobLog {
		tail := m.jobLog[len(m.jobLog)-maxJobLog/2:]
		m.jobLog = tail[strings.IndexByte(tail, '\n')+1:]
	}
}

// jobLogText is the log plus each agent's unfinished line.
func (m model) jobLogText() string {
	text := m.jobLog
	for agent, partial := range m.jobPartial {
		if strings.TrimSpace(partial) != "" {
			text += agent + "  " + partial + "\n"
		}
	}
	return strings.TrimRight(text, "\n")
}

func sendText(ctx context.Context, stream chan<- jobText, agent string) func(string) {
	return func(text string) {
		select {
		case stream <- jobText{agent: agent, text: text}:
		case <-ctx.Done():
		}
	}
}

func startPlanCmd(ctx context.Context, project, goal, file string, lead config.Agent, stream chan<- jobText) tea.Cmd {
	return func() tea.Msg {
		defer close(stream)
		planCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		result, err := planning.Execute(planCtx, planning.Options{
			Project:    project,
			Goal:       goal,
			File:       file,
			Lead:       &lead,
			Permission: acpdriver.PermissionDeny,
			OnText:     sendText(planCtx, stream, lead.Name),
		})
		return planResultMsg{result: result, err: err}
	}
}

// newEngine builds an engine for the current run, wired to the job channels
// when a run job is active.
func (m model) newEngine(ctx context.Context) (*execution.Engine, error) {
	opts := execution.Options{
		Store:         m.store,
		RunID:         m.run.ID,
		Rules:         m.rules,
		Config:        m.runConfig,
		WaitForReview: true,
	}
	if stream, permissions, changes := m.jobStream, m.jobPermissions, m.jobChanges; stream != nil {
		opts.OnText = func(agent, text string) { sendText(ctx, stream, agent)(text) }
		// Workers ask the user, the same as the chat lead.
		opts.OnPermission = func(request *acpdriver.PermissionRequest) {
			select {
			case permissions <- request:
			case <-ctx.Done():
				request.Cancel()
			}
		}
		opts.OnChange = func() {
			select {
			case changes <- struct{}{}:
			default:
			}
		}
	}
	return execution.New(ctx, opts)
}

func runEngineCmd(ctx context.Context, engine *execution.Engine, stream chan<- jobText) tea.Cmd {
	return func() tea.Msg {
		defer close(stream)
		outcome, err := engine.Run(ctx)
		return runResultMsg{outcome: outcome, err: err}
	}
}

func runOutcomeText(outcome string, err error) string {
	switch {
	case err != nil && outcome == execution.RunStopped:
		return "run stopped; running tasks were interrupted. /run resumes them."
	case err != nil:
		return err.Error()
	case outcome == execution.RunCompleted:
		return "every task is merged into the integration branch.\nNothing was pushed; merge or push it yourself."
	case outcome == execution.RunBlocked:
		return "no task can make progress.\n/tasks shows blocked tasks; /retry <task> requeues one."
	case outcome == execution.RunBudget:
		return "stopped by the run budget (max_turns or max_tokens in .switch/config.toml)."
	default:
		return "run " + outcome
	}
}
