package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Colours are deliberately dull. A screen where everything is bright is a
// screen where nothing stands out, and the only thing worth spotting at a
// glance here is a hostage block.
var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	headStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("66"))
	ruleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	faintStyle   = lipgloss.NewStyle().Faint(true)
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("179"))
	badStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("167"))
	goodStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("71"))
	boldStyle    = lipgloss.NewStyle().Bold(true)
	hostageStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
)

// The map draws thousands of cells per frame, so those go out as raw escapes
// rather than through a style object.
const (
	cReset   = "\x1b[0m"
	cHostage = "\x1b[1;38;5;203m"

	pinFree      = "\x1b[38;5;238m"
	pinMovable   = "\x1b[38;5;67m"
	pinImmovable = "\x1b[38;5;137m"
)

func faint(s string) string { return faintStyle.Render(s) }
func head(s string) string  { return headStyle.Render(s) }

func rule(w int) string { return ruleStyle.Render(strings.Repeat("─", w)) }

func padVisible(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// joinPanes lays two columns of text side by side. lipgloss.JoinHorizontal is
// not used because the map and bar strings carry escapes it would try to
// re-wrap; padding by visible width keeps the separator straight.
func joinPanes(left, right []string, leftWidth int) []string {
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	sep := ruleStyle.Render(" │ ")
	out := make([]string, n)
	for i := 0; i < n; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		out[i] = padVisible(l, leftWidth) + sep + r
	}
	return out
}
