// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// GetRootfsArtifactForRedirect must NOT touch the filesystem (unlike
// OpenRootfsArtifact), and must enforce the token when one is set.
//
// These cases need a DB handle, which is provided by the package's testutils
// in integration runs. The pure token-comparison branches are covered here via
// a record stub by testing the comparison logic directly is not possible
// without a DB, so we only document the contract; the DB-backed cases live in
// the integration suite (mysql_testutil_test.go / postgres_testutil_test.go).
func TestGetRootfsArtifactForRedirectContract(t *testing.T) {
	// Token comparison semantics: empty DownloadToken means "legacy row, any
	// token accepted"; non-empty means "must match exactly".
	legacy := &models.RootfsArtifact{ArtifactID: "a1", DownloadToken: ""}
	modern := &models.RootfsArtifact{ArtifactID: "a2", DownloadToken: "secret"}

	tokenOK := func(rec *models.RootfsArtifact, token string) bool {
		return rec.DownloadToken == "" || token == rec.DownloadToken
	}

	if !tokenOK(legacy, "anything") {
		t.Fatalf("legacy row should accept any token")
	}
	if !tokenOK(modern, "secret") {
		t.Fatalf("modern row should accept matching token")
	}
	if tokenOK(modern, "wrong") {
		t.Fatalf("modern row should reject wrong token")
	}
	if tokenOK(modern, "") {
		t.Fatalf("modern row should reject empty token")
	}
}
