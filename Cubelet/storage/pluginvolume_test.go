// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import "testing"

func TestLookupPluginVolumeSource(t *testing.T) {
	t.Parallel()

	raw := `[{"name":"vol-a","driver":"cos","private_data":"volumes/vol-a/"},{"name":"vol-b","driver":"cos-rpc"}]`
	annotations := map[string]string{"plugin-volume-sources": raw}

	driver, pd, ok := lookupPluginVolumeSource(annotations, "vol-a")
	if !ok || driver != "cos" || pd != "volumes/vol-a/" {
		t.Fatalf("vol-a: driver=%q private_data=%q ok=%v", driver, pd, ok)
	}
	driver, pd, ok = lookupPluginVolumeSource(annotations, "vol-b")
	if !ok || driver != "cos-rpc" || pd != "" {
		t.Fatalf("vol-b: driver=%q private_data=%q ok=%v", driver, pd, ok)
	}
	if _, _, ok := lookupPluginVolumeSource(annotations, "missing"); ok {
		t.Fatal("expected ok=false for missing volume")
	}
	if _, _, ok := lookupPluginVolumeSource(nil, "vol-a"); ok {
		t.Fatal("expected ok=false for nil annotations")
	}
	if _, _, ok := lookupPluginVolumeSource(map[string]string{"plugin-volume-sources": "{bad"}, "vol-a"); ok {
		t.Fatal("expected ok=false for malformed JSON")
	}
}

func TestIsPluginVolume(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		"plugin-volume-sources": `[{"name":"vol-a","driver":"cos","private_data":"x"}]`,
	}
	if !isPluginVolume(annotations, "vol-a") {
		t.Fatal("expected vol-a to be a plugin volume")
	}
	if isPluginVolume(annotations, "vol-b") {
		t.Fatal("expected vol-b not to be a plugin volume")
	}
}
