// Package control is the run control plane: a Unix socket through which
// agents report to Switch during a turn. Agents reach it with the internal
// agent CLI (`switch __control agent`) or the MCP bridge (`switch __control
// mcp`), which Switch offers to every agent session.
package control

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
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
	id     string // the run agent or subagent ID
}

type Server struct {
	Path string

	store    *store.Store
	runID    string
	team     Team
	exe      string
	onText   func(agent, text string)
	listener net.Listener
	created  os.FileInfo // the socket file this server made

	mu        sync.Mutex
	subagents Subagents
	tokens    map[string]identity
	blocks    map[string]string // task ID -> reason
	wg        sync.WaitGroup
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
	}
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
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("switch-%d-%s", os.Getuid(), filepath.Base(dir)))
		path = filepath.Join(dir, "control.sock")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Server) Close() error {
	err := s.listener.Close()
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
	s.mu.Lock()
	s.tokens[token] = identity{agent: agent.Name, role: agent.Role, taskID: taskID, id: agent.ID}
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
	}, nil
}

// Subagents runs the helpers workers start for their tasks and hands work on
// when an agent runs out of room: the run engine implements it in a run, the
// team in the chat.
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
}

// SetSubagents lets workers start subagents through this server.
func (s *Server) SetSubagents(subagents Subagents) {
	s.mu.Lock()
	s.subagents = subagents
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

// handleConn answers one JSON request per line. A broken connection only
// ends that connection; the run carries on.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	encoder := json.NewEncoder(conn)
	for scanner.Scan() {
		// Long enough for switch_ask, which waits for a teammate's answer.
		_ = conn.SetDeadline(time.Now().Add(askTimeout + time.Minute))
		var req Request
		var resp Response
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			resp = Response{Error: "bad request: " + err.Error()}
		} else if result, err := s.handle(req); err != nil {
			resp = Response{Error: err.Error()}
		} else {
			resp = Response{OK: true, Result: result}
		}
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

var errUnauthorized = errors.New("unauthorized: unknown or revoked token")

// askTimeout bounds one switch_ask round trip.
const askTimeout = 10 * time.Minute

// Member is one agent as switch_team reports it.
type Member struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Driver string `json:"driver"`
	Status string `json:"status"`
	You    bool   `json:"you,omitempty"`
}

// Team is the agent roster behind switch_team and switch_ask in the chat.
type Team interface {
	Members(caller string) []Member
	// Ask sends a message from one agent to another and returns the reply.
	Ask(ctx context.Context, from, to, message string) (string, error)
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
func (s *Server) handle(req Request) (any, error) {
	result, err := s.answer(req)
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
	if !ok || host == nil || who.role == "subagent" {
		return ""
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

func (s *Server) answer(req Request) (any, error) {
	s.mu.Lock()
	who, ok := s.tokens[req.Token]
	subagents := s.subagents
	s.mu.Unlock()
	if !ok {
		return nil, errUnauthorized
	}
	timeout := 10 * time.Second
	switch req.Method {
	case "ask", "wait":
		timeout = askTimeout
	case "stop":
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var params struct {
		Message string   `json:"message"`
		To      string   `json:"to"`
		Title   string   `json:"title"`
		Brief   string   `json:"brief"`
		Role    string   `json:"role"`
		Deps    []string `json:"deps"`
		Verify  []string `json:"verify"`
		Name    string   `json:"name"`
		Names   []string `json:"names"`
		Files   []string `json:"files"`
		Driver  string   `json:"driver"`
		Model   string   `json:"model"`
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
		if message == "" || strings.TrimSpace(params.To) == "" {
			return nil, errors.New("ask needs a teammate (to) and a message")
		}
		return s.team.Ask(ctx, who.agent, strings.TrimSpace(params.To), message)

	case "whoami":
		return map[string]string{"agent": who.agent, "role": who.role, "task": who.taskID, "run": s.runID}, nil

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
		if who.role != "lead" {
			return nil, errors.New("forbidden: only the lead can delegate tasks")
		}
		return s.delegate(ctx, who, params.Title, params.Brief, params.Role, params.Deps, params.Verify)

	case "spawn", "wait", "message", "stop", "subagents":
		return s.handleSubagents(ctx, subagents, who, req.Method, params.Name, params.Names, params.Brief, params.Files, params.Driver, params.Model, message)

	case "report":
		if who.role != "subagent" {
			return nil, errors.New("switch_report is for subagents; workers report with switch_complete")
		}
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
		if who.role == "subagent" {
			return nil, errors.New("subagents report to their worker with switch_report; they do not hand off")
		}
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

// handleSubagents serves a worker's subagent tools. Only the worker assigned
// to a running task may use them; the lead and subagents may not.
func (s *Server) handleSubagents(ctx context.Context, subagents Subagents, who identity, method, name string, names []string, brief string, files []string, driver, model, message string) (any, error) {
	switch who.role {
	case "lead":
		return nil, errors.New("forbidden: the lead runs workers, not subagents; delegate a task instead")
	case "subagent":
		return nil, errors.New("forbidden: subagents cannot start or manage subagents")
	}
	if subagents == nil {
		return nil, errors.New("subagents are available during a run, or in the chat while you answer a teammate's question")
	}
	// In a run, only the worker assigned to a running task may use them; in
	// the chat, the team allows a teammate that is answering a question.
	if s.team == nil {
		if _, err := s.workerTask(ctx, who); err != nil {
			return nil, err
		}
	}
	name = strings.TrimSpace(name)
	switch method {
	case "spawn":
		sa, err := subagents.Spawn(who.agent, who.taskID, execution.SpawnRequest{Name: name, Brief: brief, Files: files, Driver: driver, Model: model})
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": sa.Name, "status": sa.Status, "next": "switch_wait collects its result"}, nil
	case "wait":
		subs, err := subagents.Wait(ctx, who.agent, who.taskID, names)
		return viewSubagents(subs), err
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

// workerTask is the caller's task, which only its assigned worker may change.
func (s *Server) workerTask(ctx context.Context, who identity) (store.Task, error) {
	if who.role == "lead" {
		return store.Task{}, errors.New("forbidden: the lead reviews tasks; it does not report on them")
	}
	task, err := s.ownTask(ctx, who)
	if err != nil {
		return store.Task{}, err
	}
	if task.Assignee != who.agent {
		return store.Task{}, errors.New("forbidden: this task is assigned to another agent")
	}
	return task, nil
}

func (s *Server) delegate(ctx context.Context, who identity, title, brief, role string, deps, verify []string) (any, error) {
	title, brief, role = strings.TrimSpace(title), strings.TrimSpace(brief), strings.TrimSpace(role)
	if title == "" || brief == "" || role == "" {
		return nil, errors.New("delegate needs title, brief, and role")
	}
	if role == "lead" {
		return nil, errors.New("tasks cannot be assigned to the lead")
	}
	tasks, err := s.store.TasksForRun(ctx, s.runID)
	if err != nil {
		return nil, err
	}
	known := map[string]string{}
	for _, task := range tasks {
		known[task.ID] = task.ID
		known[task.ID[strings.LastIndexByte(task.ID, ':')+1:]] = task.ID
	}
	ids := make([]string, 0, len(deps))
	for _, dep := range deps {
		id, ok := known[dep]
		if !ok {
			return nil, fmt.Errorf("unknown dependency %q", dep)
		}
		ids = append(ids, id)
	}
	id := fmt.Sprintf("%s:delegated-%d", s.runID, len(tasks)+1)
	task := store.Task{ID: id, RunID: s.runID, Title: title, Brief: brief, Role: role, Deps: ids, Verify: verify, Status: execution.StatusQueued}
	if err := s.store.SaveTask(ctx, task); err != nil {
		return nil, err
	}
	s.event(ctx, who, "delegate", id+": "+title)
	return map[string]string{"id": id}, nil
}

func (s *Server) event(ctx context.Context, who identity, kind, message string) {
	if s.store == nil {
		return
	}
	_ = s.store.AddEvent(ctx, store.Event{RunID: s.runID, TaskID: who.taskID, Agent: who.agent, Kind: kind, Message: message})
}
