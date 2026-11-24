package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// A task runs on the git branch switch/<run>/task-<id>, so an id must stay a
// valid branch name component that the worktree name never truncates.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`)

const maxIDLength = 40

// planStart matches an object whose first key is a plan's.
var planStart = regexp.MustCompile(`(?i)^\{\s*"(goal|tasks)"`)

type Plan struct {
	Goal  string `json:"goal"`
	Tasks []Task `json:"tasks"`
}

type Task struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Brief      string   `json:"brief"`
	Role       string   `json:"role"`
	Agent      string   `json:"agent,omitempty"` // pins the task to this worker
	Deps       []string `json:"deps,omitempty"`
	Files      []string `json:"files,omitempty"`
	Acceptance []string `json:"acceptance,omitempty"`
	Verify     []string `json:"verify,omitempty"`
}

// Parse finds the plan in agent output: the first JSON object that decodes,
// validates and passes checks. Models wrap the JSON in prose, code fences and
// other snippets, so each '{' is tried in turn; text after the plan is ignored.
func Parse(output string, checks ...func(Plan) error) (Plan, error) {
	var planErr error
	var planLike bool
	var planRead int64 = -1
	for i := strings.IndexByte(output, '{'); i >= 0; {
		plan, read, err := decode(output[i:])
		for _, check := range checks {
			if err == nil {
				err = check(plan)
			}
		}
		if err == nil {
			return plan, nil
		}
		// Report the plan's own error: an object that starts like a plan beats
		// one that does not (such as a task inside a broken plan), then the one
		// that read the most text beats a stray brace in prose.
		like := planStart.MatchString(output[i:])
		if (like && !planLike) || (like == planLike && read > planRead) {
			planErr, planLike, planRead = err, like, read
		}
		next := strings.IndexByte(output[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	if planErr == nil {
		return Plan{}, fmt.Errorf("no JSON object found in plan output")
	}
	return Plan{}, planErr
}

// decode reads one plan from the start of text and reports how much of text
// it read.
func decode(text string) (Plan, int64, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	var plan Plan
	if err := decoder.Decode(&plan); err != nil {
		read := decoder.InputOffset()
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			read = syntax.Offset
		} else if errors.Is(err, io.ErrUnexpectedEOF) {
			read = int64(len(text))
		}
		return Plan{}, read, fmt.Errorf("decode plan: %w", err)
	}
	// Ids, roles and agents are keys, so stray whitespace must not hide a match.
	for i := range plan.Tasks {
		task := &plan.Tasks[i]
		task.ID, task.Role, task.Agent = strings.TrimSpace(task.ID), strings.TrimSpace(task.Role), strings.TrimSpace(task.Agent)
		for j := range task.Deps {
			task.Deps[j] = strings.TrimSpace(task.Deps[j])
		}
	}
	return plan, decoder.InputOffset(), plan.Validate()
}

func (p Plan) Validate() error {
	if strings.TrimSpace(p.Goal) == "" {
		return fmt.Errorf("plan goal is required")
	}
	if len(p.Tasks) == 0 {
		return fmt.Errorf("plan must contain at least one task")
	}
	if len(p.Tasks) > 32 {
		return fmt.Errorf("plan has %d tasks; maximum is 32", len(p.Tasks))
	}

	seen := make(map[string]bool, len(p.Tasks))
	folded := make(map[string]bool, len(p.Tasks)) // branch names ignore case on macOS and Windows
	for i, task := range p.Tasks {
		if task.ID == "" {
			return fmt.Errorf("task %d: id is required", i+1)
		}
		if len(task.ID) > maxIDLength || !idPattern.MatchString(task.ID) || strings.Contains(task.ID, "..") || strings.HasSuffix(task.ID, ".lock") {
			return fmt.Errorf("task %q: id must work as a git branch name: at most %d letters, digits, dots, underscores or dashes, starting and ending with a letter or digit, with no \"..\" and no \".lock\" ending", task.ID, maxIDLength)
		}
		if folded[strings.ToLower(task.ID)] {
			return fmt.Errorf("duplicate task id %q (ids are compared ignoring case)", task.ID)
		}
		seen[task.ID], folded[strings.ToLower(task.ID)] = true, true
		if strings.TrimSpace(task.Title) == "" {
			return fmt.Errorf("task %q: title is required", task.ID)
		}
		if strings.TrimSpace(task.Brief) == "" {
			return fmt.Errorf("task %q: brief is required", task.ID)
		}
		if strings.TrimSpace(task.Role) == "" {
			return fmt.Errorf("task %q: role is required", task.ID)
		}
		if len(task.Acceptance) == 0 {
			return fmt.Errorf("task %q: at least one acceptance criterion is required", task.ID)
		}
	}

	for _, task := range p.Tasks {
		for _, dep := range task.Deps {
			if dep == task.ID {
				return fmt.Errorf("task %q depends on itself", task.ID)
			}
			if !seen[dep] {
				return fmt.Errorf("task %q depends on unknown task %q", task.ID, dep)
			}
		}
	}

	if err := validateAcyclic(p.Tasks); err != nil {
		return err
	}
	return nil
}

func validateAcyclic(tasks []Task) error {
	graph := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		for _, dep := range task.Deps {
			graph[task.ID] = append(graph[task.ID], dep)
		}
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(tasks))
	var visit func(string) error
	visit = func(id string) error {
		switch color[id] {
		case gray:
			return fmt.Errorf("dependency cycle involving task %q", id)
		case black:
			return nil
		}
		color[id] = gray
		for _, dep := range graph[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		color[id] = black
		return nil
	}

	for _, task := range tasks {
		if err := visit(task.ID); err != nil {
			return err
		}
	}
	return nil
}
