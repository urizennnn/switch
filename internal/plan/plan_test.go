package plan

import (
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
	output := strings.Replace(validPlan, `"id": "api"`, `"id": "schema"`, 1)
	_, err := Parse(output)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestParseRejectsMissingAcceptance(t *testing.T) {
	output := strings.Replace(validPlan, `"acceptance": ["migration works"],`, ``, 1)
	_, err := Parse(output)
	if err == nil || !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("expected acceptance error, got %v", err)
	}
}
