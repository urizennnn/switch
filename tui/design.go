package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// Design tokens. Every view uses these values so the product keeps one visual
// language for its whole lifetime.
var (
	accent     = lipgloss.Color("#ff6b3d")
	teal       = lipgloss.Color("#3ee0c0")
	text       = lipgloss.Color("#f4f2ec")
	muted      = lipgloss.Color("#b7bdc9")
	dim        = lipgloss.Color("#7d8797")
	border     = lipgloss.Color("#4b5363")
	red        = lipgloss.Color("#ff5f6d")
	green      = lipgloss.Color("#48e08f")
	yellow     = lipgloss.Color("#f5c451")
	surface    = lipgloss.Color("#171b22")
	surfaceAlt = lipgloss.Color("#303847")
)

var (
	brandStyle        = lipgloss.NewStyle().Foreground(accent).Bold(true)
	titleStyle        = lipgloss.NewStyle().Foreground(teal).Bold(true)
	labelStyle        = lipgloss.NewStyle().Foreground(accent).Bold(true)
	agentStyle        = lipgloss.NewStyle().Foreground(teal).Bold(true)
	userMarkerStyle   = lipgloss.NewStyle().Foreground(muted)
	switchMarkerStyle = lipgloss.NewStyle().Foreground(dim)
	textStyle         = lipgloss.NewStyle().Foreground(text)
	mutedStyle        = lipgloss.NewStyle().Foreground(muted)
	dimStyle          = lipgloss.NewStyle().Foreground(dim)
	errorStyle        = lipgloss.NewStyle().Foreground(red).Bold(true)
	successStyle      = lipgloss.NewStyle().Foreground(green).Bold(true)
	warningStyle      = lipgloss.NewStyle().Foreground(yellow).Bold(true)
	systemStyle       = lipgloss.NewStyle().Foreground(dim).Italic(true)

	selectedStyle = lipgloss.NewStyle().
			Background(surfaceAlt).
			Foreground(text).
			Bold(true)

	activeStyle = lipgloss.NewStyle().
			Background(accent).
			Foreground(surface).
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
	return mutedStyle.Render(key) + "  " + textStyle.Render(value)
}

func commandRow(label, description string) string {
	return labelStyle.Render(fmt.Sprintf("%-26s", label)) + " " + mutedStyle.Render(description)
}

func panel(width, height int, title, content string) string {
	head := ""
	if strings.TrimSpace(title) != "" {
		head = titleStyle.Render(strings.ToUpper(title)) + "\n"
	}
	// Height is only a minimum; clip to the rows inside the border so long
	// content cannot push this panel taller than its neighbour.
	return panelStyle.
		Width(max(10, width)).
		Height(max(3, height)).
		Render(truncateLines(head+strings.TrimRight(content, "\n"), max(1, height-2)))
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
		Render(brandStyle.Render("› ") + textStyle.Render(value))
}

func statusBar(width int, left, right string) string {
	rightWidth := lipgloss.Width(right)
	leftWidth := max(10, width-rightWidth-spaceM)
	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		mutedStyle.Width(leftWidth).Render(left),
		dimStyle.Width(rightWidth).Align(lipgloss.Right).Render(right),
	)
}

func modalPanel(width int, title, content, footer string) string {
	body := content
	if footer != "" {
		body += "\n\n" + dimStyle.Render(footer)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Background(surface).
		Padding(1, 2).
		Width(max(30, width)).
		Render(titleStyle.Render(strings.ToUpper(title)) + "\n\n" + body)
}

func palettePanel(width int, content string) string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
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

func tailLines(value string, limit int) string {
	value = strings.TrimRight(value, "\n")
	if limit < 1 {
		return value
	}
	lines := strings.Split(value, "\n")
	if len(lines) <= limit {
		return value
	}
	return "…\n" + strings.Join(lines[len(lines)-limit+1:], "\n")
}

// truncateLines keeps at most limit lines, ending with an ellipsis when cut.
func truncateLines(value string, limit int) string {
	lines := strings.Split(value, "\n")
	if len(lines) <= limit {
		return value
	}
	return strings.Join(lines[:max(0, limit-1)], "\n") + "\n" + dimStyle.Render("…")
}
