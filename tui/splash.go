package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

var (
	accent = lipgloss.Color("#ff7849")
	teal   = lipgloss.Color("#5fc5b0")
	dim    = lipgloss.Color("#555964")
	muted  = lipgloss.Color("#8b8f9a")
	text   = lipgloss.Color("#e6e4dd")
	border = lipgloss.Color("#3a3f4b")
)

func (m model) SplashView() string {
	colWidth := (m.Width / 2) - 4

	// ── left column ───────────────────────────────────────────
	left := lipgloss.NewStyle().
		Width(colWidth).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.NewStyle().Foreground(accent).Bold(true).Render("switch"),
			lipgloss.NewStyle().Foreground(dim).Render("multi-agent reasoning"),
			"\n",
			lipgloss.NewStyle().Foreground(muted).Render(
				fmt.Sprintf("claude-sonnet-4-6\n%d minds active", len(m.Minds)),
			),
			lipgloss.NewStyle().Foreground(dim).Render("~/switch"),
		))

	// ── vertical divider ──────────────────────────────────────
	dividerHeight := lipgloss.Height(left)
	var dividerStr strings.Builder
	for range dividerHeight {
		dividerStr.WriteString("│\n")
	}
	mid := lipgloss.NewStyle().
		Foreground(border).
		Render(dividerStr.String())

	// ── right column ──────────────────────────────────────────
	right := lipgloss.NewStyle().
		Width(colWidth).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.NewStyle().Foreground(accent).Bold(true).Render("Getting started"),
			lipgloss.NewStyle().Foreground(muted).Render(
				"State a problem and switch deliberates\nacross all minds in parallel.",
			),
			"\n",
			lipgloss.NewStyle().Foreground(teal).Bold(true).Render("Keybindings"),
			lipgloss.NewStyle().Foreground(muted).Render(
				lipgloss.NewStyle().Foreground(text).Render("ctrl+a")+" approve   "+
					lipgloss.NewStyle().Foreground(text).Render("ctrl+r")+" reject\n"+
					lipgloss.NewStyle().Foreground(text).Render("ctrl+v")+" toggle minds   "+
					lipgloss.NewStyle().Foreground(text).Render("?")+" help",
			),
		))

	// ── join columns with divider ─────────────────────────────
	columns := lipgloss.JoinHorizontal(lipgloss.Top, left, mid, right)

	// ── outer box ─────────────────────────────────────────────
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Width(m.Width - 2).
		Render(columns)

	// ── input bar ─────────────────────────────────────────────
	inputBar := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(m.Width - 2).
		Render(
			lipgloss.NewStyle().Foreground(dim).Render("› ") +
				m.Prompter.View(),
		)

	// ── status line ───────────────────────────────────────────
	status := lipgloss.NewStyle().
		Foreground(dim).
		Padding(0, 1).
		Render("urizen · ~/switch · sonnet-4.6 · 3 minds")

	return lipgloss.JoinVertical(lipgloss.Left, box, inputBar, status)
}
