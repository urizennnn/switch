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
}

type Server struct {
	Path string

	store    *store.Store
	runID    string
	exe      string
	onText   func(agent, text string)
	listener net.Listener
	created  os.FileInfo // the socket file this server made

	mu     sync.Mutex
	tokens map[string]identity
	blocks map[string]string // task ID -> reason
	wg     sync.WaitGroup
}

type Options struct {
	Store    *store.Store
	RunID    string
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
		Path: path, store: opts.Store, runID: opts.RunID, exe: exe, onText: opts.OnText,
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
	s.tokens[token] = identity{agent: agent.Name, role: agent.Role, taskID: taskID}
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
		_ = conn.SetDeadline(time.Now().Add(time.Minute))
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

func (s *Server) handle(req Request) (any, error) {
	s.mu.Lock()
	who, ok := s.tokens[req.Token]
	s.mu.Unlock()
	if !ok {
		return nil, errUnauthorized
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var params struct {
		Message string   `json:"message"`
		Title   string   `json:"title"`
		Brief   string   `json:"brief"`
		Role    string   `json:"role"`
		Deps    []string `json:"deps"`
		Verify  []string `json:"verify"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("bad params: %w", err)
		}
	}
	message := strings.TrimSpace(params.Message)

	switch req.Method {
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

	case "approve", "merge":
		return nil, errors.New("forbidden: only the user approves and merges work")

	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
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
	_ = s.store.AddEvent(ctx, store.Event{RunID: s.runID, TaskID: who.taskID, Agent: who.agent, Kind: kind, Message: message})
}
