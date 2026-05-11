package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

const banner = `
   ██████╗██╗   ██╗██████╗ ███████╗    ██████╗ ███████╗███╗   ██╗ ██████╗██╗  ██╗
  ██╔════╝██║   ██║██╔══██╗██╔════╝    ██╔══██╗██╔════╝████╗  ██║██╔════╝██║  ██║
  ██║     ██║   ██║██████╔╝█████╗      ██████╔╝█████╗  ██╔██╗ ██║██║     ███████║
  ██║     ██║   ██║██╔══██╗██╔══╝      ██╔══██╗██╔══╝  ██║╚██╗██║██║     ██╔══██║
  ╚██████╗╚██████╔╝██████╔╝███████╗    ██████╔╝███████╗██║ ╚████║╚██████╗██║  ██║
   ╚═════╝ ╚═════╝ ╚═════╝ ╚══════╝    ╚═════╝ ╚══════╝╚═╝  ╚═══╝ ╚═════╝╚═╝  ╚═╝`

type Config struct {
	Concurrency    int
	Total          int
	Template       string
	Warmup         int
	Mode           string
	Output         string
	APIURL         string
	APIKey         string
	ThemeName      string
	DryRun         bool
	DryLatencyMean float64
	DryLatencyStd  float64
	DryErrorRate   float64
	NoTUI          bool

	elapsed float64
}

func parseConfig() *Config {
	cfg := &Config{}

	flag.IntVar(&cfg.Concurrency, "c", 5, "Max parallel in-flight requests")
	flag.IntVar(&cfg.Concurrency, "concurrency", 5, "Max parallel in-flight requests")
	flag.IntVar(&cfg.Total, "n", 20, "Total create(/delete) iterations")
	flag.IntVar(&cfg.Total, "total", 20, "Total create(/delete) iterations")
	flag.StringVar(&cfg.Template, "t", "", "Template ID (overrides CUBE_TEMPLATE_ID)")
	flag.StringVar(&cfg.Template, "template", "", "Template ID (overrides CUBE_TEMPLATE_ID)")
	flag.IntVar(&cfg.Warmup, "w", 0, "Warmup rounds before measurement")
	flag.IntVar(&cfg.Warmup, "warmup", 0, "Warmup rounds before measurement")
	flag.StringVar(&cfg.Mode, "m", "create-delete", "Benchmark mode: create-delete | create-only")
	flag.StringVar(&cfg.Mode, "mode", "create-delete", "Benchmark mode: create-delete | create-only")
	flag.StringVar(&cfg.Output, "o", "", "Export JSON report to file")
	flag.StringVar(&cfg.Output, "output", "", "Export JSON report to file")
	flag.StringVar(&cfg.APIURL, "api-url", "", "CubeAPI base URL (overrides E2B_API_URL)")
	flag.StringVar(&cfg.APIKey, "api-key", "", "API key (overrides E2B_API_KEY)")
	flag.StringVar(&cfg.ThemeName, "theme", "auto", "Color theme: dark | light | auto")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "Simulate API calls with random latencies")

	var noTUI bool
	flag.BoolVar(&noTUI, "no-tui", false, "Disable interactive TUI (auto-detected in non-TTY)")

	dryLatency := flag.String("dry-latency", "80,30", "Dry-run latency: mean,stddev in ms")
	flag.Float64Var(&cfg.DryErrorRate, "dry-error-rate", 0.02, "Dry-run simulated error rate 0.0-1.0")

	flag.Parse()

	cfg.NoTUI = noTUI || !term.IsTerminal(int(os.Stdout.Fd()))

	cfg.DryLatencyMean = 80
	cfg.DryLatencyStd = 30
	if parts := strings.Split(*dryLatency, ","); len(parts) == 2 {
		if m, err := strconv.ParseFloat(parts[0], 64); err == nil {
			cfg.DryLatencyMean = m
		}
		if s, err := strconv.ParseFloat(parts[1], 64); err == nil {
			cfg.DryLatencyStd = s
		}
	}

	if cfg.DryRun {
		if cfg.Template == "" {
			cfg.Template = "dry-run-template"
		}
		if cfg.APIURL == "" {
			cfg.APIURL = "http://localhost:3000 (dry-run)"
		}
		if cfg.APIKey == "" {
			cfg.APIKey = "dry-run"
		}
	} else {
		if cfg.Template == "" {
			cfg.Template = os.Getenv("CUBE_TEMPLATE_ID")
		}
		if cfg.APIURL == "" {
			cfg.APIURL = strings.TrimRight(os.Getenv("E2B_API_URL"), "/")
		}
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv("E2B_API_KEY")
		}
	}

	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Total < 1 {
		cfg.Total = 1
	}

	return cfg
}

func renderBanner() {
	styled := T.Banner.Render(banner)
	fmt.Println(lipgloss.PlaceHorizontal(80, lipgloss.Center, styled))
	fmt.Println()
}

func renderConfig(cfg *Config) {
	hostname, _ := os.Hostname()

	kvs := []kvPair{
		{"Template", cfg.Template},
		{"API URL", cfg.APIURL},
		{"Concurrency", fmt.Sprintf("%d", cfg.Concurrency)},
		{"Total Requests", fmt.Sprintf("%d", cfg.Total)},
		{"Warmup Rounds", fmt.Sprintf("%d", cfg.Warmup)},
		{"Mode", cfg.Mode},
		{"Host", hostname},
		{"Go", runtime.Version()},
		{"Time", time.Now().UTC().Format("2006-01-02 15:04:05 UTC")},
	}

	var content strings.Builder
	for _, kv := range kvs {
		content.WriteString(fmt.Sprintf("  %-16s %s\n",
			T.Heading.Render(kv.Key),
			T.Value.Render(kv.Value),
		))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(T.Border).
		Padding(1, 3).
		Width(78).
		Render(T.Heading.Render("  Configuration") + "\n\n" + content.String())

	fmt.Println(box)
	fmt.Println()
}

func renderDryRunBanner(cfg *Config) {
	content := fmt.Sprintf("  %s — simulating API calls with random latencies\n"+
		"    latency: %s    error rate: %s",
		T.Warn.Bold(true).Render("DRY-RUN MODE"),
		T.Accent.Render(fmt.Sprintf("N(%.0f, %.0f) ms", cfg.DryLatencyMean, cfg.DryLatencyStd)),
		T.Accent.Render(fmt.Sprintf("%.0f%%", cfg.DryErrorRate*100)),
	)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(T.Warn.GetForeground()).
		Padding(0, 2).
		Width(78).
		Render(content)
	fmt.Println(box)
	fmt.Println()
}

func exportJSON(results []IterResult, cfg *Config) {
	var okResults []IterResult
	for _, r := range results {
		if r.Err == "" {
			okResults = append(okResults, r)
		}
	}

	createTimes := extractTimes(okResults, func(r IterResult) float64 { return r.CreateMs })
	deleteTimes := extractTimes(okResults, func(r IterResult) float64 { return r.DeleteMs })

	statBlock := func(vals []float64) map[string]interface{} {
		if len(vals) == 0 {
			return nil
		}
		return map[string]interface{}{
			"count": len(vals),
			"min":   Min(vals),
			"max":   Max(vals),
			"avg":   Mean(vals),
			"std":   StdDev(vals),
			"p50":   Percentile(vals, 50),
			"p90":   Percentile(vals, 90),
			"p95":   Percentile(vals, 95),
			"p99":   Percentile(vals, 99),
		}
	}

	raw := make([]map[string]interface{}, len(results))
	for i, r := range results {
		entry := map[string]interface{}{
			"seq":       r.Seq,
			"create_ms": r.CreateMs,
			"delete_ms": r.DeleteMs,
		}
		if r.Err != "" {
			entry["error"] = r.Err
		}
		raw[i] = entry
	}

	successRate := 0.0
	if len(results) > 0 {
		successRate = float64(len(okResults)) / float64(len(results))
	}
	throughput := 0.0
	if cfg.elapsed > 0 {
		throughput = float64(len(okResults)) / cfg.elapsed
	}

	report := map[string]interface{}{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"config": map[string]interface{}{
			"template":    cfg.Template,
			"api_url":     cfg.APIURL,
			"concurrency": cfg.Concurrency,
			"total":       cfg.Total,
			"warmup":      cfg.Warmup,
			"mode":        cfg.Mode,
		},
		"summary": map[string]interface{}{
			"total_time_s":   cfg.elapsed,
			"successful":     len(okResults),
			"errors":         len(results) - len(okResults),
			"success_rate":   successRate,
			"throughput_qps": throughput,
		},
		"create": statBlock(createTimes),
		"raw":    raw,
	}
	if cfg.Mode == "create-delete" {
		report["delete"] = statBlock(deleteTimes)
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "JSON marshal error: %v\n", err)
		return
	}
	if err := os.WriteFile(cfg.Output, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Write error: %v\n", err)
		return
	}
	fmt.Printf("  %s %s\n", T.Muted.Render("Report saved to"), lipgloss.NewStyle().Bold(true).Render(cfg.Output))
}

func collectWithSimpleProgress(ch <-chan IterResult, total int) []IterResult {
	var results []IterResult
	lastPrint := time.Now()
	for r := range ch {
		results = append(results, r)
		if time.Since(lastPrint) > 200*time.Millisecond || len(results) == total {
			pct := float64(len(results)) / float64(total) * 100
			errors := 0
			for _, rr := range results {
				if rr.Err != "" {
					errors++
				}
			}
			fmt.Printf("\r  Progress: %s %d/%d (errors: %d)",
				T.Accent.Render(fmt.Sprintf("%.0f%%", pct)),
				len(results), total, errors,
			)
			lastPrint = time.Now()
		}
	}
	fmt.Println()
	fmt.Println()
	return results
}

func main() {
	cfg := parseConfig()

	switch cfg.ThemeName {
	case "light":
		T = LightTheme
	case "dark":
		T = DarkTheme
	default:
		T = DetectTheme()
	}

	if !cfg.DryRun {
		if cfg.Template == "" {
			fmt.Fprintln(os.Stderr, T.Error.Render("ERROR:")+" template ID not set. Use -t or set CUBE_TEMPLATE_ID.")
			os.Exit(1)
		}
		if cfg.APIURL == "" {
			fmt.Fprintln(os.Stderr, T.Error.Render("ERROR:")+" API URL not set. Use --api-url or set E2B_API_URL.")
			os.Exit(1)
		}
		if cfg.APIKey == "" {
			fmt.Fprintln(os.Stderr, T.Error.Render("ERROR:")+" API key not set. Use --api-key or set E2B_API_KEY.")
			os.Exit(1)
		}
	}

	renderBanner()
	renderConfig(cfg)

	if cfg.DryRun {
		renderDryRunBanner(cfg)
	}

	resultCh := make(chan IterResult, cfg.Total)

	startTime := time.Now()

	go RunBenchmark(cfg, resultCh)

	var allResults []IterResult

	if cfg.NoTUI {
		allResults = collectWithSimpleProgress(resultCh, cfg.Total)
	} else {
		m := newModel(cfg, resultCh)
		p := tea.NewProgram(m)
		finalModel, err := p.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			os.Exit(1)
		}
		fm := finalModel.(model)
		allResults = fm.results
	}

	cfg.elapsed = time.Since(startTime).Seconds()

	RenderReport(allResults, cfg)

	if cfg.Output != "" {
		exportJSON(allResults, cfg)
	}

	hasErrors := false
	for _, r := range allResults {
		if r.Err != "" {
			hasErrors = true
			break
		}
	}
	if hasErrors && !cfg.DryRun {
		os.Exit(1)
	}
}
