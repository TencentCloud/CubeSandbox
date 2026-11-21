// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
)

func manifestWith(t *testing.T, snapID, rootfs, memory string, extra ...string) *checkpoint.Manifest {
	t.Helper()
	cat, err := json.Marshal(&SnapshotCatalogEntry{SnapshotID: snapID, Kind: CatalogKindPauseSnapshot,
		RootfsVol: rootfs, MemoryVol: memory, SpecDir: "4C4096M"})
	if err != nil {
		t.Fatal(err)
	}
	m := &checkpoint.Manifest{SnapshotID: snapID, Attributes: map[string]string{xfss3CatalogAttr: string(cat)}}
	for _, v := range append([]string{rootfs, memory}, extra...) {
		m.Files = append(m.Files, checkpoint.File{Role: checkpoint.RoleVolume, Name: v})
	}
	for _, f := range []string{"catalog.json", "metadata.json", "snapshot/config.json", "snapshot/state.json", "sandbox_spec.json"} {
		m.Files = append(m.Files, checkpoint.File{Role: checkpoint.RoleMeta, Name: f})
	}
	return m
}

func TestXFSS3CatalogBindsDiskNamesToTheSnapshot(t *testing.T) {
	const snap = "snap-names0000000000000000001"
	if _, err := xfss3CatalogFromManifest(manifestWith(t, snap, "tpl-"+snap+"-rootfs", "tpl-"+snap+"-memory")); err != nil {
		t.Fatalf("canonical names rejected: %v", err)
	}
	for name, m := range map[string]*checkpoint.Manifest{
		"foreign rootfs":  manifestWith(t, snap, "sb-other-rootfs-gen0", "tpl-"+snap+"-memory"),
		"foreign memory":  manifestWith(t, snap, "tpl-"+snap+"-rootfs", "tpl-tpl-template-memory"),
		"same name twice": manifestWith(t, snap, "tpl-"+snap+"-rootfs", "tpl-"+snap+"-rootfs"),
		"extra object":    manifestWith(t, snap, "tpl-"+snap+"-rootfs", "tpl-"+snap+"-memory", "sb-victim-rootfs-gen0"),
	} {
		if _, err := xfss3CatalogFromManifest(m); !errors.Is(err, checkpoint.ErrCorrupt) {
			t.Errorf("%s: want ErrCorrupt, got %v", name, err)
		}
	}
}

func TestXFSS3ClaimLeftoverReplacesOnlyMarkedObjects(t *testing.T) {
	if replace, err := xfss3ClaimLeftover("tpl-s-rootfs", "s", false, false); replace || err != nil {
		t.Fatalf("absent object: replace=%v err=%v", replace, err)
	}
	if replace, err := xfss3ClaimLeftover("tpl-s-rootfs", "s", true, true); !replace || err != nil {
		t.Fatalf("leftover of a marked import: replace=%v err=%v", replace, err)
	}
	replace, err := xfss3ClaimLeftover("tpl-s-rootfs", "s", true, false)
	if replace || err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("unmarked object must never be replaced: replace=%v err=%v", replace, err)
	}
}
