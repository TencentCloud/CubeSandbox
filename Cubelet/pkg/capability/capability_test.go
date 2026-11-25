// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetHasIsLevelAware(t *testing.T) {
	set := Set{CapUpdateSnapshotCapture: 2, CapGuestFSFreeze: 0}
	require.True(t, set.Has(CapUpdateSnapshotCapture, 1))
	require.True(t, set.Has(CapUpdateSnapshotCapture, 2))
	require.False(t, set.Has(CapUpdateSnapshotCapture, 3))
	// Declared at 0 is a statement, not an absence.
	require.True(t, set.Declared(CapGuestFSFreeze))
	require.False(t, set.Has(CapGuestFSFreeze, 1))
	require.True(t, set.KnownUnsupported(CapGuestFSFreeze, 1))
	// Absent is never unsupported.
	require.False(t, set.Declared("guest.not_probed"))
	require.False(t, set.KnownUnsupported("guest.not_probed", 1))
	require.False(t, set.Has("guest.not_probed", 0))
}

func TestKnownUnsupportedRequiresADeclarationBelowTheLevel(t *testing.T) {
	set := Set{CapUpdateSnapshotCapture: 1}
	require.False(t, set.KnownUnsupported(CapUpdateSnapshotCapture, 1), "supported is not unsupported")
	require.True(t, set.KnownUnsupported(CapUpdateSnapshotCapture, 2), "below the asked level")
	require.False(t, set.KnownUnsupported("update.absent", 1))
}

func TestCloneAndEqualTreatEmptyAsAbsent(t *testing.T) {
	require.Nil(t, Set(nil).Clone())
	require.Nil(t, Set{}.Clone(), "an empty set stays absent instead of becoming {}")
	require.True(t, Set(nil).Equal(Set{}), "neither declares anything")

	set := Set{CapUpdateSnapshotCapture: 1}
	clone := set.Clone()
	require.True(t, set.Equal(clone))
	clone[CapUpdateSnapshotCapture] = 2
	require.False(t, set.Equal(clone), "clone is independent")
	require.False(t, set.Equal(Set{CapUpdateSnapshotCapture: 2}))
	require.False(t, set.Equal(Set{CapUpdateSnapshotCapture: 1, CapErrorsCoded: 1}))
}

func TestNamesAreSorted(t *testing.T) {
	set := Set{"b.b": 1, "a.a": 1, "c.c": 1}
	require.Equal(t, []string{"a.a", "b.b", "c.c"}, set.Names())
	require.Empty(t, Set(nil).Names())
}

func TestValidateAcceptsDottedLowercaseNames(t *testing.T) {
	require.NoError(t, Validate(Set{
		CapUpdateSnapshotCapture:       LevelInitial,
		CapUpdateSnapshotCaptureFSOnly: LevelInitial,
		"cubelet.example_capability":   LevelInitial,
	}))
	require.NoError(t, Validate(nil))
}

func TestValidateRejectsMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  Set
	}{
		{"empty name", Set{"": 1}},
		{"uppercase", Set{"Update.Snapshot": 1}},
		{"leading separator", Set{".update": 1}},
		{"space", Set{"update snapshot": 1}},
		{"slash", Set{"update/../escape": 1}},
		{"too long", Set{string(make([]byte, maxNameLength+1)): 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, Validate(tc.set))
		})
	}
}

func TestValidateRejectsTooManyEntries(t *testing.T) {
	set := make(Set, maxEntries+1)
	for i := 0; i <= maxEntries; i++ {
		set[fmt.Sprintf("padding.%d", i)] = 1
	}
	require.Error(t, Validate(set))
}

func TestCodedErrorCodeReadsThePrefix(t *testing.T) {
	require.Equal(t, ErrCodePauseBusy,
		CodedErrorCode(ErrCodePauseBusy+": sandbox pause forbidding, terminate exec tasks first"))
	require.Equal(t, ErrCodeUnknownAction,
		CodedErrorCode(ErrCodeUnknownAction+": unknown update ext action: Foo"))
	// Prose before a colon is not a code.
	require.Empty(t, CodedErrorCode("update sandbox failed: boom"))
	require.Empty(t, CodedErrorCode("Others(\"Other: something\")"))
	require.Empty(t, CodedErrorCode(""))
	require.Empty(t, CodedErrorCode(ErrCodePauseBusy), "no separator means no code")
}
