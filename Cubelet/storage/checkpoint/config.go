// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package checkpoint publishes a sealed local pause package to an S3
// compatible object store and brings it back on another node.
//
// A checkpoint is self-contained: every byte a restore needs is either in an
// uploaded chunk or declared zero by the manifest, so a node that never saw
// the origin can rebuild the package from the object store alone. Objects
// of one upload session live under their own prefix and are never rewritten.
// The fixed candidate key is written last and is the only way a reader finds
// a session, so a reader either sees a complete session or none.
package checkpoint

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultConfigPath is the S3 credential file one-click writes for s3lvol.
// xfs-s3 reuses it so both backends talk to the same object store.
const DefaultConfigPath = "/data/cubelet/s3.cfg"

// DefaultPrefix keeps checkpoint objects apart from s3lvol objects in a
// shared bucket.
const DefaultPrefix = "cube-xfs-s3"

// Config is the object store a node exports to and imports from.
type Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
	NoTLS     bool
	Prefix    string
	// ConditionalWrites is ConditionalRequired (the default) or
	// ConditionalOptional: whether a store that ignores If-None-Match may
	// still hold checkpoints, committed without that protection.
	ConditionalWrites string
	// MaxBandwidthMbps caps this node's checkpoint traffic, uploads and
	// downloads together, in megabits per second. Zero means no cap.
	MaxBandwidthMbps int64
	// Incremental exports reuse chunks the sandbox's lineage or template
	// base already hold. RootfsChunkMiB and MemoryChunkMiB size their
	// windows; zero means the defaults of ChunkProfile.
	Incremental    bool
	RootfsChunkMiB int64
	MemoryChunkMiB int64
	// ChunkProfile picks the default window sizes for the kind of store:
	// ChunkProfileRemote (the default) or ChunkProfileLowLatency. A remote
	// object store answers each request in tens of milliseconds or more,
	// and a request's time grows much more slowly than its size, so a
	// transfer moves more bytes per request, and per second, with bigger
	// windows; it also sends fewer requests to a store that caps them.
	// LowLatency is for a store on the node or next to it, where requests
	// are cheap and smaller windows keep incremental checkpoints small.
	ChunkProfile string
	// ExportWorkers and ReadWorkers are how many chunks an export, and a
	// readback or import, moves at once; zero means DefaultTransferWorkers.
	// Each worker holds one chunk in memory.
	ExportWorkers int
	ReadWorkers   int
	// SweepRequestsPerSecond caps the requests the background sweep of one
	// lineage sends, listing and deleting what a newer checkpoint
	// superseded. Zero means DefaultSweepRequestsPerSecond.
	SweepRequestsPerSecond int
	// Verify is VerifyFull (read everything back and trial-restore it) or
	// VerifyExistence (check every chunk is in the store with its size).
	// Empty means full.
	Verify string
	// ImportVerify is how an import checks chunks it takes from the node's
	// local baseline: ImportVerifyFull reads each back and hashes it;
	// ImportVerifyFIEMAP trusts a chunk whose blocks the clone still shares
	// with a baseline that has not moved since it was saved, as an export
	// does. Empty means full.
	ImportVerify string
}

// Values of Config.ImportVerify.
const (
	ImportVerifyFull   = "full"
	ImportVerifyFIEMAP = "fiemap"
)

// Values of Config.Verify.
const (
	VerifyFull      = "full"
	VerifyExistence = "existence"
)

// Values of Config.ChunkProfile.
const (
	ChunkProfileRemote     = "remote"
	ChunkProfileLowLatency = "low_latency"
)

// DefaultTransferWorkers is the default of ExportWorkers and ReadWorkers.
// Against a remote store each connection is bound by request latency rather
// than bandwidth, so more workers move more bytes per second until the node
// or the store runs out of capacity. 32 was the fastest setting measured on
// a production S3-compatible store; each worker holds one chunk in memory.
// The low-latency profile has no measurement of its own and uses the same.
const (
	DefaultTransferWorkers = 32
	MaxTransferWorkers     = 64
)

// DefaultSweepRequestsPerSecond leaves most of a per-account request quota,
// which every node shares, to the exports and imports the sweep runs
// beside.
const DefaultSweepRequestsPerSecond = 30

// ChunkSizes returns the rootfs and memory window sizes in bytes: 16 MiB
// and 4 MiB for a remote store, 4 MiB and 1 MiB for a low-latency one,
// unless rootfs_chunk_mib or memory_chunk_mib set them.
func (c *Config) ChunkSizes() (rootfs, memory int64) {
	rootfs, memory = 16<<20, 4<<20
	if c.ChunkProfile == ChunkProfileLowLatency {
		rootfs, memory = 4<<20, 1<<20
	}
	if c.RootfsChunkMiB > 0 {
		rootfs = c.RootfsChunkMiB << 20
	}
	if c.MemoryChunkMiB > 0 {
		memory = c.MemoryChunkMiB << 20
	}
	return rootfs, memory
}

// TransferWorkers returns how many chunks an export and a readback or
// import move at once.
func (c *Config) TransferWorkers() (export, read int) {
	export, read = DefaultTransferWorkers, DefaultTransferWorkers
	if c.ExportWorkers > 0 {
		export = c.ExportWorkers
	}
	if c.ReadWorkers > 0 {
		read = c.ReadWorkers
	}
	return export, read
}

// SweepRate is the request rate of one lineage sweep.
func (c *Config) SweepRate() int {
	if c.SweepRequestsPerSecond > 0 {
		return c.SweepRequestsPerSecond
	}
	return DefaultSweepRequestsPerSecond
}

// BandwidthBytesPerSecond is MaxBandwidthMbps in bytes per second.
func (c *Config) BandwidthBytesPerSecond() int64 {
	return c.MaxBandwidthMbps * 1_000_000 / 8
}

// Values of Config.ConditionalWrites.
const (
	ConditionalRequired = "required"
	ConditionalOptional = "optional"
)

// LoadConfig reads the key=value file at path. Unknown keys are ignored;
// "buckets" may list several buckets and the first one is used.
//
// Transfer keys and their defaults:
//
//	chunk_profile = remote             # or low_latency
//	memory_chunk_mib, rootfs_chunk_mib # 1-256; override the profile's 4 / 16 (low_latency: 1 / 4)
//	export_workers = 32                # 1-64; CUBE_XFSS3_EXPORT_WORKERS overrides it
//	read_workers = 32                  # 1-64; CUBE_XFSS3_READ_WORKERS overrides it
//	sweep_requests_per_second = 30     # 1-1000
//	max_bandwidth_mbps = 0             # no cap
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path) // NOCC:Path Traversal()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg := &Config{Prefix: DefaultPrefix, PathStyle: true, ConditionalWrites: ConditionalRequired}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "access_key_id":
			cfg.AccessKey = value
		case "secret_access_key":
			cfg.SecretKey = value
		case "endpoint":
			cfg.Endpoint = value
		case "region":
			cfg.Region = value
		case "bucket", "buckets":
			if cfg.Bucket == "" {
				cfg.Bucket = firstBucket(value)
			}
		case "path_style":
			cfg.PathStyle = parseBool(value, true)
		case "no_tls":
			cfg.NoTLS = parseBool(value, false)
		case "checkpoint_prefix":
			if value != "" {
				cfg.Prefix = strings.Trim(value, "/")
			}
		case "conditional_writes":
			cfg.ConditionalWrites = strings.ToLower(value)
		case "incremental":
			cfg.Incremental = parseBool(value, false)
		case "rootfs_chunk_mib", "memory_chunk_mib":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 1 || n > 256 {
				return nil, fmt.Errorf("%s: %s %q is not between 1 and 256", path, key, value)
			}
			if key == "rootfs_chunk_mib" {
				cfg.RootfsChunkMiB = n
			} else {
				cfg.MemoryChunkMiB = n
			}
		case "chunk_profile":
			cfg.ChunkProfile = strings.ToLower(value)
		case "export_workers", "read_workers":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > MaxTransferWorkers {
				return nil, fmt.Errorf("%s: %s %q is not between 1 and %d", path, key, value, MaxTransferWorkers)
			}
			if key == "export_workers" {
				cfg.ExportWorkers = n
			} else {
				cfg.ReadWorkers = n
			}
		case "sweep_requests_per_second":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 1000 {
				return nil, fmt.Errorf("%s: sweep_requests_per_second %q is not between 1 and 1000", path, value)
			}
			cfg.SweepRequestsPerSecond = n
		case "verify":
			cfg.Verify = strings.ToLower(value)
		case "import_verify":
			cfg.ImportVerify = strings.ToLower(value)
		case "max_bandwidth_mbps":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%s: max_bandwidth_mbps %q is not a non-negative integer", path, value)
			}
			cfg.MaxBandwidthMbps = n
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports the first missing field.
func (c *Config) Validate() error {
	switch {
	case c == nil:
		return fmt.Errorf("checkpoint store is not configured")
	case strings.TrimSpace(c.Endpoint) == "":
		return fmt.Errorf("checkpoint store endpoint is empty")
	case strings.TrimSpace(c.Bucket) == "":
		return fmt.Errorf("checkpoint store bucket is empty")
	case c.AccessKey == "" || c.SecretKey == "":
		return fmt.Errorf("checkpoint store credentials are empty")
	}
	if strings.Contains(c.Prefix, "..") {
		return fmt.Errorf("checkpoint prefix %q is invalid", c.Prefix)
	}
	switch c.Verify {
	case "", VerifyFull, VerifyExistence:
	default:
		return fmt.Errorf("verify %q is neither %s nor %s", c.Verify, VerifyFull, VerifyExistence)
	}
	switch c.ImportVerify {
	case "", ImportVerifyFull, ImportVerifyFIEMAP:
	default:
		return fmt.Errorf("import_verify %q is neither %s nor %s", c.ImportVerify, ImportVerifyFull, ImportVerifyFIEMAP)
	}
	switch c.ChunkProfile {
	case "", ChunkProfileRemote, ChunkProfileLowLatency:
	default:
		return fmt.Errorf("chunk_profile %q is neither %s nor %s", c.ChunkProfile, ChunkProfileRemote, ChunkProfileLowLatency)
	}
	for name, n := range map[string]int{"export_workers": c.ExportWorkers, "read_workers": c.ReadWorkers} {
		if n < 0 || n > MaxTransferWorkers {
			return fmt.Errorf("%s %d is not between 1 and %d", name, n, MaxTransferWorkers)
		}
	}
	if c.SweepRequestsPerSecond < 0 || c.SweepRequestsPerSecond > 1000 {
		return fmt.Errorf("sweep_requests_per_second %d is not between 1 and 1000", c.SweepRequestsPerSecond)
	}
	switch c.ConditionalWrites {
	case "", ConditionalRequired, ConditionalOptional:
	default:
		return fmt.Errorf("conditional_writes %q is neither %s nor %s", c.ConditionalWrites, ConditionalRequired, ConditionalOptional)
	}
	return nil
}

// firstBucket accepts "b1", "b1,b2" and the JSON list one-click writes,
// ["b1","b2"].
func firstBucket(v string) string {
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(v), "["), "]"))
	first, _, _ := strings.Cut(v, ",")
	return strings.Trim(strings.TrimSpace(first), `"'`)
}

func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
