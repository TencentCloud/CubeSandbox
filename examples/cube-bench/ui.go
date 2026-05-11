package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const maxRecentLogs = 8

type resultMsg IterResult
type tickMsg time.Time
type doneMsg struct{}

type model struct {
	cfg       *Config
	resultCh  <-chan IterResult
	results   []IterResult
	total     int
	completed int
	errors    int
	startTime time.Time
	progress  progress.Model
	recent    []IterResult
	qpsCount  int
	qpsStart  time.Time
	done      bool
	width     int
}

func newModel(cfg *Config, resultCh <-chan IterResult) model {
	p := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(50),
	)
	return model{
		cfg:      cfg,
		resultCh: resultCh,
		total:    cfg.Total,
		progress: p,
		qpsStart: time.Now(),
		width:    80,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(waitForResult(m.resultCh), tickCmd())
}

func waitForResult(ch <-chan IterResult) tea.Cmd {
	return func() tea.Msg {
		r, ok := <-ch
		if !ok {
			return doneMsg{}
		}
		return resultMsg(r)
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.progress.Width = msg.Width - 10
		if m.progress.Width < 20 {
			m.progress.Width = 20
		}
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil

	case resultMsg:
		r := IterResult(msg)
		m.results = append(m.results, r)
		m.completed++
		if r.Err != "" {
			m.errors++
		}
		if m.startTime.IsZero() {
			m.startTime = time.Now()
		}
		m.qpsCount++
		m.recent = append(m.recent, r)
		if len(m.recent) > maxRecentLogs {
			m.recent = m.recent[len(m.recent)-maxRecentLogs:]
		}
		return m, waitForResult(m.resultCh)

	case doneMsg:
		m.done = true
		return m, tea.Quit

	case tickMsg:
		if m.done {
			return m, nil
		}
		return m, tickCmd()

	case progress.FrameMsg:
		progressModel, cmd := m.progress.Update(msg)
		m.progress = progressModel.(progress.Model)
		return m, cmd
	}

	return m, nil
}

func (m model) View() string {
	if m.width == 0 {
		return ""
	}

	var b strings.Builder
	w := m.width
	if w < 40 {
		w = 40
	}

	// Progress bar
	pct := 0.0
	if m.total > 0 {
		pct = float64(m.completed) / float64(m.total)
	}
	elapsed := time.Since(m.startTime)
	if m.startTime.IsZero() {
		elapsed = 0
	}

	label := "Benchmarking"
	if m.cfg.DryRun {
		label = "Benchmarking (dry-run)"
	}

	progressLine := fmt.Sprintf("  %s  %s  %d/%d  %s",
		T.Heading.Render(label),
		m.progress.ViewAs(pct),
		m.completed, m.total,
		elapsed.Truncate(time.Millisecond),
	)
	b.WriteString(progressLine)
	b.WriteString("\n\n")

	// Stats + Recent logs side by side
	statsStr := m.renderStats(elapsed)
	logsStr := m.renderRecentLogs()

	statsW := w/3 - 2
	if statsW < 20 {
		statsW = 20
	}
	logsW := w - statsW - 6
	if logsW < 30 {
		logsW = 30
	}

	statsBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(T.Border).
		Width(statsW).
		Padding(0, 1).
		Render(statsStr)

	logsBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(T.Border).
		Width(logsW).
		Padding(0, 1).
		Render(logsStr)

	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, statsBox, " ", logsBox))
	b.WriteString("\n")

	return b.String()
}

func (m model) renderStats(elapsed time.Duration) string {
	var b strings.Builder
	b.WriteString(T.Heading.Render(" Live Stats") + "\n\n")

	okCount := m.completed - m.errors
	var createAvg, deleteAvg float64
	count := 0
	for _, r := range m.results {
		if r.Err == "" {
			createAvg += r.CreateMs
			deleteAvg += r.DeleteMs
			count++
		}
	}
	if count > 0 {
		createAvg /= float64(count)
		deleteAvg /= float64(count)
	}

	qps := 0.0
	qpsDur := time.Since(m.qpsStart).Seconds()
	if qpsDur > 0 {
		qps = float64(m.qpsCount) / qpsDur
	}

	kv := func(key, val string) {
		b.WriteString(fmt.Sprintf("  %-12s %s\n", key, val))
	}

	kv("Completed", T.OK.Render(fmt.Sprintf("%d", okCount))+" / "+fmt.Sprintf("%d", m.total))
	errStyle := T.Muted
	if m.errors > 0 {
		errStyle = T.Error
	}
	kv("Errors", errStyle.Render(fmt.Sprintf("%d", m.errors)))
	kv("QPS", T.Accent.Render(fmt.Sprintf("%.1f", qps))+" req/s")
	kv("Avg Create", LatencyStyle(createAvg).Render(fmt.Sprintf("%.0f ms", createAvg)))
	kv("Avg Delete", LatencyStyle(deleteAvg).Render(fmt.Sprintf("%.0f ms", deleteAvg)))
	kv("Elapsed", fmt.Sprintf("%.1fs", elapsed.Seconds()))

	return b.String()
}

func (m model) renderRecentLogs() string {
	var b strings.Builder
	b.WriteString(T.Heading.Render(" Recent Operations") + "\n\n")

	if len(m.recent) == 0 {
		b.WriteString(T.Muted.Render("  waiting..."))
		return b.String()
	}

	for i := len(m.recent) - 1; i >= 0; i-- {
		r := m.recent[i]
		if r.Err != "" {
			b.WriteString(T.Error.Render(fmt.Sprintf("  #%4d  ERR  %s", r.Seq, truncate(r.Err, 50))))
		} else {
			b.WriteString(fmt.Sprintf("  %s  CREATE %s  DELETE %s",
				T.Muted.Render(fmt.Sprintf("#%4d", r.Seq)),
				LatencyStyle(r.CreateMs).Render(fmt.Sprintf("%7.0fms", r.CreateMs)),
				LatencyStyle(r.DeleteMs).Render(fmt.Sprintf("%7.0fms", r.DeleteMs)),
			))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
