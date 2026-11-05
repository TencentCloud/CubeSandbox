// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube_egress_ca

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBakeReportsRuntimeBundle(t *testing.T) {
	for _, bundle := range []string{
		"etc/ssl/certs/ca-certificates.crt",
		"etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"etc/pki/tls/certs/ca-bundle.crt",
	} {
		t.Run(bundle, func(t *testing.T) {
			root := t.TempDir()
			ca, _ := makeCA(t, "cube")
			public, _ := makeCA(t, "existing-public-root")
			mustWrite(t, root, bundle, string(public))
			for attempt := 0; attempt < 2; attempt++ {
				res, err := Bake(root, ca)
				if err != nil || res.BundlePath != "/"+bundle {
					t.Fatalf("attempt=%d result=%+v err=%v", attempt, res, err)
				}
				contents := mustReadFile(t, filepath.Join(root, bundle))
				if !strings.Contains(contents, string(public)) || strings.Count(contents, string(ca)) != 1 {
					t.Fatal("bundle lost its original trust roots or duplicated the new CA")
				}
			}
		})
	}
}

func TestBakeSeedsRuntimeBundleForAnchorOnlyImage(t *testing.T) {
	root := t.TempDir()
	ca, _ := makeCA(t, "cube")
	mustWrite(t, root, "etc/pki/ca-trust/source/anchors/"+AnchorFileName, string(ca))
	res, err := Bake(root, ca)
	if err != nil || !res.Seeded || res.BundlePath != "/etc/ssl/certs/ca-certificates.crt" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if got := mustReadFile(t, filepath.Join(root, seedBundlePath)); got != string(ca) {
		t.Fatal("seeded runtime bundle does not contain the requested CA")
	}
}
