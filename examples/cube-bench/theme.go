package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type Theme struct {
	Banner   lipgloss.Style
	Heading  lipgloss.Style
	Value    lipgloss.Style
	Accent   lipgloss.Style
	Border   lipgloss.Color
	BorderOK lipgloss.Color
	Muted    lipgloss.Style
	OK       lipgloss.Style
	Error    lipgloss.Style
	Warn     lipgloss.Style

	BarActive lipgloss.Color
	BarDone   lipgloss.Color

	LatFast lipgloss.Style
	LatOK   lipgloss.Style
	LatWarn lipgloss.Style
	LatSlow lipgloss.Style
	LatCrit lipgloss.Style

	GradeS lipgloss.Style
	GradeA lipgloss.Style
	GradeB lipgloss.Style
	GradeC lipgloss.Style
	GradeD lipgloss.Style
}

var (
	DarkTheme = Theme{
		Banner:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		Heading:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")),
		Value:    lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		Accent:   lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
		Border:   lipgloss.Color("12"),
		BorderOK: lipgloss.Color("10"),
		Muted:    lipgloss.NewStyle().Faint(true),
		OK:       lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
		Error:    lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		Warn:     lipgloss.NewStyle().Foreground(lipgloss.Color("11")),

		BarActive: lipgloss.Color("14"),
		BarDone:   lipgloss.Color("10"),

		LatFast: lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
		LatOK:   lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		LatWarn: lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		LatSlow: lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		LatCrit: lipgloss.NewStyle().Foreground(lipgloss.Color("1")),

		GradeS: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13")),
		GradeA: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")),
		GradeB: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11")),
		GradeC: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9")),
		GradeD: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")),
	}

	LightTheme = Theme{
		Banner:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4")),
		Heading:  lipgloss.NewStyle().Bold(true),
		Value:    lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		Accent:   lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
		Border:   lipgloss.Color("4"),
		BorderOK: lipgloss.Color("2"),
		Muted:    lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		OK:       lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		Error:    lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		Warn:     lipgloss.NewStyle().Foreground(lipgloss.Color("208")),

		BarActive: lipgloss.Color("4"),
		BarDone:   lipgloss.Color("2"),

		LatFast: lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		LatOK:   lipgloss.NewStyle().Foreground(lipgloss.Color("22")),
		LatWarn: lipgloss.NewStyle().Foreground(lipgloss.Color("208")),
		LatSlow: lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		LatCrit: lipgloss.NewStyle().Foreground(lipgloss.Color("52")),

		GradeS: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5")),
		GradeA: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("22")),
		GradeB: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208")),
		GradeC: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")),
		GradeD: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("52")),
	}

	T = DarkTheme
)

func DetectTheme() Theme {
	if v := os.Getenv("COLORFGBG"); v != "" {
		parts := strings.Split(v, ";")
		if bg, err := strconv.Atoi(parts[len(parts)-1]); err == nil && bg >= 8 {
			return LightTheme
		}
	}
	if os.Getenv("TERM_LIGHT") != "" {
		return LightTheme
	}
	return DarkTheme
}

func LatencyStyle(ms float64) lipgloss.Style {
	switch {
	case ms < 100:
		return T.LatFast
	case ms < 300:
		return T.LatOK
	case ms < 500:
		return T.LatWarn
	case ms < 1000:
		return T.LatSlow
	default:
		return T.LatCrit
	}
}

func GradeResult(p99Ms float64, successRate float64) (string, lipgloss.Style) {
	type grade struct {
		threshold float64
		rateMin   float64
		letter    string
		style     lipgloss.Style
	}
	grades := []grade{
		{100, 0.999, "S", T.GradeS},
		{200, 0.99, "A", T.GradeA},
		{500, 0.95, "B", T.GradeB},
		{1000, 0.90, "C", T.GradeC},
	}
	for _, g := range grades {
		if p99Ms <= g.threshold && successRate >= g.rateMin {
			return g.letter, g.style
		}
	}
	return "D", T.GradeD
}
