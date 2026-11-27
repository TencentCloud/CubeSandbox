// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"slices"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

func TestGCRespectsRuleAndRequestPrefixes(t *testing.T) {
	for _, tc := range []struct{ rule, request string }{
		{"logs/", "logs/current/"},
		{"logs/current/", "logs/"},
	} {
		s := New(blobstore.Config{Retention: []blobstore.RetentionRule{{Prefix: tc.rule, ExpireAfter: time.Hour}}})
		now := time.Now()
		for _, k := range []string{"logs/current/a", "logs/current-old/b", "logs-old/c"} {
			s.PutRaw(k, nil, now.Add(-2*time.Hour))
		}
		result, err := s.GC(t.Context(), blobstore.GCOptions{Prefix: tc.request, Now: now})
		if err != nil || result.ExpiredObjects != 1 {
			t.Fatalf("GC(%q, %q): %+v, %v", tc.rule, tc.request, result, err)
		}
		var keys []string
		err = s.List(t.Context(), "logs/", func(obj blobstore.ObjectInfo) error {
			keys = append(keys, obj.Key)
			return nil
		})
		if err != nil || !slices.Equal(keys, []string{"logs/current-old/b"}) {
			t.Fatalf("remaining logs: %v, %v", keys, err)
		}
	}
}
