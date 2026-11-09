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

	acp "github.com/coder/acp-go-sdk"
	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/control"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
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
	subs    map[string]*chatSub           // subagents of teammates, by worker/name

	// Set by the TUI before the team is used.
	ctx    context.Context
	events chan<- streamMsg                    // live views of teammates and their subagents
	perms  chan<- *acpdriver.PermissionRequest // actions that need the user's answer
	subCfg config.SubagentConfig
}

func newTeam(project string, fleet []config.Agent, lead string) *team {
	return &team{
		project: project, fleet: fleet, lead: lead,
		warm: map[string]*acpdriver.Session{}, warming: map[string]bool{}, asked: map[string]bool{},
		active: map[string]*acpdriver.Session{}, subs: map[string]*chatSub{}, ctx: context.Background(),
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
		})
		if err == nil {
			server.SetSubagents(t)
			t.server = server
		}
	})
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
		if agent.Name == t.lead || t.warm[agent.Name] != nil || t.warming[agent.Name] || !adapterAvailable(agent.Driver) {
			continue
		}
		t.warming[agent.Name] = true
		out = append(out, agent)
	}
	return out
}

// park keeps a session warm for an agent, closing any session it replaces.
func (t *team) park(name string, session *acpdriver.Session) {
	if t == nil || session == nil {
		return
	}
	t.mu.Lock()
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
	return session
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
	case t.warm[name] != nil:
		return "ready"
	case t.warming[name]:
		return "starting"
	default:
		return "idle"
	}
}

// close stops every warm session in parallel and the team server.
func (t *team) close() {
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
	}
	t.asked[to] = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.asked, to)
		t.mu.Unlock()
	}()

	session := t.take(to)
	if session == nil {
		var err error
		if session, err = chat.StartSession(ctx, t.project, agent.Name, agent.Driver, agent.Model, t.grant); err != nil {
			return "", fmt.Errorf("start %s: %w", to, err)
		}
	}
	defer t.park(to, session) // stays warm for the next question or a lead switch
	t.mu.Lock()
	t.active[to] = session
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.active, to)
		t.mu.Unlock()
		t.stopSubs(to) // they only serve this answer
	}()

	prompt := fmt.Sprintf("You are %s (%s) on a Switch team working in %s. Your teammate %s asks:\n\n%s\n\n"+
		"Answer directly and concisely. Do not edit files; %s decides what happens next.", to, agent.Role, t.project, from, message, from)
	if t.subCfg.MaxPerBuilder > 0 {
		prompt += fmt.Sprintf("\n\nYou can start up to %d read-only subagents with switch_spawn to explore in parallel. "+
			"Collect their results with switch_wait before you answer; they stop when you answer.", t.subCfg.MaxPerBuilder)
	}
	t.emit(to, acpdriver.Event{Type: acpdriver.EventStart, Text: from + " asks: " + message})
	turnCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()
	reply, err := session.RunTurn(turnCtx, prompt, acpdriver.TurnOptions{
		OnEvent:      func(event acpdriver.Event) { t.emit(to, event) },
		OnPermission: t.permit(to),
	})
	if err != nil {
		t.emit(to, acpdriver.Event{Type: acpdriver.EventError, Err: err})
	}
	if err != nil {
		return "", fmt.Errorf("%s did not answer: %w", to, err)
	}
	return strings.TrimSpace(reply), nil
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
		return fmt.Sprintf("[Switch] You are %s, the lead of a Switch team. Teammates: %s.", name, strings.Join(mates, "; "))
	}
	return fmt.Sprintf("[Switch] You are %s, the lead of a Switch team. Teammates: %s. "+
		"These are your team, not your own subagents: consult one with %s (or `$SWITCH_BIN __control agent ask <name> <message>`). "+
		"The user starts team work with /plan and /run.", name, strings.Join(mates, "; "), tools)
}
