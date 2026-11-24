package tui

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The status line is the only chrome outside the panes: what is happening
// now, whether anything waits on the user, the one key that helps, and where
// they are (project and branch).

type statusLine struct {
	color  color.Color
	mark   string
	label  string
	timer  string
	detail string // truncated to fit
	extra  string // dropped first on a narrow terminal
	hint   string
}

func (m model) statusLine() statusLine {
	lead := m.lead.Name
	queued, active, review, merged, blocked := m.taskCounts()
	switch {
	case m.permission != nil:
		who := lead // workers, teammates and subagents always name themselves
		if m.permission.Agent != "" {
			who = strings.ReplaceAll(m.permission.Agent, "/", " › ")
		}
		return statusLine{color: violet, mark: "▲", label: promptName(m.permission), detail: who + " asks: " + m.permission.Title, hint: promptKeys(m.permission)}

	case m.state == "error" || m.state == "exited":
		detail := "the last turn failed"
		switch {
		case m.connectErr != nil:
			detail = m.connectErr.Error()
		case m.state == "exited":
			detail = m.lead.Driver + " stopped"
		}
		return statusLine{color: red, mark: "✕", label: m.state, detail: detail, hint: "/lead · /doctor"}

	case m.state == "cancelling":
		return statusLine{color: yellow, mark: "◐", label: "cancelling", detail: lead + " is stopping"}

	case m.connecting:
		return statusLine{color: yellow, mark: "◐", label: "connecting", detail: lead + " · " + m.lead.Driver}

	case m.busy:
		step := m.step
		if step == "" {
			step = "thinking"
		}
		hint := ""
		if m.state == "working" {
			hint = "esc cancel"
		}
		return statusLine{color: yellow, mark: "◐", label: m.state, timer: since(m.turnStarted), detail: lead + " · " + step, hint: hint}

	case m.job == "plan":
		detail := lead + " · reading the project"
		if n := len(planTitle.FindAllStringIndex(m.planText, -1)); n > 0 {
			detail = fmt.Sprintf("%s · %d tasks drafted", lead, n)
		}
		return statusLine{color: yellow, mark: "◐", label: "planning", timer: since(m.jobStarted), detail: detail, hint: "esc cancel"}

	case m.job == "run" || (m.runOwner != 0 && m.run != nil):
		total := queued + active + review + merged + blocked
		parts := []string{fmt.Sprintf("%d/%d merged", merged, total)}
		for _, c := range []struct {
			n    int
			word string
		}{{active, "working"}, {review, "review"}, {blocked, "blocked"}} {
			if c.n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", c.n, c.word))
			}
			if c.word == "working" && m.runningSubagents() > 0 {
				parts = append(parts, plural(m.runningSubagents(), "subagent"))
			}
		}
		line := statusLine{color: yellow, mark: "◐", label: "running", detail: strings.Join(parts, " · "), extra: progressBar(merged, total, 12), hint: "/tasks"}
		if m.job == "run" {
			line.timer = since(m.jobStarted)
		}
		return line

	case review+blocked > 0:
		var parts []string
		if review > 0 {
			parts = append(parts, plural(review, "task")+" ready for review")
		}
		if blocked > 0 {
			parts = append(parts, plural(blocked, "task")+" blocked")
		}
		return statusLine{color: violet, mark: "●", label: "review", detail: strings.Join(parts, " · "), hint: "/tasks"}

	case m.installing != "":
		return statusLine{color: yellow, mark: "◐", label: "installing", detail: m.installing}
	}

	model := m.lead.Model
	if m.chat != nil && m.chat.Agent == m.lead.Driver {
		model = m.chat.Model
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		model = "default model"
	}
	detail := lead + " · " + model
	if m.ctxSize > 0 {
		detail += fmt.Sprintf(" · %d%% context", m.ctxUsed*100/m.ctxSize)
	}
	extra := ""
	if warm := m.warmWorkers(); warm > 0 {
		extra = plural(warm, "worker") + " warm"
	}
	return statusLine{color: teal, mark: "●", label: "ready", detail: detail, extra: extra, hint: "/ commands"}
}

func (m model) statusView(width int) string {
	s := m.statusLine()
	gap := "   "
	head := lipgloss.NewStyle().Foreground(s.color).Render(s.mark+" ") +
		lipgloss.NewStyle().Foreground(s.color).Bold(true).Render(strings.ToUpper(s.label))
	if s.timer != "" {
		head += " " + mutedStyle.Render(s.timer)
	}

	// Right-hand segments, dropped in this order as the terminal narrows.
	extra := ""
	if s.extra != "" {
		extra = mutedStyle.Render(s.extra)
	}
	project := dimStyle.Render(truncate(shortenHome(m.project), 40))
	branch := ""
	if m.branch != "" {
		branch = mutedStyle.Render("⎇ " + truncate(m.branch, 24))
		if m.dirty > 0 {
			branch += warningStyle.Render(fmt.Sprintf(" ✱%d", m.dirty))
		}
	}
	hint := dimStyle.Render(s.hint)
	right := func() string {
		var parts []string
		for _, part := range []string{extra, strings.TrimSpace(project + "  " + branch), hint} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, gap)
	}
	room := func() int { return width - lipgloss.Width(head) - len(gap) - lipgloss.Width(right()) - spaceM }
	for _, drop := range []*string{&extra, &project, &branch} {
		if room() >= min(24, lipgloss.Width(s.detail)) {
			break
		}
		*drop = ""
	}
	left := head
	if n := room(); n >= 8 && s.detail != "" {
		left += gap + mutedStyle.Render(truncate(s.detail, n))
	}
	return statusBar(width, left, right())
}

// windowTitle names Switch and what waits on the user, so another window or
// tab shows it.
func (m model) windowTitle() string {
	_, _, review, _, _ := m.taskCounts()
	switch {
	case m.permission != nil:
		return "▲ " + promptName(m.permission) + " · switch"
	case review > 0:
		return fmt.Sprintf("● %d to review · switch", review)
	}
	return "switch"
}

// attentionAfter is how long a lead turn runs before its end is worth a ping.
const attentionAfter = 30 * time.Second

// ping rings the bell and sends a desktop notification (OSC 9) when Switch
// comes to need the user while the terminal is in the background: a
// permission prompt opens, a task is ready for review, the run ends, or a
// long lead turn finishes.
func (m model) ping(prev model) tea.Cmd {
	if !m.blurred {
		return nil
	}
	_, _, review, _, _ := m.taskCounts()
	_, _, reviewBefore, _, _ := prev.taskCounts()
	running := func(x model) bool { return x.job == "run" || x.runOwner != 0 }
	var text string
	switch {
	case m.permission != nil && m.permission != prev.permission:
		text = m.statusLine().detail
	case review > reviewBefore:
		text = plural(review, "task") + " ready for review"
	case running(prev) && !running(m) && m.run != nil:
		text, _, _ = strings.Cut(runOutcomeText(m.run.Status, nil), "\n")
	case prev.busy && !m.busy && time.Since(prev.turnStarted) >= attentionAfter:
		text = m.lead.Name + " finished"
	default:
		return nil
	}
	// Agents write prompt titles; a control character would end the sequence.
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, truncate(text, 120))
	return tea.Raw("\a" + ansi.Notify("Switch: "+text))
}

func (m model) warmWorkers() int {
	n := 0
	for _, agent := range m.dashboardFleet() {
		if agent.Name != m.lead.Name {
			if status := m.team.status(agent.Name); status == "ready" || status == "answering" {
				n++
			}
		}
	}
	return n
}

func progressBar(done, total, width int) string {
	if total <= 0 {
		return ""
	}
	filled := done * width / total
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// since formats elapsed time as 14s, 3:10 or 1:02:03.
func since(start time.Time) string {
	if start.IsZero() {
		return ""
	}
	d := time.Since(start).Round(time.Second)
	h, mins, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%d:%02d:%02d", h, mins, s)
	case mins > 0:
		return fmt.Sprintf("%d:%02d", mins, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

type clockMsg struct{}

// clockTick advances spinners and timers while any agent works.
func clockTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return clockMsg{} })
}

func (m model) timing() bool { return m.busy || m.job != "" || len(m.agentBusy) > 0 }

type gitStatusMsg struct {
	branch string
	dirty  int
}

func gitStatusCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "--branch")
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0") // never hold the index lock the user's git needs
		out, err := cmd.Output()
		if err != nil {
			return gitStatusMsg{}
		}
		return parseGitStatus(string(out))
	}
}

func parseGitStatus(out string) gitStatusMsg {
	var s gitStatusMsg
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		branch, ok := strings.CutPrefix(line, "## ")
		if !ok {
			if line != "" {
				s.dirty++
			}
			continue
		}
		branch = strings.TrimPrefix(branch, "No commits yet on ")
		branch, _, _ = strings.Cut(branch, "...")
		branch, _, _ = strings.Cut(branch, " [")
		if branch == "HEAD (no branch)" {
			branch = "detached"
		}
		s.branch = branch
	}
	return s
}
