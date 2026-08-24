// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// SandboxImport materializes the disks a sandbox runs on when Master placed
// it on a node that holds no copy of the template, snapshot or pause package
// it restores from.
//
// Every disk lands under a name derived from the sandbox id, so a cross-node
// sandbox owns what it runs on exactly like a same-node one owns the clones
// it makes off the local package: destroy deletes by those names, and no
// later create is left short of something this one consumed. import_lvol
// already yields a RW volume — what cloning would have produced — so the
// import writes straight onto the final name and nothing has to be moved,
// promoted or sealed afterwards.
//
// Deliberately nothing package-shaped is built here. A package is the static
// set of snapshot objects belonging to the node that created it, and a node
// that merely runs a restore has no owner for one: Master never learns the
// copy exists, sandbox destroy must not take shared state with it, and
// Cubelet has no package refcount — so it would sit there forever. The
// description a package carries (sandbox_spec.json and the run-template
// config) is not lost by skipping it: the metadata disk imported below is a
// byte copy of the package's own, and the description travels on it.
type SandboxImport struct {
	SandboxID string
	UUIDs     *cow.RemoteUUIDs
}

// CrossNodeSandboxImport reports the imports a create owes, or nil when this
// node can derive the sandbox's disks locally.
//
// Everything it needs is in the request, so each step of a create can ask
// again instead of passing state along, and every step names the same disks.
// The sandbox id is read from the request rather than taken as an argument
// because the create workflow runs id assignment in parallel with the step
// that first needs these disks; the id is settled before the workflow starts
// (see prepareCrossNodeRestore in the cubebox service).
//
// Master sends remote_uuids on every S3 restore, same-node ones included, so
// uuids alone only say where the package could be fetched from. cross_node is
// Master's verdict that it placed this restore off the package's node, and
// only that verdict turns clones into imports.
func CrossNodeSandboxImport(ann map[string]string) *SandboxImport {
	if len(ann) == 0 {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(ann[constants.MasterAnnotationSnapshotCrossNode]), "true") {
		return nil
	}
	raw := strings.TrimSpace(ann[constants.MasterAnnotationStorageBackend])
	if raw == "" {
		return nil
	}
	if backend, err := cow.NormalizeBackend(raw); err != nil || backend != cow.BackendS3 {
		return nil
	}
	id := strings.TrimSpace(ann[constants.MasterAnnotationDesiredSandboxID])
	if id == "" {
		return nil
	}
	uuids := cow.ParseRemoteUUIDs(ann[constants.MasterAnnotationSnapshotRemoteUUIDs])
	if uuids.Empty() {
		return nil
	}
	return &SandboxImport{SandboxID: id, UUIDs: uuids}
}

// SandboxObjectNamePrefix is what every disk minted for a sandbox is named
// after. Ownership is decided on this prefix, so a disk that belongs to a
// sandbox must be named through the helpers below and nothing else may be.
func SandboxObjectNamePrefix(sandboxID string) string {
	return fmt.Sprintf("sb-%s-", strings.TrimSpace(sandboxID))
}

// SandboxRootfsName is the private rootfs a sandbox runs on, however it was
// produced: cloned from a local package, or imported from S3 on a node that
// has none. The name is what destroy keys on.
func SandboxRootfsName(sandboxID string, gen uint32) string {
	return fmt.Sprintf("%srootfs-gen%d", SandboxObjectNamePrefix(sandboxID), gen)
}

// SandboxMemoryName is the private memory image a cross-node restore imports.
// A same-node restore reads the package's own memory snapshot instead and
// must never be given one of these: that disk belongs to the template or
// snapshot and outlives every sandbox started from it.
func SandboxMemoryName(sandboxID string) string {
	return SandboxObjectNamePrefix(sandboxID) + "memory"
}

// MetadataDir is where the sandbox's metadata disk is mounted. It doubles as
// the directory the restore reads its description from, which is why it is
// laid out like a package home even though no package lives on this node.
func (s *SandboxImport) MetadataDir() string {
	return SnapshotMetaDir(cow.BackendS3, SnapshotKindNormal, s.SandboxID)
}

// EnsureMetadata imports the metadata disk under the sandbox's own name and
// mounts it, returning the directory.
//
// Idempotent by design, because the two restore paths reach it at different
// times: a Resume must read sandbox_spec.json before the thin request can
// even be expanded, so it lands at the Create entry, while a create from a
// snapshot does not know its sandbox id until createid has run and gets here
// on the way to the run template.
func (s *SandboxImport) EnsureMetadata(ctx context.Context) (string, error) {
	uuid := strings.TrimSpace(s.UUIDs.Metadata)
	if uuid == "" {
		// Packages exported before metadata joined the payload cannot be
		// restored elsewhere at all: the spec and the kernel／image config
		// have no other carrier.
		return "", fmt.Errorf("cross-node restore of sandbox %s carries no metadata export", s.SandboxID)
	}
	if _, err := s.importOne(ctx, S3MetadataVolumeName(s.SandboxID), "metadata", uuid); err != nil {
		return "", err
	}
	dir := s.MetadataDir()
	if err := MountS3MetadataAt(ctx, cow.BackendS3, s.SandboxID, dir); err != nil {
		return "", fmt.Errorf("mount imported metadata for sandbox %s: %w", s.SandboxID, err)
	}
	return dir, nil
}

// Rootfs imports the package rootfs as this sandbox's disk, returning the
// cubecow name it landed under and its device path.
func (s *SandboxImport) Rootfs(ctx context.Context, gen uint32) (string, string, error) {
	uuid := strings.TrimSpace(s.UUIDs.Rootfs)
	if uuid == "" {
		return "", "", fmt.Errorf("cross-node restore of sandbox %s carries no rootfs export", s.SandboxID)
	}
	name := SandboxRootfsName(s.SandboxID, gen)
	dev, err := s.importOne(ctx, name, "rootfs", uuid)
	if err != nil {
		return "", "", err
	}
	return name, dev, nil
}

// Memory imports the package memory image as this sandbox's disk. A
// rootfs-only package has none, and reports an empty name rather than an
// error so the caller can cold-start.
func (s *SandboxImport) Memory(ctx context.Context) (string, string, error) {
	uuid := strings.TrimSpace(s.UUIDs.Memory)
	if uuid == "" {
		return "", "", nil
	}
	name := SandboxMemoryName(s.SandboxID)
	dev, err := s.importOne(ctx, name, "memory", uuid)
	if err != nil {
		return "", "", err
	}
	return name, dev, nil
}

// importOne materializes one exported object under name and opens its block
// device. A retried Create finds its own leftover and reuses it; the names
// are unique to this sandbox, so nothing else can have put one there.
//
// Unlike [S3Cow.fetchOne] this does not accept "the object is here now" as
// proof that a failed import succeeded. That tolerance exists for package
// objects, where two creates may import the same name concurrently and one
// legitimately loses the race. Here a failure means the object found is the
// empty shell s3lvol leaves behind, and mounting it reports a corrupt
// filesystem several steps later.
func (s *SandboxImport) importOne(ctx context.Context, name, role, uuid string) (string, error) {
	if IsS3MetadataBaseName(name) {
		return "", fmt.Errorf("refusing to import %s onto the node-local s3 metadata base", role)
	}
	store, err := requireS3Cow()
	if err != nil {
		return "", err
	}
	if store == nil {
		return "", fmt.Errorf("s3 cow store is not initialized")
	}
	importer, ok := store.engine.(cowVolumeImporter)
	if !ok || importer == nil {
		return "", fmt.Errorf("cubecow engine cannot import %s %s", role, name)
	}
	present, err := cowObjectPresent(store.GetVolumeInfo(ctx, name))
	if err != nil {
		return "", fmt.Errorf("lookup %s %s: %w", role, name, err)
	}
	if !present {
		if _, err := importer.ImportLvol(name, uuid); err != nil && !isCowSemantic(err, cubecow.SemAlreadyExists) {
			return "", fmt.Errorf("import %s %s (remote_uuid=%s): %w", role, name, uuid, err)
		}
	}
	if _, err := store.engine.ActivateVolume(name); err != nil && !isCowSemantic(err, cubecow.SemAlreadyExists) {
		return "", fmt.Errorf("activate imported %s %s: %w", role, name, err)
	}
	dev, err := store.ResolveDevPath(ctx, name, cowKindVolume)
	if err != nil {
		return "", fmt.Errorf("resolve imported %s %s: %w", role, name, err)
	}
	return dev, nil
}
