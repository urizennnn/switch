package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Design tokens. Every view uses these values so the product keeps one visual
// language for its whole lifetime.
var (
	accent     = lipgloss.Color("#ff7849")
	teal       = lipgloss.Color("#5fc5b0")
	text       = lipgloss.Color("#e6e4dd")
	muted      = lipgloss.Color("#8b8f9a")
	dim        = lipgloss.Color("#555964")
	border     = lipgloss.Color("#3a3f4b")
	red        = lipgloss.Color("#e05d5d")
	surface    = lipgloss.Color("#20242c")
	surfaceAlt = lipgloss.Color("#2a2f3a")
)

var (
	brandStyle  = lipgloss.NewStyle().Foreground(accent).Bold(true)
	titleStyle  = lipgloss.NewStyle().Foreground(teal).Bold(true)
	labelStyle  = lipgloss.NewStyle().Foreground(accent).Bold(true)
	agentStyle  = lipgloss.NewStyle().Foreground(teal).Bold(true)
	textStyle   = lipgloss.NewStyle().Foreground(text)
	mutedStyle  = lipgloss.NewStyle().Foreground(muted)
	dimStyle    = lipgloss.NewStyle().Foreground(dim)
	errorStyle  = lipgloss.NewStyle().Foreground(red)
	systemStyle = lipgloss.NewStyle().Foreground(dim).Italic(true)

	selectedStyle = lipgloss.NewStyle().
			Background(surfaceAlt).
			Foreground(text).
			Bold(true)

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1)

	inputStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1)
)

const (
	spaceXS = 0
	spaceS  = 1
	spaceM  = 2
)

func section(title string) string {
	return titleStyle.Render(strings.ToUpper(title)) + "\n"
}

func keyValue(key, value string) string {
	return dimStyle.Render(key) + "  " + mutedStyle.Render(value)
}

func panel(width, height int, title, content string) string {
	head := ""
	if strings.TrimSpace(title) != "" {
		head = titleStyle.Render(strings.ToUpper(title)) + "\n"
	}
	return panelStyle.
		Width(max(10, width)).
		Height(max(3, height)).
		Render(head + content)
}

func appHeader(width int, left, right string) string {
	rightWidth := lipgloss.Width(right)
	leftWidth := max(10, width-rightWidth-spaceM)
	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		lipgloss.NewStyle().Width(leftWidth).Render(left),
		lipgloss.NewStyle().Width(rightWidth).Align(lipgloss.Right).Render(right),
	)
}

func inputBar(width int, value string) string {
	return inputStyle.
		Width(max(10, width-2)).
		Render(dimStyle.Render("› ") + value)
}

func statusBar(width int, left, right string) string {
	rightWidth := lipgloss.Width(right)
	leftWidth := max(10, width-rightWidth-spaceM)
	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		dimStyle.Width(leftWidth).Render(left),
		dimStyle.Width(rightWidth).Align(lipgloss.Right).Render(right),
	)
}

func palettePanel(width int, content string) string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Background(surface).
		Padding(0, 1).
		Width(max(20, width-2)).
		Render(content)
}

func truncate(value string, limit int) string {
	if limit < 1 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func truncateLines(value string, limit int) string {
	lines := strings.Split(value, "\n")
	if len(lines) <= limit {
		return value
	}
	return strings.Join(lines[:limit], "\n") + "\n" + dimStyle.Render("…")
}
