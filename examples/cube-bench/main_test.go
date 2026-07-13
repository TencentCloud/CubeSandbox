package main

import "testing"

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
