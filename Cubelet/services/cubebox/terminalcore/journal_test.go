// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestFileJournalRoundTripAndCorruptRecordIsolation(t *testing.T) {
	journal, err := NewFileJournal(t.TempDir())
	require.NoError(t, err)
	record := JournalRecord{
		SessionID: uuid.NewString(),
		ExecID:    "cubelet-term-test",
		Target: TargetMetadata{
			SandboxID:          "sandbox-a",
			ContainerID:        "container-a",
			Namespace:          "default",
			RuntimeContainerID: "container-a",
		},
		OpenedAt: time.Now().UTC().Round(0),
	}
	require.NoError(t, journal.Put(record))

	records, err := journal.List()
	require.NoError(t, err)
	require.Equal(t, []JournalRecord{record}, records)

	require.NoError(t, os.WriteFile(filepath.Join(journal.dir, "corrupt.json"), []byte("{"), 0o600))
	records, err = journal.List()
	require.Error(t, err)
	require.Equal(t, []JournalRecord{record}, records, "a corrupt record must not hide valid recovery work")

	require.NoError(t, journal.Remove(record.SessionID))
	records, err = journal.List()
	require.Error(t, err, "the intentionally corrupt record remains visible")
	require.Empty(t, records)
}

func TestFileJournalRejectsUnsafeSessionID(t *testing.T) {
	journal, err := NewFileJournal(t.TempDir())
	require.NoError(t, err)
	require.Error(t, journal.Put(JournalRecord{
		SessionID: "../escape",
		ExecID:    "exec",
		Target: TargetMetadata{
			SandboxID:          "sandbox",
			RuntimeContainerID: "container",
		},
		OpenedAt: time.Now(),
	}))
}
