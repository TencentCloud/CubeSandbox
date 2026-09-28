// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"regexp"
	"strconv"
	"strings"
)

// PullProgress is a best-effort snapshot of source-image pull progress.
//
// Two granularities are represented because the two pull backends expose
// different information:
//
//   - skopeo copy (dockerless path) reports per-blob byte counters, so
//     TotalBytes/DownloadedBytes are populated and Percent is byte-accurate.
//   - docker pull (docker path) in non-TTY mode only reports per-layer status
//     transitions, so only TotalLayers/CompletedLayers are populated and
//     Percent is derived from completed layers.
//
// Consumers should prefer byte fields when TotalBytes > 0 and fall back to the
// layer fields otherwise.
type PullProgress struct {
	TotalBytes      int64
	DownloadedBytes int64
	TotalLayers     int
	CompletedLayers int
	SpeedBPS        int64
	Percent         float64
}

// ProgressFunc receives pull-progress updates. Implementations must be safe to
// call from the goroutines that stream subprocess output. A nil ProgressFunc
// disables progress streaming and preserves the original buffered-exec
// behaviour (see command.go).
type ProgressFunc func(PullProgress)

// progressParser consumes subprocess output one logical line at a time and
// returns the latest cumulative progress snapshot plus whether the snapshot
// changed. Implementations are not safe for concurrent use; callers must
// serialise feed calls.
type progressParser interface {
	feed(line string) (PullProgress, bool)
}

// ---------------------------------------------------------------------------
// skopeo copy parser (byte-level)
// ---------------------------------------------------------------------------

// skopeoBlobSizeRe matches an in-progress "Copying blob" line, e.g.
//
//	Copying blob sha256:abc123 [=====>------] 5.0MiB / 30.0MiB
//	Copying blob abc123 5.0MiB / 30.0MiB
//
// Capture groups: 1=blob id, 2=downloaded size token, 3=total size token.
var skopeoBlobSizeRe = regexp.MustCompile(`(?i)copying\s+blob\s+(\S+).*?([0-9.]+\s*[kmgt]?i?b)\s*/\s*([0-9.]+\s*[kmgt]?i?b)`)

// skopeoBlobDoneRe matches a completed blob line, e.g. "Copying blob abc done".
var skopeoBlobDoneRe = regexp.MustCompile(`(?i)copying\s+blob\s+(\S+)\s+done`)

type skopeoBlobState struct {
	downloaded int64
	total      int64
	finished   bool
}

type skopeoParser struct {
	totalHint int64
	blobs     map[string]*skopeoBlobState
	order     []string
}

func newSkopeoParser(totalHint int64) *skopeoParser {
	return &skopeoParser{
		totalHint: totalHint,
		blobs:     make(map[string]*skopeoBlobState),
	}
}

func (p *skopeoParser) blob(id string) *skopeoBlobState {
	if st, ok := p.blobs[id]; ok {
		return st
	}
	st := &skopeoBlobState{}
	p.blobs[id] = st
	p.order = append(p.order, id)
	return st
}

func (p *skopeoParser) feed(line string) (PullProgress, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return PullProgress{}, false
	}
	if m := skopeoBlobDoneRe.FindStringSubmatch(line); m != nil {
		st := p.blob(m[1])
		st.finished = true
		if st.total > 0 {
			st.downloaded = st.total
		}
		return p.snapshot(), true
	}
	if m := skopeoBlobSizeRe.FindStringSubmatch(line); m != nil {
		st := p.blob(m[1])
		if v, ok := parseHumanSize(m[2]); ok {
			st.downloaded = v
		}
		if v, ok := parseHumanSize(m[3]); ok && v > 0 {
			st.total = v
		}
		return p.snapshot(), true
	}
	return PullProgress{}, false
}

func (p *skopeoParser) snapshot() PullProgress {
	var downloaded, total int64
	completed := 0
	for _, id := range p.order {
		st := p.blobs[id]
		downloaded += st.downloaded
		total += st.total
		if st.finished {
			completed++
		}
	}
	// Prefer the inspect-derived total when it is larger; per-blob totals only
	// become known as each blob starts transferring, so early on the summed
	// total under-reports and would make Percent jump around.
	if p.totalHint > total {
		total = p.totalHint
	}
	prog := PullProgress{
		TotalBytes:      total,
		DownloadedBytes: downloaded,
		TotalLayers:     len(p.order),
		CompletedLayers: completed,
	}
	if total > 0 {
		prog.Percent = clampPercent(float64(downloaded) / float64(total) * 100)
	}
	return prog
}

// ---------------------------------------------------------------------------
// docker pull parser (layer-level)
// ---------------------------------------------------------------------------

// dockerLayerRe matches a per-layer docker pull status line, e.g.
//
//	a1b2c3d4e5f6: Pull complete
//	a1b2c3d4e5f6: Downloading
//
// Capture groups: 1=layer id (short hex), 2=status text.
var dockerLayerRe = regexp.MustCompile(`^([0-9a-f]{12,}):\s+(.+?)\s*$`)

type dockerParser struct {
	statusByLayer map[string]string
	order         []string
}

func newDockerParser() *dockerParser {
	return &dockerParser{statusByLayer: make(map[string]string)}
}

func (p *dockerParser) feed(line string) (PullProgress, bool) {
	m := dockerLayerRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return PullProgress{}, false
	}
	id, status := m[1], m[2]
	if _, ok := p.statusByLayer[id]; !ok {
		p.order = append(p.order, id)
	}
	p.statusByLayer[id] = status
	return p.snapshot(), true
}

func dockerLayerComplete(status string) bool {
	s := strings.ToLower(status)
	return strings.Contains(s, "pull complete") || strings.Contains(s, "already exists")
}

func (p *dockerParser) snapshot() PullProgress {
	completed := 0
	for _, id := range p.order {
		if dockerLayerComplete(p.statusByLayer[id]) {
			completed++
		}
	}
	prog := PullProgress{
		TotalLayers:     len(p.order),
		CompletedLayers: completed,
	}
	if len(p.order) > 0 {
		prog.Percent = clampPercent(float64(completed) / float64(len(p.order)) * 100)
	}
	return prog
}

// ---------------------------------------------------------------------------
// Docker-compatible Engine API pull parser (byte-level when available)
// ---------------------------------------------------------------------------

type engineLayerState struct {
	current int64
	total   int64
	status  string
}

type enginePullParser struct {
	layers map[string]*engineLayerState
	order  []string
}

func newEnginePullParser() *enginePullParser {
	return &enginePullParser{layers: make(map[string]*engineLayerState)}
}

func (p *enginePullParser) layer(id string) *engineLayerState {
	if st, ok := p.layers[id]; ok {
		return st
	}
	st := &engineLayerState{}
	p.layers[id] = st
	p.order = append(p.order, id)
	return st
}

func (p *enginePullParser) feed(event enginePullEvent) (PullProgress, bool) {
	id := strings.TrimSpace(event.ID)
	if id == "" {
		return PullProgress{}, false
	}
	st := p.layer(id)
	st.status = event.Status
	if event.ProgressDetail.Current > 0 {
		st.current = event.ProgressDetail.Current
	}
	if event.ProgressDetail.Total > 0 {
		st.total = event.ProgressDetail.Total
	}
	if engineLayerComplete(event.Status) && st.total > 0 {
		st.current = st.total
	}
	return p.snapshot(), true
}

func engineLayerComplete(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return strings.Contains(s, "pull complete") ||
		strings.Contains(s, "already exists") ||
		strings.Contains(s, "download complete") ||
		s == "exists" ||
		s == "done"
}

func (p *enginePullParser) snapshot() PullProgress {
	var downloaded, total int64
	completed := 0
	for _, id := range p.order {
		st := p.layers[id]
		if engineLayerComplete(st.status) {
			completed++
		}
		if st.total > 0 {
			total += st.total
			if st.current > st.total {
				downloaded += st.total
			} else {
				downloaded += st.current
			}
		} else if st.current > 0 {
			downloaded += st.current
		}
	}
	prog := PullProgress{
		TotalBytes:      total,
		DownloadedBytes: downloaded,
		TotalLayers:     len(p.order),
		CompletedLayers: completed,
	}
	if total > 0 {
		prog.Percent = clampPercent(float64(downloaded) / float64(total) * 100)
	} else if len(p.order) > 0 {
		prog.Percent = clampPercent(float64(completed) / float64(len(p.order)) * 100)
	}
	return prog
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var humanSizeRe = regexp.MustCompile(`(?i)^\s*([0-9.]+)\s*([kmgt]?)(i?)b\s*$`)

// parseHumanSize converts a size token such as "5.0MiB", "30 MB" or "512B"
// into bytes. The presence of an "i" selects a binary (1024) multiplier,
// otherwise a decimal (1000) multiplier is used, mirroring how skopeo formats
// sizes. It returns ok=false when the token cannot be parsed.
func parseHumanSize(token string) (int64, bool) {
	m := humanSizeRe.FindStringSubmatch(token)
	if m == nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	base := 1000.0
	if m[3] == "i" || m[3] == "I" {
		base = 1024.0
	}
	exp := 0.0
	switch strings.ToLower(m[2]) {
	case "k":
		exp = 1
	case "m":
		exp = 2
	case "g":
		exp = 3
	case "t":
		exp = 4
	}
	mult := 1.0
	for i := 0.0; i < exp; i++ {
		mult *= base
	}
	return int64(value * mult), true
}

func clampPercent(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
