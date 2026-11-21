// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package remotestatus

import (
	"context"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	snapshotv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/snapshot/v1"
	"gorm.io/gorm"
)

// cubeletStateExported is an xfs-s3 origin reporting that its upload is
// committed. It is never terminal: only a readback elsewhere settles a row.
const cubeletStateExported = "exported"

// listVerifierNodes returns the IPs of healthy nodes. A test hook.
var listVerifierNodes = func() []string {
	var ips []string
	for _, n := range localcache.GetHealthyNodes(-1) {
		if n != nil && strings.TrimSpace(n.HostIP()) != "" {
			ips = append(ips, strings.TrimSpace(n.HostIP()))
		}
	}
	return ips
}

// pickVerifier chooses the node that reads an xfs-s3 checkpoint back. It is
// never the origin: a readback there would read the origin's own view and
// prove nothing about what another node gets. The choice is stable per
// snapshot so repeated polls reuse one node's progress.
func pickVerifier(origin, snapshotID string) string {
	var candidates []string
	for _, ip := range listVerifierNodes() {
		if ip != origin && !verifierExcluded(snapshotID, ip) {
			candidates = append(candidates, ip)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
	h := fnv.New32a()
	_, _ = h.Write([]byte(snapshotID))
	return candidates[int(h.Sum32()%uint32(len(candidates)))]
}

// settleXFSS3 settles an xfs-s3 pause row in two steps. The origin reports
// its upload; a failed upload fails the row, and one still running keeps it
// in progress. Once the origin has committed, or cannot be asked at all, a
// different node reads the checkpoint back from the store. Its ready carries
// the verified manifest digest, which replaces export_uuids so a later
// cross-node Resume restores exactly the session that was read back.
func settleXFSS3(ctx context.Context, db *gorm.DB, item workItem, wait time.Duration) bool {
	snapshotID := strings.TrimSpace(item.snapshotID)
	origin := strings.TrimSpace(item.hostIP)
	if snapshotID == "" {
		return true
	}
	if origin != "" {
		st, err := queryXFSS3(ctx, origin, item, wait)
		switch {
		case ctx.Err() != nil:
			// The watch window closed; the reconcile loop carries on.
			return false
		case err != nil:
			// The origin may be gone for good; the checkpoint may still be
			// complete in the store.
			log.G(ctx).Warnf("remote_status xfs-s3 %s: origin %s unreachable, asking a verifier: %v", snapshotID, origin, err)
		case st.GetState() == constants.RemoteStatusFailed:
			return writeXFSS3Terminal(ctx, db, item, constants.RemoteStatusFailed, "")
		case st.GetState() == cubeletStateExported:
		case st.GetState() == "":
			log.G(ctx).Warnf("remote_status xfs-s3 %s: origin %s cannot report its export (%s), asking a verifier",
				snapshotID, origin, st.GetMessage())
		default:
			return false
		}
	}
	verifier := pickVerifier(origin, snapshotID)
	if verifier == "" {
		log.G(ctx).Warnf("remote_status xfs-s3 %s: no node other than origin %s can read it back", snapshotID, origin)
		return false
	}
	st, err := queryXFSS3(ctx, verifier, item, wait)
	if err != nil {
		log.G(ctx).Warnf("remote_status xfs-s3 %s: verifier %s query failed: %v", snapshotID, verifier, err)
		return false
	}
	if stage, ok := verifierCannotVouch(st.GetMessage()); ok {
		// This node cannot run the trial (other CPU or images, no
		// isolation, too many failed tries). The checkpoint is not at
		// fault; try another node.
		log.G(ctx).Warnf("remote_status xfs-s3 %s: verifier %s cannot vouch (%s), trying another node for %s",
			snapshotID, verifier, stage, verifierExclusionTTL)
		excludeVerifier(snapshotID, verifier)
	}
	if verifierFailedRestore(st.GetMessage()) {
		// A restore that fails on one node may be that node. Only when
		// nodes that differ fail it too is it the checkpoint.
		n := noteRestoreFailure(snapshotID, verifier)
		if n >= restoreFailuresToFail {
			log.G(ctx).Warnf("remote_status xfs-s3 %s: %d different nodes failed to restore it; last on %s: %s",
				snapshotID, n, verifier, st.GetMessage())
			return writeXFSS3Terminal(ctx, db, item, constants.RemoteStatusFailed, "")
		}
		log.G(ctx).Warnf("remote_status xfs-s3 %s: verifier %s failed to restore it, trying another node: %s",
			snapshotID, verifier, st.GetMessage())
		excludeVerifier(snapshotID, verifier)
	}
	switch st.GetState() {
	case constants.RemoteStatusReady:
		return writeXFSS3Terminal(ctx, db, item, constants.RemoteStatusReady, strings.TrimSpace(st.GetMessage()))
	case constants.RemoteStatusFailed:
		log.G(ctx).Warnf("remote_status xfs-s3 %s: readback on %s failed: %s", snapshotID, verifier, st.GetMessage())
		return writeXFSS3Terminal(ctx, db, item, constants.RemoteStatusFailed, "")
	}
	return false
}

// verifierExclusionTTL bounds how long a verifier that could not vouch for
// a snapshot is skipped: its images or capacity may change.
const verifierExclusionTTL = 30 * time.Minute

var verifierExclusions sync.Map // snapshot id + "|" + node ip -> time.Time

func excludeVerifier(snapshotID, ip string) {
	verifierExclusions.Store(snapshotID+"|"+ip, nowFn().Add(verifierExclusionTTL))
}

func verifierExcluded(snapshotID, ip string) bool {
	v, ok := verifierExclusions.Load(snapshotID + "|" + ip)
	if !ok {
		return false
	}
	if nowFn().After(v.(time.Time)) {
		verifierExclusions.Delete(snapshotID + "|" + ip)
		return false
	}
	return true
}

// restoreFailuresToFail is how many different nodes must fail to restore a
// checkpoint before it is failed. A single node may fail for reasons of its
// own, and a cluster with one other node never fails a checkpoint this way:
// it stays pending, which is never mistaken for ready.
const restoreFailuresToFail = 2

var restoreFailures sync.Map // snapshot id -> *restoreFailureSet

type restoreFailureSet struct {
	mu    sync.Mutex
	nodes map[string]struct{}
}

// noteRestoreFailure records that ip failed to restore snapshotID and
// returns how many different nodes have. It lives in memory: after a
// Master restart the halted verifiers report their failure again.
func noteRestoreFailure(snapshotID, ip string) int {
	v, _ := restoreFailures.LoadOrStore(snapshotID, &restoreFailureSet{nodes: map[string]struct{}{}})
	set := v.(*restoreFailureSet)
	set.mu.Lock()
	defer set.mu.Unlock()
	set.nodes[ip] = struct{}{}
	return len(set.nodes)
}

// verifierFailedRestore recognizes a trial whose restore failed after the
// data was verified. Cubelet stops trying on that node and keeps reporting
// it as "retry(restore): ...".
func verifierFailedRestore(msg string) bool {
	return strings.HasPrefix(strings.TrimSpace(msg), "retry(restore)")
}

// verifierCannotVouch recognizes a trial that could not run on the verifier
// for reasons of that node: Cubelet reports them as "retry(<stage>): ...".
// "exhausted" is a node that gave up after too many failed tries.
func verifierCannotVouch(msg string) (string, bool) {
	for _, stage := range []string{"compat", "deps", "isolation", "exhausted"} {
		if strings.HasPrefix(strings.TrimSpace(msg), "retry("+stage+")") {
			return stage, true
		}
	}
	return "", false
}

func queryXFSS3(ctx context.Context, hostIP string, item workItem, wait time.Duration) (*snapshotv1.StatusResponse, error) {
	queryCtx := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, wait+5*time.Second)
		defer cancel()
	}
	return queryStatus(queryCtx, cubeletAddr(hostIP), &snapshotv1.StatusRequest{
		SnapshotId:  item.snapshotID,
		Backend:     constants.SnapshotBackendXFSS3,
		WaitReadyMs: int32(wait / time.Millisecond),
	})
}

func writeXFSS3Terminal(ctx context.Context, db *gorm.DB, item workItem, next, locator string) bool {
	updates := map[string]any{
		"remote_status": next,
		"updated_at":    nowTimestamp(),
	}
	if next == constants.RemoteStatusReady {
		if locator == "" {
			log.G(ctx).Warnf("remote_status xfs-s3 %s: ready without a verified locator; not publishing", item.snapshotID)
			return false
		}
		updates["export_uuids"] = locator
	}
	res := db.WithContext(ctx).Table(item.table).
		Where("snapshot_id = ? AND remote_status = ?", item.snapshotID, constants.RemoteStatusInProgress).
		Updates(updates)
	if res.Error != nil {
		log.G(ctx).Warnf("remote_status update %s %s failed: %v", item.kind, item.snapshotID, res.Error)
		return false
	}
	if res.RowsAffected > 0 {
		log.G(ctx).Infof("remote_status %s %s -> %s", item.kind, item.snapshotID, next)
	}
	restoreFailures.Delete(item.snapshotID)
	return true
}
