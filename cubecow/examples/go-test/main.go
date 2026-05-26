// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// COW-Storage Go FFI Test
//
// This program demonstrates how to drive the cubecow engine from Go
// via CGO + dynamic linking (libcubecow.so), mirroring the way real
// callers (e.g. cubelet) embed cubecow: the host assembles the
// cubecow `AppConfig` in memory, marshals it to JSON, and hands the
// JSON blob to libcubecow's `*_from_json` entry points instead of
// pointing at a TOML file on disk.
//
// Four configuration sources are supported, in priority order:
//
//   1. -json-config-inline '<json>'  Raw JSON string, passed verbatim.
//   2. -json-config <path>           Path to a JSON config file.
//   3. Inline-config flags           Build the JSON in-process from
//                                    -backend, -reflink-root-dir.
//   4. -config <path>                Legacy TOML path (kept for
//                                    backwards compatibility with
//                                    scripts/go_e2e_test.sh).
//
// In modes (1)–(3) we call cubecow.InitFromJSON /
// InitWithoutLoggingFromJSON; mode (4) keeps the original TOML-based
// cubecow.Init / InitWithoutLogging path.
//
// Usage:
//
//	./cubecow-test -config /path/to/cubecow.toml                     # TOML
//	./cubecow-test -json-config /path/to/cubecow.json                # JSON file
//	./cubecow-test -json-config-inline '{"log":{}}'                  # JSON literal
//	./cubecow-test -backend reflink \
//	               -reflink-root-dir /var/lib/cubecow/reflink        # built from flags
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"cubecow-go-test/cubecow"
)

// emitOpTiming prints a single machine-parseable line capturing the
// wall-clock duration of one cubecow FFI op. Lines are scraped by
// scripts/go_e2e_test.sh's run_probe to populate the latency-summary
// table at the end of an e2e run.
//
// Format (stable; grep anchor is the literal `[TIMING]` prefix):
//
//	[TIMING] op=<kebab-name> ms=<float> status=<ok|err>[ err=<msg>]
//
// We deliberately measure *only* the FFI call (excluding fmt.Printf /
// json.MarshalIndent / etc.), so the number reported is the cubecow
// engine's response latency for that single operation, not a noisy
// process-level wall clock that includes Go-side formatting and stdout
// I/O. ms is printed with three decimals (microsecond precision).
func emitOpTiming(op string, d time.Duration, opErr error) {
	ms := float64(d.Microseconds()) / 1000.0
	if opErr == nil {
		fmt.Printf("[TIMING] op=%s ms=%.3f status=ok\n", op, ms)
		return
	}
	// Sanitize the error message so it stays on a single line and never
	// confuses the bash-side parser (which splits on whitespace + `=`).
	msg := strings.ReplaceAll(opErr.Error(), "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	fmt.Printf("[TIMING] op=%s ms=%.3f status=err err=%q\n", op, ms, msg)
}

const (
	// Default volume size: 1 GiB
	defaultVolumeSizeBytes = 1 * 1024 * 1024 * 1024
	// Default volume name for testing
	defaultVolumeName = "go-test-vol"
	// Default snapshot name for testing
	defaultSnapshotName = "go-test-snap"
)

// ---------------------------------------------------------------------------
// Inline cubecow config — mirrors the cubecow `AppConfig` JSON schema:
//
//	{ "log":     {...},
//	  "backend": {"kind": "reflink",
//	              "reflink": {"root_dir": "..."}} }
//
// We use map[string]any for the open-ended `log` object so every
// default landed by the Rust side stays in effect; only fields the
// operator explicitly overrides surface in the JSON we hand to
// libcubecow. This is the same "preserve defaults" trick a real
// caller's BuildCubecowInitJSON would apply.
// ---------------------------------------------------------------------------

type cubecowInlineConfig struct {
	Log     map[string]any        `json:"log"`
	Backend *cubecowBackendConfig `json:"backend,omitempty"`
}

type cubecowBackendConfig struct {
	Kind    *string               `json:"kind,omitempty"`
	Reflink *cubecowReflinkConfig `json:"reflink,omitempty"`
}

type cubecowReflinkConfig struct {
	RootDir *string `json:"root_dir,omitempty"`
}

// stringPtrFlag is a flag.Value that distinguishes "unset" (nil) from
// "explicitly set to the empty string". This lets us only emit fields
// in the JSON when the operator set them explicitly.
type stringPtrFlag struct {
	val *string
}

func (s *stringPtrFlag) String() string {
	if s == nil || s.val == nil {
		return ""
	}
	return *s.val
}

func (s *stringPtrFlag) Set(v string) error {
	cp := v
	s.val = &cp
	return nil
}

func main() {
	// --- TOML / JSON config sources --------------------------------------
	configPath := flag.String("config", "", "path to cubecow TOML config file (legacy)")
	jsonConfigPath := flag.String("json-config", "", "path to a JSON cubecow config file")
	jsonConfigInline := flag.String("json-config-inline", "", "inline JSON cubecow config string")

	// --- Inline JSON builder flags ---------------------------------------
	backendKindFlag := stringPtrFlag{}
	flag.Var(&backendKindFlag, "backend", "backend.kind (currently only \"reflink\" is supported; inline config)")
	reflinkRootDirFlag := stringPtrFlag{}
	flag.Var(&reflinkRootDirFlag, "reflink-root-dir", "backend.reflink.root_dir (inline config)")

	dumpJSON := flag.Bool("dump-json", false, "print the JSON payload that would be handed to libcubecow and exit")

	// --- Action / lifecycle flags ---------------------------------------
	action := flag.String("action", "all", "action to perform: init, create-vol, delete-vol, resize-vol, vol-info, list-vols, create-snap, delete-snap, list-snaps, metrics, all")
	volName := flag.String("vol", defaultVolumeName, "volume name")
	snapName := flag.String("snap", defaultSnapshotName, "snapshot name")
	volSize := flag.Uint64("size", defaultVolumeSizeBytes, "volume size in bytes")
	newSize := flag.Uint64("new-size", 2*defaultVolumeSizeBytes, "new volume size for resize (bytes)")
	noCleanup := flag.Bool("no-cleanup", false, "for -action all: skip the final DeleteSnapshot/DeleteVolume so the volume and snapshot remain on disk for inspection")
	withLogging := flag.Bool("logging", false, "initialize cubecow with its built-in logging subscriber (writes per the config's [log] section)")
	flag.Parse()

	// Resolve which initialization mode we're using.
	mode, payload, err := resolveInitMode(
		*configPath, *jsonConfigPath, *jsonConfigInline,
		backendKindFlag.val, reflinkRootDirFlag.val,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(2)
	}

	if *dumpJSON {
		switch mode {
		case initModeJSONInline, initModeJSONFile, initModeBuilt:
			fmt.Println(payload)
			return
		default:
			fmt.Fprintf(os.Stderr, "ERROR: -dump-json only meaningful with a JSON-based init mode\n")
			os.Exit(2)
		}
	}

	fmt.Println("=== COW-Storage Go FFI Test ===")
	fmt.Printf("Init mode  : %s\n", mode)
	switch mode {
	case initModeTOML:
		fmt.Printf("TOML path  : %s\n", *configPath)
	case initModeJSONFile:
		fmt.Printf("JSON file  : %s\n", *jsonConfigPath)
	case initModeJSONInline:
		fmt.Printf("JSON inline: %d bytes\n", len(payload))
	case initModeBuilt:
		fmt.Printf("JSON built : %d bytes (from flags)\n", len(payload))
	}
	fmt.Printf("Action     : %s\n\n", *action)

	// Initialize engine.
	fmt.Println("[1] Initializing engine...")
	engine, err := initEngine(mode, *configPath, payload, *withLogging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: failed to initialize engine: %v\n", err)
		os.Exit(1)
	}
	defer engine.Shutdown()
	fmt.Println("    Engine initialized successfully.")
	fmt.Println()

	switch *action {
	case "init":
		fmt.Println("Engine initialized. Use other actions to test operations.")

	case "create-vol":
		doCreateVolume(engine, *volName, *volSize)

	case "delete-vol":
		doDeleteVolume(engine, *volName)

	case "resize-vol":
		doResizeVolume(engine, *volName, *newSize)

	case "vol-info":
		doGetVolumeInfo(engine, *volName)

	case "list-vols":
		doListVolumes(engine)

	case "create-snap":
		doCreateSnapshot(engine, *volName, *snapName)

	case "delete-snap":
		doDeleteSnapshot(engine, *snapName)

	case "list-snaps":
		doListSnapshots(engine, *volName)

	case "metrics":
		doGetMetrics(engine)

	case "all":
		runFullTest(engine, *volName, *snapName, *volSize, *newSize, *noCleanup)

	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown action: %s\n", *action)
		os.Exit(1)
	}

	fmt.Println("\n=== Test completed ===")
}

// ---------------------------------------------------------------------------
// Init-mode resolution
// ---------------------------------------------------------------------------

type initMode int

const (
	initModeNone initMode = iota
	initModeTOML
	initModeJSONFile
	initModeJSONInline
	initModeBuilt
)

func (m initMode) String() string {
	switch m {
	case initModeTOML:
		return "toml"
	case initModeJSONFile:
		return "json-file"
	case initModeJSONInline:
		return "json-inline"
	case initModeBuilt:
		return "json-built-from-flags"
	default:
		return "none"
	}
}

// resolveInitMode picks exactly one initialization source from the
// command-line and returns the JSON payload (when applicable). The
// precedence order is documented in the package-level comment.
func resolveInitMode(
	tomlPath, jsonPath, jsonInline string,
	backendKind, reflinkRootDir *string,
) (initMode, string, error) {
	// 1. -json-config-inline wins.
	if jsonInline != "" {
		if !json.Valid([]byte(jsonInline)) {
			return initModeNone, "", fmt.Errorf("-json-config-inline is not valid JSON")
		}
		return initModeJSONInline, jsonInline, nil
	}

	// 2. -json-config <file>.
	if jsonPath != "" {
		raw, err := os.ReadFile(jsonPath)
		if err != nil {
			return initModeNone, "", fmt.Errorf("read JSON config %q: %w", jsonPath, err)
		}
		if !json.Valid(raw) {
			return initModeNone, "", fmt.Errorf("file %q is not valid JSON", jsonPath)
		}
		return initModeJSONFile, string(raw), nil
	}

	// 3. Inline-config builder flags.
	hasInlineFlag := backendKind != nil || reflinkRootDir != nil
	if hasInlineFlag {
		built, err := buildInlineJSON(backendKind, reflinkRootDir)
		if err != nil {
			return initModeNone, "", err
		}
		return initModeBuilt, built, nil
	}

	// 4. Legacy TOML.
	if tomlPath != "" {
		return initModeTOML, "", nil
	}

	return initModeNone, "", fmt.Errorf("no config source provided (use one of -config / -json-config / -json-config-inline / -backend ...)")
}

// buildInlineJSON marshals the per-flag inline cubecow config: only
// fields the caller actually set surface in the JSON, so libcubecow's
// Rust-side defaults stay in effect for everything else.
func buildInlineJSON(backendKind, reflinkRootDir *string) (string, error) {
	cfg := cubecowInlineConfig{
		// `log` is a required top-level object in the AppConfig
		// schema, but every leaf has a server-side default. Emitting
		// `{}` keeps the schema happy while preserving defaults.
		Log: map[string]any{},
	}

	if backendKind != nil || reflinkRootDir != nil {
		bc := &cubecowBackendConfig{Kind: backendKind}
		if reflinkRootDir != nil {
			bc.Reflink = &cubecowReflinkConfig{RootDir: reflinkRootDir}
		}
		cfg.Backend = bc
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal inline cubecow config: %w", err)
	}
	return string(raw), nil
}

// initEngine dispatches on the resolved init mode and calls the
// matching cubecow entry point.
func initEngine(mode initMode, tomlPath, jsonPayload string, withLogging bool) (*cubecow.Engine, error) {
	switch mode {
	case initModeTOML:
		if withLogging {
			return cubecow.Init(tomlPath)
		}
		return cubecow.InitWithoutLogging(tomlPath)
	case initModeJSONFile, initModeJSONInline, initModeBuilt:
		if withLogging {
			return cubecow.InitFromJSON(jsonPayload)
		}
		return cubecow.InitWithoutLoggingFromJSON(jsonPayload)
	default:
		return nil, fmt.Errorf("internal error: unhandled init mode %s", mode)
	}
}

// runFullTest runs a complete lifecycle test:
// create volume -> get info -> resize -> create snapshot ->
// list snapshots -> list volumes -> get metrics -> delete snapshot -> delete volume
//
// When skipCleanup is true the final DeleteSnapshot / DeleteVolume are
// skipped so the operator can inspect the live artefacts (volume
// files, FICLONE-shared extents) after the probe returns.
func runFullTest(engine *cubecow.Engine, volName, snapName string, volSize, resizeSize uint64, skipCleanup bool) {
	fmt.Println("--- Running full lifecycle test ---")
	fmt.Println()

	// Step 1: Create volume
	doCreateVolume(engine, volName, volSize)

	// Step 2: Get volume info
	doGetVolumeInfo(engine, volName)

	// Step 3: Get volume block info
	doGetVolumeBlockInfo(engine, volName)

	// Step 4: Resize volume
	doResizeVolume(engine, volName, resizeSize)

	// Step 5: Create snapshot
	doCreateSnapshot(engine, volName, snapName)

	// Step 6: List snapshots
	doListSnapshots(engine, volName)

	// Step 7: List volumes
	doListVolumes(engine, "")

	// Step 8: Get metrics
	doGetMetrics(engine)

	// Step 9: Cleanup - delete snapshot then volume (unless -no-cleanup)
	if skipCleanup {
		fmt.Println("[Cleanup] -no-cleanup set; preserving snapshot and volume for inspection.")
		fmt.Printf("    volume   : %s\n", volName)
		fmt.Printf("    snapshot : %s\n", snapName)
		fmt.Println("    Re-query with e.g.:")
		fmt.Println("        cubecow-test -config <cfg> -action list-vols")
		fmt.Println("        cubecow-test -config <cfg> -action list-snaps -vol " + volName)
		fmt.Println("        cubecow-test -config <cfg> -action vol-info  -vol " + volName)
		fmt.Println("        sudo ls -lh <reflink_root_dir>/volumes/")
		fmt.Println("        sudo xfs_io -c 'fiemap -v' <reflink_root_dir>/volumes/<vol>/<vol>")
		fmt.Println("    When finished, clean up manually with:")
		fmt.Println("        cubecow-test -config <cfg> -action delete-snap -snap " + snapName)
		fmt.Println("        cubecow-test -config <cfg> -action delete-vol  -vol  " + volName)
	} else {
		fmt.Println("[Cleanup] Deleting snapshot and volume...")
		doDeleteSnapshot(engine, snapName)
		doDeleteVolume(engine, volName)
	}

	fmt.Println("\n--- Full lifecycle test completed ---")
}

// ---------------------------------------------------------------------------
// Individual test operations
// ---------------------------------------------------------------------------

func doCreateVolume(engine *cubecow.Engine, name string, sizeBytes uint64) {
	fmt.Printf("[CreateVolume] name=%q size=%d bytes\n", name, sizeBytes)

	start := time.Now()
	devicePath, err := engine.CreateVolume(name, sizeBytes)
	emitOpTiming("create-vol", time.Since(start), err)

	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Printf("    OK: device_path=%s\n\n", devicePath)
}

func doDeleteVolume(engine *cubecow.Engine, name string) {
	fmt.Printf("[DeleteVolume] name=%q\n", name)

	start := time.Now()
	err := engine.DeleteVolume(name)
	emitOpTiming("delete-vol", time.Since(start), err)
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Println("    OK: volume deleted.")
	fmt.Println()
}

func doResizeVolume(engine *cubecow.Engine, name string, newSizeBytes uint64) {
	fmt.Printf("[ResizeVolume] name=%q new_size=%d bytes\n", name, newSizeBytes)

	start := time.Now()
	oldSize, newSize, err := engine.ResizeVolume(name, newSizeBytes)
	emitOpTiming("resize-vol", time.Since(start), err)
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Printf("    OK: old_size=%d -> new_size=%d\n\n", oldSize, newSize)
}

func doGetVolumeInfo(engine *cubecow.Engine, name string) {
	fmt.Printf("[GetVolumeInfo] name=%q\n", name)

	vol, err := engine.GetVolumeInfo(name)
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Printf("    Name:           %s\n", vol.Name)
	fmt.Printf("    SizeBytes:      %d\n", vol.SizeBytes)
	fmt.Printf("    DevicePath:     %s\n", vol.DevicePath)
	fmt.Printf("    SnapshotCount:  %d\n", vol.SnapshotCount)
	fmt.Printf("    CreatedAt:      %s\n\n", vol.CreatedAt)
}

func doGetVolumeBlockInfo(engine *cubecow.Engine, name string) {
	fmt.Printf("[GetVolumeBlockInfo] name=%q\n", name)

	info, err := engine.GetVolumeBlockInfo(name)
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Printf("    NumBlocks:  %d\n", info.NumBlocks)
	fmt.Printf("    BlockSize:  %d bytes\n\n", info.BlockSize)
}

func doListVolumes(engine *cubecow.Engine) {
	fmt.Println("[ListVolumes]")

	result, err := engine.ListVolumes(0, "")
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Printf("    Total: %d volumes\n", result.TotalCount)
	for i, v := range result.Volumes {
		fmt.Printf("    [%d] %s (size=%d, device=%s)\n",
			i, v.Name, v.SizeBytes, v.DevicePath)
	}
	fmt.Println()
}

func doCreateSnapshot(engine *cubecow.Engine, sourceName, snapName string) {
	fmt.Printf("[CreateSnapshot] source=%q snapshot=%q\n", sourceName, snapName)

	start := time.Now()
	devicePath, err := engine.CreateSnapshot(sourceName, snapName)
	emitOpTiming("create-snap", time.Since(start), err)
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Printf("    OK: device_path=%s\n\n", devicePath)
}

func doDeleteSnapshot(engine *cubecow.Engine, snapName string) {
	fmt.Printf("[DeleteSnapshot] name=%q\n", snapName)

	start := time.Now()
	err := engine.DeleteSnapshot(snapName)
	emitOpTiming("delete-snap", time.Since(start), err)
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Println("    OK: snapshot deleted.")
	fmt.Println()
}

func doListSnapshots(engine *cubecow.Engine, volumeName string) {
	fmt.Printf("[ListSnapshots] volume=%q\n", volumeName)

	result, err := engine.ListSnapshots(volumeName, 0, "")
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}
	fmt.Printf("    Found %d snapshots\n", len(result.Snapshots))
	for i, s := range result.Snapshots {
		fmt.Printf("    [%d] %s (origin=%s, size=%d, device=%s)\n",
			i, s.Name, s.OriginVolume, s.SizeBytes, s.DevicePath)
	}
	fmt.Println()
}

func doGetMetrics(engine *cubecow.Engine) {
	fmt.Println("[GetMetrics]")

	metrics, err := engine.GetMetrics()
	if err != nil {
		fmt.Printf("    ERROR: %v\n\n", err)
		return
	}

	// Pretty print metrics
	data, _ := json.MarshalIndent(metrics, "    ", "  ")
	fmt.Printf("    %s\n\n", string(data))
}
