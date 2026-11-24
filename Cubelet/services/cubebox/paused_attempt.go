// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// claimPausedAttempt stamps the PAUSED tombstone with the attempt that is about
// to replace it. The final create step writes the new row's CreateAttemptID,
// but a resume can fail in an earlier step (storage, network, netfile). Without
// this claim the row would still carry the original create's id, and the failed
// resume's failover would mistake it for a newer owner and skip cleanup.
//
// The caller holds the per-sandbox lifecycle lock for the pause resume, so the
// read/write pair cannot race a Pause or another Resume.
func (l *local) claimPausedAttempt(ctx context.Context, sandboxID, attemptID string) error {
	if l == nil || l.cubeboxManger == nil || attemptID == "" {
		return nil
	}
	sb, err := l.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) {
			return nil
		}
		return err
	}
	if sb == nil {
		return nil
	}
	st := sb.GetStatus()
	if st == nil || st.Get().State() != cubebox.ContainerState_CONTAINER_PAUSED {
		return nil
	}
	sb.Lock()
	defer sb.Unlock()
	sb.CreateAttemptID = attemptID
	// Internal bookkeeping must not dispatch a lifecycle update event to the
	// metrics/listener pipeline mid-resume.
	return l.cubeboxManger.Save(ctx, sb, cubes.WithNoEvent)
}
