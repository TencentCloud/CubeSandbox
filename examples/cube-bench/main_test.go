package main

import (
	"encoding/json"
	"testing"
)

func TestPrepareHostMountCompactsValidArray(t *testing.T) {
	got, err := prepareHostMount(`[
		{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}
	]`)
	if err != nil {
		t.Fatalf("prepareHostMount returned error: %v", err)
	}

	want := `[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`
	if got != want {
		t.Fatalf("prepareHostMount=%q, want %q", got, want)
	}
}

func TestPrepareHostMountPreservesCompactedArray(t *testing.T) {
	got, err := prepareHostMount(`[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`)
	if err != nil {
		t.Fatalf("prepareHostMount returned error: %v", err)
	}

	want := `[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`
	if got != want {
		t.Fatalf("prepareHostMount=%q, want %q", got, want)
	}
}

func TestPrepareHostMountRejectsInvalidJSON(t *testing.T) {
	if _, err := prepareHostMount(`[{"hostPath":]`); err == nil {
		t.Fatal("prepareHostMount returned nil error, want invalid JSON error")
	}
}

func TestPrepareHostMountRejectsNonArrayJSON(t *testing.T) {
	if _, err := prepareHostMount(`{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}`); err == nil {
		t.Fatal("prepareHostMount returned nil error, want non-array error")
	}
}

func TestPrepareHostMountAllowsEmptyInput(t *testing.T) {
	got, err := prepareHostMount("")
	if err != nil {
		t.Fatalf("prepareHostMount returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("prepareHostMount returned %q, want empty string", got)
	}
}

func TestPrepareHostMountRejectsEmptyArray(t *testing.T) {
	if _, err := prepareHostMount(`[]`); err == nil {
		t.Fatal("prepareHostMount returned nil error, want empty array error")
	}
}

func TestParseNetworkPolicy(t *testing.T) {
	got, err := parseNetworkPolicy("rules")
	if err != nil {
		t.Fatalf("parseNetworkPolicy(rules): %v", err)
	}
	if got != networkPolicyRules {
		t.Fatalf("got %q, want %q", got, networkPolicyRules)
	}

	got, err = parseNetworkPolicy("")
	if err != nil {
		t.Fatalf("parseNetworkPolicy(\"\"): %v", err)
	}
	if got != networkPolicyNone {
		t.Fatalf("got %q, want %q", got, networkPolicyNone)
	}

	if _, err := parseNetworkPolicy("stress"); err == nil {
		t.Fatal("parseNetworkPolicy(stress) returned nil error, want rejection")
	}
}

func TestBuildCreateRequestBodyNoneOmitsNetwork(t *testing.T) {
	raw, err := buildCreateRequestBody("tpl-1", "", networkPolicyNone)
	if err != nil {
		t.Fatalf("buildCreateRequestBody: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["allow_internet_access"]; ok {
		t.Fatalf("none policy must omit allow_internet_access, got %s", body["allow_internet_access"])
	}
	if _, ok := body["network"]; ok {
		t.Fatalf("none policy must omit network, got %s", body["network"])
	}
}

func TestBuildCreateRequestBodyRulesShape(t *testing.T) {
	raw, err := buildCreateRequestBody("tpl-1", "", networkPolicyRules)
	if err != nil {
		t.Fatalf("buildCreateRequestBody: %v", err)
	}

	var body struct {
		TemplateID          string `json:"templateID"`
		AllowInternetAccess *bool  `json:"allow_internet_access"`
		Network             *struct {
			AllowOut []string `json:"allowOut"`
			Rules    []struct {
				Name   string `json:"name"`
				Action struct {
					Allow  bool `json:"allow"`
					Inject []struct {
						Header string `json:"header"`
						Secret string `json:"secret"`
					} `json:"inject"`
				} `json:"action"`
			} `json:"rules"`
		} `json:"network"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.TemplateID != "tpl-1" {
		t.Fatalf("templateID=%q, want tpl-1", body.TemplateID)
	}
	if body.AllowInternetAccess == nil || *body.AllowInternetAccess {
		t.Fatalf("allow_internet_access=%v, want false", body.AllowInternetAccess)
	}
	if body.Network == nil {
		t.Fatal("network missing")
	}

	fp := networkFingerprint(networkPolicyRules)
	if len(body.Network.AllowOut) != fp.AllowOut {
		t.Fatalf("allowOut count=%d, want %d", len(body.Network.AllowOut), fp.AllowOut)
	}
	if len(body.Network.Rules) != fp.Rules {
		t.Fatalf("rules count=%d, want %d", len(body.Network.Rules), fp.Rules)
	}

	injectRules := 0
	for _, r := range body.Network.Rules {
		if len(r.Action.Inject) > 0 {
			injectRules++
		}
	}
	if injectRules != fp.InjectRules {
		t.Fatalf("inject rules=%d, want %d", injectRules, fp.InjectRules)
	}
	if fp.AllowOut != 24 || fp.Rules != 6 || fp.InjectRules != 2 {
		t.Fatalf("unexpected fingerprint: %+v", fp)
	}
}

func TestBuildCreateRequestBodyRulesKeepsHostMount(t *testing.T) {
	hostMount := `[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`
	raw, err := buildCreateRequestBody("tpl-1", hostMount, networkPolicyRules)
	if err != nil {
		t.Fatalf("buildCreateRequestBody: %v", err)
	}

	var body struct {
		Metadata map[string]string `json:"metadata"`
		Network  *struct{}         `json:"network"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Metadata["host-mount"] != hostMount {
		t.Fatalf("host-mount=%q, want %q", body.Metadata["host-mount"], hostMount)
	}
	if body.Network == nil {
		t.Fatal("network missing when host-mount is set")
	}
}
