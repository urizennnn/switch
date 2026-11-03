package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/planning"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// A job is a background agent turn outside the chat: /plan or /run. Its text
// streams into the main pane and esc cancels it.

type jobTextMsg struct {
	text string
}

type jobPermissionMsg struct {
	request *acpdriver.PermissionRequest
}

type jobStreamClosedMsg struct{}

type planResultMsg struct {
	result *planning.Result
	err    error
}

type runResultMsg struct {
	result *execution.Result
	err    error
}

func (m *model) startJob(name, state string) (context.Context, chan string, chan *acpdriver.PermissionRequest) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.job = name
	m.jobCancel = cancel
	m.jobStream = make(chan string, 64)
	m.jobPermissions = make(chan *acpdriver.PermissionRequest)
	m.jobBuffer.Reset()
	m.busy = true
	m.state = state
	return ctx, m.jobStream, m.jobPermissions
}

func (m *model) endJob() {
	if m.jobCancel != nil {
		m.jobCancel()
	}
	m.job = ""
	m.jobCancel = nil
	m.jobStream = nil
	m.jobPermissions = nil
	m.jobBuffer.Reset()
	m.busy = false
	m.state = "ready"
	m.refreshDashboard()
}

// stopJob cancels a running job and waits briefly for its adapter to exit, so
// quitting does not leave an orphan process.
func (m *model) stopJob() {
	if m.jobCancel == nil {
		return
	}
	m.jobCancel()
	timeout := time.After(5 * time.Second)
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

func waitJob(stream <-chan string, permissions <-chan *acpdriver.PermissionRequest) tea.Cmd {
	return func() tea.Msg {
		select {
		case text, ok := <-stream:
			if !ok {
				return jobStreamClosedMsg{}
			}
			return jobTextMsg{text: text}
		case request := <-permissions:
			return jobPermissionMsg{request: request}
		}
	}
}

func streamTo(ctx context.Context, stream chan<- string) func(string) {
	return func(text string) {
		select {
		case stream <- text:
		case <-ctx.Done():
		}
	}
}

func startPlanCmd(ctx context.Context, project, goal, file string, lead config.Agent, stream chan<- string) tea.Cmd {
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
			OnText:     streamTo(planCtx, stream),
		})
		return planResultMsg{result: result, err: err}
	}
}

func runTaskCmd(ctx context.Context, state *store.Store, runID string, ruleSet rules.Set, stream chan<- string, permissions chan<- *acpdriver.PermissionRequest) tea.Cmd {
	return func() tea.Msg {
		defer close(stream)
		result, err := execution.RunNext(ctx, execution.Options{
			Store:  state,
			RunID:  runID,
			Rules:  ruleSet,
			OnText: streamTo(ctx, stream),
			// Workers ask the user, the same as the chat lead.
			OnPermission: func(request *acpdriver.PermissionRequest) {
				select {
				case permissions <- request:
				case <-ctx.Done():
					request.Cancel()
				}
			},
		})
		return runResultMsg{result: result, err: err}
	}
}

func runSummary(result *execution.Result, err error) (title, body string) {
	if errors.Is(err, execution.ErrNoReadyTask) {
		return "run", err.Error()
	}
	if result == nil {
		return "run failed", err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "task %s\n", result.Task.Title)
	fmt.Fprintf(&b, "worker %s (%s)\n", result.Agent.Name, result.Agent.Driver)
	fmt.Fprintf(&b, "status %s · attempt %d\n", result.Task.Status, result.Task.Attempts)
	fmt.Fprintf(&b, "worktree %s\n", truncate(result.Task.Worktree, 56))
	switch {
	case result.Task.Status == execution.StatusInterrupted:
		b.WriteString("\ncancelled; /run resumes it")
	case err != nil:
		b.WriteString("\n" + err.Error())
	case strings.TrimSpace(result.Output) != "":
		b.WriteString("\n" + strings.TrimSpace(result.Output))
	}
	return "run", b.String()
}
