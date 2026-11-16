package tui

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/control"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/planning"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/runcli"
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
	m.jobStarted = time.Now()
	m.planText = ""
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
	if m.job == "plan" && len(m.planText) < maxJobLog*4 {
		m.planText += text
	}
	pending := m.jobPartial[agent] + text
	lines := strings.Split(pending, "\n")
	m.jobPartial[agent] = lines[len(lines)-1]
	label := strings.ReplaceAll(agent, "/", " › ") // a subagent is worker/name
	for _, line := range lines[:len(lines)-1] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m.jobLog += label + "  " + line + "\n"
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
			text += strings.ReplaceAll(agent, "/", " › ") + "  " + partial + "\n"
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

func startPlanCmd(ctx context.Context, project, goal, file string, lead config.Agent, ruleSet rules.Set, stream chan<- jobText) tea.Cmd {
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
			Rules:      &ruleSet,
		})
		return planResultMsg{result: result, err: err}
	}
}

// newEngine builds an engine for the current run, wired to the job channels
// when a run job is active.
func (m model) newEngine(ctx context.Context, server *control.Server) (*execution.Engine, error) {
	opts := execution.Options{
		Store:         m.store,
		RunID:         m.run.ID,
		Rules:         m.rules,
		Config:        m.runConfig,
		Handoffs:      config.Handoffs(m.dashboardFleet()),
		WaitForReview: true,
	}
	if server != nil {
		opts.Control = server
	}
	if stream, permissions, changes := m.jobStream, m.jobPermissions, m.jobChanges; stream != nil {
		opts.OnText = func(agent, text string) { sendText(ctx, stream, agent)(text) }
		streams := m.streams
		opts.OnEvent = func(agent string, event acpdriver.Event) {
			select {
			case streams <- streamMsg{agent: agent, event: event}:
			case <-ctx.Done():
			}
		}
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

func runEngineCmd(ctx context.Context, engine *execution.Engine, server *control.Server, stream chan<- jobText) tea.Cmd {
	return func() tea.Msg {
		defer close(stream)
		defer server.Close()
		outcome, err := engine.Run(ctx)
		return runResultMsg{outcome: outcome, err: err}
	}
}

// startRun opens the run's control plane and an engine wired to it.
func (m model) startRun(ctx context.Context) (*execution.Engine, *control.Server, error) {
	stream := m.jobStream
	server, err := control.Listen(control.Options{
		Store: m.store, RunID: m.run.ID, StateDir: config.StateDir(),
		OnText: func(agent, text string) { sendText(ctx, stream, agent)(text) },
	})
	if err != nil {
		return nil, nil, err
	}
	engine, err := m.newEngine(ctx, server)
	if err != nil {
		server.Close()
		return nil, nil, err
	}
	server.SetSubagents(engine)
	return engine, server, nil
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

type detachMsg struct {
	title string
	body  string
}

// detachCmd starts the run in a background process that outlives the TUI.
// It cannot ask for permissions, so the user picks allow or deny up front.
func detachCmd(project, runID, permission string) tea.Cmd {
	return func() tea.Msg {
		pid, logPath, err := runcli.Detach(project, config.StateDir(), runID, permission)
		if err != nil {
			return detachMsg{title: "detach failed", body: err.Error()}
		}
		return detachMsg{title: "run detached", body: fmt.Sprintf(
			"pid %d is driving the run (worker permissions: %s)\nlog: %s\n\nIt keeps going after you quit.\n/tasks and /review approve its work · /stop stops it", pid, permission, logPath)}
	}
}

func stopDetachedCmd(runID string) tea.Cmd {
	return func() tea.Msg {
		if err := runcli.Stop(config.StateDir(), runID); err != nil {
			return detachMsg{title: "stop failed", body: err.Error()}
		}
		return detachMsg{title: "run stopped", body: "the background run stopped; running tasks were interrupted.\n/run resumes them."}
	}
}

var (
	planTitle = regexp.MustCompile(`"title"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	planTask  = regexp.MustCompile(`"id"\s*:`)
	planStart = regexp.MustCompile("```|\\{\\s*\"(goal|tasks)\"")
	planTool  = regexp.MustCompile(`^\[tool\] (.+?)(?: \((\w+)\))?$`)
)

// planProgress shows the lead's plan as it is written: its thinking, then
// each task title, instead of the raw JSON it streams.
func (m model) planProgress(width, height int) string {
	text := m.planText
	inner := max(10, width-4)
	header := activeStyle.Render(m.lead.Name+" is planning") + dimStyle.Render(" · "+time.Since(m.jobStarted).Round(time.Second).String())

	var lines []string
	start := -1 // where the plan JSON or its code fence begins
	if loc := planStart.FindStringIndex(text); loc != nil {
		start = loc[0]
	}
	prose := text
	if start >= 0 {
		prose = text[:start]
	}
	var thoughts []string
	for _, line := range strings.Split(prose, "\n") {
		line = strings.TrimSpace(line)
		if tool := planTool.FindStringSubmatch(line); tool != nil {
			line = "using " + tool[1] // tool calls read as a step, not a log line
		}
		if line != "" {
			thoughts = append(thoughts, line)
		}
	}
	for _, line := range thoughts[max(0, len(thoughts)-2):] {
		lines = append(lines, mutedStyle.Render(truncate(line, inner)))
	}

	titles := planTitle.FindAllStringSubmatch(text, -1)
	for _, match := range titles {
		title, err := strconv.Unquote(`"` + match[1] + `"`)
		if err != nil {
			title = match[1]
		}
		lines = append(lines, successStyle.Render("✓ ")+textStyle.Render(truncate(title, inner-2)))
	}
	switch {
	case start < 0 && text == "":
		lines = append(lines, dimStyle.Render("reading the project…"))
	case start >= 0 && len(planTask.FindAllStringIndex(text, -1)) > len(titles):
		lines = append(lines, dimStyle.Render(fmt.Sprintf("… drafting task %d", len(titles)+1)))
	case start >= 0:
		lines = append(lines, dimStyle.Render(fmt.Sprintf("… %d tasks so far", len(titles))))
	}
	room := max(1, height-4)
	if len(lines) > room {
		lines = append([]string{dimStyle.Render(fmt.Sprintf("  %d more above", len(lines)-room+1))}, lines[len(lines)-room+1:]...)
	}
	return header + "\n" + strings.Join(lines, "\n")
}
