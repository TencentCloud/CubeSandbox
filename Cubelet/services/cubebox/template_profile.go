// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

// Template memory hotset profiling ("hot-pages.json").
//
// When CUBE_TEMPLATE_PROFILE_ENABLE is set, AppSnapshot runs a profiling
// phase after the snapshot package and catalog are in place and before the
// template is finalized for distribution: two verification sandboxes are
// started from the brand-new template through the normal create path, and
// the memory pages the restored guest touches between resume and ready are
// collected from the VMM process (/proc/<pid>/maps + pagemap). The
// intersection of the two rounds drops one-off page faults; the result is
// merged into extents and written next to memory-ranges inside the package
// metadata, where the VMM consumes it at fast-restore time to prewarm the
// page cache (hypervisor/vmm/src/hotset.rs).
//
// The phase is strictly optional and fail-open: any error — including the
// overall budget running out — is returned to the caller, which logs a
// warning and publishes the template without a profile.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"google.golang.org/protobuf/proto"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const (
	// HotPagesFileName is the profile file the VMM looks up inside the
	// snapshot state dir (<meta>/snapshot/), next to memory-ranges.
	HotPagesFileName = "hot-pages.json"
	// HotPagesVersion is the profile schema version the consumer accepts.
	HotPagesVersion = 1

	// EnvTemplateProfileEnable turns the profiling phase on (default off).
	EnvTemplateProfileEnable = "CUBE_TEMPLATE_PROFILE_ENABLE"
	// EnvTemplateProfileTimeoutSec bounds the whole phase in seconds.
	EnvTemplateProfileTimeoutSec = "CUBE_TEMPLATE_PROFILE_TIMEOUT_SEC"

	// verifyRounds is the number of verification restores; the intersection
	// of their touch sets drops one-off page faults.
	verifyRounds = 2
	// hotPagesMaxTotalDen caps the profile extent total at 1/N of the memory
	// volume — the consumer rejects anything larger.
	hotPagesMaxTotalDen = 2
	// extentCoalescePages merges hot pages separated by small gaps so the
	// extent list stays short.
	extentCoalescePages = 16
	// minRoundBudget is the minimum remaining time for another round.
	minRoundBudget = 5 * time.Second
	// defaultProfileBudgetSec is the default total phase budget.
	defaultProfileBudgetSec = 60
)

func templateProfileEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvTemplateProfileEnable))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func templateProfileBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvTemplateProfileTimeoutSec))
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(defaultProfileBudgetSec) * time.Second
}

// runTemplateProfilePhase executes the two-round verification restores and
// writes the merged profile to profilePath. Returned errors are advisory:
// the caller logs them and publishes the template without a profile.
func (s *service) runTemplateProfilePhase(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	createReq *cubebox.RunCubeSandboxRequest,
	backend, templateID, profilePath string,
	memFileSize uint64,
) error {
	deadline := time.Now().Add(templateProfileBudget())
	roundSets := make([]map[uint64]struct{}, 0, verifyRounds)
	for round := 0; round < verifyRounds; round++ {
		remaining := time.Until(deadline)
		if remaining <= minRoundBudget {
			return fmt.Errorf("budget exhausted after %d/%d rounds", round, verifyRounds)
		}
		set, err := s.profileVerificationRound(ctx, stepLog, createReq, backend, templateID, round, remaining)
		if err != nil {
			return fmt.Errorf("verification round %d: %w", round, err)
		}
		roundSets = append(roundSets, set)
	}

	pages := intersectPageSets(roundSets)
	if len(pages) == 0 {
		return errors.New("empty page intersection across rounds")
	}
	extents, totalBytes := mergeHotExtents(pages, extentCoalescePages, memFileSize)
	if totalBytes == 0 {
		return errors.New("merged profile is empty")
	}
	if totalBytes*hotPagesMaxTotalDen > memFileSize {
		return fmt.Errorf("profile total %d bytes exceeds 1/%d of memory size %d",
			totalBytes, hotPagesMaxTotalDen, memFileSize)
	}
	if err := writeHotPagesFile(profilePath, templateID, memFileSize, extents); err != nil {
		return fmt.Errorf("write profile: %w", err)
	}
	stepLog.Infof("template profile: wrote %s: %d extents, %d pages, %d bytes (%.1f%% of memory)",
		profilePath, len(extents), len(pages), totalBytes,
		float64(totalBytes)/float64(memFileSize)*100)
	return nil
}

// profileVerificationRound starts one verification sandbox from the new
// template through the normal create path, waits for it to become ready
// (Create returns), collects the touched page set from the VMM process and
// destroys the sandbox. Errors leave no sandbox behind.
func (s *service) profileVerificationRound(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	createReq *cubebox.RunCubeSandboxRequest,
	backend, templateID string,
	round int,
	budget time.Duration,
) (map[uint64]struct{}, error) {
	verifyReq, err := buildProfileVerifyRequest(createReq, templateID, round)
	if err != nil {
		return nil, err
	}

	verifyCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	verifyCtx = inheritIncomingMetadata(verifyCtx, ctx)
	verifyCtx = CubeLog.WithRequestTrace(verifyCtx, &CubeLog.RequestTrace{
		Action:       "TemplateProfile",
		RequestID:    verifyReq.GetRequestID(),
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "TemplateProfileRound",
	})
	verifyCtx = namespaces.WithNamespace(verifyCtx, namespaces.Default)

	roundLog := stepLog.WithFields(CubeLog.Fields{
		"round":     round,
		"sandboxID": verifyReq.GetAnnotations()[constants.MasterAnnotationDesiredSandboxID],
	})

	createRsp, err := s.Create(verifyCtx, verifyReq)
	if err != nil {
		return nil, fmt.Errorf("create verification sandbox: %w", err)
	}
	if !ret.IsSuccessCode(createRsp.GetRet().GetRetCode()) {
		return nil, fmt.Errorf("create verification sandbox: %s", createRsp.GetRet().GetRetMsg())
	}
	sandboxID := createRsp.GetSandboxID()
	roundLog.Infof("verification sandbox created: %s", sandboxID)

	// Always destroy the verification sandbox, even when collection failed.
	pages, collectErr := s.collectVerificationPageset(verifyCtx, roundLog, backend, templateID, sandboxID)
	destroyCtx, destroyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer destroyCancel()
	destroyCtx = inheritIncomingMetadata(destroyCtx, ctx)
	destroyCtx = namespaces.WithNamespace(destroyCtx, namespaces.Default)
	destroyRsp, destroyErr := s.Destroy(destroyCtx, &cubebox.DestroyCubeSandboxRequest{
		RequestID: verifyReq.GetRequestID(),
		SandboxID: sandboxID,
	})
	switch {
	case destroyErr != nil:
		roundLog.Warnf("verification sandbox %s destroy failed: %v", sandboxID, destroyErr)
	case !ret.IsSuccessCode(destroyRsp.GetRet().GetRetCode()):
		roundLog.Warnf("verification sandbox %s destroy failed: %s", sandboxID, destroyRsp.GetRet().GetRetMsg())
	}
	if collectErr != nil {
		return nil, collectErr
	}
	return pages, nil
}

// buildProfileVerifyRequest derives a template-restore create request from
// the template BUILD request: same workload spec, minus the build marker so
// the workflow takes the create-from-template (restore) path.
func buildProfileVerifyRequest(createReq *cubebox.RunCubeSandboxRequest, templateID string, round int) (*cubebox.RunCubeSandboxRequest, error) {
	req, ok := proto.Clone(createReq).(*cubebox.RunCubeSandboxRequest)
	if !ok || req == nil {
		return nil, errors.New("clone create request failed")
	}
	req.RequestID = fmt.Sprintf("%s-prof-%d", createReq.GetRequestID(), round)
	annos := req.GetAnnotations()
	if annos == nil {
		annos = map[string]string{}
	}
	// Restore is keyed on the template id annotation without the
	// appsnapshot build marker (workflow.CreateContext.IsRetoreSnapshot).
	delete(annos, constants.MasterAnnotationsAppSnapshotCreate)
	annos[constants.MasterAnnotationAppSnapshotTemplateID] = templateID
	// Deterministic sandbox id (createid honors the desired id): distinct
	// from user sandboxes and traceable across the two rounds.
	annos[constants.MasterAnnotationDesiredSandboxID] = fmt.Sprintf("%s-prof-%d", templateID, round)
	annos[constants.AnnotationTemplateProfileVerify] = "true"
	req.Annotations = annos
	return req, nil
}

// collectVerificationPageset resolves the template memory volume, locates
// the VMM process of sandboxID holding it mapped, and reads the present
// pages out of its pagemap.
func (s *service) collectVerificationPageset(
	ctx context.Context,
	stepLog *log.CubeWrapperLogEntry,
	backend, templateID, sandboxID string,
) (map[uint64]struct{}, error) {
	entry, err := storage.GetLocalSnapshotFor(ctx, backend, templateID)
	if err != nil || entry == nil {
		return nil, fmt.Errorf("resolve template catalog: %w", err)
	}
	if strings.TrimSpace(entry.MemoryVol) == "" {
		return nil, errors.New("template catalog has no memory volume")
	}
	memoryDevPath, err := storage.ResolveObjectPathFor(ctx, backend, entry.MemoryVol, entry.MemoryKind)
	if err != nil {
		return nil, fmt.Errorf("resolve memory volume %s: %w", entry.MemoryVol, err)
	}

	cb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("load sandbox record: %w", err)
	}
	pid := findVMMProcessPid(recordedSandboxPIDs(cb), memoryDevPath)
	if pid <= 0 {
		return nil, fmt.Errorf("no process maps %s for sandbox %s", memoryDevPath, sandboxID)
	}

	pages, err := collectPresentPageset(pid, memoryDevPath)
	if err != nil {
		return nil, fmt.Errorf("collect pagemap of pid %d: %w", pid, err)
	}
	stepLog.Infof("collected %d present pages from pid %d (%s)", len(pages), pid, memoryDevPath)
	return pages, nil
}

// findVMMProcessPid picks the candidate pid whose /proc/<pid>/maps maps the
// template memory volume — i.e. the process hosting the restored VM. Exact
// path matches win; basename matches are only a fallback, so a same-basename
// file under a different directory cannot shadow the real volume.
func findVMMProcessPid(candidates []int, memoryDevPath string) int {
	base := filepath.Base(memoryDevPath)
	fallback := 0
	for _, pid := range candidates {
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		content, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
		if err != nil {
			continue
		}
		exact, loose := mapsContainPath(string(content), memoryDevPath, base)
		if exact {
			return pid
		}
		if loose && fallback == 0 {
			fallback = pid
		}
	}
	return fallback
}

// mapsContainPath reports whether any maps line references memoryDevPath,
// separately for exact path equality and basename-only matches (the latter
// tolerates dm/loop path renames between resolve time and mmap time).
func mapsContainPath(mapsData, memoryDevPath, baseName string) (exact, loose bool) {
	for _, line := range strings.Split(mapsData, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 6 {
			continue
		}
		path := fields[5]
		if path == memoryDevPath {
			return true, true
		}
		if filepath.Base(path) == baseName {
			loose = true
		}
	}
	return false, loose
}

// procMapLine is one entry of /proc/<pid>/maps backed by the memory volume.
type procMapLine struct {
	startAddr  uint64
	endAddr    uint64
	fileOffset uint64
}

// parseMapsLine parses "start-end perms offset dev inode path" fields and
// keeps only readable mappings of memoryDevPath. Basename match tolerates
// dm/loop path renames between resolve time and mmap time.
func parseMapsLine(line, memoryDevPath string) (procMapLine, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 6 {
		return procMapLine{}, false
	}
	path := fields[5]
	if path != memoryDevPath && filepath.Base(path) != filepath.Base(memoryDevPath) {
		return procMapLine{}, false
	}
	if !strings.HasPrefix(fields[1], "r") {
		return procMapLine{}, false
	}
	dash := strings.IndexByte(fields[0], '-')
	if dash <= 0 {
		return procMapLine{}, false
	}
	start, err := strconv.ParseUint(fields[0][:dash], 16, 64)
	if err != nil {
		return procMapLine{}, false
	}
	end, err := strconv.ParseUint(fields[0][dash+1:], 16, 64)
	if err != nil {
		return procMapLine{}, false
	}
	offset, err := strconv.ParseUint(fields[2], 16, 64)
	if err != nil {
		return procMapLine{}, false
	}
	return procMapLine{startAddr: start, endAddr: end, fileOffset: offset}, true
}

// collectPresentPageset reads /proc/<pid>/maps + pagemap and returns the set
// of present page numbers (in memory-file page units) among the process's
// mappings of memoryDevPath.
func collectPresentPageset(pid int, memoryDevPath string) (map[uint64]struct{}, error) {
	mapsData, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil, fmt.Errorf("read maps: %w", err)
	}
	pagemap, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", pid)) // NOCC:Path Traversal()
	if err != nil {
		return nil, fmt.Errorf("open pagemap: %w", err)
	}
	defer pagemap.Close()
	return parsePresentPageset(string(mapsData), pagemap, memoryDevPath, uint64(os.Getpagesize()))
}

// parsePresentPageset walks the maps entries matching memoryDevPath and reads
// the pagemap present bits for their vaddr ranges. Split from
// collectPresentPageset for fixture-driven unit tests.
func parsePresentPageset(mapsData string, pagemap io.ReadSeeker, memoryDevPath string, pageSize uint64) (map[uint64]struct{}, error) {
	pages := map[uint64]struct{}{}
	for _, line := range strings.Split(mapsData, "\n") {
		m, ok := parseMapsLine(line, memoryDevPath)
		if !ok {
			continue
		}
		startPage := m.startAddr / pageSize
		endPage := m.endAddr / pageSize
		if endPage <= startPage {
			continue
		}
		count := endPage - startPage
		raw := make([]byte, count*8)
		if _, err := pagemap.Seek(int64(startPage*8), 0); err != nil {
			return nil, fmt.Errorf("seek pagemap: %w", err)
		}
		if _, err := io.ReadFull(pagemap, raw); err != nil {
			return nil, fmt.Errorf("read pagemap: %w", err)
		}
		for i := 0; i < len(raw); i += 8 {
			// Bit 63: page present. Swapped-out pages are not resident and
			// must not enter the hotset.
			if binary.LittleEndian.Uint64(raw[i:])&(1<<63) == 0 {
				continue
			}
			// VMM mappings are page-aligned: mapping page i covers memory
			// file offset fileOffset + (i/8)*pageSize.
			pages[m.fileOffset/pageSize+uint64(i/8)] = struct{}{}
		}
	}
	return pages, nil
}

// intersectPageSets keeps only pages present in every round.
func intersectPageSets(sets []map[uint64]struct{}) map[uint64]struct{} {
	if len(sets) == 0 {
		return nil
	}
	result := sets[0]
	for _, other := range sets[1:] {
		for page := range result {
			if _, ok := other[page]; !ok {
				delete(result, page)
			}
		}
	}
	return result
}

// mergeHotExtents converts a present-page set into [offset, len] byte
// extents, merging runs separated by at most coalesce pages. Extents are
// clamped to memFileSize; the caller rejects over-cap profiles.
func mergeHotExtents(pages map[uint64]struct{}, coalesce uint64, memFileSize uint64) ([][2]uint64, uint64) {
	if len(pages) == 0 {
		return nil, 0
	}
	nums := make([]uint64, 0, len(pages))
	for page := range pages {
		nums = append(nums, page)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })

	pageSize := uint64(os.Getpagesize())
	extents := make([][2]uint64, 0, len(pages))
	start, end := nums[0], nums[0]+1 // [start, end) in pages
	flush := func() {
		offset := start * pageSize
		length := (end - start) * pageSize
		if offset >= memFileSize {
			return
		}
		if offset+length > memFileSize {
			length = memFileSize - offset
		}
		if length == 0 {
			return
		}
		extents = append(extents, [2]uint64{offset, length})
	}
	for _, page := range nums[1:] {
		if page-end <= coalesce {
			if page >= end {
				end = page + 1
			}
			continue
		}
		flush()
		start, end = page, page+1
	}
	flush()

	total := uint64(0)
	for _, e := range extents {
		total += e[1]
	}
	return extents, total
}

// hotPagesFile is the profile schema consumed by hypervisor/vmm hotset.rs.
type hotPagesFile struct {
	Version     int         `json:"version"`
	TemplateID  string      `json:"template_id"`
	MemFileSize uint64      `json:"mem_file_size"`
	ProfiledAt  string      `json:"profiled_at"`
	Extents     [][2]uint64 `json:"extents"`
}

// writeHotPagesFile atomically writes the profile (temp file + rename) so a
// consumer never observes a partial file.
func writeHotPagesFile(path, templateID string, memFileSize uint64, extents [][2]uint64) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}
	payload, err := json.Marshal(hotPagesFile{
		Version:     HotPagesVersion,
		TemplateID:  templateID,
		MemFileSize: memFileSize,
		ProfiledAt:  time.Now().UTC().Format(time.RFC3339),
		Extents:     extents,
	})
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	tmp, err := os.CreateTemp(dir, HotPagesFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp profile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write profile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close profile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename profile: %w", err)
	}
	return nil
}
