package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func RenderReport(results []IterResult, cfg *Config) {
	var okResults []IterResult
	for _, r := range results {
		if r.Err == "" {
			okResults = append(okResults, r)
		}
	}

	totalElapsed := 0.0
	if len(results) > 0 {
		totalElapsed = cfg.elapsed
	}
	successRate := 0.0
	if len(results) > 0 {
		successRate = float64(len(okResults)) / float64(len(results))
	}
	qps := 0.0
	if totalElapsed > 0 {
		qps = float64(len(okResults)) / totalElapsed
	}
	errorCount := len(results) - len(okResults)

	// ── Summary panel ──
	summaryContent := renderKV([]kvPair{
		{"Total Time", T.Accent.Render(fmt.Sprintf("%.2fs", totalElapsed))},
		{"Success Rate", renderSuccessRate(successRate, len(okResults), len(results))},
		{"Throughput", T.Accent.Render(fmt.Sprintf("%.2f", qps)) + " sandboxes/sec"},
	})

	summaryBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(T.BorderOK).
		Padding(1, 3).
		Width(78).
		Render(T.Heading.Render("  Summary") + "\n\n" + summaryContent)
	fmt.Println()
	fmt.Println(summaryBox)

	if len(okResults) == 0 {
		fmt.Println(T.Error.Bold(true).Render("\n  No successful results to report."))
		return
	}

	createTimes := extractTimes(okResults, func(r IterResult) float64 { return r.CreateMs })
	deleteTimes := extractTimes(okResults, func(r IterResult) float64 { return r.DeleteMs })

	renderLatencySection("CREATE", createTimes)
	if cfg.Mode == "create-delete" {
		renderLatencySection("DELETE", deleteTimes)
	}

	// ── Sparkline timeline ──
	sparkContent := renderSparklines(createTimes, deleteTimes, cfg.Mode)
	sparkBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(T.Border).
		Padding(1, 3).
		Width(78).
		Render(T.Heading.Render("  Latency Timeline") + "\n" +
			T.Muted.Render("  each char = avg of a time bucket, left=first right=last") + "\n\n" +
			sparkContent)
	fmt.Println()
	fmt.Println(sparkBox)

	// ── Errors ──
	if errorCount > 0 {
		renderErrors(results, errorCount)
	}

	// ── Grade ──
	p99 := Percentile(createTimes, 99)
	letter, style := GradeResult(p99, successRate)
	gradeStr := fmt.Sprintf("  Performance Grade:  %s   %s",
		style.Reverse(true).Render(fmt.Sprintf(" %s ", letter)),
		T.Muted.Render(fmt.Sprintf("(P99=%.0fms, success=%.1f%%)", p99, successRate*100)),
	)
	gradeBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(style.GetForeground()).
		Padding(1, 0).
		Width(78).
		Align(lipgloss.Center).
		Render(gradeStr)
	fmt.Println()
	fmt.Println(gradeBox)
	fmt.Println()
}

func renderLatencySection(label string, times []float64) {
	avg := Mean(times)
	std := StdDev(times)
	minV := Min(times)
	maxV := Max(times)
	p50 := Percentile(times, 50)
	p90 := Percentile(times, 90)
	p95 := Percentile(times, 95)
	p99 := Percentile(times, 99)

	// Percentile table
	headers := []string{"min", "avg", "std", "P50", "P90", "P95", "P99", "max"}
	vals := []float64{minV, avg, std, p50, p90, p95, p99, maxV}

	colW := 9
	headerLine := "  "
	valueLine := "  "
	sepLine := "  "
	for _, h := range headers {
		headerLine += fmt.Sprintf("%*s", colW, h)
		sepLine += strings.Repeat("─", colW)
	}
	for _, v := range vals {
		valueLine += LatencyStyle(v).Render(fmt.Sprintf("%*.1f", colW, v))
	}

	// Histogram
	buckets := Histogram(times, 12)
	maxCount := 0
	for _, b := range buckets {
		if b.Count > maxCount {
			maxCount = b.Count
		}
	}

	var histLines []string
	for _, bkt := range buckets {
		bar := HistogramBar(bkt.Count, maxCount, 35)
		mid := (bkt.Lo + bkt.Hi) / 2
		style := LatencyStyle(mid)
		pctOfTotal := 0.0
		if len(times) > 0 {
			pctOfTotal = float64(bkt.Count) / float64(len(times)) * 100
		}
		histLines = append(histLines,
			fmt.Sprintf("  %s  %s  %s",
				style.Render(fmt.Sprintf("%7.0f - %7.0f ms", bkt.Lo, bkt.Hi)),
				style.Render(fmt.Sprintf("%-35s", bar)),
				T.Muted.Render(fmt.Sprintf("%4d (%4.1f%%)", bkt.Count, pctOfTotal)),
			),
		)
	}

	content := headerLine + "\n" + sepLine + "\n" + valueLine + "\n\n" +
		lipgloss.NewStyle().Bold(true).Render("  Distribution:") + "\n" +
		strings.Join(histLines, "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(T.Border).
		Padding(1, 2).
		Width(78).
		Render(T.Heading.Render(fmt.Sprintf("  %s Latency", label)) + "\n\n" + content)

	fmt.Println()
	fmt.Println(box)
}

func renderSparklines(createTimes, deleteTimes []float64, mode string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("  %-10s %s  %s\n",
		lipgloss.NewStyle().Bold(true).Render("CREATE"),
		Sparkline(createTimes, 50),
		T.Muted.Render(fmt.Sprintf("%.0f .. %.0f ms", Min(createTimes), Max(createTimes))),
	))
	if mode == "create-delete" && len(deleteTimes) > 0 {
		b.WriteString(fmt.Sprintf("  %-10s %s  %s\n",
			lipgloss.NewStyle().Bold(true).Render("DELETE"),
			Sparkline(deleteTimes, 50),
			T.Muted.Render(fmt.Sprintf("%.0f .. %.0f ms", Min(deleteTimes), Max(deleteTimes))),
		))
	}
	return b.String()
}

func renderErrors(results []IterResult, errorCount int) {
	var errLines []string
	shown := 0
	for _, r := range results {
		if r.Err != "" && shown < 20 {
			errLines = append(errLines,
				fmt.Sprintf("  %s  %s",
					T.Muted.Render(fmt.Sprintf("#%d", r.Seq)),
					T.Error.Render(r.Err),
				),
			)
			shown++
		}
	}
	if errorCount > 20 {
		errLines = append(errLines, T.Muted.Render(fmt.Sprintf("  ... and %d more", errorCount-20)))
	}
	errBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("1")).
		Padding(1, 2).
		Width(78).
		Render(T.Error.Bold(true).Render(fmt.Sprintf("  Errors (%d)", errorCount)) + "\n\n" +
			strings.Join(errLines, "\n"))
	fmt.Println()
	fmt.Println(errBox)
}

func renderSuccessRate(rate float64, ok, total int) string {
	style := T.OK
	if rate < 0.99 {
		style = T.Warn
	}
	if rate < 0.9 {
		style = T.Error
	}
	return style.Render(fmt.Sprintf("%.1f%%", rate*100)) +
		fmt.Sprintf("  (%d/%d)", ok, total)
}

type kvPair struct {
	Key   string
	Value string
}

func renderKV(pairs []kvPair) string {
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(fmt.Sprintf("  %-16s %s\n", T.Heading.Render(p.Key), p.Value))
	}
	return b.String()
}

func extractTimes(results []IterResult, fn func(IterResult) float64) []float64 {
	out := make([]float64, len(results))
	for i, r := range results {
		out[i] = fn(r)
	}
	return out
}
