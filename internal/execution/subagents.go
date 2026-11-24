package execution

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// Subagents are helpers a worker starts for its own task. They run in the
// worker's worktree, report only to that worker, and cannot start subagents
// of their own. A worker's task cannot complete while any of them runs, and
// any still running when the worker's turn ends are stopped.

// Subagent statuses.
const (
	SubRunning     = "running"
	SubDone        = "done"
	SubFailed      = "failed"
	SubStopped     = "stopped"
	SubInterrupted = "interrupted"
)

// SpawnRequest is what a worker asks for in switch_spawn.
type SpawnRequest struct {
	Name   string
	Brief  string
	Files  []string // files the subagent may edit; none means read-only
	Driver string   // defaults to the worker's adapter
	Model  string
}

// subagent is the live state of one running subagent.
type subagent struct {
	rec    store.Subagent
	cancel context.CancelFunc
	inbox  []string      // follow-ups delivered when the current turn ends
	report string        // its switch_report result
	done   chan struct{} // closed when it stops running
}

var subagentName = regexp.MustCompile(`[^a-z0-9-]+`)

var errBudget = errors.New("the run budget is used up (max_turns or max_tokens); finish the task yourself")

// Spawn starts a subagent for the worker's running task.
func (e *Engine) Spawn(parent, taskID string, req SpawnRequest) (store.Subagent, error) {
	limit := e.opts.Config.Subagents.MaxPerBuilder
	if limit <= 0 {
		return store.Subagent{}, errors.New("subagents are turned off for this project ([run.subagents] max_per_builder)")
	}
	brief := strings.TrimSpace(req.Brief)
	if brief == "" {
		return store.Subagent{}, errors.New("spawn needs a brief")
	}
	ctx, cancel := saveContext()
	defer cancel()
	task, worker, slot, err := e.workerTask(ctx, parent, taskID)
	if err != nil {
		return store.Subagent{}, err
	}
	worker.Driver = acpdriver.DriverName(worker.Driver) // a run planned by an older build stored it as configured
	driver, model := strings.TrimSpace(req.Driver), strings.TrimSpace(req.Model)
	if driver == "" {
		driver = worker.Driver
	}
	if model == "" && driver == worker.Driver {
		model = worker.Model
	}
	if allowed := e.opts.Config.Subagents.Drivers; len(allowed) > 0 && !slices.Contains(allowed, driver) {
		return store.Subagent{}, fmt.Errorf("subagents may use %s; %s is not allowed ([run.subagents] drivers)", strings.Join(allowed, ", "), driver)
	}
	if ok, err := e.budgetLeft(); err != nil || !ok {
		return store.Subagent{}, errBudget
	}
	files := make([]string, 0, len(req.Files))
	for _, file := range req.Files {
		if file = strings.TrimSpace(file); file != "" {
			file = filepath.Clean(file)
			if filepath.IsAbs(file) || file == ".." || strings.HasPrefix(file, ".."+string(filepath.Separator)) {
				return store.Subagent{}, fmt.Errorf("file claim %q must be inside the task worktree and relative to its root", file)
			}
			files = append(files, file)
		}
	}

	e.subMu.Lock()
	defer e.subMu.Unlock()
	if e.runCtx == nil {
		return store.Subagent{}, errors.New("subagents run only while the run is active")
	}
	existing, err := e.opts.Store.SubagentsForRun(ctx, e.run.ID)
	if err != nil {
		return store.Subagent{}, err
	}
	if err := e.room(taskID, files); err != nil {
		return store.Subagent{}, err
	}

	name := uniqueName(subagentName.ReplaceAllString(strings.ToLower(strings.TrimSpace(req.Name)), "-"), taskID, existing)
	rec := store.Subagent{
		ID: taskID + "/" + name, RunID: e.run.ID, TaskID: taskID, Parent: parent, Name: name,
		Driver: driver, Model: model, Brief: brief, Files: files, Status: SubRunning,
	}
	if err := e.opts.Store.SaveSubagent(ctx, rec); err != nil {
		return store.Subagent{}, err
	}
	sa := &subagent{rec: rec}
	e.subs[rec.ID] = sa
	env := e.env(taskID, slot)
	e.startSubagent(sa, task, worker, env, firstLine(brief), SubagentPrompt(task, rec, rules.Subagent(e.opts.Rules, worker.Role, worker.Name, planID(task.ID)))+envNote(env))
	e.log(ctx, taskID, parent, "subagent", "started "+name+": "+firstLine(brief))
	return rec, nil
}

// room checks that one more subagent claiming files may run for the task:
// under the limit, and editing no file a running sibling claims. Callers hold
// subMu.
func (e *Engine) room(taskID string, files []string) error {
	running := 0
	for _, sa := range e.subs {
		if sa.rec.TaskID != taskID || sa.rec.Status != SubRunning {
			continue
		}
		running++
		if file, ok := overlap(files, sa.rec.Files); ok {
			return fmt.Errorf("%s already edits %s; give each subagent different files", sa.rec.Name, file)
		}
	}
	if running >= e.opts.Config.Subagents.MaxPerBuilder {
		return fmt.Errorf("you already have %d subagents running, the limit; wait for one with switch_wait or stop one", running)
	}
	return nil
}

// startSubagent runs turns until the subagent has no follow-up waiting, with
// its worker's env. Callers hold subMu.
func (e *Engine) startSubagent(sa *subagent, task store.Task, worker store.RunAgent, env []string, label, prompt string) {
	runCtx := e.runCtx // Run clears e.runCtx when it returns, which this may outlive
	ctx, cancel := context.WithCancel(runCtx)
	sa.cancel, sa.done = cancel, make(chan struct{})
	sa.rec.Status = SubRunning
	agent := store.RunAgent{ID: sa.rec.ID, RunID: e.run.ID, Name: sa.rec.Parent + "/" + sa.rec.Name, Role: "subagent", Driver: sa.rec.Driver, Model: sa.rec.Model}
	go func() {
		defer cancel()
		for {
			turnCtx, stop := context.WithTimeout(ctx, time.Duration(e.opts.Config.TurnTimeout)*time.Second)
			result, err := e.opts.Turn(turnCtx, TurnRequest{
				Label: label,
				Agent: agent, TaskID: task.ID, Cwd: e.dir(task.Worktree), SessionID: sa.rec.SessionID,
				Prompt: prompt, Permission: e.opts.Permission, Ask: e.opts.OnPermission != nil, Env: env,
			})
			stop()
			e.recordTurn(task, agent, result)

			e.subMu.Lock()
			if result.SessionID != "" {
				sa.rec.SessionID = result.SessionID
			}
			switch {
			case ctx.Err() != nil && runCtx.Err() != nil:
				sa.rec.Status = SubInterrupted
			case ctx.Err() != nil:
				sa.rec.Status = SubStopped
			case err != nil:
				sa.rec.Status, sa.rec.Summary = SubFailed, err.Error()
			case len(sa.inbox) > 0:
				prompt, sa.inbox = strings.Join(sa.inbox, "\n\n"), nil
				label = sa.rec.Parent + ": " + firstLine(prompt)
				e.subMu.Unlock()
				continue
			default:
				sa.rec.Status, sa.rec.Summary = SubDone, strings.TrimSpace(result.Output)
				if sa.report != "" {
					sa.rec.Summary = sa.report
				}
			}
			rec := sa.rec
			close(sa.done)
			e.subMu.Unlock()

			saveCtx, cancelSave := saveContext()
			_ = e.opts.Store.SaveSubagent(saveCtx, rec)
			e.log(saveCtx, task.ID, rec.Parent, "subagent", rec.Name+" "+rec.Status)
			cancelSave()
			return
		}
	}()
}

// Message sends a follow-up. A running subagent gets it when its current turn
// ends; a finished one starts again in the same session.
func (e *Engine) Message(parent, taskID, name, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("message needs text")
	}
	if ok, err := e.budgetLeft(); err != nil || !ok { // either way it costs another turn
		return "", errBudget
	}
	ctx, cancel := saveContext()
	defer cancel()
	task, worker, slot, err := e.workerTask(ctx, parent, taskID)
	if err != nil {
		return "", err
	}
	e.subMu.Lock()
	defer e.subMu.Unlock()
	sa, err := e.ownSubagent(parent, taskID, name)
	if err != nil {
		return "", err
	}
	if sa.rec.Status == SubRunning {
		sa.inbox = append(sa.inbox, text)
		return name + " gets this when its current turn ends", nil
	}
	if e.runCtx == nil || e.runCtx.Err() != nil {
		return "", errors.New("the run is not active")
	}
	if err := e.room(taskID, sa.rec.Files); err != nil {
		return "", err
	}
	sa.report = ""
	e.startSubagent(sa, task, worker, e.env(taskID, slot), parent+": "+firstLine(text), text)
	_ = e.opts.Store.SaveSubagent(ctx, sa.rec)
	e.log(ctx, taskID, parent, "subagent", name+" resumed")
	return name + " is working on it", nil
}

// Stop stops one running subagent, or all of the task's when name is empty,
// and waits for them to end.
func (e *Engine) Stop(parent, taskID, name string) (int, error) {
	e.subMu.Lock()
	var stopping []*subagent
	for _, sa := range e.subs {
		if sa.rec.TaskID == taskID && sa.rec.Parent == parent && sa.rec.Status == SubRunning && (name == "" || sa.rec.Name == name) {
			stopping = append(stopping, sa)
		}
	}
	if name != "" && len(stopping) == 0 {
		_, err := e.ownSubagent(parent, taskID, name)
		e.subMu.Unlock()
		if err != nil {
			return 0, err
		}
		return 0, nil // already finished
	}
	for _, sa := range stopping {
		sa.cancel()
	}
	e.subMu.Unlock()
	for _, sa := range stopping {
		select {
		case <-sa.done:
		case <-time.After(15 * time.Second):
		}
	}
	return len(stopping), nil
}

// Wait blocks until the named subagents (or all of the task's) have stopped
// running, or ctx ends, and returns their state.
func (e *Engine) Wait(ctx context.Context, parent, taskID string, names []string) ([]store.Subagent, error) {
	e.subMu.Lock()
	var waiting []chan struct{}
	for _, name := range names {
		if _, err := e.ownSubagent(parent, taskID, name); err != nil {
			e.subMu.Unlock()
			return nil, err
		}
	}
	for _, sa := range e.subs {
		if sa.rec.TaskID == taskID && sa.rec.Parent == parent && sa.rec.Status == SubRunning && (len(names) == 0 || slices.Contains(names, sa.rec.Name)) {
			waiting = append(waiting, sa.done)
		}
	}
	e.subMu.Unlock()
	for _, done := range waiting {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	subs, err := e.List(parent, taskID)
	if err != nil || len(names) == 0 {
		return subs, err
	}
	return slices.DeleteFunc(subs, func(sa store.Subagent) bool { return !slices.Contains(names, sa.Name) }), nil
}

// List is the task's subagents, including ones from earlier attempts.
func (e *Engine) List(parent, taskID string) ([]store.Subagent, error) {
	ctx, cancel := saveContext()
	defer cancel()
	all, err := e.opts.Store.SubagentsForRun(ctx, e.run.ID)
	if err != nil {
		return nil, err
	}
	e.subMu.Lock()
	defer e.subMu.Unlock()
	var subs []store.Subagent
	for _, sa := range all {
		if sa.TaskID != taskID || sa.Parent != parent {
			continue
		}
		if live, ok := e.subs[sa.ID]; ok {
			sa = live.rec // the store lags a turn that is still ending
		}
		subs = append(subs, sa)
	}
	return subs, nil
}

// Report records a subagent's result for its worker.
func (e *Engine) Report(id, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("report needs your result")
	}
	e.subMu.Lock()
	defer e.subMu.Unlock()
	sa, ok := e.subs[id]
	if !ok {
		return errors.New("no running subagent for this session")
	}
	sa.report = text
	return nil
}

// RunningSubagents counts a task's running subagents.
func (e *Engine) RunningSubagents(taskID string) int {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	n := 0
	for _, sa := range e.subs {
		if sa.rec.TaskID == taskID && sa.rec.Status == SubRunning {
			n++
		}
	}
	return n
}

// ownSubagent finds the worker's subagent by name, also a finished one an
// earlier Switch process ran. Callers hold subMu.
func (e *Engine) ownSubagent(parent, taskID, name string) (*subagent, error) {
	id := taskID + "/" + name
	if _, ok := e.subs[id]; !ok {
		ctx, cancel := saveContext()
		defer cancel()
		subs, _ := e.opts.Store.SubagentsForRun(ctx, e.run.ID)
		for _, rec := range subs {
			if rec.ID == id && rec.Status != SubRunning {
				e.subs[id] = &subagent{rec: rec}
			}
		}
	}
	sa, ok := e.subs[id]
	if !ok || sa.rec.Parent != parent {
		return nil, fmt.Errorf("you have no subagent named %q; switch_subagents lists them", name)
	}
	return sa, nil
}

// workerTask is the caller's task, which must be running and assigned to it,
// the worker, and the slot its task runs in: Ready gives it by the worker's
// place in the run.
func (e *Engine) workerTask(ctx context.Context, parent, taskID string) (store.Task, store.RunAgent, int, error) {
	task, ok, err := e.opts.Store.GetTask(ctx, taskID)
	if err != nil {
		return store.Task{}, store.RunAgent{}, 0, err
	}
	if !ok || task.Assignee != parent || task.Status != StatusRunning {
		return store.Task{}, store.RunAgent{}, 0, errors.New("subagents can only be started for your running task")
	}
	agents, err := e.opts.Store.RunAgents(ctx, e.run.ID)
	if err != nil {
		return store.Task{}, store.RunAgent{}, 0, err
	}
	for i, agent := range agents {
		if agent.Name == parent {
			return task, agent, i + 1, nil
		}
	}
	return store.Task{}, store.RunAgent{}, 0, fmt.Errorf("unknown worker %s", parent)
}

// recoverSubagents marks subagents a crashed process left running.
func (e *Engine) recoverSubagents(ctx context.Context) {
	subs, err := e.opts.Store.SubagentsForRun(ctx, e.run.ID)
	if err != nil {
		return
	}
	for _, sa := range subs {
		if sa.Status == SubRunning {
			sa.Status = SubInterrupted
			_ = e.opts.Store.SaveSubagent(ctx, sa)
		}
	}
}

// overlap reports a file both claims cover, counting a directory as covering
// the files under it.
func overlap(a, b []string) (string, bool) {
	for _, x := range a {
		for _, y := range b {
			if x == "." || y == "." || x == y || strings.HasPrefix(x, y+string(filepath.Separator)) || strings.HasPrefix(y, x+string(filepath.Separator)) {
				return x, true
			}
		}
	}
	return "", false
}

func uniqueName(name, taskID string, existing []store.Subagent) string {
	name = strings.Trim(name, "-")
	if name == "" {
		name = "sub"
	}
	taken := map[string]bool{}
	for _, sa := range existing {
		if sa.TaskID == taskID {
			taken[sa.Name] = true
		}
	}
	candidate := name
	for i := 2; taken[candidate]; i++ {
		candidate = fmt.Sprintf("%s-%d", name, i)
	}
	return candidate
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}
