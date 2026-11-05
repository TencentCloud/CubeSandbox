// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package binding

import (
	"strings"
	"testing"
)

func TestNormalizePrefix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		want  string
		error string
	}{
		{name: "adds trailing slash", raw: "workspaces/user-a", want: "workspaces/user-a/"},
		{name: "keeps trailing slash", raw: "workspaces/user-a/", want: "workspaces/user-a/"},
		{name: "strips leading slash", raw: "/workspaces/user-a/", want: "workspaces/user-a/"},
		{name: "unicode key", raw: "团队/用户-a/", want: "团队/用户-a/"},
		{name: "empty", raw: "", error: "empty"},
		{name: "only slashes", raw: "///", error: "empty"},
		{name: "backslash", raw: `a\b/`, error: "backslash"},
		{name: "dot segment", raw: "a/./b/", error: `"."`},
		{name: "dotdot segment", raw: "a/../b/", error: `".."`},
		{name: "empty segment", raw: "a//b/", error: "empty path segment"},
		{name: "control character", raw: "a/\x01/", error: "control"},
		{name: "plugin namespace", raw: ".cube-sandbox/x/", error: "reserved"},
		{name: "legacy root", raw: "volumes/", error: "plugin-managed"},
		{name: "legacy nested", raw: "volumes/user-a/", error: "plugin-managed"},
		{name: "too long", raw: strings.Repeat("a", maxPrefixLen+1), error: "longer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePrefix(tc.raw)
			if tc.error != "" {
				if err == nil {
					t.Fatalf("NormalizePrefix(%q) = %q, want error", tc.raw, got)
				}
				if !strings.Contains(err.Error(), tc.error) {
					t.Errorf("error = %q, want it to mention %q", err, tc.error)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizePrefix(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("NormalizePrefix(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseCreateLegacy(t *testing.T) {
	for _, raw := range []string{"", "   ", "{}", "null"} {
		req, err := ParseCreate(raw, "vol-1")
		if err != nil {
			t.Fatalf("ParseCreate(%q): %v", raw, err)
		}
		if req.Dynamic {
			t.Errorf("ParseCreate(%q).Dynamic = true, want false", raw)
		}
		if req.Prefix != "volumes/vol-1/" {
			t.Errorf("ParseCreate(%q).Prefix = %q, want volumes/vol-1/", raw, req.Prefix)
		}
		if req.Bucket != "" || req.CredRef != "" {
			t.Errorf("ParseCreate(%q) = %+v, want empty bucket/ref", raw, req)
		}
	}
}

func TestParseCreateDynamic(t *testing.T) {
	req, err := ParseCreate(`{"bucket":"user-bucket","prefix":"/workspaces/user-a","cred_ref":"team-a"}`, "vol-1")
	if err != nil {
		t.Fatalf("ParseCreate: %v", err)
	}
	if !req.Dynamic {
		t.Error("Dynamic = false, want true")
	}
	if req.Bucket != "user-bucket" {
		t.Errorf("Bucket = %q, want user-bucket", req.Bucket)
	}
	if req.Prefix != "workspaces/user-a/" {
		t.Errorf("Prefix = %q, want workspaces/user-a/", req.Prefix)
	}
	if req.CredRef != "team-a" {
		t.Errorf("CredRef = %q, want team-a", req.CredRef)
	}
}

func TestParseCreateDefaultsMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		wantPrefix string
	}{
		{"ref only", `{"cred_ref":"team-a"}`, "volumes/vol-1/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseCreate(tc.raw, "vol-1")
			if err != nil {
				t.Fatalf("ParseCreate: %v", err)
			}
			if !req.Dynamic {
				t.Error("Dynamic = false, want true")
			}
			if req.Prefix != tc.wantPrefix {
				t.Errorf("Prefix = %q, want %q", req.Prefix, tc.wantPrefix)
			}
		})
	}
}

func TestParseCreateRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"invalid json", `bucket=user-bucket`, "JSON object"},
		{"array", `["a"]`, "JSON object"},
		{"bucket only requires reference", `{"bucket":"user-bucket"}`, "requires cred_ref"},
		{"prefix only requires reference", `{"prefix":"workspaces/user-a/"}`, "requires cred_ref"},
		{"bucket and prefix require reference", `{"bucket":"user-bucket","prefix":"workspaces/user-a/"}`, "requires cred_ref"},
		{"invalid bucket", `{"bucket":"bad/bucket","cred_ref":"team-a"}`, "unsupported character"},
		{"bucket traversal", `{"bucket":"..","cred_ref":"team-a"}`, "at least 3"},
		{"invalid prefix", `{"prefix":"a/../b/","cred_ref":"team-a"}`, `".."`},
		{"invalid ref", `{"cred_ref":"../escape"}`, "must not contain .."},
		{"oversize", `{"prefix":"` + strings.Repeat("a", MaxPrivateDataLen) + `","cred_ref":"team-a"}`, "over the"},
		{"nul", "{\"bucket\":\"abc\",\"prefix\":\"a\\u0000b/\",\"cred_ref\":\"team-a\"}", "control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCreate(tc.raw, "vol-1")
			if err == nil {
				t.Fatal("ParseCreate succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseCreateRejectsBadVolumeID(t *testing.T) {
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, "a b"} {
		if _, err := ParseCreate("", id); err == nil {
			t.Errorf("ParseCreate with volume id %q succeeded, want error", id)
		}
	}
}

func TestParseStateLegacy(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"plain prefix", "volumes/vol-1/"},
		{"plain prefix without slash", "volumes/vol-1"},
		{"json string prefix", `"volumes/vol-1/"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ParseState(tc.raw, "vol-1")
			if err != nil {
				t.Fatalf("ParseState: %v", err)
			}
			if b.Dynamic {
				t.Error("Dynamic = true, want false")
			}
			if b.Prefix != "volumes/vol-1/" {
				t.Errorf("Prefix = %q, want volumes/vol-1/", b.Prefix)
			}
			if b.Bucket != "" || b.CredRef != "" {
				t.Errorf("Binding = %+v, want empty bucket/ref", b)
			}
		})
	}
}

func TestParseStateDynamic(t *testing.T) {
	raw := `{"v":1,"bucket":"user-bucket","prefix":"workspaces/user-a/","cred_ref":"team-a"}`
	b, err := ParseState(raw, "vol-1")
	if err != nil {
		t.Fatalf("ParseState: %v", err)
	}
	if !b.Dynamic || b.Bucket != "user-bucket" || b.Prefix != "workspaces/user-a/" || b.CredRef != "team-a" {
		t.Errorf("Binding = %+v, want the encoded state", b)
	}
}

func TestParseStateRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"unknown version", `{"v":2,"bucket":"bcd","prefix":"a/"}`, "unsupported state version"},
		{"missing version", `{"bucket":"bcd","prefix":"a/"}`, "unsupported state version"},
		{"no bucket", `{"v":1,"prefix":"a/"}`, "missing bucket"},
		{"no prefix", `{"v":1,"bucket":"bcd"}`, "missing bucket"},
		{"invalid prefix", `{"v":1,"bucket":"bcd","prefix":"../a/","cred_ref":"team-a"}`, `".."`},
		{"reserved prefix", `{"v":1,"bucket":"bcd","prefix":".cube-sandbox/a/","cred_ref":"team-a"}`, "reserved"},
		{"legacy namespace", `{"v":1,"bucket":"bcd","prefix":"volumes/x/","cred_ref":"team-a"}`, "plugin-managed"},
		{"missing cred_ref", `{"v":1,"bucket":"bcd","prefix":"a/"}`, "requires cred_ref"},
		{"invalid ref", `{"v":1,"bucket":"bcd","prefix":"a/","cred_ref":"a/b"}`, "unsupported character"},
		{"malformed object", `{not json`, "recognized volume state"},
		{"unsafe legacy prefix", "volumes/../x/", "not a recognized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseState(tc.raw, "vol-1")
			if err == nil {
				t.Fatal("ParseState succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestEncodeStateRoundTrip(t *testing.T) {
	in := Binding{Bucket: "user-bucket", Prefix: "workspaces/user-a/", CredRef: "team-a", Dynamic: true}
	raw, err := EncodeState(in)
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	out, err := ParseState(raw, "vol-1")
	if err != nil {
		t.Fatalf("ParseState: %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestEncodeStateOversize(t *testing.T) {
	_, err := EncodeState(Binding{Bucket: "bcd", Prefix: strings.Repeat("a", MaxPrivateDataLen) + "/", CredRef: "team-a"})
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("EncodeState error = %v, want the size limit", err)
	}
}

// A dynamic create that omits prefix falls back to volumes/<volume_id>/. The
// state it writes must parse back: Create, Attach and Destroy all use the same
// rules or the volume is born unmountable.
func TestDynamicDefaultPrefixRoundTrip(t *testing.T) {
	req, err := ParseCreate(`{"bucket":"user-bucket","cred_ref":"team-a"}`, "vol-1")
	if err != nil {
		t.Fatalf("ParseCreate: %v", err)
	}
	if req.Prefix != "volumes/vol-1/" {
		t.Fatalf("Prefix = %q, want the legacy default", req.Prefix)
	}
	raw, err := EncodeState(Binding{Bucket: req.Bucket, Prefix: req.Prefix, CredRef: req.CredRef, Dynamic: true})
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	b, err := ParseState(raw, "vol-1")
	if err != nil {
		t.Fatalf("ParseState rejected the state Create just wrote: %v", err)
	}
	if b.Prefix != req.Prefix || b.Bucket != req.Bucket || b.CredRef != req.CredRef {
		t.Errorf("round trip = %+v, want %+v", b, req)
	}
}

// Only the exact default is allowed back; a persisted state must not be able
// to point a destructive delete at another volume's legacy prefix.
func TestParseStateDefaultPrefixOnlyForOwnVolume(t *testing.T) {
	if _, err := ParseState(`{"v":1,"bucket":"bcd","prefix":"volumes/vol-1/","cred_ref":"team-a"}`, "vol-1"); err != nil {
		t.Errorf("ParseState rejected the volume's own default prefix: %v", err)
	}
	for _, prefix := range []string{"volumes/vol-2/", "volumes/", "volumes/vol-1/nested/"} {
		raw := `{"v":1,"bucket":"bcd","prefix":"` + prefix + `","cred_ref":"team-a"}`
		if _, err := ParseState(raw, "vol-1"); err == nil {
			t.Errorf("ParseState accepted %q for vol-1", prefix)
		}
	}
}

func TestValidateBucket(t *testing.T) {
	for _, bucket := range []string{"abc", "User-Bucket.1", "my_bucket"} {
		if err := ValidateBucket(bucket); err != nil {
			t.Errorf("ValidateBucket(%q): %v", bucket, err)
		}
	}
	for _, bucket := range []string{"", "ab", "bad/bucket", "bad bucket", strings.Repeat("a", maxBucketLen+1)} {
		if err := ValidateBucket(bucket); err == nil {
			t.Errorf("ValidateBucket(%q) succeeded, want error", bucket)
		}
	}
}

func TestValidateCredRef(t *testing.T) {
	for _, ref := range []string{"team-a", "user.1", "a_b"} {
		if err := ValidateCredRef(ref); err != nil {
			t.Errorf("ValidateCredRef(%q): %v", ref, err)
		}
	}
	for _, ref := range []string{"", ".", "..", "a..b", "-a", "a/b", "a b", strings.Repeat("a", 65)} {
		if err := ValidateCredRef(ref); err == nil {
			t.Errorf("ValidateCredRef(%q) succeeded, want error", ref)
		}
	}
}
