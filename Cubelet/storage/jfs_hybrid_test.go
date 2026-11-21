// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/jfsdiff"
)

// These tests drive two real cubecow reflink engines on a reflink-capable
// filesystem: one with clone_method=copy_file_range standing in for the
// JuiceFS store, and one or two FICLONE engines standing in for node-local
// XFS. Set JFSDIFF_REFLINK_DIR to a directory on XFS (reflink=1) or Btrfs.

type hybridNode struct {
	h      *jfsHybridStore
	engine *cubecow.Engine
}

type hybridEnv struct {
	t            *testing.T
	dir          string
	sharedEngine *cubecow.Engine
	shared       *XfsCow
	homes        string
	catalog      map[string]*SnapshotCatalogEntry
}

func reflinkTestDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("JFSDIFF_REFLINK_DIR")
	if base == "" {
		t.Skip("JFSDIFF_REFLINK_DIR not set (needs XFS reflink=1 or Btrfs)")
	}
	d, err := os.MkdirTemp(base, "hybrid-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func newTestReflinkEngine(t *testing.T, root, method string) *cubecow.Engine {
	t.Helper()
	level := "warn"
	logFile := filepath.Join(filepath.Dir(root), "cubecow.log")
	cfg := &Config{}
	cfg.Cow.Log.Level = &level
	cfg.Cow.Log.File = &logFile
	payload, err := cfg.buildCowInitJSON(CowBackendConfig{
		Kind:    cowBackendReflink,
		Reflink: CowReflinkBackendConfig{RootDir: &root, CloneMethod: &method},
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := cubecow.InitWithoutLoggingFromJSON(string(payload))
	if err != nil {
		t.Fatalf("init cubecow at %s: %v", root, err)
	}
	t.Cleanup(e.Close)
	return e
}

func newHybridEnv(t *testing.T) *hybridEnv {
	dir := reflinkTestDir(t)
	env := &hybridEnv{t: t, dir: dir, homes: filepath.Join(dir, "jfs", "pause-snapshots"),
		catalog: map[string]*SnapshotCatalogEntry{}}
	env.sharedEngine = newTestReflinkEngine(t, filepath.Join(dir, "jfs", "objects"), "copy_file_range")
	env.shared = newCowVolumeManager(env.sharedEngine)
	return env
}

func (env *hybridEnv) node(name string) *hybridNode {
	t := env.t
	e := newTestReflinkEngine(t, filepath.Join(env.dir, name, "xfs", "objects"), "ficlone")
	h := &jfsHybridStore{
		shared:       env.shared,
		local:        newCowVolumeManager(e),
		localEngine:  e,
		sharedEngine: env.sharedEngine,
		localRoot:    filepath.Join(env.dir, name, "xfs", jfsLocalDirName),
		publishSem:   make(chan struct{}, 1),
		packageEntry: func(_ context.Context, id string) (*SnapshotCatalogEntry, error) {
			if e, ok := env.catalog[id]; ok {
				return e, nil
			}
			return nil, ErrSnapshotCatalogNotFound
		},
	}
	for _, d := range []string{h.replicaDir(), h.tmpDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &hybridNode{h: h, engine: e}
}

// template creates a JuiceFS template rootfs tpl-<id>-rootfs: a mostly empty
// 64 MiB image with some data and an allocated all-zero region.
func (env *hybridEnv) template(id string) {
	t := env.t
	ctx := context.Background()
	if _, err := env.shared.CreateTemplateBuildRootfs(ctx, id, 64<<20); err != nil {
		// CreateTemplateBuildRootfs formats ext4; fall back to a raw volume
		// when mkfs is unavailable in the test environment.
		if _, err2 := env.sharedEngine.CreateVolume(cowTemplateBuildRootfsName(id), 64<<20); err2 != nil {
			t.Fatalf("create build rootfs: %v / %v", err, err2)
		}
	}
	info, err := env.sharedEngine.GetObjectInfo(cowTemplateBuildRootfsName(id))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(info.DevicePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3<<20)
	rand.New(rand.NewSource(1)).Read(data)
	f.WriteAt(data, 16<<20)
	f.WriteAt(make([]byte, 2<<20), 40<<20)
	f.Sync()
	f.Close()
	if _, err := env.shared.CommitTemplateRootfs(ctx, cowTemplateBuildRootfsName(id), id); err != nil {
		t.Fatal(err)
	}
}

// home creates the JuiceFS package home + catalog entry for snapshot id.
func (env *hybridEnv) home(id string) string {
	home := filepath.Join(env.homes, id)
	if err := os.MkdirAll(filepath.Join(home, SnapshotMetadataDir), 0o755); err != nil {
		env.t.Fatal(err)
	}
	env.catalog[id] = &SnapshotCatalogEntry{SnapshotID: id, SnapshotPath: home,
		RootfsVol: cowTemplateRootfsName(id), RootfsKind: cowKindSnapshot}
	return home
}

func objPath(t *testing.T, e *cubecow.Engine, name string) string {
	t.Helper()
	info, err := e.GetObjectInfo(name)
	if err != nil {
		t.Fatalf("object %s: %v", name, err)
	}
	return info.DevicePath
}

func fileSum(t *testing.T, p string) [32]byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func scribble(t *testing.T, p string, seed int64, n int) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	rnd := rand.New(rand.NewSource(seed))
	blk := make([]byte, 4096)
	for i := 0; i < n; i++ {
		rnd.Read(blk)
		f.WriteAt(blk, int64(rnd.Intn(int(st.Size()/4096)))*4096)
	}
	f.Sync()
}

func TestJFSHybridLifecycle(t *testing.T) {
	env := newHybridEnv(t)
	ctx := context.Background()
	const tpl = "tpl0"
	env.template(tpl)
	a, b := env.node("a"), env.node("b")
	tplName := cowTemplateRootfsName(tpl)

	// Step 2: sandbox rootfs on A is a local clone of a sparse local replica.
	sb, err := a.h.CreateSandboxRootfsFromTemplate(ctx, "sb1", tpl, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sb.FilePath, filepath.Join(env.dir, "a")) {
		t.Fatalf("sandbox rootfs %s is not on node-local storage", sb.FilePath)
	}
	if fileSum(t, objPath(t, a.engine, tplName)) != fileSum(t, objPath(t, env.sharedEngine, tplName)) {
		t.Fatal("local replica content differs from the JuiceFS template")
	}
	rec, err := a.h.readReplica(tplName)
	// 3 MiB of data plus any ext4 metadata; the 2 MiB allocated zero region
	// is read but not written.
	if err != nil || rec.DataBytes < 3<<20 || rec.DataBytes+(2<<20) > rec.ReadBytes || rec.DataBytes > 8<<20 {
		t.Fatalf("replica record %+v err %v (want ~3 MiB data, zero region skipped)", rec, err)
	}
	if a.h.Name() != env.shared.Name() {
		t.Fatal("hybrid store must report the jfs store name")
	}

	// Step 3: pause commits locally; publish writes the diff into the home.
	scribble(t, sb.FilePath, 7, 400)
	const snap = "snap1"
	home := env.home(snap)
	meta := filepath.Join(home, SnapshotMetadataDir)
	pkg, err := a.h.CommitTemplateRootfs(ctx, sb.VolumeName, snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pkg.FilePath, filepath.Join(env.dir, "a")) {
		t.Fatalf("package rootfs %s should be committed locally", pkg.FilePath)
	}
	if _, err := env.sharedEngine.GetObjectInfo(pkg.VolumeName); err == nil {
		t.Fatal("package rootfs must not be on JuiceFS")
	}
	scribble(t, sb.FilePath, 8, 100) // after pause; must not leak into the package
	pkgSum := fileSum(t, pkg.FilePath)
	if jfsRootfsPublishedIn(meta) {
		t.Fatal("published before publish ran")
	}
	if err := a.h.publish(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if !jfsRootfsPublishedIn(meta) {
		t.Fatal("published marker missing")
	}
	var prec jfsPublishRecord
	if raw, err := os.ReadFile(filepath.Join(meta, jfsRootfsPublished)); err != nil || json.Unmarshal(raw, &prec) != nil {
		t.Fatalf("read publish record: %v", err)
	}
	if prec.LiveSourceBytes == 0 || prec.LiveSourceBytes >= prec.BlobBytes {
		t.Fatalf("live source served %d of %d bytes; want most but not all (sandbox kept writing after the pause)",
			prec.LiveSourceBytes, prec.BlobBytes)
	}
	st, _ := os.Stat(filepath.Join(meta, jfsRootfsDiffBlob))
	if st.Size() == 0 || st.Size() > 400*4096 {
		t.Fatalf("blob size %d, want a small diff", st.Size())
	}
	if err := a.h.publish(ctx, snap); err != nil {
		t.Fatal("publish must be idempotent:", err)
	}

	// Local resume on A clones the local package.
	sbA2, err := a.h.CreateSandboxRootfsFromTemplate(ctx, "sb1r", snap, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fileSum(t, sbA2.FilePath) != pkgSum {
		t.Fatal("local resume content differs")
	}

	// Step 4: B imports from the replica of the same template + published diff.
	sbB, err := b.h.CreateSandboxRootfsFromTemplate(ctx, "sb1x", snap, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sbB.FilePath, filepath.Join(env.dir, "b")) {
		t.Fatalf("cross-node rootfs %s is not on B", sbB.FilePath)
	}
	if fileSum(t, objPath(t, b.engine, cowTemplateRootfsName(snap))) != pkgSum {
		t.Fatal("imported package differs from the origin package")
	}
	if fileSum(t, sbB.FilePath) != pkgSum {
		t.Fatal("cross-node sandbox rootfs differs from the origin package")
	}

	// Second generation on B: pause again, publish relative to B's replica,
	// import back on A.
	scribble(t, sbB.FilePath, 9, 300)
	const snap2 = "snap2"
	env.home(snap2)
	pkg2, err := b.h.CommitTemplateRootfs(ctx, sbB.VolumeName, snap2)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.h.publish(ctx, snap2); err != nil {
		t.Fatal(err)
	}
	m2, err := readManifestFor(env.catalog[snap2])
	if err != nil || m2.Base != tplName {
		t.Fatalf("gen-2 manifest base %q err %v, want %q", m2.Base, err, tplName)
	}
	sbA3, err := a.h.CreateSandboxRootfsFromTemplate(ctx, "sb2x", snap2, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fileSum(t, sbA3.FilePath) != fileSum(t, pkg2.FilePath) {
		t.Fatal("gen-2 import differs")
	}

	// Deleting a package removes its local rootfs; JuiceFS is untouched.
	if err := b.h.DeleteByKind(ctx, cowTemplateRootfsName(snap2), cowKindSnapshot); err != nil {
		t.Fatal(err)
	}
	if b.h.hasLocal(cowTemplateRootfsName(snap2)) {
		t.Fatal("package rootfs not deleted")
	}

	// Sweep: a package whose home is gone is deleted after the grace period;
	// an unpublished package whose home exists is published.
	const snap3 = "snap3"
	env.home(snap3)
	sb3, _ := a.h.CreateSandboxRootfsFromTemplate(ctx, "sb3", tpl, 0, 0)
	scribble(t, sb3.FilePath, 10, 10)
	if _, err := a.h.CommitTemplateRootfs(ctx, sb3.VolumeName, snap3); err != nil {
		t.Fatal(err)
	}
	delete(env.catalog, snap) // snap1 deleted elsewhere (e.g. after cross-node resume)
	old := time.Now().Add(-2 * jfsLocalGCGrace)
	os.Chtimes(objPath(t, a.engine, cowTemplateRootfsName(snap)), old, old)
	a.h.sweep(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for !jfsRootfsPublishedIn(filepath.Join(env.catalog[snap3].SnapshotPath, SnapshotMetadataDir)) {
		if time.Now().After(deadline) {
			t.Fatal("sweep did not publish the pending package")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if a.h.hasLocal(cowTemplateRootfsName(snap)) {
		t.Fatal("sweep did not delete the orphan package rootfs")
	}
}

func TestJFSHybridStaleReplicaAndGrace(t *testing.T) {
	env := newHybridEnv(t)
	ctx := context.Background()
	env.template("tplS")
	a := env.node("a")
	name := cowTemplateRootfsName("tplS")
	if err := a.h.ensureLocalRootfs(ctx, name); err != nil {
		t.Fatal(err)
	}
	// Rebuild the template under the same name with different content.
	if err := env.shared.DeleteByKind(ctx, name, cowKindSnapshot); err != nil {
		t.Fatal(err)
	}
	p := objPath(t, env.sharedEngine, cowTemplateBuildRootfsName("tplS"))
	f, _ := os.OpenFile(p, os.O_WRONLY, 0)
	f.WriteAt(bytes.Repeat([]byte{7}, 4096), 0)
	f.Sync()
	f.Close()
	time.Sleep(10 * time.Millisecond)
	if _, err := env.shared.CommitTemplateRootfs(ctx, cowTemplateBuildRootfsName("tplS"), "tplS"); err != nil {
		t.Fatal(err)
	}
	if err := a.h.ensureLocalRootfs(ctx, name); err != nil {
		t.Fatal(err)
	}
	if fileSum(t, objPath(t, a.engine, name)) != fileSum(t, objPath(t, env.sharedEngine, name)) {
		t.Fatal("stale replica was not refreshed")
	}

	// A fresh unpublished package with no home survives the sweep (grace).
	sb, _ := a.h.CreateSandboxRootfsFromTemplate(ctx, "sbg", "tplS", 0, 0)
	if _, err := a.h.CommitTemplateRootfs(ctx, sb.VolumeName, "fresh"); err != nil {
		t.Fatal(err)
	}
	a.h.sweep(ctx)
	if !a.h.hasLocal(cowTemplateRootfsName("fresh")) {
		t.Fatal("sweep deleted a package still inside the grace period")
	}
	// A cross-node import of an unpublished, unknown package fails cleanly.
	b := env.node("b")
	if _, err := b.h.CreateSandboxRootfsFromTemplate(ctx, "x", "fresh", 0, 0); err == nil {
		t.Fatal("expected failure importing an unpublished package")
	}
}

func readManifestFor(e *SnapshotCatalogEntry) (*jfsdiff.Manifest, error) {
	return jfsdiff.ReadManifest(filepath.Join(e.SnapshotPath, SnapshotMetadataDir, jfsRootfsDiffManifest))
}
