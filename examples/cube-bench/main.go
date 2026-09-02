package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
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
	// dispatchElapsedBits holds math.Float64bits of the wall-clock span of the
	// scheduled dispatch loop (first to last request release), excluding
	// per-sandbox lifetime tails. Stored atomically: on an early TUI quit the
	// report path reads it while the dispatcher goroutine may still write it.
	dispatchElapsedBits atomic.Uint64
	// released counts scheduled requests actually admitted to a worker slot
	// (semaphore acquired), so the TUI in-flight gauge reflects real releases
	// instead of reconstructing them from the planned arrival schedule.
	released atomic.Int64
}

// setDispatchElapsed publishes the dispatch window; getDispatchElapsed reads
// it back. Both stay race-free against a dispatcher goroutine that is still
// running because the TUI was quit early.
func (c *Config) setDispatchElapsed(v float64) {
	c.dispatchElapsedBits.Store(math.Float64bits(v))
}

func (c *Config) getDispatchElapsed() float64 {
	return math.Float64frombits(c.dispatchElapsedBits.Load())
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

	// A worker slot is held for the request's whole occupancy, so the
	// dispatcher can only honor the requested Poisson rate when
	// concurrency >= rate x mean occupancy; otherwise queue delays measure
	// the client's own concurrency limit, not the scheduler. Warn loudly.
	if cfg.Scheduled {
		meanLife := (cfg.LifetimeMin + cfg.LifetimeMax) / 2
		// occS estimates how long a worker slot is held per request; known is
		// false when no estimate exists (real run without a lifetime hold:
		// only the unknown create/delete latency occupies the slot).
		// Dry-run occupancy uses the synthetic latencies and the clamped
		// lifetime sleep (dryRunMaxLifetimeSleep), never the configured
		// lifetime range; create-only holds the slot for the create alone.
		occS, known := 0.0, false
		switch {
		case cfg.DryRun:
			occS, known = cfg.DryLatencyMean/1000, true
			if cfg.Mode == "create-delete" {
				occS += cfg.DryLatencyMean * 0.4 / 1000
				if cfg.hasLifetime {
					occS += min(meanLife, dryRunMaxLifetimeSleep.Seconds())
				}
			}
		case cfg.hasLifetime && cfg.Mode == "create-delete" && meanLife >= 1:
			// Real create-delete run: create/delete latency is unknown ahead
			// but the lifetime dominates for any realistic setting.
			occS, known = meanLife, true
		case cfg.hasLifetime && cfg.Mode == "create-delete":
			// Sub-second mean lifetime: the unknown create/delete tail is
			// comparable to the hold (see README), so a precise-looking
			// rate x lifetime threshold would understate the real slot
			// requirement. Fall through to the qualitative warning.
		}
		switch {
		case cfg.Rate > 0 && known:
			// Little's-law steady state assumes an unbounded stream; a finite
			// run can never hold more than Total requests in flight, so cap
			// the estimate (a 500-request burst at 50/s with a 65s mean
			// lifetime needs ~500 slots, not 3250).
			if needed := min(cfg.Rate*occS, float64(cfg.Total)); float64(cfg.Concurrency) < needed {
				fmt.Fprintf(os.Stderr, "WARNING: --concurrency %d is below rate(%g/s) x mean occupancy(%gs) = %.0f; "+
					"the dispatcher will stall on the semaphore and neither the arrival rate nor queue-delay metrics will be honored\n",
					cfg.Concurrency, cfg.Rate, occS, needed)
			}
		case cfg.Rate > 0 && cfg.Mode == "create-delete":
			// No reliable occupancy estimate (no lifetime hold, or a
			// sub-second lifetime whose create/delete tail is comparable):
			// warn qualitatively instead of printing a precise-looking
			// threshold. Worded as a caution: plenty of rate-paced runs
			// without a lifetime hold are perfectly healthy. Create-only
			// runs hold a slot only for the create call and get no note;
			// the end-of-run saturation check covers the stall case.
			fmt.Fprintf(os.Stderr, "NOTE: at --rate %g/s each worker slot is held for a request's whole residency "+
				"(create, any lifetime hold, delete). If --concurrency (%d) is low relative to rate x mean "+
				"residency, the dispatcher stalls and queue-delay metrics then measure the client's own "+
				"semaphore, not the scheduler; the end-of-run saturation check will say if that happened.\n",
				cfg.Rate, cfg.Concurrency)
		case !cfg.DryRun && cfg.hasLifetime && cfg.Mode == "create-delete":
			// ASAP dispatch with lifetimes: every worker slot is held for at
			// least the full lifetime, so the run time is bounded below by
			// total x mean lifetime / concurrency. Use meanLife directly:
			// occS deliberately stays 0 for sub-second lifetimes (a
			// precise-looking rate x lifetime threshold would understate the
			// slot requirement there), but this lower bound only needs the
			// hold — the omitted create/delete tail makes the true run
			// longer still.
			if est := float64(cfg.Total) * meanLife / float64(cfg.Concurrency); est > 300 {
				fmt.Fprintf(os.Stderr, "WARNING: %d requests with mean lifetime %gs at --concurrency %d will take at least ~%.0fs; "+
					"raise --concurrency or add --rate to pace arrivals\n",
					cfg.Total, meanLife, cfg.Concurrency, est)
			}
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

	// In scheduled mode the pool drives per-request bodies; the cached body
	// (warmup/fallback) uses -t / CUBE_TEMPLATE_ID when set and only falls
	// back to the first pool template when neither is given.
	bodyTemplate := cfg.Template
	if bodyTemplate == "" && len(cfg.Templates) > 0 {
		bodyTemplate = cfg.Templates[0].TemplateID
	}
	// The cached body serves warmup requests, which otherwise get no TTL
	// safety net: on a lifetime-bearing workload an interrupted run would leak
	// them. Give warmup the same server-side fallback as measured requests,
	// keyed to the top of the lifetime range.
	var warmupTimeout *int64
	if cfg.Scheduled && cfg.hasLifetime {
		t := int64(cfg.LifetimeMax) + 60
		warmupTimeout = &t
	}
	requestBody, err := buildCreateRequestBodyWithTimeout(bodyTemplate, cfg.hostMountValue, cfg.NetworkPolicy, warmupTimeout)
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
		// An early TUI quit exports a truncated sample: mark it so partial
		// dispatch_*/queue_delay_* aggregates are not misread as run
		// characteristics (compare pops the marker and notes the group),
		// and skip the saturation marker whose own sample is truncated.
		partial := runTruncated(results, cfg)
		if partial {
			summaryBlock["partial"] = 1
		}
		// Dispatch-side throughput: requests released per second of the
		// dispatch window. Unlike throughput_qps (total requests over the
		// whole run), this excludes per-sandbox lifetime tails, so it
		// reflects the arrival rate the scheduler actually saw. Only
		// meaningful for rate-paced runs: without --rate the "window" is
		// just the client's release burst, so the keys are omitted.
		if de := cfg.getDispatchElapsed(); cfg.Rate > 0 && de > 0 {
			summaryBlock["dispatch_window_s"] = de
			summaryBlock["dispatch_qps"] = float64(len(results)) / de
		}
		// Queue delay is measured per dispatched request, independent of
		// create success, so it uses all results.
		delays := extractTimes(results, func(r IterResult) float64 { return r.SchedDelayMs })
		if len(delays) > 0 {
			summaryBlock["queue_delay_p50_ms"] = Percentile(delays, 50)
			summaryBlock["queue_delay_p95_ms"] = Percentile(delays, 95)
			summaryBlock["queue_delay_p99_ms"] = Percentile(delays, 99)
		}
		// Machine-readable marker for the end-of-run saturation check: when
		// set, the dispatcher fell permanently behind pace and the
		// queue_delay_*/dispatch_* keys above describe the client's
		// semaphore, not the offered schedule. compare surfaces this as a
		// warning instead of a metric row.
		if !partial {
			if sat, _, _ := dispatchSaturated(results, cfg); sat {
				summaryBlock["dispatch_saturated"] = 1
			}
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

	// An early TUI quit truncates the sample: say so next to the on-screen
	// report so its numbers are not read as a finished run.
	partial := runTruncated(allResults, cfg)
	if partial {
		fmt.Fprintf(os.Stderr, "NOTE: run quit early with %d of %d requests dispatched; "+
			"the report above and any exported aggregates cover a truncated sample.\n",
			len(allResults), cfg.Total)
	}

	// If queue-delay p95 exceeds the mean inter-arrival time, the dispatcher
	// fell permanently behind and the offered load degenerated to bursts; the
	// queue_delay_* metrics then measure the client semaphore, not the
	// scheduler. Surface that instead of letting the numbers read as a valid
	// scheduled-mode run; exportJSON marks the report machine-readably.
	// Skipped on an early-quit truncated sample — the p95 would describe the
	// partial run, and the export carries a "partial" marker instead.
	if sat, p95, interArrival := dispatchSaturated(allResults, cfg); sat && !partial {
		fmt.Fprintf(os.Stderr, "WARNING: queue-delay p95 (%.0fms) exceeded the mean inter-arrival time (%.0fms): "+
			"the dispatcher fell permanently behind and the offered load degenerated to bursts — "+
			"queue_delay_*/dispatch metrics no longer describe the offered schedule. Either --concurrency is too "+
			"low for the offered rate (each slot is held for create+lifetime+delete) or create latency blew up "+
			"server-side — check create.p95 before raising --concurrency.\n",
			p95, interArrival)
	}

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
