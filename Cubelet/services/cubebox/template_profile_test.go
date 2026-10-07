// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// ---- parseMapsLine ----

const testMemoryDev = "/dev/cubecow/tpl-a-memory"

func TestParseMapsLineExactMatch(t *testing.T) {
	line := "00001000-00003000 r--p 00000000 08:01 123 " + testMemoryDev
	m, ok := parseMapsLine(line, testMemoryDev, mapsDevT{}, false)
	if !ok {
		t.Fatalf("expected match")
	}
	if m.startAddr != 0x1000 || m.endAddr != 0x3000 || m.fileOffset != 0 {
		t.Fatalf("unexpected fields: %+v", m)
	}
}

func TestParseMapsLineBasenameFallback(t *testing.T) {
	// dev path renamed between resolve and mmap: basename matches only with
	// device agreement
	verified := mapsDevT{major: 0x8, minor: 0x1, ino: 123}
	line := "00001000-00003000 rw-p 00000000 08:01 123 /dev/dm-7"
	if _, ok := parseMapsLine(line, testMemoryDev, verified, true); ok {
		t.Fatalf("dm-7 basename must not match tpl-a-memory")
	}
	line = "00001000-00003000 rw-p 00000000 08:01 123 /dev/cubecow/tpl-a-memory2"
	if _, ok := parseMapsLine(line, testMemoryDev, verified, true); ok {
		t.Fatalf("different volume must not match")
	}
	line = "00001000-00003000 rw-p 00000000 259:12 999 /dev/nvme0n12 " // basename differs
	if _, ok := parseMapsLine(line, "/dev/nvme0n1", verified, true); ok {
		t.Fatalf("basename prefix must not match")
	}
	line = "00001000-00003000 rw-p 00000000 08:01 123 /data/cube/tpl-a-memory"
	m, ok := parseMapsLine(line, "/dev/mapper/tpl-a-memory", verified, true)
	if !ok || m.startAddr != 0x1000 {
		t.Fatalf("expected verified basename fallback match: %+v ok=%v", m, ok)
	}
	// same basename but a different device number: a different volume
	other := verified
	other.minor++
	if _, ok := parseMapsLine(line, "/dev/mapper/tpl-a-memory", other, true); ok {
		t.Fatalf("basename fallback must require device-number agreement")
	}
	// identity unknown (volume cannot be stated): basename must not match
	if _, ok := parseMapsLine(line, "/dev/mapper/tpl-a-memory", verified, false); ok {
		t.Fatalf("basename fallback must be refused without stat identity")
	}
	// regular-file volume: the inode must agree too
	regIdent := mapsDevT{major: 0x8, minor: 0x1, ino: 123, needIno: true}
	if _, ok := parseMapsLine(line, "/dev/mapper/tpl-a-memory", regIdent, true); !ok {
		t.Fatalf("matching inode must be accepted for regular files")
	}
	wrongIno := regIdent
	wrongIno.ino++
	if _, ok := parseMapsLine(line, "/dev/mapper/tpl-a-memory", wrongIno, true); ok {
		t.Fatalf("basename fallback must require inode agreement for regular files")
	}
}

func TestParseMapsLineRejects(t *testing.T) {
	cases := map[string]string{
		"non readable":   "00001000-00003000 ---p 00000000 08:01 123 " + testMemoryDev,
		"anon mapping":   "00001000-00003000 rw-p 00000000 00:00 0",
		"malformed addr": "zzz r--p 00000000 08:01 123 " + testMemoryDev,
		"short line":     "00001000 r--p",
	}
	for name, line := range cases {
		if _, ok := parseMapsLine(line, testMemoryDev, mapsDevT{}, false); ok {
			t.Fatalf("%s: expected no match", name)
		}
	}
}

// ---- collectPresentPageset (fixture-driven core) ----

func pagemapFixture(entries map[uint64]uint64, pages uint64) []byte {
	raw := make([]byte, pages*8)
	for page, entry := range entries {
		if page >= pages {
			continue
		}
		binary.LittleEndian.PutUint64(raw[page*8:], entry)
	}
	return raw
}

func TestParsePresentPageset(t *testing.T) {
	const pageSize = uint64(4096)
	mapsData := strings.Join([]string{
		// mapping A: vaddr pages 1-2, file offset 0
		"00001000-00003000 r--p 00000000 08:01 123 " + testMemoryDev,
		// mapping B: vaddr pages 4-7, file offset 0x8000 (file page 8)
		"00004000-00008000 rw-p 00008000 08:01 123 " + testMemoryDev,
		// other file: excluded
		"00009000-0000a000 r--p 00000000 08:01 456 /dev/other",
		// non readable: excluded
		"0000b000-0000c000 ---p 00000000 08:01 123 " + testMemoryDev,
		"", // trailing newline
	}, "\n")

	entries := map[uint64]uint64{
		1: 1 << 63,           // present
		2: 0,                 // swapped out
		4: 1<<63 | 0x8000000, // present (frame number bits)
		5: 0,                 // not present
		6: 1 << 63,           // present
		7: 1 << 63,           // present
	}
	raw := pagemapFixture(entries, 8)
	pm := bytes.NewReader(raw)

	pages, err := parsePresentPageset(context.Background(), mapsData, pm, testMemoryDev, pageSize)
	if err != nil {
		t.Fatalf("parsePresentPageset: %v", err)
	}
	want := map[uint64]struct{}{
		0:  {}, // mapping A: vaddr page 1 present, file offset 0
		8:  {}, // mapping B: vaddr page 4 present, file offset 0x8000 = page 8
		10: {}, // vaddr page 6 -> file page 8+2
		11: {}, // vaddr page 7 -> file page 8+3
	}
	if len(pages) != len(want) {
		t.Fatalf("got %v want %v", pages, want)
	}
	for page := range want {
		if _, ok := pages[page]; !ok {
			t.Fatalf("missing page %d in %v", page, pages)
		}
	}
}

// ---- intersection & extent merge ----

func TestIntersectPageSets(t *testing.T) {
	sets := []map[uint64]struct{}{
		{1: {}, 2: {}, 3: {}, 9: {}},
		{2: {}, 3: {}, 8: {}},
		{2: {}, 3: {}, 7: {}},
	}
	got := intersectPageSets(sets)
	if len(got) != 2 {
		t.Fatalf("expected {2,3}, got %v", got)
	}
	for _, page := range []uint64{2, 3} {
		if _, ok := got[page]; !ok {
			t.Fatalf("missing %d: %v", page, got)
		}
	}
	if got := intersectPageSets(nil); got != nil {
		t.Fatalf("nil input should give nil")
	}
}

func TestMergeHotExtentsAdjacentAndCoalesce(t *testing.T) {
	const pageSize = uint64(4096)
	pages := map[uint64]struct{}{1: {}, 2: {}, 3: {}, 100: {}, 101: {}}
	extents, total := mergeHotExtents(pages, 0, 1<<30)
	if len(extents) != 2 {
		t.Fatalf("coalesce=0: expected 2 extents, got %v", extents)
	}
	if extents[0][0] != pageSize || extents[0][1] != 3*pageSize {
		t.Fatalf("unexpected first extent %v", extents[0])
	}
	if extents[1][0] != 100*pageSize || extents[1][1] != 2*pageSize {
		t.Fatalf("unexpected second extent %v", extents[1])
	}
	if total != 5*pageSize {
		t.Fatalf("unexpected total %d", total)
	}

	// coalesce 16: gap 4..99 is 95 pages — stays split
	extents, _ = mergeHotExtents(pages, 16, 1<<30)
	if len(extents) != 2 {
		t.Fatalf("coalesce=16: expected 2 extents, got %v", extents)
	}

	// small gap merges: {1, 4} with coalesce 16 -> one extent [4096, 4*4096)
	extents, _ = mergeHotExtents(map[uint64]struct{}{1: {}, 4: {}}, 16, 1<<30)
	if len(extents) != 1 || extents[0][0] != pageSize || extents[0][1] != 4*pageSize {
		t.Fatalf("gap merge failed: %v", extents)
	}
}

func TestMergeHotExtentsClampsToFileSize(t *testing.T) {
	const pageSize = uint64(4096)
	memSize := uint64(2) * pageSize // 2 pages of memory
	pages := map[uint64]struct{}{0: {}, 1: {}, 2: {}, 3: {}}
	extents, total := mergeHotExtents(pages, 0, memSize)
	if total != memSize {
		t.Fatalf("expected clamped total %d, got %d", memSize, total)
	}
	if len(extents) != 1 || extents[0][0] != 0 || extents[0][1] != memSize {
		t.Fatalf("unexpected extents %v", extents)
	}
}

func TestCheckHotProfileCaps(t *testing.T) {
	const pageSize = uint64(4096)
	memSize := uint64(1) << 30 // 1GiB memory volume

	// well-formed small profile passes
	if err := checkHotProfileCaps([][2]uint64{{0, 64 * pageSize}}, 64*pageSize, memSize); err != nil {
		t.Fatalf("small profile rejected: %v", err)
	}

	// empty profile
	if err := checkHotProfileCaps(nil, 0, memSize); err == nil {
		t.Fatal("empty profile accepted")
	}

	// total over the 1/hotPagesMaxTotalDen share of the volume
	if err := checkHotProfileCaps([][2]uint64{{0, memSize}}, memSize, memSize); err == nil {
		t.Fatal("over-total profile accepted")
	}

	// extent count over the cap: hotPagesMaxExtents+1 one-page extents
	over := make([][2]uint64, hotPagesMaxExtents+1)
	var total uint64
	for i := range over {
		over[i] = [2]uint64{uint64(i) * 8 * pageSize, pageSize}
		total += pageSize
	}
	if err := checkHotProfileCaps(over, total, memSize); err == nil {
		t.Fatal("over-count profile accepted")
	}

	// exactly at the cap is fine
	at := over[:hotPagesMaxExtents]
	if err := checkHotProfileCaps(at, total-pageSize, memSize); err != nil {
		t.Fatalf("at-cap profile rejected: %v", err)
	}
}

// ---- profile file ----

func TestWriteHotPagesFileAtomicAndReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot", HotPagesFileName)
	extents := [][2]uint64{{0, 4096}, {8192, 4096}}
	if err := writeHotPagesFile(path, "tpl-x", 1<<20, extents); err != nil {
		t.Fatalf("writeHotPagesFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var parsed hotPagesFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Version != HotPagesVersion || parsed.TemplateID != "tpl-x" || parsed.MemFileSize != 1<<20 {
		t.Fatalf("unexpected header: %+v", parsed)
	}
	if len(parsed.Extents) != 2 || parsed.Extents[0] != [2]uint64{0, 4096} {
		t.Fatalf("unexpected extents: %v", parsed.Extents)
	}
	// no temp files left behind
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// ---- verify request derivation ----

func TestBuildProfileVerifyRequest(t *testing.T) {
	orig := &cubebox.RunCubeSandboxRequest{
		RequestID: "req-1",
		Annotations: map[string]string{
			constants.MasterAnnotationsAppSnapshotCreate:    "true",
			constants.MasterAnnotationAppSnapshotTemplateID: "tpl-a",
			constants.MasterAnnotationStorageBackend:        "xfs",
		},
	}
	req, err := buildProfileVerifyRequest(orig, "tpl-a", 1)
	if err != nil {
		t.Fatalf("buildProfileVerifyRequest: %v", err)
	}
	if req.GetRequestID() != "req-1-prof-1" {
		t.Fatalf("unexpected request id %q", req.GetRequestID())
	}
	annos := req.GetAnnotations()
	if _, ok := annos[constants.MasterAnnotationsAppSnapshotCreate]; ok {
		t.Fatalf("build marker must be removed")
	}
	if annos[constants.MasterAnnotationAppSnapshotTemplateID] != "tpl-a" {
		t.Fatalf("template id must be kept")
	}
	if annos[constants.MasterAnnotationDesiredSandboxID] != "tpl-a-prof-1" {
		t.Fatalf("unexpected desired sandbox id %q", annos[constants.MasterAnnotationDesiredSandboxID])
	}
	if annos[constants.MasterAnnotationStorageBackend] != "xfs" {
		t.Fatalf("unrelated annotations must be kept")
	}
	// original request untouched
	if orig.GetAnnotations()[constants.MasterAnnotationsAppSnapshotCreate] != "true" {
		t.Fatalf("original request was mutated")
	}
	if orig.GetRequestID() != "req-1" {
		t.Fatalf("original request id mutated")
	}
}

// ---- env switches ----

var testEnvMu sync.Mutex

func TestTemplateProfileSwitches(t *testing.T) {
	testEnvMu.Lock()
	defer testEnvMu.Unlock()

	os.Unsetenv(EnvTemplateProfileEnable)
	os.Unsetenv(EnvTemplateProfileTimeoutSec)
	if templateProfileEnabled() {
		t.Fatalf("default must be disabled")
	}
	if templateProfileBudget() != 60*1000*1000*1000 {
		t.Fatalf("default budget mismatch: %v", templateProfileBudget())
	}

	for _, v := range []string{"1", "true", "YES", "on"} {
		t.Setenv(EnvTemplateProfileEnable, v)
		if !templateProfileEnabled() {
			t.Fatalf("enable value %q not accepted", v)
		}
	}
	os.Unsetenv(EnvTemplateProfileEnable)
	for _, v := range []string{"0", "false", "", "off"} {
		t.Setenv(EnvTemplateProfileEnable, v)
		if templateProfileEnabled() {
			t.Fatalf("enable value %q must be rejected", v)
		}
	}
	os.Unsetenv(EnvTemplateProfileEnable)

	t.Setenv(EnvTemplateProfileTimeoutSec, "7")
	if templateProfileBudget() != 7*1000*1000*1000 {
		t.Fatalf("custom budget mismatch: %v", templateProfileBudget())
	}
	t.Setenv(EnvTemplateProfileTimeoutSec, "not-a-number")
	if templateProfileBudget() != 60*1000*1000*1000 {
		t.Fatalf("invalid budget must fall back to default")
	}
	os.Unsetenv(EnvTemplateProfileTimeoutSec)
}

// ---- vmm pid discovery (no /proc fixture needed for the empty case) ----

func TestFindVMMProcessPidEmptyCandidates(t *testing.T) {
	if got := findVMMProcessPid(nil, testMemoryDev); got != 0 {
		t.Fatalf("expected 0 for empty candidates, got %d", got)
	}
	// self pid is skipped
	self := os.Getpid()
	if got := findVMMProcessPid([]int{self, -1, 0, 1}, testMemoryDev); got != 0 {
		t.Fatalf("expected self/special pids to be skipped, got %d", got)
	}
}

// ---- device-verified basename fallback ----

// mapsLineFor renders a maps line from the real stat identity of path.
func mapsLineFor(t *testing.T, path string) (string, mapsDevT) {
	t.Helper()
	ident, ok := resolveMapsDev(path)
	if !ok {
		t.Fatalf("resolveMapsDev(%s) failed", path)
	}
	return fmt.Sprintf("00001000-00003000 rw-p 00000000 %02x:%02x %d %s",
		ident.major, ident.minor, ident.ino, path), ident
}

func TestMapsContainPathFallbackRequiresDeviceAgreement(t *testing.T) {
	real := filepath.Join(t.TempDir(), "tpl-a-memory")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	line, ident := mapsLineFor(t, real)

	// exact path always matches, regardless of the device field
	if exact, _ := mapsContainPath(line, real, "tpl-a-memory", ident, true); !exact {
		t.Fatalf("expected exact match")
	}
	// verified basename fallback: same basename, same device number
	renamed := "/run/other/dir/tpl-a-memory"
	if exact, loose := mapsContainPath(line, renamed, "tpl-a-memory", ident, true); exact || !loose {
		t.Fatalf("expected verified loose match, got exact=%v loose=%v", exact, loose)
	}
	// same basename but the identity is unknown: must not match
	if _, loose := mapsContainPath(line, renamed, "tpl-a-memory", ident, false); loose {
		t.Fatalf("loose match must be refused when the volume cannot be stated")
	}
	// same basename, different device number: a different volume — must not match
	other := ident
	other.minor++
	if _, loose := mapsContainPath(line, renamed, "tpl-a-memory", other, true); loose {
		t.Fatalf("loose match must require device-number agreement")
	}
	if ident.needIno {
		// regular files must also agree on the inode
		wrongIno := ident
		wrongIno.ino++
		if _, loose := mapsContainPath(line, renamed, "tpl-a-memory", wrongIno, true); loose {
			t.Fatalf("loose match must require inode agreement for regular files")
		}
	}
}

func TestResolveMapsDevRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.bin")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	ident, ok := resolveMapsDev(path)
	if !ok {
		t.Fatalf("expected identity for a statable regular file")
	}
	if !ident.needIno || ident.ino == 0 {
		t.Fatalf("regular file identity must carry the inode: %+v", ident)
	}
	if _, ok := resolveMapsDev(filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatalf("identity must be refused for a missing path")
	}
}
