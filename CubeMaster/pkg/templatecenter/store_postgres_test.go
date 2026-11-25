// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpsertReplicaPostgreSQLUpdatePath(t *testing.T) {
	env := newPGDockerEnv(t)
	defer env.teardown()

	oldDB := store.db
	store.db = openMigratedPostgresGORM(t, env)
	defer func() { store.db = oldDB }()

	ctx := context.Background()
	templateID := "tpl-pg-upsert"
	replica := ReplicaStatus{
		NodeID: "node-a",
		NodeIP: "10.0.0.1",
		Status: "BUILDING",
		Phase:  "pulling",
	}
	require.NoError(t, UpsertReplica(ctx, templateID, "cubebox", replica))

	replica.Status = "READY"
	replica.Phase = "ready"
	require.NoError(t, UpsertReplica(ctx, templateID, "cubebox", replica))
}

// Read-then-write on PostgreSQL: one GORM chain after First emits UPDATE ... FROM
// (42712).
func TestUpsertReplicaKeepingReadyPostgreSQL(t *testing.T) {
	env := newPGDockerEnv(t)
	defer env.teardown()

	oldDB := store.db
	store.db = openMigratedPostgresGORM(t, env)
	defer func() { store.db = oldDB }()

	ctx := context.Background()
	templateID := "tpl-pg-repair"
	failed := ReplicaStatus{NodeID: "node-a", NodeIP: "10.0.0.1", Status: "FAILED", Phase: "failed", LastJobID: "job-1"}

	require.NoError(t, upsertReplicaKeepingReady(ctx, templateID, "cubebox", failed))

	// Repeated values: PostgreSQL reports matched rows, so no probe read is needed.
	require.NoError(t, upsertReplicaKeepingReady(ctx, templateID, "cubebox", failed))

	ready := ReplicaStatus{NodeID: "node-a", NodeIP: "10.0.0.1", Status: "READY", Phase: "ready", LastJobID: "job-2"}
	require.NoError(t, upsertReplicaKeepingReady(ctx, templateID, "cubebox", ready))

	require.NoError(t, upsertReplicaKeepingReady(ctx, templateID, "cubebox", failed))

	replicas, err := ListReplicas(ctx, templateID)
	require.NoError(t, err)
	require.Len(t, replicas, 1)
	require.Equal(t, ReplicaStatusReady, replicas[0].Status)
}
