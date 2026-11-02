package tui

import "charm.land/lipgloss/v2"

var (
	accent = lipgloss.Color("#ff7849")
	teal   = lipgloss.Color("#5fc5b0")
	dim    = lipgloss.Color("#555964")
	muted  = lipgloss.Color("#8b8f9a")
	text   = lipgloss.Color("#e6e4dd")
	border = lipgloss.Color("#3a3f4b")
	red    = lipgloss.Color("#e05d5d")

	labelStyle   = lipgloss.NewStyle().Foreground(accent).Bold(true)
	agentStyle   = lipgloss.NewStyle().Foreground(teal).Bold(true)
	mutedStyle   = lipgloss.NewStyle().Foreground(muted)
	dimStyle     = lipgloss.NewStyle().Foreground(dim)
	textStyle    = lipgloss.NewStyle().Foreground(text)
	errorStyle   = lipgloss.NewStyle().Foreground(red)
	systemStyle  = lipgloss.NewStyle().Foreground(dim).Italic(true)
	headerStyle  = lipgloss.NewStyle().Foreground(accent).Bold(true)
	dividerStyle = lipgloss.NewStyle().Foreground(border)
)

var paletteSelected = lipgloss.NewStyle().
	Foreground(text).
	Background(lipgloss.Color("#2a2f3a")).
	Bold(true)
