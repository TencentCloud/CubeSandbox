// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetachUsesRecordedMount(t *testing.T) {
	for _, scenario := range []string{"mounted", "absent", "legacy-source", "references", "bad-json", "bad-base", "bad-source", "verify-failure", "unmount-failure"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			const id, namespace = "session-1", "test"
			target, err := mountPath(base, namespace, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			source := "server:/exports/" + id
			if scenario == "legacy-source" {
				source += "/data"
			}
			meta := map[string]string{"base_dir": base, "source": source}
			if scenario == "bad-base" {
				meta["base_dir"] = "relative"
			}
			if scenario == "bad-source" {
				meta["source"] = "server:/exports/other"
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			o := options{id: id, namespace: namespace, baseDir: filepath.Join(base, "unused"), metadata: string(encoded)}
			if scenario == "bad-json" || scenario == "references" {
				o.metadata = "invalid"
			}
			if scenario == "references" {
				o.refCount = 1
			}
			verified, unmounted := false, false
			verify := func(gotTarget, gotSource string) (bool, error) {
				verified = true
				if gotTarget != target || gotSource != source {
					t.Fatalf("unexpected mount: %q %q", gotTarget, gotSource)
				}
				if scenario == "verify-failure" {
					return false, errors.New("mount mismatch")
				}
				return scenario != "absent", nil
			}
			unmount := func(_ context.Context, binary string, args ...string) error {
				unmounted = true
				if binary != "umount" || !reflect.DeepEqual(args, []string{"-i", "--", target}) {
					t.Fatalf("unexpected unmount: %s %v", binary, args)
				}
				if scenario == "unmount-failure" {
					return errors.New("busy")
				}
				return nil
			}
			err = detachWith(t.Context(), o, verify, unmount)
			wantError := scenario == "bad-json" || scenario == "bad-base" || scenario == "bad-source" || scenario == "verify-failure" || scenario == "unmount-failure"
			if (err != nil) != wantError {
				t.Fatalf("detach error = %v, want error = %t", err, wantError)
			}
			wantVerify := scenario != "references" && scenario != "bad-json" && scenario != "bad-base" && scenario != "bad-source"
			wantUnmount := wantVerify && scenario != "absent" && scenario != "verify-failure"
			if verified != wantVerify || unmounted != wantUnmount {
				t.Fatalf("verified=%t unmounted=%t", verified, unmounted)
			}
			_, statErr := os.Stat(target)
			wantRemoved := !wantError && scenario != "references"
			if wantRemoved && !errors.Is(statErr, os.ErrNotExist) || !wantRemoved && statErr != nil {
				t.Fatalf("mountpoint removed=%t, stat error=%v", wantRemoved, statErr)
			}
		})
	}
}

func TestMatchMountRejectsDuplicateMounts(t *testing.T) {
	const line = "12 1 0:20 / /volumes/nfs rw - nfs server:/export/session-1 rw,vers=3\n"
	if found, err := matchMount(line+line, "/volumes/nfs", "server:/export/session-1"); err == nil || found {
		t.Fatalf("duplicate mount accepted: found=%t err=%v", found, err)
	}
}
