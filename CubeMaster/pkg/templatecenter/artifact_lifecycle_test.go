// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
	"testing"
)

func TestCountArtifactReferencesRejectsLikeWildcards(t *testing.T) {
	for _, artifactID := range []string{"rfs-bad%id", "rfs-bad_id"} {
		_, err := countArtifactReferencesTx(context.Background(), nil, artifactID, "")
		if err == nil {
			t.Fatalf("expected wildcard artifact id %q to be rejected", artifactID)
		}
		if !strings.Contains(err.Error(), "LIKE wildcard") {
			t.Fatalf("unexpected error for %q: %v", artifactID, err)
		}
	}
}
