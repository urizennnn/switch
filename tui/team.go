package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/control"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// team is the project's configured agents and their warm sessions. Every
// agent other than the lead is started in the background, so switching leads
// adopts a running session instead of starting one, and switch_ask can reach
// a teammate quickly. It is shared with the control server's goroutines, so
// all state is behind mu. A nil team (a model built without New) has no
// warm sessions.
type team struct {
	project string
	server  *control.Server // nil when another Switch already serves this project
	once    sync.Once

	mu      sync.Mutex
	fleet   []config.Agent
	lead    string
	warm    map[string]*acpdriver.Session
	warming map[string]bool
	asked   map[string]bool               // answering a switch_ask right now
	active  map[string]*acpdriver.Session // the session answering, so it can be stopped
	turns   map[string]context.Context    // the answer under way, which the teammate's questions to the user end with
	subs    map[string]*chatSub           // subagents of teammates, by worker/name
	working map[string]bool               // agents whose run task turn is running
	rules   rules.Set                     // the lead's; each teammate adds its role's and its own
	engine  *execution.Engine             // the run this Switch drives, whose workers Steer reaches
	steers  map[string]string             // what Steer could not get into a teammate's answer, for its next prompt

	// Set by the TUI before the team is used.
	base   context.Context                     // sessions run on it, as the lead's do, so quitting can close them gracefully
	ctx    context.Context                     // under base, for turns, emit and permit
	stop   context.CancelFunc                  // ends ctx; close calls it first
	events chan<- streamMsg                    // live views of teammates and their subagents
	perms  chan<- *acpdriver.PermissionRequest // actions that need the user's answer
	subCfg config.SubagentConfig

	usage     map[string]acpdriver.UsageInfo // context use by agent
	handoffAt int                            // percent of context at which to hand off
	handoffs  chan<- handoffMsg              // the lead's handoffs, for the TUI to carry out
}

func newTeam(project string, fleet []config.Agent, lead string) *team {
	return &team{
		project: project, fleet: fleet, lead: lead,
		warm: map[string]*acpdriver.Session{}, warming: map[string]bool{}, asked: map[string]bool{},
		active: map[string]*acpdriver.Session{}, turns: map[string]context.Context{}, subs: map[string]*chatSub{}, base: context.Background(), ctx: context.Background(),
		usage: map[string]acpdriver.UsageInfo{}, steers: map[string]string{},
	}
}

// listen serves switch_team and switch_ask for chat sessions. A failure (for
// example another Switch in this project) leaves agents without team tools.
func (t *team) listen() {
	t.once.Do(func() {
		sum := sha256.Sum256([]byte(t.project))
		server, err := control.Listen(control.Options{
			RunID:    hex.EncodeToString(sum[:])[:8],
			StateDir: config.StateDir(),
			Team:     t,
			OnText:   func(agent, text string) { t.emit(agent, acpdriver.Event{Type: acpdriver.EventMessage, Text: text}) },
		})
		if err == nil {
			server.SetSubagents(t)
			t.mu.Lock() // bindRun reads it on the UI goroutine while the lead connects
			t.server = server
			t.mu.Unlock()
		}
	})
}

// bindRun shows the chat's lead the run this Switch drives or watches, nil
// when nothing drives one, and lets Steer reach the workers of an engine
// running here.
func (t *team) bindRun(st *store.Store, run *store.Run, engine *execution.Engine) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.engine = engine
	server := t.server
	t.mu.Unlock()
	switch {
	case server == nil: // another Switch serves this project's chat
	case run == nil:
		server.Bind(nil, "", nil)
	case engine == nil: // detached: its engine polls the store
		server.Bind(st, run.ID, nil)
	default:
		server.Bind(st, run.ID, engine.Wake)
	}
}

// grant gives an agent's session its team tools: the MCP bridge and the env
// for `$SWITCH_BIN __control agent`.
func (t *team) grant(name string) ([]acp.McpServer, []string) {
	if t == nil || t.server == nil {
		return nil, nil
	}
	agent, _ := t.agent(name)
	g, err := t.server.Grant(store.RunAgent{Name: name, Role: agent.Role}, "")
	if err != nil {
		return nil, nil
	}
	return g.MCPServers, g.Env
}

func (t *team) agent(name string) (config.Agent, bool) {
	for _, agent := range t.fleet {
		if agent.Name == name {
			return agent, true
		}
	}
	return config.Agent{}, false
}

func (t *team) setLead(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.lead = name
	t.mu.Unlock()
}

// toWarm marks the agents to start in the background: everyone but the lead
// whose adapter is available and who is not already warm.
func (t *team) toWarm() []config.Agent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []config.Agent
	for _, agent := range t.fleet {
		if agent.Name == t.lead || live(t.warm[agent.Name]) || t.warming[agent.Name] || !adapterAvailable(agent.Driver) {
			continue
		}
		t.warming[agent.Name] = true
		out = append(out, agent)
	}
	return out
}

// park keeps a session warm for an agent, closing any session it replaces.
// One whose adapter has exited is closed instead.
func (t *team) park(name string, session *acpdriver.Session) {
	if t == nil || session == nil {
		return
	}
	t.mu.Lock()
	if !live(session) {
		delete(t.warming, name)
		t.mu.Unlock()
		go session.Close(context.Background())
		return
	}
	old := t.warm[name]
	t.warm[name] = session
	delete(t.warming, name)
	t.mu.Unlock()
	if old != nil && old != session {
		go old.Close(context.Background())
	}
}

func (t *team) warmFailed(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.warming, name)
	t.mu.Unlock()
}

// take removes an agent's warm session for exclusive use, or returns nil.
func (t *team) take(name string) *acpdriver.Session {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.warm[name]
	delete(t.warm, name)
	if !live(session) {
		if session != nil {
			go session.Close(context.Background())
		}
		return nil
	}
drain: // what a turn that was handed off or abandoned left behind
	for {
		select {
		case <-session.Events:
		default:
			break drain
		}
	}
	return session
}

// startSession starts a session that outlives ctx, as the request or run that
// needs it ends long before it does, but gives up if ctx ends first.
func (t *team) startSession(ctx context.Context, name, driver, model string, grant func(string) ([]acp.McpServer, []string)) (*acpdriver.Session, error) {
	life, end := context.WithCancel(t.base)
	abort := context.AfterFunc(ctx, end) // ending life kills the adapter, which ends the start
	session, err := chat.StartSession(life, t.project, name, driver, model, grant)
	if !abort() {
		if err == nil {
			go session.Close(context.Background())
		}
		return nil, ctx.Err()
	}
	return session, err
}

// live reports whether a session is there and its adapter still runs.
func live(session *acpdriver.Session) bool {
	if session == nil {
		return false
	}
	select {
	case <-session.Done():
		return false
	default:
		return true
	}
}

func (t *team) status(name string) string {
	if t == nil {
		return "idle"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case name == t.lead:
		return "leading"
	case t.asked[name]:
		return "answering"
	case t.working[name]:
		return "working"
	case live(t.warm[name]):
		return "ready"
	case t.warming[name]:
		return "starting"
	default:
		return "idle"
	}
}

// setRules keeps teammates on the rules the lead has, such as without the
// project's once the user skips them.
func (t *team) setRules(set rules.Set) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.rules = set
	t.mu.Unlock()
}

// setRunTasks records whose run turn is running, a task's or the run lead's
// review: asking one would start a second session beside it, streaming into
// the same live view. The run's lead need not be the chat's.
func (t *team) setRunTasks(tasks []store.Task, agents []store.RunAgent) {
	if t == nil {
		return
	}
	working := map[string]bool{}
	for _, task := range tasks {
		switch task.Status {
		case execution.StatusRunning:
			working[task.Assignee] = true
		case execution.StatusReviewing:
			for _, agent := range agents {
				if agent.Role == "lead" {
					working[agent.Name] = true
				}
			}
		}
	}
	t.mu.Lock()
	t.working = working
	t.mu.Unlock()
}

// close stops every warm session in parallel and the team server.
func (t *team) close() {
	if t.stop != nil {
		t.stop() // frees emit and permit and ends answers in flight, which the server waits for
	}
	for _, agent := range t.fleet {
		t.stopSubs(agent.Name)
	}
	t.mu.Lock()
	sessions := t.warm
	t.warm = map[string]*acpdriver.Session{}
	t.mu.Unlock()
	var wg sync.WaitGroup
	for _, session := range sessions {
		wg.Add(1)
		go func(s *acpdriver.Session) {
			defer wg.Done()
			_ = s.Close(context.Background())
		}(session)
	}
	wg.Wait()
	if t.server != nil {
		_ = t.server.Close()
	}
}

// Members implements control.Team for switch_team.
func (t *team) Members(caller string) []control.Member {
	members := make([]control.Member, 0, len(t.fleet))
	for _, agent := range t.fleet {
		role := agent.Role
		if role == "lead" && agent.Name != t.leadName() {
			role = "standby lead"
		}
		members = append(members, control.Member{
			Name: agent.Name, Role: role, Driver: agent.Driver,
			Status: t.status(agent.Name), You: agent.Name == caller,
		})
	}
	return members
}

func (t *team) leadName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lead
}

// Lead implements control.Team, which refuses switch_progress from the lead.
func (t *team) Lead() string { return t.leadName() }

// Ask implements control.Team for switch_ask: it sends the message to the
// teammate's session (warm when possible) and returns the reply. A teammate
// being asked cannot ask others, which keeps consultations from looping.
func (t *team) Ask(ctx context.Context, from, to, message string) (string, error) {
	if to == from {
		return "", errors.New("you cannot ask yourself")
	}
	agent, ok := t.agent(to)
	if !ok {
		return "", fmt.Errorf("no teammate named %q; switch_team lists them", to)
	}
	t.mu.Lock()
	switch {
	case t.asked[from]:
		t.mu.Unlock()
		return "", errors.New("answer the question you were asked; teammates you consult cannot consult others")
	case to == t.lead:
		t.mu.Unlock()
		return "", fmt.Errorf("%s is the lead and is talking with the user; answer in your reply instead", to)
	case t.asked[to]:
		t.mu.Unlock()
		return "", fmt.Errorf("%s is answering another question; try again shortly", to)
	case t.working[to]:
		t.mu.Unlock()
		return "", fmt.Errorf("%s is working on a run task; ask again when it finishes", to)
	}
	t.asked[to] = true
	base := t.rules
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.asked, to)
		t.mu.Unlock()
	}()
	set, err := rules.Scoped(base, agent.Role, agent.Name, "")
	if err != nil {
		return "", fmt.Errorf("%s's rules: %w", to, err)
	}

	// The answer, a start included, ends before the control server gives up
	// on it, or when Switch quits.
	turnCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()
	defer context.AfterFunc(t.ctx, cancel)()
	session := t.take(to)
	if session == nil {
		if session, err = t.startSession(turnCtx, agent.Name, agent.Driver, agent.Model, t.grant); err != nil {
			return "", fmt.Errorf("start %s: %w", to, err)
		}
		t.resetUsage(to)
	}
	t.mu.Lock()
	t.active[to], t.turns[to] = session, turnCtx
	steered := t.steers[to] // taken once there is a session to send it, so a failed start keeps it
	delete(t.steers, to)
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.active, to)
		delete(t.turns, to)
		t.mu.Unlock()
		t.stopSubs(to) // they only serve this answer
	}()

	asker, decider := "Your teammate "+from, from
	if from == "user" { // an @mention typed in the chat
		asker, decider = "The user", "the user"
	}
	prompt := fmt.Sprintf("You are %s (%s) on a Switch team working in %s. %s asks:\n\n%s\n\n"+
		"Answer directly and concisely. Do not edit files; %s decides what happens next.", to, agent.Role, t.project, asker, message, decider)
	if t.subCfg.MaxPerBuilder > 0 {
		prompt += fmt.Sprintf("\n\nYou can start up to %d read-only subagents with switch_spawn to explore in parallel. "+
			"Collect their results with switch_wait before you answer; they stop when you answer.", t.subCfg.MaxPerBuilder)
	}
	prompt = steered + set.Prompt(prompt)
	t.emit(to, acpdriver.Event{Type: acpdriver.EventStart, Text: from + " asks: " + message})
	reply, err := session.RunTurn(turnCtx, prompt, acpdriver.TurnOptions{
		OnEvent:      func(event acpdriver.Event) { t.emit(to, event) },
		OnPermission: t.permit(to),
	})
	if err != nil && turnCtx.Err() != nil {
		// Its prompt may still be running, and the next ask would take the end of it as its answer.
		go session.Close(context.Background())
	} else {
		t.park(to, session) // stays warm for the next question or a lead switch
	}
	if err != nil {
		t.emit(to, acpdriver.Event{Type: acpdriver.EventError, Err: err})
		return "", fmt.Errorf("%s did not answer: %w", to, err)
	}
	return strings.TrimSpace(reply), nil
}

// Steer implements control.Team for /steer and switch_steer: it sends text to
// a teammate's answer or, through the engine, a worker's turn in the run this
// Switch drives. A teammate whose adapter cannot take it mid-turn gets it at
// the start of its next prompt, whatever it is asked: unlike a worker's, its
// turns serve no task to keep it for. The lead is never steered: the user
// talks to it.
func (t *team) Steer(ctx context.Context, to, text string) (string, error) {
	t.mu.Lock()
	lead, session, engine := t.lead, t.active[to], t.engine
	t.mu.Unlock()
	switch {
	case to == lead:
		return "", fmt.Errorf("%s is the lead, talking with the user; it is not steered", to)
	case session == nil && engine != nil:
		return engine.Steer(ctx, to, text)
	case session == nil:
		return "", fmt.Errorf("%s has no turn running in this Switch", to)
	}
	err := session.Steer(ctx, text)
	if err == nil {
		return to + " got it in its running turn", nil
	}
	t.mu.Lock()
	t.steers[to] += "Sent to you during your previous turn: " + text + "\n\n"
	t.mu.Unlock()
	return fmt.Sprintf("%s could not take it mid-turn (%v), so it gets it at the start of its next prompt", to, err), nil
}

// Question implements switch_question in the chat: a teammate's question
// waits among the user's permission prompts for as long as the answer that
// asked it goes on, as its token outlives that turn.
func (t *team) Question(ctx context.Context, agent, _, question string, options []string) string {
	t.mu.Lock()
	turn := t.turns[agent]
	t.mu.Unlock()
	if turn == nil {
		return "you have no turn running to take the user's answer; decide yourself"
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(turn, cancel)()
	return execution.AskUser(ctx, t.permit(agent), agent, question, options)
}

// describe is the team note prepended to the lead's messages, so it knows
// its teammates without calling a tool.
func (t *team) describe(name string) string {
	if t == nil || len(t.fleet) < 2 {
		return ""
	}
	var mates []string
	for _, m := range t.Members(name) {
		if !m.You {
			mates = append(mates, fmt.Sprintf("%s (%s, %s, %s)", m.Name, m.Role, m.Driver, m.Status))
		}
	}
	tools := "the switch_team and switch_ask tools"
	if t.server == nil {
		return fmt.Sprintf("[Switch] You are %s, the lead of a Switch team. Teammates: %s.", name, strings.Join(mates, "; ")) + t.limitNote(name)
	}
	return fmt.Sprintf("[Switch] You are %s, the lead of a Switch team. Teammates: %s. "+
		"These are your team, not your own subagents: consult one with %s (or `$SWITCH_BIN __control agent ask <name> <message>`). "+
		"The user starts team work with /plan and /run.", name, strings.Join(mates, "; "), tools) + t.limitNote(name)
}

// askedMsg is a teammate's answer to the user's @mention.
type askedMsg struct {
	to, reply string
	err       error
	gen       int
}

// mention splits "@name message" when name is a teammate other than the
// lead; anything else is a message for the lead.
func (m model) mention(text string) (to, message string, ok bool) {
	rest, at := strings.CutPrefix(text, "@")
	name, message, _ := strings.Cut(rest, " ")
	agent, found := m.fleetAgent(name)
	if !at || !found || agent.Name == m.lead.Name {
		return "", "", false
	}
	return agent.Name, strings.TrimSpace(message), true
}

// askTeammate sends an @mention the way the lead's switch_ask goes, without
// using the lead's context. Its answer joins the conversation when it comes;
// tab follows it live meanwhile.
func (m model) askTeammate(to, question string) (tea.Model, tea.Cmd) {
	if question == "" {
		m.notify("ask "+to, "type the question after the name")
		return m, nil
	}
	m.input.Reset()
	m.home = false
	m.setFocus("")
	addAside(&m.entries, entry{kind: entrySystem, text: "you asked " + to + ": " + question})
	m.refreshViewport()
	t, ctx, gen := m.team, m.ctx, m.gen
	return m, func() tea.Msg {
		reply, err := t.Ask(ctx, "user", to, question)
		return askedMsg{to: to, reply: reply, err: err, gen: gen}
	}
}

// completeMention is tab after a leading @: it completes a teammate's name
// as far as the names agree, and lists them when that adds nothing.
func (m *model) completeMention() bool {
	typed, at := strings.CutPrefix(m.input.Value(), "@")
	if !at || strings.Contains(typed, " ") {
		return false
	}
	var names []string
	for _, agent := range m.dashboardFleet() {
		if agent.Name != m.lead.Name && strings.HasPrefix(agent.Name, typed) {
			names = append(names, agent.Name)
		}
	}
	if len(names) == 0 {
		return false
	}
	common := names[0]
	for _, name := range names[1:] {
		for !strings.HasPrefix(name, common) {
			_, size := utf8.DecodeLastRuneInString(common)
			common = common[:len(common)-size]
		}
	}
	switch {
	case len(names) == 1:
		common += " "
	case common == typed:
		m.notify("teammates", strings.Join(names, " · "))
	}
	m.input.SetValue("@" + common)
	m.input.CursorEnd()
	return true
}

// limitNote tells the lead how full its context is, and to hand off once it
// passes the threshold.
func (t *team) limitNote(name string) string {
	limits := t.Limits(name)
	if limits.Percent() < 0 {
		return ""
	}
	note := fmt.Sprintf(" Your context window is %d%% full.", limits.Percent())
	if limits.Near() {
		to := limits.HandoffTo
		if to == "" {
			to = "a teammate you name"
		}
		note += fmt.Sprintf(" That is past the %d%% handoff point: answer this message, then call switch_handoff with a note "+
			"(what the user wants, what is done, what is left) so %s takes over as lead.", limits.HandoffAt, to)
	}
	return note
}
