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
