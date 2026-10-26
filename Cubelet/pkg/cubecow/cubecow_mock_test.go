// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubecow

import "testing"

func TestMapError(t *testing.T) {
	testCases := []struct {
		name       string
		rc         int32
		wantCode   SemanticCode
		wantAction Action
	}{
		{name: "ok", rc: 0, wantCode: SemOK, wantAction: ActFail},
		{name: "not_found", rc: -1, wantCode: SemNotFound, wantAction: ActFail},
		{name: "already_exists", rc: -2, wantCode: SemAlreadyExists, wantAction: ActFail},
		{name: "resource_exhausted", rc: -3, wantCode: SemResourceExhausted, wantAction: ActFail},
		{name: "invalid_argument", rc: -4, wantCode: SemInvalidArgument, wantAction: ActBug},
		{name: "io_error", rc: -6, wantCode: SemIoError, wantAction: ActMarkStorageUnhealthy},
		{name: "config_error", rc: -10, wantCode: SemConfigError, wantAction: ActOpsIntervention},
		{name: "precondition_failed", rc: -11, wantCode: SemPreconditionFailed, wantAction: ActFail},
		{name: "null_pointer", rc: -12, wantCode: SemNullPointer, wantAction: ActBug},
		{name: "invalid_utf8", rc: -13, wantCode: SemInvalidUTF8, wantAction: ActBug},
		{name: "panic", rc: -99, wantCode: SemPanic, wantAction: ActMarkStoragePanic},
		{name: "unknown", rc: -12345, wantCode: SemanticCode(-12345), wantAction: ActFail},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotCode, gotAction := MapError(tc.rc)
			if gotCode != tc.wantCode {
				t.Fatalf("MapError(%d) code = %v, want %v", tc.rc, gotCode, tc.wantCode)
			}
			if gotAction != tc.wantAction {
				t.Fatalf("MapError(%d) action = %v, want %v", tc.rc, gotAction, tc.wantAction)
			}
		})
	}
}

func TestCowErrorError(t *testing.T) {
	err := (&CowError{Code: SemNotFound, Action: ActFail, RawRC: -1, Message: "missing volume"}).Error()
	if err != "cubecow error code=not_found raw_rc=-1 action=fail: missing volume" {
		t.Fatalf("unexpected error string: %s", err)
	}
}

func TestOpenHandleClosed(t *testing.T) {
	engine := &Engine{}
	_, err := engine.openHandle()
	if err == nil {
		t.Fatal("expected openHandle to fail on closed engine")
	}
	cerr, ok := err.(*CowError)
	if !ok {
		t.Fatalf("expected CowError, got %T", err)
	}
	if cerr.Code != ErrClosed {
		t.Fatalf("unexpected code: %v", cerr.Code)
	}
}
