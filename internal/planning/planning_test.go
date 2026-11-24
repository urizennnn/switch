package planning

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/plan"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

func TestCheckWorkersNeedsANonLeadWorkerPerRole(t *testing.T) {
	planned := plan.Plan{Tasks: []plan.Task{{ID: "a", Role: "builder"}}}
	fleet := []config.Agent{{Name: "lead", Role: "lead"}, {Name: "builder-1", Role: "builder"}}
	if err := checkWorkers(planned, fleet, "lead"); err != nil {
		t.Fatalf("valid fleet rejected: %v", err)
	}
	// builder-1 leads, so nobody can build.
	if err := checkWorkers(planned, fleet, "builder-1"); err == nil || !strings.Contains(err.Error(), `"builder" worker`) {
		t.Fatalf("err = %v, want missing builder", err)
	}
}

// The planner is a fresh session: what the user and the lead discussed
// reaches it in the prompt, and with no goal it plans from that alone.
func TestPlanFromTheConversation(t *testing.T) {
	fleet := []config.Agent{{Name: "lead", Role: "lead"}, {Name: "builder-1", Role: "builder"}}
	talk := "[user]\nadd a --json flag to status\n\n[assistant]\nAgreed: status gets --json."
	prompt := buildPrompt("add the flag", talk, fleet, rules.Set{}, "lead")
	if !strings.Contains(prompt, "Goal:\nadd the flag") || !strings.Contains(prompt, "Conversation so far") || !strings.Contains(prompt, talk) {
		t.Fatalf("prompt with a goal and a conversation:\n%s", prompt)
	}
	if bare := buildPrompt("", talk, fleet, rules.Set{}, "lead"); !strings.Contains(bare, "conversation below") || !strings.Contains(bare, talk) {
		t.Fatalf("prompt without a goal:\n%s", bare)
	}
	if strings.Contains(buildPrompt("add the flag", "", fleet, rules.Set{}, "lead"), "Conversation so far") {
		t.Fatal("a prompt without a conversation has its heading")
	}

	project := newProject(t)
	// No reachable lead: a plan that gets past the goal check fails to start it.
	if _, err := Execute(context.Background(), Options{Project: project, Agent: "no-such-adapter"}); err == nil || !strings.Contains(err.Error(), "provide a goal") {
		t.Fatalf("no goal and no conversation: err = %v", err)
	}
	if _, err := Execute(context.Background(), Options{Project: project, Agent: "no-such-adapter", Conversation: talk}); err == nil || !strings.Contains(err.Error(), "no-such-adapter") {
		t.Fatalf("a conversation and no goal: err = %v, want the lead started", err)
	}
}

func TestExecuteNeedsGitBeforePlanning(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// A goal with no reachable lead: the git check must fail first.
	_, err := Execute(context.Background(), Options{Project: t.TempDir(), Goal: "anything", Agent: "no-such-adapter"})
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("err = %v, want the git repository check", err)
	}
}

func TestCheckWorkersAcceptsOnlyWorkersAsPins(t *testing.T) {
	fleet := []config.Agent{{Name: "lead", Role: "lead"}, {Name: "builder-1", Role: "builder"}, {Name: "builder-2", Role: "builder"}}
	for agent, want := range map[string]bool{"builder-2": true, "builder-1": false, "lead": false, "nobody": false} {
		planned := plan.Plan{Tasks: []plan.Task{{ID: "a", Role: "builder", Agent: agent}}}
		err := checkWorkers(planned, fleet, "builder-1")
		if (err == nil) != want {
			t.Errorf("pinned to %s: err = %v", agent, err)
		}
	}
}

const twoTasks = `{"goal": "g", "tasks": [
	{"id": "schema", "title": "Schema", "brief": "b", "role": "builder", "acceptance": ["ok"]},
	{"id": "api", "title": "API", "brief": "b", "role": "builder", "agent": "builder-2", "deps": ["schema"], "acceptance": ["ok"]}
]}`

func TestAskLeadFixesARejectedPlanOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	check := func(output string) (plan.Plan, error) { return plan.Parse(output) }
	broken := strings.Replace(twoTasks, `"deps": ["schema"]`, `"deps": ["db"]`, 1)

	var prompts []string
	turn := func(outputs ...string) func(string) (string, error) {
		prompts = nil
		return func(prompt string) (string, error) {
			prompts = append(prompts, prompt)
			return outputs[len(prompts)-1], nil
		}
	}

	planned, err := askLead(turn(broken, twoTasks), "plan it", check, nil)
	if err != nil || len(planned.Tasks) != 2 {
		t.Fatalf("plan = %#v, err = %v", planned, err)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[1], `task "api" depends on unknown task "db"`) {
		t.Fatalf("prompts = %q, want the exact error in the fix-up turn", prompts)
	}

	if _, err := askLead(turn(twoTasks), "plan it", check, nil); err != nil || len(prompts) != 1 {
		t.Fatalf("a valid plan took %d turns, err = %v", len(prompts), err)
	}

	_, err = askLead(turn(broken, "still "+broken), "plan it", check, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Fatalf("err = %v, want the plan's error", err)
	}
	match := regexp.MustCompile(`kept in (\S+):`).FindStringSubmatch(err.Error())
	if match == nil {
		t.Fatalf("err = %v, want where the plan is kept", err)
	}
	kept, readErr := os.ReadFile(match[1])
	if readErr != nil || !strings.Contains(string(kept), broken) || !strings.Contains(string(kept), "still "+broken) {
		t.Fatalf("kept plan = %q, %v", kept, readErr)
	}
	if fixed, err := plan.Parse(strings.Replace(string(kept), `"deps": ["db"]`, `"deps": ["schema"]`, 1)); err != nil || len(fixed.Tasks) != 2 {
		t.Fatalf("the kept file does not load once fixed: %v", err)
	}
}

func TestAKeptPlanLoadsOnceItsLatestCopyIsFixed(t *testing.T) {
	project := newProject(t)
	cfg, _, err := config.LoadOrInit(project)
	if err != nil {
		t.Fatal(err)
	}
	check := func(output string) (plan.Plan, error) {
		return plan.Parse(output, func(p plan.Plan) error { return checkWorkers(p, cfg.Fleet(), "lead") })
	}
	// The first answer parses, but no worker is a tester; the fix-up answer
	// names an unknown dep.
	tester := strings.Replace(twoTasks, `"role": "builder", "agent"`, `"role": "tester", "agent"`, 1)
	broken := strings.Replace(twoTasks, `"deps": ["schema"]`, `"deps": ["db"]`, 1)
	answers := []string{tester, broken}
	_, err = askLead(func(string) (string, error) {
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}, "plan it", check, nil)
	match := regexp.MustCompile(`kept in (\S+):`).FindStringSubmatch(fmt.Sprint(err))
	if match == nil {
		t.Fatalf("err = %v, want where the plan is kept", err)
	}
	kept, err := os.ReadFile(match[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(match[1], []byte(strings.Replace(string(kept), `"deps": ["db"]`, `"deps": ["schema"]`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Execute(context.Background(), Options{Project: project, File: match[1]})
	if err != nil {
		t.Fatalf("Execute --file with the latest copy fixed: %v", err)
	}
	if role := result.Plan.Tasks[1].Role; role != "builder" {
		t.Fatalf("api role = %q: loaded the first answer, not the fixed one", role)
	}
}

// newProject makes a git repository with one commit and private Switch
// config and state.
func newProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	runGit(t, project, "init")
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, project, "add", "README.md")
	runGit(t, project, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	return project
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func writePlan(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecuteSavesTheRunForTheChosenLead(t *testing.T) {
	project := newProject(t)
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.LoadOrInit(project)
	if err != nil {
		t.Fatal(err)
	}
	var chosen config.Agent
	for _, agent := range cfg.Fleet() {
		if agent.Name == "builder-1" {
			chosen = agent // as after /lead builder-1
		}
	}

	result, err := Execute(context.Background(), Options{Project: project, File: writePlan(t, twoTasks), Lead: &chosen})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Uncommitted != 1 {
		t.Fatalf("uncommitted = %d, want only notes.txt (Switch's own .switch files do not count)", result.Uncommitted)
	}

	state, err := store.Open(config.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx := context.Background()
	agents, err := state.RunAgents(ctx, result.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, agent := range agents {
		roles[agent.Name] = agent.Role
	}
	if want := map[string]string{"lead": "standby", "builder-1": "lead", "builder-2": "builder"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("run roles = %v, want %v: only the chosen lead reviews and takes handoffs", roles, want)
	}

	tasks, err := state.TasksForRun(ctx, result.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	api := tasks[1]
	if api.Assignee != "builder-2" || !reflect.DeepEqual(api.Deps, []string{result.Run.ID + ":schema"}) {
		t.Fatalf("api task = assignee %q, deps %q", api.Assignee, api.Deps)
	}
}

func TestExecuteResetsTheModelWhenTheAgentChanges(t *testing.T) {
	project := newProject(t)
	file := writePlan(t, twoTasks)
	for _, c := range []struct{ agent, driver, model string }{
		{"claude", "claude", ""},
		{"opencode", "opencode", "opencode/deepseek-v4.1-flash-free"},
		// Named as in a config: the same drivers.
		{"claude-code", "claude", ""},
		{"OpenCode", "opencode", "opencode/deepseek-v4.1-flash-free"},
	} {
		result, err := Execute(context.Background(), Options{Project: project, File: file, Agent: c.agent})
		if err != nil {
			t.Fatalf("Execute --agent %s: %v", c.agent, err)
		}
		if result.Lead.Driver != c.driver || result.Lead.Model != c.model {
			t.Fatalf("--agent %s: lead = %s %q, want %s %q", c.agent, result.Lead.Driver, result.Lead.Model, c.driver, c.model)
		}
		if result.Agents[0].Role != "lead" || result.Agents[0].Driver != c.driver || result.Agents[0].Model != c.model {
			t.Fatalf("--agent %s: saved lead = %#v", c.agent, result.Agents[0])
		}
	}
}

func TestExecuteRefusesABranchNamedSwitchBeforePlanning(t *testing.T) {
	project := newProject(t)
	runGit(t, project, "branch", "switch")
	// No reachable lead: the branch check must fail first.
	_, err := Execute(context.Background(), Options{Project: project, Goal: "anything", Agent: "no-such-adapter"})
	if err == nil || !strings.Contains(err.Error(), `branch named "switch"`) {
		t.Fatalf("err = %v, want the switch branch named", err)
	}
}

func TestExecuteLeavesNothingWhenSavingFails(t *testing.T) {
	project := newProject(t)
	state, err := store.Open(config.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	state.Close()
	db, err := sql.Open("sqlite", config.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_tasks BEFORE INSERT ON tasks BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Execute(context.Background(), Options{Project: project, File: writePlan(t, twoTasks)})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the save failure", err)
	}
	if branches := runGit(t, project, "branch", "--list", "switch/*"); branches != "" {
		t.Fatalf("run branches left behind: %s", branches)
	}
	if worktrees := runGit(t, project, "worktree", "list"); strings.Count(worktrees, "\n") != 0 {
		t.Fatalf("worktrees left behind:\n%s", worktrees)
	}
	entries, _ := filepath.Glob(filepath.Join(config.StateDir(), "worktrees", "*", "*"))
	if len(entries) != 0 {
		t.Fatalf("worktree folders left behind: %v", entries)
	}
	state, err = store.Open(config.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if _, ok, _ := state.LatestRun(context.Background(), project); ok {
		t.Fatal("a half-made run was saved")
	}
}
