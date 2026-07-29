package qos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	AnnotationVersion         = uint64(1)
	BandwidthRefillTimeMillis = uint64(100)
	PacketsRefillTimeMillis   = uint64(1000)
	BlockIORefillTimeMillis   = uint64(1000)
	bytesPerMbpsPerRefill     = uint64(12_500)
	bytesPerMiB               = uint64(1024 * 1024)
)

type Config struct {
	Network *NetworkConfig `json:"network,omitempty"`
	BlockIO *BlockIOConfig `json:"blockIo,omitempty"`
}

type NetworkConfig struct {
	BandwidthMbps       uint32 `json:"bandwidthMbps,omitempty"`
	PacketsPerSecond    uint32 `json:"packetsPerSecond,omitempty"`
	bandwidthConfigured bool
	packetsConfigured   bool
}

type BlockIOConfig struct {
	ThroughputMiBps      uint32 `json:"throughputMiBps,omitempty"`
	IOPS                 uint32 `json:"iops,omitempty"`
	throughputConfigured bool
	iopsConfigured       bool
}

func (c *BlockIOConfig) UnmarshalJSON(data []byte) error {
	type blockIOConfigJSON struct {
		ThroughputMiBps *uint32 `json:"throughputMiBps,omitempty"`
		IOPS            *uint32 `json:"iops,omitempty"`
	}
	var decoded blockIOConfigJSON
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	c.ThroughputMiBps = 0
	c.IOPS = 0
	c.throughputConfigured = false
	c.iopsConfigured = false
	if decoded.ThroughputMiBps != nil {
		c.ThroughputMiBps = *decoded.ThroughputMiBps
		c.throughputConfigured = true
	}
	if decoded.IOPS != nil {
		c.IOPS = *decoded.IOPS
		c.iopsConfigured = true
	}
	return nil
}

func (c *NetworkConfig) UnmarshalJSON(data []byte) error {
	type networkConfigJSON struct {
		BandwidthMbps    *uint32 `json:"bandwidthMbps,omitempty"`
		PacketsPerSecond *uint32 `json:"packetsPerSecond,omitempty"`
	}
	var decoded networkConfigJSON
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	c.BandwidthMbps = 0
	c.PacketsPerSecond = 0
	c.bandwidthConfigured = false
	c.packetsConfigured = false
	if decoded.BandwidthMbps != nil {
		c.BandwidthMbps = *decoded.BandwidthMbps
		c.bandwidthConfigured = true
	}
	if decoded.PacketsPerSecond != nil {
		c.PacketsPerSecond = *decoded.PacketsPerSecond
		c.packetsConfigured = true
	}
	return nil
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var decoded plain
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	*c = Config(decoded)
	return nil
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	if c.Network == nil && c.BlockIO == nil {
		return errors.New("qos must configure network or blockIo")
	}
	if c.Network != nil {
		if c.Network.bandwidthConfigured && c.Network.BandwidthMbps == 0 {
			return errors.New("qos.network.bandwidthMbps must be at least 1")
		}
		if c.Network.packetsConfigured && c.Network.PacketsPerSecond == 0 {
			return errors.New("qos.network.packetsPerSecond must be at least 1")
		}
		if c.Network.BandwidthMbps == 0 && c.Network.PacketsPerSecond == 0 {
			return errors.New("qos.network must configure bandwidthMbps or packetsPerSecond")
		}
	}
	if c.BlockIO != nil {
		if c.BlockIO.throughputConfigured && c.BlockIO.ThroughputMiBps == 0 {
			return errors.New("qos.blockIo.throughputMiBps must be at least 1")
		}
		if c.BlockIO.iopsConfigured && c.BlockIO.IOPS == 0 {
			return errors.New("qos.blockIo.iops must be at least 1")
		}
		if c.BlockIO.ThroughputMiBps == 0 && c.BlockIO.IOPS == 0 {
			return errors.New("qos.blockIo must configure throughputMiBps or iops")
		}
	}
	return nil
}

func Clone(in *Config) *Config {
	if in == nil {
		return nil
	}
	out := *in
	if in.Network != nil {
		network := *in.Network
		out.Network = &network
	}
	if in.BlockIO != nil {
		blockIO := *in.BlockIO
		out.BlockIO = &blockIO
	}
	return &out
}

type TemplateResources struct {
	CPU    string
	Memory string
}

func ResolveTemplate(explicit *Config, resources TemplateResources) (*Config, error) {
	inferred := inferTemplateDefaults(resources)
	return resolveTemplate(explicit, inferred)
}

var inferTemplateDefaults = func(TemplateResources) *Config {
	return nil
}

func resolveTemplate(explicit, inferred *Config) (*Config, error) {
	if err := explicit.Validate(); err != nil {
		return nil, err
	}
	if err := inferred.Validate(); err != nil {
		return nil, fmt.Errorf("inferred qos: %w", err)
	}
	resolved := Clone(inferred)
	if resolved == nil {
		resolved = &Config{}
	}
	mergeExplicit(resolved, explicit)
	if resolved.Network == nil && resolved.BlockIO == nil {
		return nil, nil
	}
	if err := resolved.Validate(); err != nil {
		return nil, err
	}
	return resolved, nil
}

func mergeExplicit(resolved, explicit *Config) {
	if explicit == nil {
		return
	}
	if explicit.Network != nil {
		if resolved.Network == nil {
			resolved.Network = &NetworkConfig{}
		}
		if explicit.Network.bandwidthConfigured || explicit.Network.BandwidthMbps > 0 {
			resolved.Network.BandwidthMbps = explicit.Network.BandwidthMbps
			resolved.Network.bandwidthConfigured = true
		}
		if explicit.Network.packetsConfigured || explicit.Network.PacketsPerSecond > 0 {
			resolved.Network.PacketsPerSecond = explicit.Network.PacketsPerSecond
			resolved.Network.packetsConfigured = true
		}
	}
	if explicit.BlockIO != nil {
		if resolved.BlockIO == nil {
			resolved.BlockIO = &BlockIOConfig{}
		}
		if explicit.BlockIO.throughputConfigured || explicit.BlockIO.ThroughputMiBps > 0 {
			resolved.BlockIO.ThroughputMiBps = explicit.BlockIO.ThroughputMiBps
			resolved.BlockIO.throughputConfigured = true
		}
		if explicit.BlockIO.iopsConfigured || explicit.BlockIO.IOPS > 0 {
			resolved.BlockIO.IOPS = explicit.BlockIO.IOPS
			resolved.BlockIO.iopsConfigured = true
		}
	}
}

type blockIOAnnotation struct {
	Bandwidth *blockIOTokenBucket `json:"bandwidth,omitempty"`
	Ops       *blockIOTokenBucket `json:"ops,omitempty"`
}

type blockIOTokenBucket struct {
	Size         uint64  `json:"size"`
	OneTimeBurst *uint64 `json:"one_time_burst,omitempty"`
	RefillTime   uint64  `json:"refill_time"`
}

func MarshalBlockIOAnnotation(cfg *BlockIOConfig) (string, error) {
	if cfg == nil {
		return "", nil
	}
	if err := (&Config{BlockIO: cfg}).Validate(); err != nil {
		return "", err
	}
	payload := blockIOAnnotation{}
	if cfg.ThroughputMiBps > 0 {
		payload.Bandwidth = &blockIOTokenBucket{
			Size:       uint64(cfg.ThroughputMiBps) * bytesPerMiB,
			RefillTime: BlockIORefillTimeMillis,
		}
	}
	if cfg.IOPS > 0 {
		payload.Ops = &blockIOTokenBucket{
			Size:       uint64(cfg.IOPS),
			RefillTime: BlockIORefillTimeMillis,
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func ParseBlockIOAnnotation(raw string) (*BlockIOConfig, error) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "{}" {
		return nil, nil
	}
	var payload blockIOAnnotation
	if err := decodeStrictJSON([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("decode block io qos annotation: %w", err)
	}
	blockIO := &BlockIOConfig{}
	if payload.Bandwidth != nil {
		value, err := parseBlockIORate(payload.Bandwidth, bytesPerMiB, "throughput")
		if err != nil {
			return nil, err
		}
		blockIO.ThroughputMiBps = value
	}
	if payload.Ops != nil {
		value, err := parseBlockIORate(payload.Ops, 1, "iops")
		if err != nil {
			return nil, err
		}
		blockIO.IOPS = value
	}
	if blockIO.ThroughputMiBps == 0 && blockIO.IOPS == 0 {
		return nil, errors.New("block io qos annotation requires bandwidth or ops")
	}
	return blockIO, nil
}

func parseBlockIORate(bucket *blockIOTokenBucket, unit uint64, name string) (uint32, error) {
	if bucket.Size == 0 || bucket.RefillTime == 0 {
		return 0, fmt.Errorf("block io qos %s bucket requires positive size and refill_time", name)
	}
	numerator := bucket.Size * 1000
	denominator := bucket.RefillTime * unit
	if denominator == 0 || numerator%denominator != 0 {
		return 0, fmt.Errorf("block io qos %s bucket does not map to an integer public rate", name)
	}
	value := numerator / denominator
	if value == 0 || value > uint64(^uint32(0)) {
		return 0, fmt.Errorf("block io qos %s is outside the supported range", name)
	}
	return uint32(value), nil
}

type annotationRequest struct {
	Qos     *annotationQos `json:"Qos"`
	Mode    string         `json:"Mode,omitempty"`
	Version uint64         `json:"Version,omitempty"`
}

type annotationQos struct {
	BandWidth annotationLimiter `json:"BandWidth"`
	OPS       annotationLimiter `json:"OPS"`
}

type annotationLimiter struct {
	Size         uint64 `json:"Size"`
	OneTimeBurst uint64 `json:"OneTimeBurst"`
	RefillTime   uint64 `json:"RefillTime"`
}

func MarshalAnnotation(cfg *Config) (string, error) {
	if cfg == nil || cfg.Network == nil {
		return "", nil
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	qos := &annotationQos{}
	if cfg.Network.BandwidthMbps > 0 {
		qos.BandWidth = annotationLimiter{
			Size:       uint64(cfg.Network.BandwidthMbps) * bytesPerMbpsPerRefill,
			RefillTime: BandwidthRefillTimeMillis,
		}
	}
	if cfg.Network.PacketsPerSecond > 0 {
		qos.OPS = annotationLimiter{
			Size:       uint64(cfg.Network.PacketsPerSecond),
			RefillTime: PacketsRefillTimeMillis,
		}
	}
	payload := annotationRequest{Qos: qos, Version: AnnotationVersion}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func ParseAnnotations(networkRaw, blockIORaw string) (*Config, error) {
	networkConfig, err := ParseAnnotation(networkRaw)
	if err != nil {
		return nil, err
	}
	blockIO, err := ParseBlockIOAnnotation(blockIORaw)
	if err != nil {
		return nil, err
	}
	if networkConfig == nil && blockIO == nil {
		return nil, nil
	}
	configured := &Config{BlockIO: blockIO}
	if networkConfig != nil {
		configured.Network = networkConfig.Network
	}
	return configured, nil
}

func HasAppliedAnnotation(networkRaw, blockIORaw string) bool {
	return strings.TrimSpace(networkRaw) != "" ||
		(strings.TrimSpace(blockIORaw) != "" && strings.TrimSpace(blockIORaw) != "{}")
}

func ParseAnnotation(raw string) (*Config, error) {
	if raw == "" {
		return nil, nil
	}
	var payload annotationRequest
	if err := decodeStrictJSON([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("decode network qos annotation: %w", err)
	}
	if payload.Version != 0 && payload.Version != AnnotationVersion {
		return nil, fmt.Errorf("unsupported network qos annotation version %d", payload.Version)
	}
	if payload.Mode != "" {
		return nil, fmt.Errorf("unsupported network qos mode %q", payload.Mode)
	}
	if payload.Qos == nil {
		return nil, errors.New("network qos annotation requires Qos")
	}
	network := &NetworkConfig{}
	bandwidthConfigured := bucketConfigured(payload.Qos.BandWidth)
	if bandwidthConfigured {
		mbps, err := parseBandwidthBucket(payload.Qos.BandWidth)
		if err != nil {
			return nil, err
		}
		network.BandwidthMbps = mbps
	}
	if bucketConfigured(payload.Qos.OPS) {
		packets, err := parsePacketsBucket(payload.Qos.OPS)
		if err != nil {
			return nil, err
		}
		network.PacketsPerSecond = packets
	}
	if !bandwidthConfigured && !bucketConfigured(payload.Qos.OPS) {
		return nil, errors.New("network qos annotation requires bandwidth or packets-per-second bucket")
	}
	return &Config{Network: network}, nil
}

func bucketConfigured(bucket annotationLimiter) bool {
	return bucket.Size != 0 || bucket.OneTimeBurst != 0 || bucket.RefillTime != 0
}

func parseBandwidthBucket(bucket annotationLimiter) (uint32, error) {
	if bucket.Size == 0 || bucket.RefillTime == 0 {
		return 0, errors.New("network qos bandwidth bucket requires positive Size and RefillTime")
	}
	numerator := bucket.Size * 8 * 1000
	denominator := bucket.RefillTime * 1_000_000
	if denominator == 0 || numerator%denominator != 0 {
		return 0, errors.New("network qos bandwidth does not map to an integer Mbps value")
	}
	mbps := numerator / denominator
	if mbps == 0 || mbps > uint64(^uint32(0)) {
		return 0, errors.New("network qos bandwidth is outside the supported Mbps range")
	}
	return uint32(mbps), nil
}

func parsePacketsBucket(bucket annotationLimiter) (uint32, error) {
	if bucket.Size == 0 || bucket.RefillTime == 0 {
		return 0, errors.New("network qos packets-per-second bucket requires positive Size and RefillTime")
	}
	numerator := bucket.Size * 1000
	if numerator%bucket.RefillTime != 0 {
		return 0, errors.New("network qos packets-per-second bucket does not map to an integer rate")
	}
	packets := numerator / bucket.RefillTime
	if packets == 0 || packets > uint64(^uint32(0)) {
		return 0, errors.New("network qos packets-per-second value is outside the supported range")
	}
	return uint32(packets), nil
}

func decodeStrictJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
