// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

var (
	tuiTitleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	tuiLabelStyle   = lipgloss.NewStyle().Faint(true)
	tuiDoneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	tuiActiveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	tuiPendingStyle = lipgloss.NewStyle().Faint(true)
	tuiErrStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	tuiOKStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
)

// imageJobSteps is the canonical ordered pipeline shown in the TUI. The runner
// happy path only writes a subset of these phases, so step state is derived by
// comparing the job's current phase position against this order.
var imageJobSteps = []string{
	"PULLING",
	"UNPACKING",
	"BUILDING_EXT4",
	"GENERATING_JSON",
	"DISTRIBUTING",
	"CREATING_TEMPLATE",
}

// stepIndexForPhase returns the index of phase within imageJobSteps, or -1 when
// the phase is unknown (so the caller can keep its high-water mark instead of
// snapping the checklist backwards).
func stepIndexForPhase(phase string) int {
	for i, s := range imageJobSteps {
		if s == phase {
			return i
		}
	}
	return -1
}

const (
	tuiBarWidth    = 36
	tuiMaxBarWidth = 60
)

// ---------------------------------------------------------------------------
// messages
// ---------------------------------------------------------------------------

type imageJobMsg struct {
	job *types.TemplateImageJobInfo
	err error
}

type buildStatusMsg struct {
	rsp *templateBuildStatusResponse
	err error
}

type pollTickMsg struct{}

// ---------------------------------------------------------------------------
// image job TUI
// ---------------------------------------------------------------------------

type imageJobTUI struct {
	c        *cli.Context
	jobID    string
	interval time.Duration

	spin    spinner.Model
	overall progress.Model
	pull    progress.Model
	dist    progress.Model

	job      *types.TemplateImageJobInfo
	stepIdx  int
	pollErr  error
	fatal    error
	start    time.Time
	width    int
	done     bool
	canceled bool
}

func newImageJobTUI(c *cli.Context, jobID string) imageJobTUI {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiActiveStyle
	return imageJobTUI{
		c:        c,
		jobID:    jobID,
		interval: watchInterval(c),
		spin:     sp,
		overall:  progress.New(progress.WithDefaultGradient(), progress.WithWidth(tuiBarWidth)),
		pull:     progress.New(progress.WithDefaultGradient(), progress.WithWidth(tuiBarWidth)),
		dist:     progress.New(progress.WithDefaultGradient(), progress.WithWidth(tuiBarWidth)),
		start:    time.Now(),
	}
}

func (m imageJobTUI) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.fetch())
}

func (m imageJobTUI) fetch() tea.Cmd {
	return func() tea.Msg {
		rsp, err := fetchTemplateImageJob(m.c, m.jobID)
		if err != nil {
			return imageJobMsg{err: err}
		}
		return imageJobMsg{job: rsp.Job}
	}
}

func (m imageJobTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.canceled = true
			m.done = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		w := msg.Width - 16
		if w > tuiMaxBarWidth {
			w = tuiMaxBarWidth
		}
		if w < 10 {
			w = 10
		}
		m.overall.Width = w
		m.pull.Width = w
		m.dist.Width = w
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case pollTickMsg:
		return m, m.fetch()
	case imageJobMsg:
		if msg.err != nil {
			// Transient fetch errors are surfaced but polling continues.
			m.pollErr = msg.err
			return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return pollTickMsg{} })
		}
		m.pollErr = nil
		if msg.job == nil {
			m.fatal = errors.New("empty job")
			m.done = true
			return m, tea.Quit
		}
		m.job = msg.job
		if idx := stepIndexForPhase(m.job.Phase); idx > m.stepIdx {
			m.stepIdx = idx
		}
		switch m.job.Status {
		case "READY":
			m.done = true
			return m, tea.Quit
		case "FAILED":
			m.fatal = errors.New(imageJobFailureMessage(m.job))
			m.done = true
			return m, tea.Quit
		}
		return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return pollTickMsg{} })
	}
	return m, nil
}

func (m imageJobTUI) View() string {
	var b strings.Builder
	b.WriteString(tuiTitleStyle.Render("● Building template from image") + "\n\n")

	templateID := ""
	imageRef := ""
	if m.job != nil {
		templateID = m.job.TemplateID
		if m.job.Artifact != nil {
			imageRef = m.job.Artifact.SourceImageRef
		}
	}
	b.WriteString(tuiKV("template", templateID))
	b.WriteString(tuiKV("job", m.jobID))
	if imageRef != "" {
		b.WriteString(tuiKV("image", imageRef))
	}
	b.WriteString("\n")

	b.WriteString(m.renderSteps())
	b.WriteString("\n")

	b.WriteString(m.renderPull())
	b.WriteString(m.renderDistribution())
	b.WriteString(m.renderOverall())

	b.WriteString("\n")
	b.WriteString(tuiLabelStyle.Render("elapsed ") + formatElapsed(time.Since(m.start)))
	if m.pollErr != nil {
		b.WriteString("  " + tuiErrStyle.Render("poll error: "+m.pollErr.Error()))
	}
	b.WriteString("\n")
	b.WriteString(tuiLabelStyle.Render("press q to stop watching (job keeps running)") + "\n")
	return b.String()
}

func (m imageJobTUI) currentStepIndex() int {
	if m.job != nil && m.job.Status == "READY" {
		return len(imageJobSteps)
	}
	return m.stepIdx
}

// imageJobFailureMessage returns a non-empty error string for a failed job so
// the command never surfaces an empty error.
func imageJobFailureMessage(job *types.TemplateImageJobInfo) string {
	if job != nil && strings.TrimSpace(job.ErrorMessage) != "" {
		return job.ErrorMessage
	}
	return "template image job failed"
}

func (m imageJobTUI) renderSteps() string {
	cur := m.currentStepIndex()
	failed := m.job != nil && m.job.Status == "FAILED"
	var b strings.Builder
	for i, step := range imageJobSteps {
		switch {
		case i < cur:
			b.WriteString("  " + tuiDoneStyle.Render("✓ "+step) + "\n")
		case i == cur && failed:
			b.WriteString("  " + tuiErrStyle.Render("✗ "+step) + "\n")
		case i == cur:
			b.WriteString("  " + m.spin.View() + tuiActiveStyle.Render(step) + "\n")
		default:
			b.WriteString("  " + tuiPendingStyle.Render("○ "+step) + "\n")
		}
	}
	if m.job != nil && m.job.Status == "READY" {
		b.WriteString("  " + tuiOKStyle.Render("✓ READY") + "\n")
	}
	return b.String()
}

func (m imageJobTUI) renderPull() string {
	if m.job == nil {
		return ""
	}
	// Only meaningful while the source image is being fetched.
	if m.job.Phase != "PULLING" && m.job.Phase != "UNPACKING" {
		if m.job.PullDownloadedBytes == 0 && m.job.PullCompletedLayers == 0 {
			return ""
		}
	}
	// When the job is finished, the pull phase is definitely complete.
	jobDone := m.job.Status == "READY" || m.job.Status == "FAILED"
	var detail string
	var pct float64
	switch {
	case m.job.PullTotalBytes > 0:
		pct = float64(m.job.PullDownloadedBytes) / float64(m.job.PullTotalBytes)
		detail = fmt.Sprintf("%s/%s", humanBytes(m.job.PullDownloadedBytes), humanBytes(m.job.PullTotalBytes))
		if m.job.PullSpeedBPS > 0 && !jobDone {
			detail += fmt.Sprintf("  %s/s", humanBytes(m.job.PullSpeedBPS))
		}
		if m.job.PullTotalLayers > 0 {
			detail += fmt.Sprintf("  layer %d/%d", m.job.PullCompletedLayers, m.job.PullTotalLayers)
		}
	case m.job.PullTotalLayers > 0:
		pct = float64(m.job.PullCompletedLayers) / float64(m.job.PullTotalLayers)
		detail = fmt.Sprintf("layer %d/%d", m.job.PullCompletedLayers, m.job.PullTotalLayers)
	default:
		return tuiLabelStyle.Render("pull   ") + m.spin.View() + tuiLabelStyle.Render("fetching image…") + "\n"
	}
	if jobDone {
		pct = 1.0
	}
	return tuiLabelStyle.Render("pull   ") + m.pull.ViewAs(clampUnit(pct)) + "  " + detail + "\n"
}

func (m imageJobTUI) renderDistribution() string {
	if m.job == nil || m.job.ExpectedNodeCount == 0 {
		return ""
	}
	pct := float64(m.job.ReadyNodeCount) / float64(m.job.ExpectedNodeCount)
	// Snap bar to 100% when the job is terminal: partial distribution is a
	// valid success path (some nodes may have had the artifact cached), so a
	// frozen sub-100% bar alongside "✓ READY" is misleading.
	if m.job.Status == "READY" {
		pct = 1.0
	}
	detail := fmt.Sprintf("%d/%d ready", m.job.ReadyNodeCount, m.job.ExpectedNodeCount)
	if m.job.FailedNodeCount > 0 {
		detail += "  " + tuiErrStyle.Render(fmt.Sprintf("%d failed", m.job.FailedNodeCount))
	}
	return tuiLabelStyle.Render("dist   ") + m.dist.ViewAs(clampUnit(pct)) + "  " + detail + "\n"
}

func (m imageJobTUI) renderOverall() string {
	pct := 0.0
	if m.job != nil {
		pct = float64(m.job.Progress) / 100
		// The server writes progress=100 atomically with status=READY, but a
		// client poll may still catch an intermediate value if the final DB
		// write raced with the poll window. Clamp defensively so the bar
		// always fills on a terminal state.
		if m.job.Status == "READY" {
			pct = 1.0
		}
	}
	return tuiLabelStyle.Render("total  ") + m.overall.ViewAs(clampUnit(pct)) + "\n"
}

// ---------------------------------------------------------------------------
// build (commit) TUI
// ---------------------------------------------------------------------------

type buildJobTUI struct {
	c        *cli.Context
	buildID  string
	interval time.Duration

	spin    spinner.Model
	overall progress.Model

	rsp      *templateBuildStatusResponse
	pollErr  error
	fatal    error
	start    time.Time
	width    int
	done     bool
	canceled bool
}

func newBuildJobTUI(c *cli.Context, buildID string) buildJobTUI {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiActiveStyle
	return buildJobTUI{
		c:        c,
		buildID:  buildID,
		interval: watchInterval(c),
		spin:     sp,
		overall:  progress.New(progress.WithDefaultGradient(), progress.WithWidth(tuiBarWidth)),
		start:    time.Now(),
	}
}

func (m buildJobTUI) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.fetch())
}

func (m buildJobTUI) fetch() tea.Cmd {
	return func() tea.Msg {
		rsp, err := fetchTemplateBuildStatus(m.c, m.buildID)
		if err != nil {
			return buildStatusMsg{err: err}
		}
		return buildStatusMsg{rsp: rsp}
	}
}

func (m buildJobTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.canceled = true
			m.done = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		w := msg.Width - 16
		if w > tuiMaxBarWidth {
			w = tuiMaxBarWidth
		}
		if w < 10 {
			w = 10
		}
		m.overall.Width = w
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case pollTickMsg:
		return m, m.fetch()
	case buildStatusMsg:
		if msg.err != nil {
			m.pollErr = msg.err
			return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return pollTickMsg{} })
		}
		m.pollErr = nil
		m.rsp = msg.rsp
		switch m.rsp.Status {
		case "ready":
			m.done = true
			return m, tea.Quit
		case "error":
			msg := m.rsp.Message
			if strings.TrimSpace(msg) == "" {
				msg = "sandbox commit build failed"
			}
			m.fatal = errors.New(msg)
			m.done = true
			return m, tea.Quit
		}
		return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return pollTickMsg{} })
	}
	return m, nil
}

func (m buildJobTUI) View() string {
	var b strings.Builder
	b.WriteString(tuiTitleStyle.Render("● Committing sandbox to template") + "\n\n")

	templateID := ""
	status := "building"
	message := ""
	pct := 0.0
	if m.rsp != nil {
		templateID = m.rsp.TemplateID
		if m.rsp.Status != "" {
			status = m.rsp.Status
		}
		message = m.rsp.Message
		pct = float64(m.rsp.Progress) / 100
	}
	b.WriteString(tuiKV("template", templateID))
	b.WriteString(tuiKV("build", m.buildID))
	b.WriteString("\n")

	switch status {
	case "ready":
		b.WriteString("  " + tuiOKStyle.Render("✓ ready") + "\n")
	case "error":
		b.WriteString("  " + tuiErrStyle.Render("✗ error") + "\n")
	default:
		b.WriteString("  " + m.spin.View() + tuiActiveStyle.Render(status) + "\n")
	}
	if message != "" {
		b.WriteString(tuiKV("phase", message))
	}
	b.WriteString("\n")
	b.WriteString(tuiLabelStyle.Render("total  ") + m.overall.ViewAs(clampUnit(pct)) + "\n")

	b.WriteString("\n")
	b.WriteString(tuiLabelStyle.Render("elapsed ") + formatElapsed(time.Since(m.start)))
	if m.pollErr != nil {
		b.WriteString("  " + tuiErrStyle.Render("poll error: "+m.pollErr.Error()))
	}
	b.WriteString("\n")
	b.WriteString(tuiLabelStyle.Render("press q to stop watching (build keeps running)") + "\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func tuiKV(key, value string) string {
	return "  " + tuiLabelStyle.Render(fmt.Sprintf("%-9s", key)) + value + "\n"
}

func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}
