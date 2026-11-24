package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// In the chat, a teammate answering a question can start read-only
// subagents to explore in parallel. They work in the project, report only to
// that teammate, and stop when it answers. Edits happen in /run tasks.

// streamMsg is one session event of an agent other than the lead.
type streamMsg struct {
	agent string
	event acpdriver.Event
}

type teamPermissionMsg struct{ request *acpdriver.PermissionRequest }

func waitStream(streams <-chan streamMsg) tea.Cmd {
	return func() tea.Msg { return <-streams }
}

func waitTeamPermission(perms <-chan *acpdriver.PermissionRequest) tea.Cmd {
	return func() tea.Msg { return teamPermissionMsg{request: <-perms} }
}

// handleStream adds another agent's event to its live view.
func (m model) handleStream(msg streamMsg) (tea.Model, tea.Cmd) {
	list := m.agentEntries[msg.agent]
	applyEvent(&list, msg.event)
	m.agentEntries[msg.agent] = list
	switch msg.event.Type {
	case acpdriver.EventStart:
		m.agentBusy[msg.agent] = time.Now()
	case acpdriver.EventTurn, acpdriver.EventError, acpdriver.EventExit:
		delete(m.agentBusy, msg.agent)
	}
	m.chatSubs = m.team.subagentList()
	return m, waitStream(m.streams)
}

// settleBusy ends what the stream cannot: a run turn that was stopped, timed
// out or cancelled ends without a closing event, so after each look at the
// store an agent stays busy only while the chat or the run has a turn for it.
func (m *model) settleBusy() {
	for name := range m.agentBusy {
		if !m.team.answering(name) && !m.runTurn(name) {
			delete(m.agentBusy, name)
		}
	}
}

// runTurn reports whether the run this TUI drives has a turn going for an
// agent: a worker's task, a subagent's, or the lead's review.
func (m model) runTurn(name string) bool {
	if m.job != "run" {
		return false
	}
	if parent, sub, ok := strings.Cut(name, "/"); ok {
		return slices.ContainsFunc(m.subagents, func(sa store.Subagent) bool {
			return sa.Parent == parent && sa.Name == sub && sa.Status == execution.SubRunning
		})
	}
	lead := slices.ContainsFunc(m.runAgents, func(a store.RunAgent) bool { return a.Name == name && a.Role == "lead" })
	return slices.ContainsFunc(m.tasks, func(task store.Task) bool {
		return task.Assignee == name && task.Status == execution.StatusRunning || lead && task.Status == execution.StatusReviewing
	})
}

// stopFocused is ctrl+x: stop whatever the followed agent is doing.
func (m model) stopFocused() (tea.Model, tea.Cmd) {
	name := m.focus
	switch {
	case name == "":
		if m.busy && m.state == "working" && m.chat != nil {
			_ = m.chat.Session.Cancel(context.Background())
			m.state = "cancelling"
		}
		return m, nil
	case m.team.stopAgent(name):
		m.withdrawPrompts(name)
		m.notify("stopping "+strings.ReplaceAll(name, "/", " › "), "")
		return m, nil
	case m.engine != nil:
		engine := m.engine
		if parent, sub, ok := strings.Cut(name, "/"); ok {
			if task, ok := m.agentTask(parent); ok {
				go engine.Stop(parent, task.ID, sub)
				m.notify("stopping "+parent+" › "+sub, parent+" gets its result as stopped")
				return m, nil
			}
		} else if engine.StopWorker(name) {
			m.notify("stopping "+name, "its task is blocked; /retry runs it again")
			return m, nil
		}
	}
	m.notify(strings.ReplaceAll(name, "/", " › ")+" is not working on anything", "")
	return m, nil
}

// readOnlyKinds are tool kinds a teammate may use without asking the user.
var readOnlyKinds = map[string]bool{"read": true, "search": true}

func (t *team) emit(agent string, event acpdriver.Event) {
	if event.Type == acpdriver.EventUsage && event.Usage != nil {
		t.setUsage(agent, *event.Usage)
	}
	if t.events == nil {
		return
	}
	select {
	case t.events <- streamMsg{agent: agent, event: event}:
	case <-t.ctx.Done():
	}
}

// permit answers a teammate's or subagent's permission request: reads inside
// its directory go ahead, anything else waits for the user.
func (t *team) permit(agent string) func(*acpdriver.PermissionRequest) {
	return func(request *acpdriver.PermissionRequest) {
		request.Agent = agent
		if readOnlyKinds[request.Kind] && request.Inside {
			request.Resolve(acpdriver.PermissionAllow)
			return
		}
		if t.perms == nil {
			request.Resolve(acpdriver.PermissionDeny)
			return
		}
		select {
		case t.perms <- request:
		case <-t.ctx.Done():
			request.Cancel()
		}
	}
}

// answering reports whether a teammate, or a subagent of one, has a turn
// going in the chat.
func (t *team) answering(name string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sa := t.subs[name]
	return t.asked[name] || sa != nil && sa.rec.Status == execution.SubRunning
}

// stopAgent cancels what a teammate or one of its subagents is doing.
func (t *team) stopAgent(name string) bool {
	if t == nil {
		return false
	}
	if parent, sub, ok := strings.Cut(name, "/"); ok {
		n, _ := t.Stop(parent, "", sub)
		return n > 0
	}
	t.mu.Lock()
	session := t.active[name]
	t.mu.Unlock()
	if session == nil {
		return false
	}
	_ = session.Cancel(context.Background())
	return true
}

type chatSub struct {
	rec     store.Subagent
	session *acpdriver.Session
	revoke  func() // ends its session's control-plane token
	cancel  context.CancelFunc
	inbox   []string
	report  string
	done    chan struct{}
}

// endSession closes the subagent's session and revokes its token. Callers
// hold mu.
func (sa *chatSub) endSession() {
	go sa.session.Close(context.Background())
	if sa.revoke != nil {
		sa.revoke()
	}
	sa.session, sa.revoke = nil, nil
}

var chatSubName = regexp.MustCompile(`[^a-z0-9-]+`)

// Spawn implements control.Subagents for teammates in the chat.
func (t *team) Spawn(parent, _ string, req execution.SpawnRequest) (store.Subagent, error) {
	limit := t.subCfg.MaxPerBuilder
	if limit <= 0 {
		return store.Subagent{}, errors.New("subagents are turned off for this project ([run.subagents] max_per_builder)")
	}
	if len(req.Files) > 0 {
		return store.Subagent{}, errors.New("in the chat, subagents are read-only; leave files empty (edits happen in /run tasks)")
	}
	brief := strings.TrimSpace(req.Brief)
	if brief == "" {
		return store.Subagent{}, errors.New("spawn needs a brief")
	}
	worker, ok := t.agent(parent)
	if !ok {
		return store.Subagent{}, fmt.Errorf("unknown teammate %s", parent)
	}
	driver, model := strings.TrimSpace(req.Driver), strings.TrimSpace(req.Model)
	if driver == "" {
		driver = worker.Driver
	}
	if model == "" && driver == worker.Driver {
		model = worker.Model
	}
	if allowed := t.subCfg.Drivers; len(allowed) > 0 && !slices.Contains(allowed, driver) {
		return store.Subagent{}, fmt.Errorf("subagents may use %s; %s is not allowed", strings.Join(allowed, ", "), driver)
	}
	t.mu.Lock()
	base := t.rules
	t.mu.Unlock()
	set := rules.Subagent(base, worker.Role, worker.Name, "") // reads the rule files, so not under mu

	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.asked[parent] {
		return store.Subagent{}, errors.New("in the chat, subagents can be started only while you answer a teammate's question")
	}
	running := 0
	taken := map[string]bool{}
	for _, sa := range t.subs {
		if sa.rec.Parent == parent {
			taken[sa.rec.Name] = true
			if sa.rec.Status == execution.SubRunning {
				running++
			}
		}
	}
	if running >= limit {
		return store.Subagent{}, fmt.Errorf("you already have %d subagents running, the limit; wait for one with switch_wait", running)
	}
	name := strings.Trim(chatSubName.ReplaceAllString(strings.ToLower(strings.TrimSpace(req.Name)), "-"), "-")
	if name == "" {
		name = "sub"
	}
	for base, i := name, 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	sa := &chatSub{rec: store.Subagent{
		ID: parent + "/" + name, Parent: parent, Name: name, Driver: driver, Model: model,
		Brief: brief, Status: execution.SubRunning, CreatedAt: time.Now(),
	}}
	t.subs[sa.rec.ID] = sa
	prompt := set.Prompt(fmt.Sprintf("You are %s, a read-only subagent that %s started to help answer a question about the project in %s. "+
		"You report only to %s.\n\nYour job:\n%s\n\nDo not create, edit, or delete any file, and do not start subagents of your own. "+
		"When you are done, give your result with the switch_report MCP tool, then reply with the same result. "+
		"Keep it short and concrete: %s reads only that.", name, parent, t.project, parent, brief, parent))
	t.runSub(sa, firstLine(brief), prompt)
	return sa.rec, nil
}

// runSub starts the subagent's session if needed and runs turns until no
// follow-up is waiting. Callers hold mu.
func (t *team) runSub(sa *chatSub, label, prompt string) {
	ctx, cancel := context.WithCancel(t.ctx)
	sa.cancel, sa.done = cancel, make(chan struct{})
	sa.rec.Status = execution.SubRunning
	id := sa.rec.ID
	start := sa.session == nil // read under mu: stopSubs ends sessions under it
	go func() {
		defer cancel()
		if start {
			var revoke func()
			grant := func(string) ([]acp.McpServer, []string) {
				if t.server == nil {
					return nil, nil
				}
				g, err := t.server.Grant(store.RunAgent{ID: id, Name: id, Role: "subagent"}, "")
				if err != nil {
					return nil, nil
				}
				revoke = g.Revoke
				return g.MCPServers, g.Env
			}
			// It outlives this run, so a finished subagent can be messaged again.
			session, err := t.startSession(ctx, id, sa.rec.Driver, sa.rec.Model, grant)
			if err != nil {
				if revoke != nil {
					revoke()
				}
				t.finishSub(sa, execution.SubFailed, "start: "+err.Error())
				return
			}
			t.mu.Lock()
			sa.session, sa.revoke = session, revoke
			t.mu.Unlock()
		}
		for {
			t.emit(id, acpdriver.Event{Type: acpdriver.EventStart, Text: label})
			t.mu.Lock()
			session := sa.session
			t.mu.Unlock()
			if session == nil { // stopSubs ended it once its parent answered
				t.emit(id, acpdriver.Event{Type: acpdriver.EventError, Err: errors.New("stopped")})
				t.finishSub(sa, execution.SubStopped, "")
				return
			}
			turnCtx, stop := context.WithTimeout(ctx, 9*time.Minute)
			reply, err := session.RunTurn(turnCtx, prompt, acpdriver.TurnOptions{
				OnEvent:      func(event acpdriver.Event) { t.emit(id, event) },
				OnPermission: t.permit(id),
			})
			stop()
			t.mu.Lock()
			switch {
			case ctx.Err() != nil:
				if sa.session != nil { // its prompt may still answer, and a follow-up would take that as its reply
					sa.endSession()
				}
				t.mu.Unlock()
				t.emit(id, acpdriver.Event{Type: acpdriver.EventError, Err: errors.New("stopped")})
				t.finishSub(sa, execution.SubStopped, "")
				return
			case err != nil:
				t.mu.Unlock()
				t.emit(id, acpdriver.Event{Type: acpdriver.EventError, Err: err})
				t.finishSub(sa, execution.SubFailed, err.Error())
				return
			case len(sa.inbox) > 0:
				prompt, sa.inbox = strings.Join(sa.inbox, "\n\n"), nil
				label = sa.rec.Parent + ": " + firstLine(prompt)
				t.mu.Unlock()
				continue
			}
			summary := strings.TrimSpace(reply)
			if sa.report != "" {
				summary = sa.report
			}
			t.mu.Unlock()
			t.finishSub(sa, execution.SubDone, summary)
			return
		}
	}()
}

func (t *team) finishSub(sa *chatSub, status, summary string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sa.rec.Status, sa.rec.UpdatedAt = status, time.Now()
	if summary != "" {
		sa.rec.Summary = summary
	}
	close(sa.done)
}

// stopSubs stops a teammate's subagents and ends their sessions once it has
// answered.
func (t *team) stopSubs(parent string) {
	t.Stop(parent, "", "")
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, sa := range t.subs {
		if sa.rec.Parent == parent && sa.session != nil {
			sa.endSession()
		}
	}
}

func (t *team) Wait(ctx context.Context, parent, _ string, names []string) ([]store.Subagent, error) {
	t.mu.Lock()
	var waiting []chan struct{}
	for _, sa := range t.subs {
		if sa.rec.Parent == parent && sa.rec.Status == execution.SubRunning && (len(names) == 0 || slices.Contains(names, sa.rec.Name)) {
			waiting = append(waiting, sa.done)
		}
	}
	t.mu.Unlock()
	for _, done := range waiting {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	subs, _ := t.List(parent, "")
	if len(names) > 0 {
		subs = slices.DeleteFunc(subs, func(sa store.Subagent) bool { return !slices.Contains(names, sa.Name) })
	}
	return subs, nil
}

func (t *team) Message(parent, _, name, text string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sa, ok := t.subs[parent+"/"+name]
	if !ok {
		return "", fmt.Errorf("you have no subagent named %q", name)
	}
	if sa.rec.Status == execution.SubRunning {
		sa.inbox = append(sa.inbox, text)
		return name + " gets this when its current turn ends", nil
	}
	if !live(sa.session) {
		return "", fmt.Errorf("%s has ended; start a new subagent", name)
	}
	sa.report = ""
	t.runSub(sa, parent+": "+firstLine(text), text)
	return name + " is working on it", nil
}

func (t *team) Stop(parent, _, name string) (int, error) {
	t.mu.Lock()
	var stopping []*chatSub
	for _, sa := range t.subs {
		if sa.rec.Parent == parent && sa.rec.Status == execution.SubRunning && (name == "" || sa.rec.Name == name) {
			stopping = append(stopping, sa)
		}
	}
	for _, sa := range stopping {
		sa.cancel()
	}
	t.mu.Unlock()
	for _, sa := range stopping {
		select {
		case <-sa.done:
		case <-time.After(15 * time.Second):
		}
	}
	return len(stopping), nil
}

func (t *team) List(parent, _ string) ([]store.Subagent, error) {
	var subs []store.Subagent
	for _, sa := range t.subagentList() {
		if sa.Parent == parent {
			subs = append(subs, sa)
		}
	}
	return subs, nil
}

func (t *team) Report(id, text string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	sa, ok := t.subs[id]
	if !ok {
		return errors.New("no running subagent for this session")
	}
	sa.report = strings.TrimSpace(text)
	return nil
}

func (t *team) RunningSubagents(string) int { return 0 } // chat teammates do not complete tasks

// subagentList is every chat subagent, oldest first, for the sidebar.
func (t *team) subagentList() []store.Subagent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	subs := make([]store.Subagent, 0, len(t.subs))
	for _, sa := range t.subs {
		subs = append(subs, sa.rec)
	}
	slices.SortFunc(subs, func(a, b store.Subagent) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return subs
}
