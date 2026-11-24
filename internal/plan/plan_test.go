package plan

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const validPlan = `{
  "goal": "Add account deletion",
  "tasks": [
    {
      "id": "schema",
      "title": "Add audit table",
      "brief": "Create the migration and store methods.",
      "role": "builder",
      "deps": [],
      "files": ["internal/store/**"],
      "acceptance": ["migration works"],
      "verify": ["go test ./internal/store/..."]
    },
    {
      "id": "api",
      "title": "Add delete endpoint",
      "brief": "Implement the endpoint.",
      "role": "builder",
      "deps": ["schema"],
      "files": ["internal/api/**"],
      "acceptance": ["204 on success"],
      "verify": ["go test ./internal/api/..."]
    }
  ]
}`

func TestParsePlainJSON(t *testing.T) {
	plan, err := Parse(validPlan)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(plan.Tasks))
	}
	if plan.Tasks[1].Deps[0] != "schema" {
		t.Fatalf("dependency = %#v", plan.Tasks[1].Deps)
	}
}

func TestParseFencedJSON(t *testing.T) {
	output := "Here is the plan:\n\n```json\n" + validPlan + "\n```\n\nLet me know."
	plan, err := Parse(output)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if plan.Goal != "Add account deletion" {
		t.Fatalf("goal = %q", plan.Goal)
	}
}

func TestParseRejectsCycle(t *testing.T) {
	output := `{"goal":"cycle","tasks":[
		{"id":"a","title":"A","brief":"A","role":"builder","deps":["b"],"acceptance":["ok"]},
		{"id":"b","title":"B","brief":"B","role":"builder","deps":["a"],"acceptance":["ok"]}
	]}`
	_, err := Parse(output)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestParseRejectsUnknownDependency(t *testing.T) {
	output := strings.Replace(validPlan, `"deps": ["schema"]`, `"deps": ["nope"]`, 1)
	_, err := Parse(output)
	if err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Fatalf("expected unknown dependency error, got %v", err)
	}
}

func TestParseRejectsDuplicateID(t *testing.T) {
	// "Schema" and "schema" name the same branch on macOS and Windows.
	for _, id := range []string{"schema", "Schema"} {
		output := strings.Replace(validPlan, `"id": "api"`, `"id": "`+id+`"`, 1)
		_, err := Parse(output)
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("id %q: expected duplicate error, got %v", id, err)
		}
	}
}

func TestParseRejectsMissingAcceptance(t *testing.T) {
	output := strings.Replace(validPlan, `"acceptance": ["migration works"],`, ``, 1)
	_, err := Parse(output)
	if err == nil || !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("expected acceptance error, got %v", err)
	}
}

func TestParseFindsThePlanInLeadOutput(t *testing.T) {
	escaped := strings.Replace(validPlan, `"Implement the endpoint."`, `"Run:\n`+"```"+`\ngo test ./...\n`+"```"+`"`, 1)
	cases := map[string]string{
		"code fence before the plan":  "First a sketch:\n```go\nfunc main() { fmt.Println(\"hi\") }\n```\n\n```json\n" + validPlan + "\n```",
		"prose with braces after it":  validPlan + "\n\nThat covers it. The {schema} task goes first.}",
		"a brace in prose before it":  "I read config.{ts,js} first.\n" + validPlan,
		"a fence inside a brief":      "```json\n" + escaped + "\n```",
		"an example before the plan":  `Shape: {"goal": "g", "tasks": []}` + "\n" + validPlan,
		"a plan wrapped in an object": `{"plan": ` + validPlan + `}`,
	}
	for name, output := range cases {
		plan, err := Parse(output)
		if err != nil {
			t.Errorf("%s: Parse: %v", name, err)
			continue
		}
		if len(plan.Tasks) != 2 || plan.Tasks[1].Deps[0] != "schema" {
			t.Errorf("%s: tasks = %#v", name, plan.Tasks)
		}
	}
}

func TestParseReportsThePlansOwnError(t *testing.T) {
	output := "I looked at {braces} first.\n" + strings.Replace(validPlan, `"deps": []`, `"deps": ["api"]`, 1)
	_, err := Parse(output)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want the plan's cycle error", err)
	}
}

func TestParseReportsASyntaxErrorInThePlan(t *testing.T) {
	// The second task is complete JSON on its own; its unknown "id" field
	// must not hide the plan's real mistake.
	cases := map[string]string{
		"invalid escape":  strings.Replace(validPlan, `store methods.`, `store methods for ids like \d+.`, 1),
		"unescaped quote": strings.Replace(validPlan, `"Add account deletion"`, `"Add "account" deletion"`, 1),
		"trailing comma":  strings.Replace(validPlan, `"verify": ["go test ./internal/store/..."]`, `"verify": ["go test ./internal/store/..."],`, 1),
	}
	for name, output := range cases {
		_, err := Parse(output)
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Errorf("%s: err = %v, want the plan's syntax error", name, err)
		}
	}
}

func TestParseTakesThePlanTheCheckAccepts(t *testing.T) {
	draft := strings.Replace(validPlan, `"role": "builder"`, `"role": "tester"`, 1)
	noTesters := func(p Plan) error {
		for _, task := range p.Tasks {
			if task.Role == "tester" {
				return errors.New("no tester worker")
			}
		}
		return nil
	}
	plan, err := Parse(draft+"\n\nRevised:\n"+validPlan, noTesters)
	if err != nil || plan.Tasks[0].Role != "builder" {
		t.Fatalf("tasks = %#v, err = %v, want the revised plan", plan.Tasks, err)
	}
	if _, err := Parse(draft, noTesters); err == nil || err.Error() != "no tester worker" {
		t.Fatalf("err = %v, want the check's error", err)
	}
}

func TestParseTrimsIDsAndDeps(t *testing.T) {
	output := strings.Replace(validPlan, `"id": "schema"`, `"id": "schema "`, 1)
	output = strings.Replace(output, `"deps": ["schema"]`, `"deps": [" schema"]`, 1)
	plan, err := Parse(output)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if plan.Tasks[0].ID != "schema" || plan.Tasks[1].Deps[0] != "schema" {
		t.Fatalf("id = %q, deps = %q", plan.Tasks[0].ID, plan.Tasks[1].Deps)
	}

	cycle := `{"goal":"cycle","tasks":[
		{"id":"a ","title":"A","brief":"A","role":"builder","deps":["b"],"acceptance":["ok"]},
		{"id":"b","title":"B","brief":"B","role":"builder","deps":["a"],"acceptance":["ok"]}
	]}`
	if _, err := Parse(cycle); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want a cycle through the padded id", err)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	output := strings.Replace(validPlan, `"deps": ["schema"]`, `"dependencies": ["schema"]`, 1)
	_, err := Parse(output)
	if err == nil || !strings.Contains(err.Error(), `unknown field "dependencies"`) {
		t.Fatalf("err = %v, want the unknown field named", err)
	}
}

func TestParseRejectsIDsThatCannotNameABranch(t *testing.T) {
	for _, id := range []string{"update-yarn.lock", "fix..parser", "trailing.", "trailing-", "-leading", "has space", strings.Repeat("a", 41)} {
		output := strings.Replace(validPlan, `"id": "api"`, `"id": "`+id+`"`, 1)
		if _, err := Parse(output); err == nil || !strings.Contains(err.Error(), "git branch") {
			t.Errorf("id %q: err = %v, want a branch name error", id, err)
		}
	}
	output := strings.Replace(validPlan, `"id": "api"`, `"id": "api.v2_x"`, 1)
	if _, err := Parse(strings.Replace(output, `"deps": ["schema"]`, `"deps": ["schema"], "agent": " builder-2 "`, 1)); err != nil {
		t.Fatalf("valid id and agent rejected: %v", err)
	}
}
