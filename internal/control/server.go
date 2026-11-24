// Package control is the run control plane: a Unix socket through which
// agents report to Switch during a turn. Agents reach it with the internal
// agent CLI (`switch __control agent`) or the MCP bridge (`switch __control
// mcp`), which Switch offers to every agent session.
package control

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/execution"
	"github.com/urizennnn/switch/internal/store"
)

// Environment variables that carry the control-plane address and credential
// into agent processes.
const (
	EnvSocket = "SWITCH_SOCKET"
	EnvToken  = "SWITCH_TOKEN"
	EnvBin    = "SWITCH_BIN" // for `$SWITCH_BIN __control agent ...`
)

type Request struct {
	Token  string          `json:"token"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// identity is who a token speaks for.
type identity struct {
	agent  string
	role   string
	taskID string
	id     string          // the run agent or subagent ID
	turn   context.Context // ends when the token is revoked, with its turn
}

type Server struct {
	Path string

	store    *store.Store
	runID    string
	team     Team
	exe      string
	onText   func(agent, text string)
	listener net.Listener
	created  os.FileInfo        // the socket file this server made
	ctx      context.Context    // ends when the server closes
	stop     context.CancelFunc // ends ctx

	mu        sync.Mutex
	subagents Subagents
	tokens    map[string]identity
	blocks    map[string]string      // task ID -> reason
	asks      map[string]*pendingAsk // asker and teammate -> the question
	bound     boundRun               // the run the chat's lead sees, set by Bind
	wg        sync.WaitGroup

	delegating sync.Mutex // delegate numbers tasks by count, so one call at a time
}

// boundRun is a run the control plane answers about: a run server's own, or
// the one the user runs while chatting, in the foreground or detached.
type boundRun struct {
	store *store.Store
	id    string
	wake  func() // tells the engine driving it, when that runs in this process
}

var (
	anyone  = []string{"lead", "worker", "teammate", "subagent"}
	workers = []string{"worker", "teammate"} // whoever works on what it was given
)

// permissions says who may call each method: the "lead", a "subagent", and
// any other agent, a "worker" in a run and a "teammate" in the chat (standby
// leads too). A run's lead is also a worker, on a task handed to it; the
// chat's lead talks with the user and works on nothing itself. Handlers check
// the rest, such as whose task it is and whether a run or the chat serves the
// call. The MCP bridge offers each agent only the tools it may call and its
// server serves, in the chat those of both the lead and a teammate (whoami
// says why). approve and merge are nobody's.
var permissions = map[string][]string{
	"whoami": anyone, "team": anyone, "task": anyone, "claim": anyone, "limits": anyone,
	"progress": {"worker", "teammate", "subagent"}, "ask": {"lead", "worker", "teammate"},
	"complete": {"worker"}, "block": {"worker"}, "handoff": {"lead", "worker"},
	"spawn": workers, "wait": workers, "message": workers, "stop": workers, "subagents": workers, "question": workers,
	"report":   {"subagent"},
	"delegate": {"lead"}, "tasks": {"lead"}, "steer": {"lead"},
}

func (s *Server) allowed(method string, who identity) bool {
	roles := permissions[method]
	switch {
	case who.role == "subagent" || who.role == "lead" && s.team != nil:
		return slices.Contains(roles, who.role)
	case who.role == "lead":
		return slices.Contains(roles, "lead") || slices.Contains(roles, "worker")
	case s.team != nil:
		return slices.Contains(roles, "teammate")
	}
	return slices.Contains(roles, "worker")
}

// serves reports whether this kind of server can answer method: a run's has
// tasks but no team to ask, the chat's a team but no tasks of its own. The
// chat's lead sees the run the user runs, if any: those tools stay offered,
// since agents list them once, and refuse while no run is bound. Handlers
// refuse the rest with a hint; whoami leaves them out.
func (s *Server) serves(method string) bool {
	switch method {
	case "ask", "steer":
		return s.team != nil
	case "task", "claim", "complete", "block":
		return s.store != nil
	}
	return true
}

type Options struct {
	Store *store.Store // run mode: tasks and events of RunID
	RunID string       // the run, or any stable key for a team server
	// Team, when set, answers switch_team and switch_ask (chat mode).
	Team     Team
	StateDir string
	// OnText receives progress reports, as if the agent had streamed them.
	OnText func(agent, text string)
	// Executable is the switch binary agents run; defaults to os.Executable.
	Executable string
}

// Listen opens the run's socket, readable and writable only by this user.
func Listen(opts Options) (*Server, error) {
	exe := opts.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	path, err := socketPath(opts.StateDir, opts.RunID)
	if err != nil {
		return nil, err
	}
	// Replace only a stale socket from a crashed process; a live one belongs
	// to the process driving this run.
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return nil, fmt.Errorf("%w: its control socket is in use", execution.ErrRunBusy)
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	// The listener must not unlink the path on close: by then it may belong
	// to another process. Close removes it only if it is still ours.
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	created, err := os.Stat(path)
	if err != nil {
		listener.Close()
		return nil, err
	}
	s := &Server{
		Path: path, store: opts.Store, runID: opts.RunID, team: opts.Team, exe: exe, onText: opts.OnText,
		listener: listener, created: created, tokens: map[string]identity{}, blocks: map[string]string{},
		asks: map[string]*pendingAsk{},
	}
	s.ctx, s.stop = context.WithCancel(context.Background())
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// socketPath is <state>/runs/<id>/control.sock, or a temp path when that
// exceeds the Unix socket path limit (104 bytes on macOS).
func socketPath(stateDir, runID string) (string, error) {
	dir := execution.RunDir(stateDir, runID)
	path := filepath.Join(dir, "control.sock")
	if len(path) > 100 {
		// Keyed by the whole run directory: runs of different state dirs can share an id prefix.
		sum := sha256.Sum256([]byte(dir))
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("switch-%d-%x", os.Getuid(), sum[:8]))
		path = filepath.Join(dir, "control.sock")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// Close stops the server and ends the requests it is serving.
func (s *Server) Close() error {
	err := s.listener.Close()
	s.stop()
	s.wg.Wait()
	if current, statErr := os.Stat(s.Path); statErr == nil && os.SameFile(current, s.created) {
		_ = os.Remove(s.Path)
	}
	return err
}

// Grant issues a token for one agent turn and returns how to reach the
// control plane: the MCP bridge for the session and env for the agent's shell.
func (s *Server) Grant(agent store.RunAgent, taskID string) (execution.Grant, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return execution.Grant{}, err
	}
	token := hex.EncodeToString(buf)
	turn, end := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.tokens[token] = identity{agent: agent.Name, role: agent.Role, taskID: taskID, id: agent.ID, turn: turn}
	s.mu.Unlock()

	env := []string{EnvSocket + "=" + s.Path, EnvToken + "=" + token, EnvBin + "=" + s.exe}
	return execution.Grant{
		Env: env,
		MCPServers: []acp.McpServer{{Stdio: &acp.McpServerStdio{
			Name:    "switch",
			Command: s.exe,
			Args:    []string{"__control", "mcp"},
			Env:     []acp.EnvVariable{{Name: EnvSocket, Value: s.Path}, {Name: EnvToken, Value: token}},
		}}},
		Revoke: func() {
			end() // its questions to the user go with it
			s.mu.Lock()
			delete(s.tokens, token)
			s.mu.Unlock()
		},
	}, nil
}

// Subagents runs the helpers workers start for their tasks, hands work on
// when an agent runs out of room, and puts agents' questions to the user: the
// run engine implements it in a run, the team in the chat.
type Subagents interface {
	Spawn(parent, taskID string, req execution.SpawnRequest) (store.Subagent, error)
	Wait(ctx context.Context, parent, taskID string, names []string) ([]store.Subagent, error)
	Message(parent, taskID, name, text string) (string, error)
	Stop(parent, taskID, name string) (int, error)
	List(parent, taskID string) ([]store.Subagent, error)
	Report(id, text string) error
	RunningSubagents(taskID string) int
	Handoff(from, taskID, to, note string) (string, error)
	Limits(agent string) execution.Limits
	// Question waits until the user answers or ctx ends, and returns the
	// answer or what to do without one.
	Question(ctx context.Context, agent, taskID, question string, options []string) string
}

// SetSubagents lets workers start subagents through this server.
func (s *Server) SetSubagents(subagents Subagents) {
	s.mu.Lock()
	s.subagents = subagents
	s.mu.Unlock()
}

// Bind shows the chat's lead the run the user runs: switch_tasks lists its
// tasks and switch_delegate proposes tasks to it. wake, when set, tells the
// engine driving it. A nil store unbinds it.
func (s *Server) Bind(st *store.Store, runID string, wake func()) {
	s.mu.Lock()
	s.bound = boundRun{store: st, id: runID, wake: wake}
	s.mu.Unlock()
}

// TakeBlock returns and clears a block the task's worker reported.
func (s *Server) TakeBlock(taskID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason, ok := s.blocks[taskID]
	delete(s.blocks, taskID)
	return reason, ok
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// handleConn answers the one JSON request a client sends. The request ends
// when the server closes or the client hangs up, so Close does not wait on
// an idle client and nothing waits on behalf of one that left. A broken
// connection only ends that connection; the run carries on.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return
	}
	// Long enough for the slowest request.
	_ = conn.SetDeadline(time.Now().Add(time.Minute + waitBudget))
	go func() { // Call sends nothing more, so the read ends when the client hangs up
		_, _ = io.Copy(io.Discard, conn)
		cancel()
	}()
	var req Request
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		resp = Response{Error: "bad request: " + err.Error()}
	} else if result, err := s.handle(ctx, req); err != nil {
		resp = Response{Error: err.Error()}
	} else {
		resp = Response{OK: true, Result: result}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

var errUnauthorized = errors.New("unauthorized: unknown or revoked token")

// askTimeout bounds a teammate's answer to switch_ask, and the user's to
// switch_question.
const askTimeout = 10 * time.Minute

// waitBudget bounds one switch_wait, switch_ask or switch_question call,
// under the per-call timeout of MCP clients (Codex: 60s); the agent calls
// again to keep waiting. A var so tests can shorten it.
var waitBudget = 45 * time.Second

// Member is one agent as switch_team reports it.
type Member struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Driver string `json:"driver"`
	Status string `json:"status"`
	You    bool   `json:"you,omitempty"`
}

// Team is the agent roster behind switch_team, switch_ask and switch_steer
// in the chat.
type Team interface {
	Members(caller string) []Member
	// Ask sends a message from one agent to another and returns the reply.
	Ask(ctx context.Context, from, to, message string) (string, error)
	// Steer sends text to an agent's running turn, or to the start of its
	// next prompt when its adapter cannot take it mid-turn, and says which.
	Steer(ctx context.Context, to, text string) (string, error)
	// Lead is the agent talking with the user.
	Lead() string
}

// pendingAsk is a question a teammate or the user is answering for an asker.
type pendingAsk struct {
	message  string
	life     context.Context // the turn that asked; its answer goes with it
	done     chan struct{}   // closed once reply, err and finished are set
	reply    string
	err      error
	finished time.Time
}

// ask puts a question to a teammate or the user (to) with answer, which runs
// under life for up to askTimeout, or rejoins the asker's pending one, and
// waits for the answer until ctx ends. They answer on their own time, so a
// long answer takes several calls of tool instead of one that outlasts the
// asker's MCP client. An answer nobody collected waits one budget for the
// asker to call again; after that the question is asked anew.
func (s *Server) ask(ctx, life context.Context, tool, from, to, message string, answer func(context.Context) (string, error)) (any, error) {
	key := from + "\n" + to
	s.mu.Lock()
	p := s.asks[key]
	if p != nil {
		select {
		case <-p.done:
			if p.message != message || time.Since(p.finished) > waitBudget || p.life.Err() != nil {
				p = nil // the asker moved on, or its turn ended and a new turn asks anew
			}
		default:
			if p.message != message {
				s.mu.Unlock()
				return nil, fmt.Errorf("%s is still answering your earlier question; call %s again with that question to get the answer", to, tool)
			}
		}
	}
	if p == nil {
		p = &pendingAsk{message: message, life: life, done: make(chan struct{})}
		s.asks[key] = p
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(life, askTimeout)
			defer cancel()
			p.reply, p.err = answer(ctx)
			p.finished = time.Now()
			close(p.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-p.done:
		s.mu.Lock()
		if s.asks[key] == p && ctx.Err() == nil { // collected, unless the asker hung up
			delete(s.asks, key)
		}
		s.mu.Unlock()
		return p.reply, p.err
	case <-ctx.Done():
		return fmt.Sprintf("%s is still answering. Call %s again with the same question to get the answer.", to, tool), nil
	}
}

// runMembers reports a run's agents and what each is working on.
func (s *Server) runMembers(ctx context.Context, who identity) ([]Member, error) {
	if s.store == nil {
		return nil, errors.New("no team information")
	}
	agents, err := s.store.RunAgents(ctx, s.runID)
	if err != nil {
		return nil, err
	}
	tasks, err := s.store.TasksForRun(ctx, s.runID)
	if err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(agents))
	for _, agent := range agents {
		status := "idle"
		if agent.Role == "lead" {
			status = "reviews and merges with the user"
		}
		for _, task := range tasks {
			if task.Assignee == agent.Name && (task.Status == execution.StatusRunning || task.Status == execution.StatusVerifying) {
				status = task.Status + " " + task.Title
			}
		}
		members = append(members, Member{Name: agent.Name, Role: agent.Role, Driver: agent.Driver, Status: status, You: agent.Name == who.agent})
	}
	return members, nil
}

// handle answers a request. An agent near its context limit gets a reminder
// to hand off with every text result, since Switch cannot interrupt its turn.
func (s *Server) handle(ctx context.Context, req Request) (any, error) {
	result, err := s.answer(ctx, req)
	if text, ok := result.(string); ok && err == nil && req.Method != "handoff" {
		if note := s.limitWarning(req.Token); note != "" {
			result = text + "\n\n" + note
		}
	}
	return result, err
}

func (s *Server) limitWarning(token string) string {
	s.mu.Lock()
	who, ok := s.tokens[token]
	host := s.subagents
	s.mu.Unlock()
	if !ok || host == nil || who.role == "subagent" || s.team != nil && who.agent != s.team.Lead() {
		return "" // in the chat only the lead hands off
	}
	limits := host.Limits(who.agent)
	if !limits.Near() {
		return ""
	}
	to := limits.HandoffTo
	if to == "" {
		to = "a teammate"
	}
	return fmt.Sprintf("[Switch] Your context is %d%% full. Hand off before you run out: call switch_handoff with a note (what is done, what is left) so %s can continue.", limits.Percent(), to)
}

func (s *Server) answer(ctx context.Context, req Request) (any, error) {
	s.mu.Lock()
	who, ok := s.tokens[req.Token]
	subagents, run := s.subagents, s.bound
	s.mu.Unlock()
	if !ok {
		return nil, errUnauthorized
	}
	if s.team == nil {
		run = boundRun{store: s.store, id: s.runID}
	} else if who.role != "subagent" {
		// In the chat the lead is whoever talks with the user, whatever its
		// configured role, as for its rules.
		switch {
		case who.agent == s.team.Lead():
			who.role = "lead"
		case who.role == "lead":
			who.role = "standby lead"
		}
	}
	if roles, known := permissions[req.Method]; known && !s.allowed(req.Method, who) {
		return nil, fmt.Errorf("forbidden: %s is for %s, not %s", req.Method, strings.Join(roles, " or "), who.role)
	}
	timeout := 10 * time.Second
	switch req.Method {
	case "ask", "wait", "question":
		timeout = waitBudget
	case "stop":
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var params struct {
		Message    string   `json:"message"`
		To         string   `json:"to"`
		Title      string   `json:"title"`
		Brief      string   `json:"brief"`
		Role       string   `json:"role"`
		Deps       []string `json:"deps"`
		Acceptance []string `json:"acceptance"`
		Verify     []string `json:"verify"`
		Name       string   `json:"name"`
		Names      []string `json:"names"`
		Files      []string `json:"files"`
		Driver     string   `json:"driver"`
		Model      string   `json:"model"`
		Options    []string `json:"options"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("bad params: %w", err)
		}
	}
	message := strings.TrimSpace(params.Message)

	switch req.Method {
	case "team":
		if s.team != nil {
			return s.team.Members(who.agent), nil
		}
		return s.runMembers(ctx, who)

	case "ask":
		if s.team == nil {
			return nil, errors.New("switch_ask is available in the chat; during a run, report to the lead with switch_progress")
		}
		to := strings.TrimSpace(params.To)
		if message == "" || to == "" {
			return nil, errors.New("ask needs a teammate (to) and a message")
		}
		return s.ask(ctx, s.ctx, "switch_ask", who.agent, to, message, func(ctx context.Context) (string, error) {
			return s.team.Ask(ctx, who.agent, to, message)
		})

	case "question":
		switch {
		case message == "":
			return nil, errors.New("question needs a message: what to ask the user")
		case len(params.Options) > 9:
			return nil, errors.New("question takes at most 9 options; the user picks one by its number")
		case subagents == nil:
			return nil, errors.New("no run is active")
		}
		// Agent names have no spaces, so the user's questions never share a teammate's key.
		return s.ask(ctx, who.turn, "switch_question", who.agent, "The user", message+"\n"+strings.Join(params.Options, "\n"), func(ctx context.Context) (string, error) {
			return subagents.Question(ctx, who.agent, who.taskID, message, params.Options), nil
		})

	case "whoami":
		var methods []string
		for method, roles := range permissions {
			may := s.allowed(method, who)
			if s.team != nil && who.role != "subagent" {
				// Agents list their tools once, and in the chat the user can make
				// any of them the lead, or the lead a teammate again, within one
				// session: they keep both roles' tools, refused by role when called.
				may = slices.Contains(roles, "lead") || slices.Contains(roles, "teammate")
			}
			if may && s.serves(method) {
				methods = append(methods, method)
			}
		}
		slices.Sort(methods)
		return map[string]any{"agent": who.agent, "role": who.role, "task": who.taskID, "run": s.runID, "methods": methods}, nil

	case "task", "claim":
		task, err := s.ownTask(ctx, who)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"id": task.ID, "title": task.Title, "brief": task.Brief, "status": task.Status,
			"acceptance": task.Acceptance, "verify": task.Verify, "feedback": task.Feedback,
		}, nil

	case "progress":
		if message == "" {
			return nil, errors.New("progress needs a message")
		}
		s.event(ctx, who, "progress", message)
		if s.onText != nil {
			s.onText(who.agent, "[progress] "+message+"\n")
		}
		return "recorded", nil

	case "complete":
		task, err := s.workerTask(ctx, who)
		if err != nil {
			return nil, err
		}
		if subagents != nil {
			if n := subagents.RunningSubagents(task.ID); n > 0 {
				return nil, fmt.Errorf("%d of your subagents are still running; collect them with switch_wait or stop them with switch_stop first", n)
			}
		}
		task.Summary = message
		if err := s.store.SaveTask(ctx, task); err != nil {
			return nil, err
		}
		s.event(ctx, who, "complete", message)
		return "recorded; Switch verifies the task when your turn ends", nil

	case "block":
		if _, err := s.workerTask(ctx, who); err != nil {
			return nil, err
		}
		if message == "" {
			return nil, errors.New("block needs a reason")
		}
		s.mu.Lock()
		s.blocks[who.taskID] = message
		s.mu.Unlock()
		s.event(ctx, who, "block", message)
		return "recorded; end your turn and the task will be blocked for the user", nil

	case "delegate":
		return s.delegate(ctx, who, run, params.Title, params.Brief, params.Role, params.Deps, params.Acceptance, params.Verify)

	case "tasks":
		if run.store == nil {
			return nil, errors.New("no run is being driven right now; /run continues the latest run, /plan starts a new one")
		}
		return runTasks(ctx, run)

	case "steer":
		to := strings.TrimSpace(params.To)
		switch {
		case s.team == nil:
			return nil, errors.New("switch_steer is available in the chat")
		case run.store == nil:
			return nil, errors.New("switch_steer reaches agents while a run is active, and none is")
		case to == "" || message == "":
			return nil, errors.New("steer needs an agent (to) and a message")
		}
		return s.team.Steer(ctx, to, fmt.Sprintf("The lead (%s) says: %s", who.agent, message))

	case "spawn", "wait", "message", "stop", "subagents":
		return s.handleSubagents(ctx, subagents, who, req.Method, params.Name, params.Names, params.Brief, params.Files, params.Driver, params.Model, message)

	case "report":
		if subagents == nil {
			return nil, errors.New("no run is active")
		}
		if err := subagents.Report(who.id, message); err != nil {
			return nil, err
		}
		return "recorded; end your turn and " + strings.SplitN(who.agent, "/", 2)[0] + " gets your result", nil

	case "limits":
		if subagents == nil {
			return nil, errors.New("limits are known only in the chat or during a run")
		}
		limits := subagents.Limits(who.agent)
		out := map[string]any{"handoff_at_percent": limits.HandoffAt, "handoff_to": limits.HandoffTo}
		if p := limits.Percent(); p >= 0 {
			out["context_percent"], out["context_used"], out["context_size"] = p, limits.Used, limits.Size
		} else {
			out["context_percent"] = "unknown: your adapter has not reported context use"
		}
		return out, nil

	case "handoff":
		if subagents == nil {
			return nil, errors.New("handoffs happen in the chat or during a run")
		}
		return subagents.Handoff(who.agent, who.taskID, params.To, message)

	case "approve", "merge":
		return nil, errors.New("forbidden: only the user approves and merges work")

	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
}

// subagentView is a subagent as its worker sees it.
type subagentView struct {
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Driver  string   `json:"driver"`
	Files   []string `json:"files,omitempty"`
	Brief   string   `json:"brief"`
	Summary string   `json:"result,omitempty"`
}

func viewSubagents(subs []store.Subagent) []subagentView {
	views := make([]subagentView, 0, len(subs))
	for _, sa := range subs {
		views = append(views, subagentView{Name: sa.Name, Status: sa.Status, Driver: sa.Driver, Files: sa.Files, Brief: sa.Brief, Summary: sa.Summary})
	}
	return views
}

// handleSubagents serves the subagent tools of a worker, or of a lead that
// was handed a task.
func (s *Server) handleSubagents(ctx context.Context, subagents Subagents, who identity, method, name string, names []string, brief string, files []string, driver, model, message string) (any, error) {
	if subagents == nil {
		return nil, errors.New("subagents are available during a run, or in the chat while you answer a teammate's question")
	}
	// In a run, only the agent working on a running task may use them; in the
	// chat, the team allows a teammate that is answering a question.
	if s.team == nil {
		if _, err := s.workerTask(ctx, who); err != nil {
			return nil, err
		}
	}
	name = strings.TrimSpace(name)
	switch method {
	case "spawn":
		sa, err := subagents.Spawn(who.agent, who.taskID, execution.SpawnRequest{Name: name, Brief: brief, Files: files, Driver: acpdriver.DriverName(driver), Model: model})
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": sa.Name, "status": sa.Status, "next": "switch_wait collects its result"}, nil
	case "wait":
		subs, err := subagents.Wait(ctx, who.agent, who.taskID, names)
		if err != nil {
			return nil, err
		}
		var running []string
		for _, sa := range subs {
			if sa.Status == execution.SubRunning {
				running = append(running, sa.Name)
			}
		}
		if len(running) > 0 {
			return map[string]any{"subagents": viewSubagents(subs), "still_running": running, "next": "call switch_wait again to keep waiting"}, nil
		}
		return viewSubagents(subs), nil
	case "message":
		if name == "" {
			return nil, errors.New("message needs the subagent's name")
		}
		return subagents.Message(who.agent, who.taskID, name, message)
	case "stop":
		n, err := subagents.Stop(who.agent, who.taskID, name)
		if err != nil {
			return nil, err
		}
		return fmt.Sprintf("stopped %d", n), nil
	default:
		subs, err := subagents.List(who.agent, who.taskID)
		return viewSubagents(subs), err
	}
}

func (s *Server) ownTask(ctx context.Context, who identity) (store.Task, error) {
	if who.taskID == "" {
		return store.Task{}, errors.New("no task for this session")
	}
	task, ok, err := s.store.GetTask(ctx, who.taskID)
	if err != nil {
		return store.Task{}, err
	}
	if !ok {
		return store.Task{}, fmt.Errorf("task %s not found", who.taskID)
	}
	return task, nil
}

// workerTask is the caller's task while it works on it. Only the task's
// assignee may change it: a worker, or the lead when the task was handed to it.
func (s *Server) workerTask(ctx context.Context, who identity) (store.Task, error) {
	task, err := s.ownTask(ctx, who)
	if err != nil {
		return store.Task{}, err
	}
	if task.Assignee != who.agent || task.Status != execution.StatusRunning {
		return store.Task{}, errors.New("forbidden: you are not working on this task")
	}
	return task, nil
}

// delegate adds a task to the run, checked the way planning checks a plan's
// tasks: it needs acceptance criteria and a role some worker other than the
// lead has. It is a proposal, blocked until the user accepts it: from the
// chat, and from a run lead's task turn, which reads a worker's handoff note
// and must not queue verify commands for Switch to run unseen.
func (s *Server) delegate(ctx context.Context, who identity, run boundRun, title, brief, role string, deps, acceptance, verify []string) (any, error) {
	if run.store == nil {
		return nil, errors.New("switch_delegate adds tasks to a run, and none is being driven right now; suggest that the user continue the latest with /run or plan new work with /plan")
	}
	title, brief, role = strings.TrimSpace(title), strings.TrimSpace(brief), strings.TrimSpace(role)
	if title == "" || brief == "" || role == "" || len(acceptance) == 0 {
		return nil, errors.New("delegate needs title, brief, role, and at least one acceptance criterion")
	}
	if role == "lead" || role == "standby" {
		return nil, errors.New("tasks cannot be assigned to the lead or a standby lead")
	}
	agents, err := run.store.RunAgents(ctx, run.id)
	if err != nil {
		return nil, err
	}
	var roles []string
	for _, agent := range agents {
		if agent.Role != "lead" && agent.Role != "standby" && !slices.Contains(roles, agent.Role) {
			roles = append(roles, agent.Role)
		}
	}
	if !slices.Contains(roles, role) {
		return nil, fmt.Errorf("no worker in this run has role %q; use one of: %s", role, strings.Join(roles, ", "))
	}

	s.delegating.Lock()
	defer s.delegating.Unlock()
	tasks, err := run.store.TasksForRun(ctx, run.id)
	if err != nil {
		return nil, err
	}
	known := map[string]string{}
	for _, task := range tasks {
		known[task.ID] = task.ID
		known[planID(task.ID)] = task.ID
	}
	ids := make([]string, 0, len(deps))
	for _, dep := range deps {
		id, ok := known[dep]
		if !ok {
			return nil, fmt.Errorf("unknown dependency %q", dep)
		}
		ids = append(ids, id)
	}
	// The chat's proposals are numbered apart, as the run's own server may
	// delegate to the same run at the same time.
	kind := "delegated"
	if s.team != nil {
		kind = "proposed"
	}
	n := len(tasks) + 1
	for known[fmt.Sprintf("%s:%s-%d", run.id, kind, n)] != "" { // SaveTask would overwrite it
		n++
	}
	id := fmt.Sprintf("%s:%s-%d", run.id, kind, n)
	task := store.Task{ID: id, RunID: run.id, Title: title, Brief: brief, Role: role, Deps: ids, Acceptance: acceptance, Verify: verify, Status: execution.StatusQueued}
	result := map[string]string{"id": id}
	if s.team != nil || who.taskID != "" {
		where := "in the chat"
		if who.taskID != "" {
			where = "on " + planID(who.taskID)
		}
		task.Status = execution.StatusBlocked
		task.LastError = fmt.Sprintf("%s%s %s; /retry %s accepts it", execution.ProposedBy, who.agent, where, planID(id))
		result["next"] = "it waits, blocked, until the user accepts it: tell them /retry " + planID(id) + " does"
	}
	if err := run.store.SaveTask(ctx, task); err != nil {
		return nil, err
	}
	_ = run.store.AddEvent(ctx, store.Event{RunID: run.id, TaskID: who.taskID, Agent: who.agent, Kind: "delegate", Message: id + ": " + title})
	if run.wake != nil {
		run.wake()
	}
	return result, nil
}

// taskView is a task as switch_tasks shows it.
type taskView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Assignee  string `json:"assignee,omitempty"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"` // its first line
	Waits     string `json:"waits,omitempty"`      // why an unfinished task is not under way
}

// runTasks is the run's tasks in plan order, with what each unfinished one
// waits for.
func runTasks(ctx context.Context, run boundRun) ([]taskView, error) {
	tasks, err := run.store.TasksForRun(ctx, run.id)
	if err != nil {
		return nil, err
	}
	agents, err := run.store.RunAgents(ctx, run.id)
	if err != nil {
		return nil, err
	}
	info, _, err := run.store.GetRun(ctx, run.id)
	if err != nil {
		return nil, err
	}
	why := execution.Explain(tasks, agents, info.Status != execution.RunBudget)
	views := make([]taskView, 0, len(tasks))
	for _, task := range tasks {
		lastError, _, _ := strings.Cut(task.LastError, "\n")
		views = append(views, taskView{ID: planID(task.ID), Title: task.Title, Status: task.Status, Assignee: task.Assignee,
			Attempts: task.Attempts, LastError: lastError, Waits: why[task.ID]})
	}
	return views, nil
}

// planID strips the run prefix that planning adds to task IDs.
func planID(id string) string {
	return id[strings.LastIndexByte(id, ':')+1:]
}

func (s *Server) event(ctx context.Context, who identity, kind, message string) {
	if s.store == nil {
		return
	}
	_ = s.store.AddEvent(ctx, store.Event{RunID: s.runID, TaskID: who.taskID, Agent: who.agent, Kind: kind, Message: message})
}
