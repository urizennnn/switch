package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/urizennnn/switch/internal/config"
)

// Rules come in three scopes, applied in this order so later ones win:
//
//	global   ~/.config/switch/rules.md, rules/roles/<role>.md, rules/agents/<agent>.md
//	project  .switch/rules.md and .switch/rules/{roles,agents,tasks}/<name>.md
//	local    .switch/rules.local.md, personal and not committed
//
// Within the role, agent and task levels the global file comes before the
// project one. HTML comments are notes for people and are never sent.

type Source struct {
	Path  string
	Scope string
	Text  string
}

type Set struct {
	Sources []Source

	cfg       config.RulesConfig // where the set came from, so it can be reloaded
	loaded    bool
	noProject bool // the user declined this project's rules
}

// File is a rule file a person can edit, whether or not it exists yet.
type File struct {
	Scope  string
	Path   string
	Exists bool
}

// Load reads the global, project, and local rule files. Missing files are
// not an error; rules are optional.
func Load(cfg config.RulesConfig) (Set, error) {
	set := Set{cfg: cfg, loaded: true}
	if err := set.add(set.globalFile(), "global"); err != nil {
		return Set{}, err
	}
	if err := set.add(set.projectFile(cfg.Project), "project"); err != nil {
		return Set{}, err
	}
	if err := set.add(set.projectFile(cfg.Local), "local"); err != nil {
		return Set{}, err
	}
	return set, nil
}

// Reload rereads the files, so edits apply without restarting Switch. A set
// not built by Load is returned as is.
func (s Set) Reload() (Set, error) {
	if !s.loaded {
		return s, nil
	}
	next, err := Load(s.cfg)
	if err != nil {
		return Set{}, err
	}
	if s.noProject {
		next = next.WithoutProject()
	}
	return next, nil
}

// Scoped rereads base and adds the role, agent, and task rules for one agent.
// Lead-only guidance belongs in roles/lead.md, which workers never receive.
// Names come from config and lead-written plans, so ones that would escape
// the rules directory are ignored.
func Scoped(base Set, role, agent, task string) (Set, error) {
	set, err := base.Reload()
	if err != nil {
		return Set{}, err
	}
	set.Sources = append([]Source(nil), set.Sources...)
	global, project := set.dirs()
	for _, level := range []struct{ kind, name string }{{"roles", role}, {"agents", agent}, {"tasks", task}} {
		name, ok := fileName(level.name)
		if !ok {
			continue
		}
		label := strings.TrimSuffix(level.kind, "s") + ":" + name
		if global != "" && level.kind != "tasks" {
			if err := set.add(filepath.Join(global, level.kind, name+".md"), "global "+label); err != nil {
				return Set{}, err
			}
		}
		if err := set.add(filepath.Join(project, level.kind, name+".md"), label); err != nil {
			return Set{}, err
		}
	}
	return set, nil
}

// Project reads every rule file in the project, used to show a project's
// rules before they are first sent.
func Project(base Set) (Set, error) {
	_, project := base.Files()
	set := Set{cfg: base.cfg}
	for _, file := range project {
		if err := set.add(file.Path, file.Scope); err != nil {
			return Set{}, err
		}
	}
	return set, nil
}

// WithoutProject drops the rules that came from the project directory.
func (s Set) WithoutProject() Set {
	out := Set{cfg: s.cfg, loaded: s.loaded, noProject: true}
	for _, source := range s.Sources {
		if !strings.HasPrefix(source.Scope, "global") {
			continue
		}
		out.Sources = append(out.Sources, source)
	}
	return out
}

// Files lists the editable rule files: the base file of each scope, even when
// missing, and every role, agent, and task file that exists.
func (s Set) Files() (global, project []File) {
	globalDir, projectDir := s.dirs()
	if path := s.globalFile(); path != "" {
		global = append(global, file("global", path))
		global = append(global, listDir(globalDir, "roles", "global ")...)
		global = append(global, listDir(globalDir, "agents", "global ")...)
	}
	if s.cfg.Project != "" {
		project = append(project, file("project", s.cfg.Project))
	}
	if s.cfg.Local != "" {
		project = append(project, file("local", s.cfg.Local))
	}
	if projectDir != "" {
		for _, kind := range []string{"roles", "agents", "tasks"} {
			project = append(project, listDir(projectDir, kind, "")...)
		}
	}
	return global, project
}

// Target resolves a rule file name, as used by `switch rules edit` and the
// /rules manager: global, project, local, roles/<name>, agents/<name>,
// tasks/<name>, global/roles/<name>, or global/agents/<name>.
func (s Set) Target(target string) (string, error) {
	globalDir, projectDir := s.dirs()
	target = strings.Trim(strings.TrimSpace(target), "/")
	switch target {
	case "global":
		if path := s.globalFile(); path != "" {
			return path, nil
		}
		return "", fmt.Errorf("global rules are turned off for this project (ignore_global)")
	case "project":
		return s.cfg.Project, nil
	case "local":
		return s.cfg.Local, nil
	}
	dir := projectDir
	rest, isGlobal := strings.CutPrefix(target, "global/")
	if isGlobal {
		if globalDir == "" {
			return "", fmt.Errorf("global rules are turned off for this project (ignore_global)")
		}
		dir = globalDir
		target = rest
	}
	kind, name, _ := strings.Cut(target, "/")
	name, ok := fileName(strings.TrimSuffix(name, ".md"))
	valid := kind == "roles" || kind == "agents" || (kind == "tasks" && !isGlobal)
	if !ok || !valid || dir == "" {
		return "", fmt.Errorf("unknown rule file %q; use global, project, local, roles/<role>, agents/<agent>, tasks/<task>, global/roles/<role>, or global/agents/<agent>", target)
	}
	return filepath.Join(dir, kind, name+".md"), nil
}

func (s Set) globalFile() string {
	if s.cfg.IgnoreGlobal {
		return ""
	}
	return expandHome(s.cfg.Global)
}

func (s Set) projectFile(path string) string {
	if s.noProject {
		return ""
	}
	return path
}

// dirs returns the global and project rules directories; either is empty
// when that scope is off.
func (s Set) dirs() (global, project string) {
	if path := s.globalFile(); path != "" {
		global = filepath.Join(filepath.Dir(path), "rules")
	}
	if s.cfg.Project != "" && !s.noProject {
		project = filepath.Join(filepath.Dir(s.cfg.Project), "rules")
	}
	return global, project
}

func file(scope, path string) File {
	_, err := os.Stat(path)
	return File{Scope: scope, Path: path, Exists: err == nil}
}

func listDir(dir, kind, prefix string) []File {
	paths, _ := filepath.Glob(filepath.Join(dir, kind, "*.md"))
	sort.Strings(paths)
	files := make([]File, 0, len(paths))
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		files = append(files, File{Scope: prefix + strings.TrimSuffix(kind, "s") + ":" + name, Path: path, Exists: true})
	}
	return files
}

// fileName rejects names that would escape the rules directory.
func fileName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	return name, true
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

func (s *Set) add(path, scope string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read rules %s: %w", path, err)
	}
	text := strings.TrimSpace(htmlComment.ReplaceAllString(string(data), ""))
	if text == "" {
		return nil
	}
	s.Sources = append(s.Sources, Source{Path: path, Scope: scope, Text: text})
	return nil
}

func (s Set) Empty() bool {
	return len(s.Sources) == 0
}

// Hash identifies the rules' content, so a session is sent them again only
// when they change. It is empty for an empty set.
func (s Set) Hash() string {
	if s.Empty() {
		return ""
	}
	sum := sha256.New()
	for _, source := range s.Sources {
		fmt.Fprintf(sum, "%s\x00%s\x00%s\x00", source.Scope, source.Path, source.Text)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// Text returns the merged rules for display.
func (s Set) Text() string {
	if len(s.Sources) == 0 {
		return ""
	}
	var b strings.Builder
	for i, source := range s.Sources {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %s rules (%s)\n\n%s", source.Scope, source.Path, source.Text)
	}
	return b.String()
}

// Prompt prepends the effective rules to a user message. ACP has no stable
// client-provided system prompt, so this is the portable injection point.
func (s Set) Prompt(user string) string {
	if len(s.Sources) == 0 {
		return user
	}
	var b strings.Builder
	b.WriteString("Follow these operating rules for this session:\n\n")
	b.WriteString(s.Text())
	b.WriteString("\n\nUser request:\n")
	b.WriteString(user)
	return b.String()
}

func expandHome(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

// Editor returns the command that opens path in the user's editor ($VISUAL,
// then $EDITOR, then vi), creating the rules directory first.
func Editor(path string) (*exec.Cmd, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	editor := strings.Fields(os.Getenv("VISUAL"))
	if len(editor) == 0 {
		editor = strings.Fields(os.Getenv("EDITOR"))
	}
	if len(editor) == 0 {
		editor = []string{"vi"}
	}
	return exec.Command(editor[0], append(editor[1:], path)...), nil
}
