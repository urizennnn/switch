package plan

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

type Plan struct {
	Goal  string `json:"goal"`
	Tasks []Task `json:"tasks"`
}

type Task struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Brief      string   `json:"brief"`
	Role       string   `json:"role"`
	Deps       []string `json:"deps,omitempty"`
	Files      []string `json:"files,omitempty"`
	Acceptance []string `json:"acceptance,omitempty"`
	Verify     []string `json:"verify,omitempty"`
}

// Parse extracts the first JSON object from agent output and validates it.
// Models often wrap JSON in prose or a fenced code block.
func Parse(output string) (Plan, error) {
	raw := extractJSON(output)
	if raw == "" {
		return Plan{}, fmt.Errorf("no JSON object found in plan output")
	}
	var plan Plan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return Plan{}, fmt.Errorf("decode plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
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
	for i, task := range p.Tasks {
		task.ID = strings.TrimSpace(task.ID)
		if task.ID == "" {
			return fmt.Errorf("task %d: id is required", i+1)
		}
		if !idPattern.MatchString(task.ID) {
			return fmt.Errorf("task %q: id must start alphanumeric and contain only letters, digits, dot, underscore, or dash", task.ID)
		}
		if seen[task.ID] {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = true
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
			dep = strings.TrimSpace(dep)
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
			graph[task.ID] = append(graph[task.ID], strings.TrimSpace(dep))
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

func extractJSON(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}

	if start := strings.Index(output, "```"); start >= 0 {
		body := output[start+3:]
		if newline := strings.IndexByte(body, '\n'); newline >= 0 {
			body = body[newline+1:]
		}
		if end := strings.Index(body, "```"); end >= 0 {
			body = body[:end]
		}
		if candidate := strings.TrimSpace(body); strings.HasPrefix(candidate, "{") {
			return candidate
		}
	}

	first := strings.Index(output, "{")
	last := strings.LastIndex(output, "}")
	if first >= 0 && last > first {
		return strings.TrimSpace(output[first : last+1])
	}
	return ""
}
