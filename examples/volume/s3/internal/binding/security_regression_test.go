package binding

import "testing"

func TestDynamicRequiresCredentialReference(t *testing.T) {
	for _, raw := range []string{`{"bucket":"tenant"}`, `{"prefix":"tenant/"}`, `{"bucket":"tenant","prefix":"tenant/"}`} {
		if _, err := ParseCreate(raw, "vol-1"); err == nil {
			t.Errorf("ParseCreate(%s) accepted a dynamic binding without cred_ref", raw)
		}
	}
	if _, err := ParseState(`{"v":1,"bucket":"tenant","prefix":"tenant/"}`, "vol-1"); err == nil {
		t.Error("ParseState accepted dynamic state without cred_ref")
	}
	if _, err := EncodeState(Binding{Bucket: "tenant", Prefix: "tenant/", Dynamic: true}); err == nil {
		t.Error("EncodeState accepted dynamic state without cred_ref")
	}
}

func TestLegacyStateCannotSelectAnotherPrefix(t *testing.T) {
	for _, raw := range []string{"volumes/vol-2/", `"volumes/vol-2/"`, "workspaces/other/"} {
		if _, err := ParseState(raw, "vol-1"); err == nil {
			t.Errorf("ParseState(%q) accepted a different prefix", raw)
		}
	}
}
