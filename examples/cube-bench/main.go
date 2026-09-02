package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
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
	HostMount      string // raw JSON array for config display and report export
	NetworkPolicy  string // none | rules
	networkFP      networkConfigFingerprint
	hostMountValue string // compacted once for request-time reuse
	requestBody    []byte
	requestHeaders map[string]string
	DryRun         bool
	DryLatencyMean float64
	DryLatencyStd  float64
	DryErrorRate   float64
	NoTUI          bool

	// Scheduled workload generator. Scheduled is true when any of the
	// scheduling flags (--workload/--rate/--lifetime/--templates) is in use;
	// false keeps the exact legacy behavior.
	Seed         int64
	Workload     string
	Rate         float64 // Poisson arrival rate in requests/sec (<=0 = asap)
	LifetimeMin  float64 // seconds
	LifetimeMax  float64 // seconds
	hasLifetime  bool
	TemplatesRaw string
	Templates    []TemplateSpec
	DumpTrace    string
	Scheduled    bool
	sequence     []ScheduledRequest

	elapsed float64
	// dispatchElapsed is the wall-clock span of the scheduled dispatch loop
	// (first to last request release), excluding per-sandbox lifetime tails.
	dispatchElapsed float64
}

type createRequest struct {
	TemplateID          string                `json:"templateID"`
	Timeout             *int64                `json:"timeout,omitempty"`
	AllowInternetAccess *bool                 `json:"allow_internet_access,omitempty"`
	Network             *sandboxNetworkConfig `json:"network,omitempty"`
	Metadata            map[string]string     `json:"metadata,omitempty"`
}

func prepareHostMount(rawJSON string) (string, error) {
	if rawJSON == "" {
		return "", nil
	}

	var mounts []json.RawMessage
	if err := json.Unmarshal([]byte(rawJSON), &mounts); err != nil {
		return "", fmt.Errorf("--host-mount must be a JSON array: %w", err)
	}
	if len(mounts) == 0 {
		return "", fmt.Errorf("--host-mount must be a non-empty JSON array")
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(rawJSON)); err != nil {
		return "", fmt.Errorf("--host-mount must be valid JSON: %w", err)
	}
	return compact.String(), nil
}

func buildCreateRequestBody(template string, hostMount string, networkPolicy string) ([]byte, error) {
	return buildCreateRequestBodyWithTimeout(template, hostMount, networkPolicy, nil)
}

func buildCreateRequestBodyWithTimeout(template string, hostMount string, networkPolicy string, timeoutS *int64) ([]byte, error) {
	reqBody := createRequest{TemplateID: template, Timeout: timeoutS}
	if hostMount != "" {
		reqBody.Metadata = map[string]string{"host-mount": hostMount}
	}
	switch networkPolicy {
	case "", networkPolicyNone:
		// empty-network baseline (historical cube-bench behavior)
	case networkPolicyRules:
		denyAll := false
		net := rulesNetworkConfig()
		reqBody.AllowInternetAccess = &denyAll
		reqBody.Network = &net
	default:
		return nil, fmt.Errorf("unsupported network policy %q", networkPolicy)
	}
	return json.Marshal(reqBody)
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
	flag.StringVar(&cfg.HostMount, "host-mount", "", "Host mount list as a JSON array")
	flag.StringVar(&cfg.NetworkPolicy, "network-policy", networkPolicyNone, "Network policy on create: none (no rules) | rules")
	flag.StringVar(&cfg.NetworkPolicy, "np", networkPolicyNone, "Short for --network-policy")
	flag.StringVar(&cfg.APIURL, "api-url", "", "CubeAPI base URL (overrides E2B_API_URL)")
	flag.StringVar(&cfg.APIKey, "api-key", "", "API key (overrides E2B_API_KEY)")
	flag.StringVar(&cfg.ThemeName, "theme", "auto", "Color theme: dark | light | auto")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "Simulate API calls with random latencies")

	// Scheduled workload generator flags (empty/zero = legacy behavior)
	flag.Int64Var(&cfg.Seed, "seed", 42, "Random seed for the pre-generated request sequence")
	flag.StringVar(&cfg.Workload, "workload", "", "Workload preset: burst | template_storm | mixed_spec")
	flag.Float64Var(&cfg.Rate, "rate", 0, "Poisson arrival rate in requests/sec (<=0 = as fast as possible)")
	lifetime := flag.String("lifetime", "", "Per-sandbox lifetime in seconds: min,max (uniform); client DELETEs at lifetime")
	flag.StringVar(&cfg.TemplatesRaw, "templates", "", "Comma-separated templateID[:weight[:cpuMillis:memMiB]] pool")
	flag.StringVar(&cfg.DumpTrace, "dump-trace", "", "Write the pre-generated request sequence to a JSON trace file (requires a scheduled workload)")

	var noTUI bool
	flag.BoolVar(&noTUI, "no-tui", false, "Disable interactive TUI (auto-detected in non-TTY)")

	dryLatency := flag.String("dry-latency", "80,30", "Dry-run latency: mean,stddev in ms")
	flag.Float64Var(&cfg.DryErrorRate, "dry-error-rate", 0.02, "Dry-run simulated error rate 0.0-1.0")

	flag.Parse()

	// Remember which flags were passed explicitly so workload presets only
	// fill in defaults and never override user input.
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	cfg.NoTUI = noTUI || !term.IsTerminal(int(os.Stdout.Fd()))

	policy, err := parseNetworkPolicy(cfg.NetworkPolicy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	cfg.NetworkPolicy = policy
	cfg.networkFP = networkFingerprint(policy)

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

	// Explicit --lifetime parses first; a workload preset only fills it in
	// when the flag was not given.
	if *lifetime != "" {
		lo, hi, err := parseLifetime(*lifetime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		cfg.LifetimeMin, cfg.LifetimeMax, cfg.hasLifetime = lo, hi, true
	}
	if cfg.Workload != "" {
		if err := applyWorkloadPreset(cfg, explicit); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	}
	if cfg.Rate < 0 {
		cfg.Rate = 0
	}

	// Template pool: --templates wins; otherwise the single -t/--template is
	// equivalent to a one-element pool with weight 1.
	if cfg.TemplatesRaw != "" {
		templates, err := parseTemplates(cfg.TemplatesRaw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		cfg.Templates = templates
	} else if cfg.Template != "" {
		cfg.Templates = []TemplateSpec{{TemplateID: cfg.Template, Weight: 1}}
	}
	if cfg.Workload == "mixed_spec" && len(cfg.Templates) < 2 {
		fmt.Fprintln(os.Stderr, "ERROR: workload mixed_spec requires --templates with at least 2 templates, e.g.")
		fmt.Fprintln(os.Stderr, "  --templates 'tpl-1c2g:6:1000:2048,tpl-2c4g:3:2000:4096,tpl-8c16g:1:8000:16384'  # 6:3:1 for 1C2G/2C4G/8C16G")
		os.Exit(1)
	}

	cfg.Scheduled = cfg.Workload != "" || cfg.Rate > 0 || cfg.hasLifetime || cfg.TemplatesRaw != ""

	if cfg.DumpTrace != "" && !cfg.Scheduled {
		fmt.Fprintln(os.Stderr, "ERROR: --dump-trace requires a scheduled workload (--workload, or --rate/--lifetime/--templates)")
		os.Exit(1)
	}

	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Total < 1 {
		cfg.Total = 1
	}

	// A worker slot is held for the full sandbox lifetime, so the dispatcher
	// can only honor the requested Poisson rate when concurrency >= rate x
	// mean lifetime; otherwise queue delays measure the client's own
	// concurrency limit, not the scheduler. Warn loudly (presets included).
	if cfg.Scheduled && cfg.Rate > 0 && cfg.hasLifetime {
		meanLife := (cfg.LifetimeMin + cfg.LifetimeMax) / 2
		if needed := cfg.Rate * meanLife; float64(cfg.Concurrency) < needed {
			fmt.Fprintf(os.Stderr, "WARNING: --concurrency %d is below rate(%g/s) x mean lifetime(%gs) = %.0f; "+
				"the dispatcher will stall on the semaphore and neither the arrival rate nor queue-delay metrics will be honored\n",
				cfg.Concurrency, cfg.Rate, meanLife, needed)
		}
	}

	// Validate host-mount early so the CLI fails fast on bad input while still
	// preserving the original JSON for config display and exported reports.
	// Cache the compacted JSON string once so benchmark throughput is not
	// polluted by repeating client-side conversion on every request.
	hostMountValue, err := prepareHostMount(cfg.HostMount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	cfg.hostMountValue = hostMountValue
	cfg.requestHeaders = map[string]string{"Authorization": "Bearer " + cfg.APIKey}

	// In scheduled mode without -t the pool drives per-request bodies; the
	// cached body (warmup/fallback) then uses the first pool template.
	bodyTemplate := cfg.Template
	if bodyTemplate == "" && len(cfg.Templates) > 0 {
		bodyTemplate = cfg.Templates[0].TemplateID
	}
	requestBody, err := buildCreateRequestBody(bodyTemplate, cfg.hostMountValue, cfg.NetworkPolicy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: create request body build failed: %v\n", err)
		os.Exit(1)
	}
	cfg.requestBody = requestBody

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
		{"Network Policy", cfg.networkFP.summary()},
	}
	if cfg.Scheduled {
		kvs = append(kvs,
			kvPair{"Workload", workloadDisplayName(cfg)},
			kvPair{"Seed", fmt.Sprintf("%d", cfg.Seed)},
			kvPair{"Rate", fmt.Sprintf("%g req/s", cfg.Rate)},
		)
		if cfg.hasLifetime {
			kvs = append(kvs, kvPair{"Lifetime", fmt.Sprintf("%g-%gs", cfg.LifetimeMin, cfg.LifetimeMax)})
		}
		var pool []string
		for _, t := range cfg.Templates {
			pool = append(pool, fmt.Sprintf("%s(w%d)", t.TemplateID, t.Weight))
		}
		kvs = append(kvs, kvPair{"Templates", strings.Join(pool, ", ")})
	}
	if cfg.HostMount != "" {
		// Pretty-print the original host-mount JSON for readability.
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(cfg.HostMount), "    ", "  "); err == nil {
			kvs = append(kvs, kvPair{"Host Mount", pretty.String()})
		} else {
			kvs = append(kvs, kvPair{"Host Mount", cfg.HostMount})
		}
	}
	kvs = append(kvs,
		kvPair{"Host", hostname},
		kvPair{"Go", runtime.Version()},
		kvPair{"Time", time.Now().UTC().Format("2006-01-02 15:04:05 UTC")},
	)

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
		if cfg.Scheduled {
			entry["template_id"] = r.TemplateID
			entry["scheduled_arrival_ms"] = r.ScheduledArrivalMs
			entry["actual_start_ms"] = r.ActualStartMs
			entry["sched_delay_ms"] = r.SchedDelayMs
			entry["lifetime_ms"] = r.LifetimeMs
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

	configBlock := map[string]interface{}{
		"template":             cfg.Template,
		"api_url":              cfg.APIURL,
		"concurrency":          cfg.Concurrency,
		"total":                cfg.Total,
		"warmup":               cfg.Warmup,
		"mode":                 cfg.Mode,
		"host_mount":           cfg.HostMount,
		"network_policy":       cfg.networkFP.Policy,
		"network_allow_out":    cfg.networkFP.AllowOut,
		"network_rules":        cfg.networkFP.Rules,
		"network_inject_rules": cfg.networkFP.InjectRules,
	}
	if cfg.Scheduled {
		configBlock["workload"] = workloadDisplayName(cfg)
		configBlock["seed"] = cfg.Seed
		configBlock["rate_per_sec"] = cfg.Rate
		configBlock["lifetime_min_s"] = cfg.LifetimeMin
		configBlock["lifetime_max_s"] = cfg.LifetimeMax
		templates := make([]map[string]interface{}, len(cfg.Templates))
		for i, t := range cfg.Templates {
			templates[i] = map[string]interface{}{
				"template_id": t.TemplateID,
				"weight":      t.Weight,
				"cpu_millis":  t.CpuMillis,
				"mem_mib":     t.MemMiB,
			}
		}
		configBlock["templates"] = templates
	}

	summaryBlock := map[string]interface{}{
		"total_time_s":   cfg.elapsed,
		"successful":     len(okResults),
		"errors":         len(results) - len(okResults),
		"success_rate":   successRate,
		"throughput_qps": throughput,
	}
	if cfg.Scheduled {
		// Dispatch-side throughput: requests released per second of the
		// dispatch window. Unlike throughput_qps (total requests over the
		// whole run), this excludes per-sandbox lifetime tails, so it
		// reflects the arrival rate the scheduler actually saw.
		if cfg.dispatchElapsed > 0 {
			summaryBlock["dispatch_window_s"] = cfg.dispatchElapsed
			summaryBlock["dispatch_qps"] = float64(len(results)) / cfg.dispatchElapsed
		}
		// Queue delay is measured per dispatched request, independent of
		// create success, so it uses all results.
		delays := extractTimes(results, func(r IterResult) float64 { return r.SchedDelayMs })
		if len(delays) > 0 {
			summaryBlock["queue_delay_p50_ms"] = Percentile(delays, 50)
			summaryBlock["queue_delay_p95_ms"] = Percentile(delays, 95)
			summaryBlock["queue_delay_p99_ms"] = Percentile(delays, 99)
		}
		type tplAgg struct{ attempts, created int }
		agg := map[string]*tplAgg{}
		for _, r := range results {
			a := agg[r.TemplateID]
			if a == nil {
				a = &tplAgg{}
				agg[r.TemplateID] = a
			}
			a.attempts++
			if r.Err == "" {
				a.created++
			}
		}
		perTemplate := map[string]interface{}{}
		for id, a := range agg {
			rate := 0.0
			if a.attempts > 0 {
				rate = float64(a.created) / float64(a.attempts)
			}
			perTemplate[id] = map[string]interface{}{
				"attempts":     a.attempts,
				"created":      a.created,
				"success_rate": rate,
			}
		}
		summaryBlock["per_template"] = perTemplate
	}

	report := map[string]interface{}{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"config":    configBlock,
		"summary":   summaryBlock,
		"create":    statBlock(createTimes),
		"raw":       raw,
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
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		if err := runCompare(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		return
	}

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
		if cfg.Template == "" && len(cfg.Templates) == 0 {
			fmt.Fprintln(os.Stderr, T.Error.Render("ERROR:")+" template ID not set. Use -t/--templates or set CUBE_TEMPLATE_ID.")
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

	// Pre-generate the request sequence before any timing starts; dumping the
	// trace must not delay the scheduled arrivals.
	var sched []ScheduledRequest
	if cfg.Scheduled {
		sched = GenerateSequence(cfg, rand.New(rand.NewSource(cfg.Seed)))
		cfg.sequence = sched
		if cfg.DumpTrace != "" {
			if err := DumpTrace(cfg.DumpTrace, cfg, sched); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("  %s %s\n\n", T.Muted.Render("Trace saved to"), lipgloss.NewStyle().Bold(true).Render(cfg.DumpTrace))
		}
	}

	client := RunWarmup(cfg, os.Stdout)

	resultCh := make(chan IterResult, cfg.Total)

	startTime := time.Now()

	if cfg.Scheduled {
		go RunScheduled(cfg, sched, resultCh, client)
	} else {
		go RunBenchmark(cfg, resultCh, client)
	}

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
