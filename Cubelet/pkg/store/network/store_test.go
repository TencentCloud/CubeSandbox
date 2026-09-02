// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network_test

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networktypes "github.com/tencentcloud/CubeSandbox/Cubelet/network/types"
	networkstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/network"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestRecoverStore(t *testing.T) {
	base := t.TempDir()
	db, err := utils.NewCubeStoreExt(base, "meta.db", 10, nil)
	require.NoError(t, err)

	store := networkstore.NewStore(db)

	tap := &networktypes.ShimNetReq{
		Interfaces: []*networktypes.Interface{
			{
				Name:   "z192.168.1.10",
				IPAddr: net.ParseIP("192.168.1.10"),
			},
		},
	}
	store.Add(networkstore.NetworkAllocation{
		SandboxID:   "tap",
		NetworkType: cubebox.NetworkType_tap.String(),
		Metadata:    tap,
		Timestamp:   time.Now().Unix(),
	})

	assert.NoError(t, store.Sync("tap"))

	assert.NoError(t, db.Close())

	db, err = utils.NewCubeStoreExt(base, "meta.db", 10, nil)
	require.NoError(t, err)

	store, err = networkstore.RecoverFromDB(db)
	require.NoError(t, err)

	tapAlloc, err := store.Get("tap")
	require.NoError(t, err)
	assert.Equal(t, cubebox.NetworkType_tap.String(), tapAlloc.NetworkType)
	assert.Equal(t, tap.GetPersistMetadata(), tapAlloc.PersistentMetadata)

	t.Logf("%+v", string(tapAlloc.PersistentMetadata))
}
