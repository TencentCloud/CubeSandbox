// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/queueworker"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/volumefile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"golang.org/x/time/rate"
)

var (
	lifeDbDir         = "lifedb"
	bucketKeyCode     = "code"
	bucketKeyLayer    = "layer"
	bucketKeyLang     = "lang"
	bucketKeyLangExt4 = "lang_ext4"

	suffixToolVersion = ".tool-version"

	dbBucketList = []*multimeta.BucketDefineInternal{
		{
			BucketDefine: &multimetadb.BucketDefine{
				Name:     bucketKeyCode,
				DbName:   "volumelifetime",
				Describe: "volume life time db",
			},
		}, {
			BucketDefine: &multimetadb.BucketDefine{
				Name:     bucketKeyLayer,
				DbName:   "volumelifetime",
				Describe: "volume life time db",
			},
		}, {
			BucketDefine: &multimetadb.BucketDefine{
				Name:     bucketKeyLang,
				DbName:   "volumelifetime",
				Describe: "volume life time db",
			},
		}, {
			BucketDefine: &multimetadb.BucketDefine{
				Name:     bucketKeyLangExt4,
				DbName:   "volumelifetime",
				Describe: "volume life time db",
			},
		},
	}
)

const metaDelete = -1

type volumeLifetime struct {
	conf           *VolumeConfig
	db             *utils.CubeStore
	cache          *metricGlobalInfo
	volumeLocalPtr *volumeLocal

	cleanupLimiter *rate.Limiter
	limiter        *semaphore.Limiter
	asyncTask      queueworker.QueueWorker
	gcLogger       *CubeLog.Entry
}

type RefSandBox struct {
	Timestamp int64
	Del       bool
}
type meta struct {
	Timestamp int64 `json:"timestamp"`
	Ref       int   `json:"ref"`

	Del      int   `json:"delete"`
	FileSize int64 `json:"fileSize"`

	RefSandBoxIDs map[string]RefSandBox

	fileType   volumefile.FileType
	userID     string
	fileSha256 string
}

func (l *volumeLifetime) baseLifetimeDBDir() string {
	return filepath.Join(l.conf.RootPath, lifeDbDir)
}

func (l *volumeLifetime) init(ctx context.Context) error {
	if err := l.initDb(); err != nil {
		return err
	}

	rt := &CubeLog.RequestTrace{
		Action: "VolumeGC",
		Caller: constants.VolumeSourceID.ID(),
		Callee: constants.VolumeSourceID.ID(),
	}

	ctx = CubeLog.WithRequestTrace(ctx, rt)
	l.gcLogger = log.AuditLogger.WithContext(ctx)

	l.cache = newMetricGlobalInfo(uint64(l.conf.AsyncBufferCap))
	l.cleanupLimiter = rate.NewLimiter(rate.Limit(l.conf.MaxCleanNum), l.conf.MaxCleanNum)
	l.limiter = semaphore.NewLimiter(int64(l.conf.MaxCleanNum))
	l.asyncTask = queueworker.NewQueueWorker(&queueworker.Options{
		QueueSize: l.conf.AsyncCleanCap,
		WorkerNum: 2,
	}, l.dealClean)

	go l.loopFlush(ctx)
	go l.loopClean(ctx, volumefile.FtCode)
	go l.loopClean(ctx, volumefile.FtLayer)

	return nil
}

func (l *volumeLifetime) initDb() error {
	if err := os.MkdirAll(path.Clean(l.baseLifetimeDBDir()), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init dir failed %s", err.Error())
	}
	var err error
	if l.db, err = utils.NewCubeStoreExt(l.baseLifetimeDBDir(), "meta.db", 10, nil); err != nil {
		return err
	}

	for _, bucket := range dbBucketList {
		bucket.CubeStore = l.db
		multimeta.RegisterBucket(bucket)
	}
	return nil
}

func (l *volumeLifetime) Add(m *meta) {
	if m.userID == "" || m.fileSha256 == "" {
		return
	}
	l.cache.add(m)
}

func (l *volumeLifetime) loopFlush(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(l.conf.FlushIntervalInSecond) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recov.WithRecover(func() {
				l.syncFlush(ctx)
			}, func(panicError interface{}) {
				CubeLog.Fatalf("syncFlush panic:%v", string(debug.Stack()))
			})
		}
	}
}

func (l *volumeLifetime) syncFlush(ctx context.Context) {
	start := time.Now()
	indexList := l.cache.getAvailableMapIndex()

	cnt := 0
	for _, index := range indexList {
		select {
		case <-ctx.Done():
			return
		default:
		}
		dataSummeryList := l.cache.getMap(index).getMapData()
		for _, data := range dataSummeryList {
			cnt++
			if err := l.syncDb(data); err != nil {
				CubeLog.Warnf("volumeLifetime syncDb [%s] fail:%v", data.realKey(), err)
			}
		}
	}
	if cnt > 0 {
		CubeLog.Debugf("volumeLifetime loopFlush[%d] done.cost:%v", cnt, time.Since(start))
	}
}

func (l *volumeLifetime) loopClean(ctx context.Context, fileType volumefile.FileType) {
	ticker := time.NewTicker(time.Duration(l.conf.CleanupIntervalInSecond) * time.Second)
	defer ticker.Stop()

	checkDiskUsageDeadline := time.Now().Add(tomlext.ToStdTime(l.conf.CheckLocalVolumeInterval))
	checkDiffFromDbDeadline := time.Now().Add(tomlext.ToStdTime(l.conf.CheckDiffFromDbInterval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recov.WithRecover(func() {

				allDbResult := l.asyncClean(ctx, fileType, l.conf.ExpiredInSecond)

				if len(allDbResult) > 0 {
					recov.WithRecover(func() {
						if checkDiskUsageDeadline.After(time.Now()) {

							return
						}
						defer func() {
							checkDiskUsageDeadline = time.Now().Add(tomlext.ToStdTime(l.conf.CheckLocalVolumeInterval))
						}()

						l.checkDiskUsage(fileType, allDbResult)
					}, func(panicError interface{}) {
						CubeLog.Fatalf("checkDiskUsage panic:%v", string(debug.Stack()))
					})

					recov.WithRecover(func() {
						if checkDiffFromDbDeadline.After(time.Now()) {

							return
						}
						defer func() {
							checkDiffFromDbDeadline = time.Now().Add(tomlext.ToStdTime(l.conf.CheckDiffFromDbInterval))
						}()
						l.checkDiskDiffFromDb(fileType, allDbResult)
					}, func(panicError interface{}) {
						CubeLog.Fatalf("checkDiskDiffFromDb panic:%v", string(debug.Stack()))
					})

				}

			}, func(panicError interface{}) {
				CubeLog.Fatalf("asyncClean panic:%v", string(debug.Stack()))
			})
		}
	}
}

func (l *volumeLifetime) asyncClean(ctx context.Context,
	fileType volumefile.FileType, expiredInSecond int64) map[string]map[string]*meta {
	bucket := getBucketName(fileType)
	rt := &CubeLog.RequestTrace{
		Action:       "VolumeLifetime",
		CalleeAction: bucket,
		Caller:       constants.VolumeSourceID.ID(),
		Callee:       constants.VolumeSourceID.ID(),
	}
	var needDelete needDeleteType

	allDbResult := map[string]map[string]*meta{}

	start := time.Now()
	all, err := l.db.ReadAll(bucket)
	if err != nil {
		CubeLog.Fatalf("volumeLifetime[%s] asyncClean fail:%v", bucket, err)
		return allDbResult
	}
	if len(all) > 0 {
		CubeLog.Warnf("volumeLifetime[%s] asyncClean ReadAll[%d] done.cost:%v", bucket, len(all), time.Since(start))
	}

	const onceCleanMaxNum = 120

	cnt, totalUserID, totalCodeFile, activeCnt := 0, 0, 0, 0
	start = time.Now()
	for k, v := range all {
		keyList := strings.SplitN(k, "|", 2)
		if len(keyList) < 2 {
			_ = l.db.Delete(bucket, k)
			continue
		}
		m := &meta{}
		err = json.Unmarshal(v, m)
		if err != nil {
			CubeLog.Errorf("volumeLifetime[%s] asyncClean ReadAll[%d] Fatal:%v,err:%v,value:%v",
				bucket, len(all), k, err, string(v))
			_ = l.db.Delete(bucket, k)
			continue
		}
		m.userID, m.fileSha256, m.fileType = keyList[0], keyList[1], fileType

		if metaDelete == m.Del && m.Ref <= 0 {

			if cnt < onceCleanMaxNum {
				l.addQ(m)
				cnt++
			}
			continue
		}
		if subm, ok := allDbResult[m.userID]; ok {
			subm[m.fileSha256] = m
			totalCodeFile++
		} else {
			subm = map[string]*meta{
				m.fileSha256: m,
			}
			allDbResult[m.userID] = subm
			totalUserID++
			totalCodeFile++
		}

		deltaT := time.Now().Unix() - m.Timestamp
		if m.Ref > 0 && deltaT <= expiredInSecond {

			activeCnt++
		}

		expired := m.Ref <= 0 && m.Timestamp > 10000 && deltaT > expiredInSecond
		if expired {
			needDelete = append(needDelete, m)
		}

		expiredException := m.Ref > 0 && m.Timestamp > 10000 && deltaT > l.conf.ExpiredExceptionInSec
		if expiredException {
			CubeLog.Fatalf("volumeLifetime found exec,Timestamp:%v, userID: %v,Ref:%d, type: %v,del:%v, sha256: %v,dirtyPods:%s",
				time.Unix(m.Timestamp, 0), m.userID, m.Ref, m.fileType, m.Del, m.fileSha256, utils.InterfaceToString(m.RefSandBoxIDs))

		}
	}

	if len(needDelete) > 0 {
		sort.Sort(needDelete)
		for _, m := range needDelete {
			l.addQ(m)
			cnt++
			if cnt >= onceCleanMaxNum {
				break
			}
		}
	}
	CubeLog.Warnf("volumeLifetime[%s] done,totalUsers:%d,totalCodeFiles:%d,need_delete:%d,asyncClean_deal:%d,expiredInSecond:%d,cost:%v",
		bucket, totalUserID, totalCodeFile, len(needDelete), cnt, expiredInSecond, time.Since(start))
	realTotalCodeFile := totalCodeFile - len(needDelete)
	if realTotalCodeFile > 0 {
		rt.RetCode = int64(activeCnt * 10000.0 / realTotalCodeFile)
		CubeLog.Trace(rt)
	}

	return allDbResult
}

func (l *volumeLifetime) addQ(m *meta) {
	if err := l.asyncTask.Push(m); err != nil {
		CubeLog.Errorf("volumeLifetime asyncTask.Push fail:%v", err)
	}
}

func (l *volumeLifetime) checkDiskUsage(fileType volumefile.FileType, dbData map[string]map[string]*meta) {
	if len(dbData) <= 0 {
		return
	}

	if !l.isUsageException(fileType, true) {
		return
	}

	bucket := getBucketName(fileType)
	start := time.Now()

	needDeleteBySize := []meta{}
	for _, subm := range dbData {
		for _, m := range subm {
			deltaT := time.Now().Unix() - m.Timestamp
			if m.Ref <= 0 && deltaT > l.conf.LeastActiveInSecond {
				needDeleteBySize = append(needDeleteBySize, *m)
			}
		}
	}

	utils.OrderedBy(lessByLastUsedTime, lessBySize).Sort(needDeleteBySize)
	cnt := 0
	for i := 0; i < len(needDeleteBySize); {
		m := needDeleteBySize[i]
		if l.hardRemove(context.Background(), &m) {
			continue
		}
		i++
		cnt++
		if cnt%10 == 0 {
			if l.isUsageReasonable(fileType) {

				break
			}
		}
	}
	if cnt > 0 {
		CubeLog.Errorf("checkDiskUsage[%s] done,syncClean_deal:%d,cost:%v", bucket, cnt, time.Since(start))
	}
}

func (l *volumeLifetime) isUsageReasonable(fileType volumefile.FileType) bool {
	freeblockPercentage, freeinodePercentage, err := utils.GetDeviceIdleRatio(l.conf.DataPath)
	bucket := getBucketName(fileType)
	if err != nil {
		CubeLog.Errorf("isUsageReasonable fail:%v", err)
		return false
	}
	CubeLog.Debugf("isUsageReasonable[%s] freeblockPercentage: %d%%,freeinodePercentage: %d%%",
		bucket, freeblockPercentage, freeinodePercentage)
	if freeblockPercentage > uint64(l.conf.MaxFreeBlocksThreshold) &&
		freeinodePercentage > uint64(l.conf.MaxFreeInodesThreshold) {
		return true
	}
	return false
}

func (l *volumeLifetime) isUsageException(fileType volumefile.FileType, alarm bool) bool {
	freeblockPercentage, freeinodePercentage, err := utils.GetDeviceIdleRatio(l.conf.DataPath)
	bucket := getBucketName(fileType)
	if err != nil {
		CubeLog.Errorf("checkDiskUsage fail:%v", err)
		return false
	}
	CubeLog.Debugf("checkDiskUsage[%s] freeblockPercentage: %d%%,freeinodePercentage: %d%%",
		bucket, freeblockPercentage, freeinodePercentage)
	if freeblockPercentage > uint64(l.conf.FreeBlocksThreshold) &&
		freeinodePercentage > uint64(l.conf.FreeInodesThreshold) {
		return false
	}

	if alarm {
		CubeLog.Fatalf("checkDiskUsage exception[%s]expiredInSecond[%d] freeblockPercentage(got:%d%% < %d%%),freeinodePercentage(got:%d%% < %d%%)",
			bucket, l.conf.ExpiredInSecond, freeblockPercentage, l.conf.FreeBlocksThreshold, freeinodePercentage, l.conf.FreeInodesThreshold)
	}
	return true
}

func (l *volumeLifetime) checkDiskDiffFromDb(fileType volumefile.FileType, dbData map[string]map[string]*meta) {
	start := time.Now()
	basePath := filepath.Join(l.conf.DataPath, getBucketName(fileType))
	denList, err := os.ReadDir(basePath)
	if err != nil {
		CubeLog.Errorf("checkDiskDiffFromDb %s fail:%v", basePath, err)
		return
	}
	dirtycnt := 0
	dealBadPathFun := func(den os.DirEntry, userID, fileSha256 string) {
		m := &meta{
			userID:     userID,
			fileSha256: fileSha256,
			fileType:   fileType,
		}
		info, err := den.Info()
		if err != nil {
			m.Timestamp = time.Now().Unix()
		} else {
			m.Timestamp = info.ModTime().Unix()
		}

		days_ago := time.Now().Add(-time.Duration(l.conf.NotInDbToDeleteExpiredInSecond) * time.Second)
		if time.Unix(m.Timestamp, 0).Before(days_ago) {
			CubeLog.Infof("checkDiskDiffFromDb[%s] found exec:%+v", basePath, m)
			l.addQ(m)
			dirtycnt++
		}
	}
	totalUserID := 0
	totalCodeFile := 0

	for _, den := range denList {
		if !den.IsDir() {
			continue
		}
		userID := den.Name()

		if len(userID) > 0 && userID[0] == '.' {
			continue
		}

		if _, ok := dbData[userID]; !ok {
			dealBadPathFun(den, userID, "")
		}

		userIDDirList, err := os.ReadDir(path.Clean(filepath.Join(basePath, userID)))
		if err != nil {
			continue
		}
		totalUserID++
		for _, subdir := range userIDDirList {
			if !subdir.IsDir() {
				continue
			}
			fileSha256 := subdir.Name()

			if len(fileSha256) > 0 && fileSha256[0] == '.' {
				continue
			}
			totalCodeFile++

			if _, ok := dbData[userID][fileSha256]; !ok {
				dealBadPathFun(subdir, userID, fileSha256)
			}
		}
	}
	if dirtycnt > 0 {
		CubeLog.Errorf("checkDiskDiffFromDb[%s] done,totalUsers:%d,totalCodeFile:%d,asyncClean_deal:%d,cost:%v",
			basePath, totalUserID, totalCodeFile, dirtycnt, time.Since(start))
	}
}

func rename2Hidden(path string) (string, error) {

	atomicPath := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s", filepath.Base(path)))
	if err := os.Rename(path, atomicPath); err != nil && !os.IsExist(err) {
		if os.IsNotExist(err) {
			return atomicPath, nil
		}
		return "", err
	}
	return atomicPath, nil
}

func (l *volumeLifetime) removeFiles(filePath string) {

	if err := os.RemoveAll(filePath); err != nil && !os.IsNotExist(err) {
		CubeLog.Warnf("rmdir[%s] failed. %s", filePath, err)
		return
	}
}

func (l *volumeLifetime) checkAndClean(filePath string) {

	toolVersionFile := filePath + suffixToolVersion
	if err := os.Remove(toolVersionFile); err != nil && !os.IsNotExist(err) {
		CubeLog.Warnf("rm %s failed. %s", toolVersionFile, err)
	}

	baseDir := filepath.Dir(filePath)
	dirs, err := os.ReadDir(baseDir)
	if err == nil && len(dirs) == 0 {
		if err := os.RemoveAll(baseDir); err != nil && !os.IsNotExist(err) {
			CubeLog.Warnf("baseDir rmdir[%s] failed. %s", baseDir, err)
			return
		}
	}
}

func (l *volumeLifetime) isStillRef(m *meta) bool {

	bucket := getBucketName(m.fileType)
	cacheMeta := l.cache.get(m)
	if cacheMeta != nil {
		deltaT := time.Now().Unix() - cacheMeta.Timestamp
		if cacheMeta.Ref > 0 ||
			(cacheMeta.Ref == 0 && deltaT <= l.conf.LeastActiveInSecond) {
			return true
		}
	}

	key := m.realKey()
	tmpV, err := l.db.Get(bucket, key)
	if err != nil {
		return false
	}
	dbMeta := &meta{}
	err = json.Unmarshal(tmpV, dbMeta)
	if err != nil {
		return false
	}
	deltaT := time.Now().Unix() - dbMeta.Timestamp
	return dbMeta.Ref > 0 || (dbMeta.Ref == 0 && deltaT <= l.conf.LeastActiveInSecond)
}

func (l *volumeLifetime) dealClean(data interface{}) (err error) {
	m, ok := data.(*meta)
	if !ok {
		return nil
	}
	if l.hardRemove(context.Background(), m) {
		l.addQ(m)
	}
	return nil
}

func (l *volumeLifetime) hardRemove(ctx context.Context, m *meta) (retry bool) {
	if !l.acquireMutex() {

		l.gcLogger.Debugf("volume gc: acquire exceed")
		return true
	}
	recov.WithRecover(func() {
		defer l.releaseMutex()

		bucket := getBucketName(m.fileType)
		mLock := l.getMultiLock(m.fileType, m.fileSha256)
		filePath := l.getVolumeDir(m.fileType, m.userID, m.fileSha256)
		if m.fileSha256 == "" {
			filePath = filepath.Join(l.conf.DataPath, bucket, m.userID)
		}
		exist, _ := utils.DenExist(filePath)
		if !exist {

			if err := l.db.Delete(bucket, m.realKey()); err != nil {
				l.gcLogger.Warnf("delete key [%s] failed. %s", m.realKey(), err)
			}
			return
		}

		mLock.Lock()
		if l.isStillRef(m) {
			mLock.Unlock()
			return
		}

		hiddenFilePath, err := rename2Hidden(filePath)
		if err != nil {
			l.gcLogger.Errorf("volume gc: rename2Hidden[%s] failed. %s", filePath, err)

			mLock.Unlock()
			return
		}

		if err := l.db.Delete(bucket, m.realKey()); err != nil {
			l.gcLogger.Warnf("delete key [%s] failed. %s", m.realKey(), err)
		}
		mLock.Unlock()

		logPrompt := fmt.Sprintf("volume gc: Timestamp:%v, userID: %v,Ref:%d, type: %v,del:%v, sha256: %v",
			time.Unix(m.Timestamp, 0), m.userID, m.Ref, m.fileType, m.Del, m.fileSha256)
		l.removeFiles(hiddenFilePath)
		l.checkAndClean(filePath)
		l.gcLogger.Errorf("%v success", logPrompt)

		userId, _ := strconv.Atoi(m.userID)
		rt := &CubeLog.RequestTrace{
			Action:       "VolumeGC",
			CalleeAction: "Deleted",
			Caller:       constants.VolumeSourceID.ID(),
			Callee:       constants.VolumeSourceID.ID(),
			RequestID:    m.fileSha256,
			AppID:        int64(userId),
		}
		CubeLog.Trace(rt)
	}, func(panicError interface{}) {
		defer l.releaseMutex()
		CubeLog.Fatalf("hardRemove[%s] panic:%v", m.fileSha256, string(debug.Stack()))
	})
	return false
}

func (l *volumeLifetime) acquireMutex() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := l.cleanupLimiter.Wait(ctx)
	if err == nil {

		return l.limiter.Acquire(ctx) == nil
	}
	return false
}
func (l *volumeLifetime) releaseMutex() {
	l.limiter.Release()
}

func (l *volumeLifetime) syncDb(m *meta) error {
	bucket := getBucketName(m.fileType)
	mLock := l.getMultiLock(m.fileType, m.fileSha256)

	mLock.RLock()

	defer mLock.RUnlock()

	key := m.realKey()
	tmpV, err := l.db.Get(bucket, key)
	if err == nil {

		oldM := &meta{}
		err := json.Unmarshal(tmpV, oldM)
		if err == nil {
			if m.Del == metaDelete || m.Ref < 0 {

				m.Timestamp = oldM.Timestamp
			}

			m.Ref = m.Ref + oldM.Ref

			for id, val := range oldM.RefSandBoxIDs {
				if val.Del {

					CubeLog.Fatalf("syncDb_got_unexpected_deleted_key [%v],oldv[%v]", id, val)
				}

				if newV, ok := m.RefSandBoxIDs[id]; ok {
					if newV.Del {

						delete(m.RefSandBoxIDs, id)
					} else {

						CubeLog.Fatalf("syncDb_got_unexpected_active_key [%v],oldv[%v]", id, val)
					}
				} else {

					if m.RefSandBoxIDs == nil {
						m.RefSandBoxIDs = make(map[string]RefSandBox)
					}
					m.RefSandBoxIDs[id] = val
				}
			}

			for id, val := range m.RefSandBoxIDs {
				if val.Del {
					delete(m.RefSandBoxIDs, id)
				}
			}
		} else {
			CubeLog.Warnf("syncDb Unmarshal key [%s] failed. %s", m.realKey(), err)
		}
	}
	value, err := json.Marshal(m)
	if err != nil {
		return err
	}
	err = l.db.Set(bucket, key, value)
	if err != nil {
		return err
	}
	return nil
}

type metricGlobalInfo struct {
	MapNum   uint64
	IndexMap map[uint64]*metricSummary
}

func newMetricGlobalInfo(num uint64) *metricGlobalInfo {
	info := metricGlobalInfo{
		MapNum:   num,
		IndexMap: make(map[uint64]*metricSummary),
	}

	for i := uint64(0); i < num; i++ {
		info.IndexMap[i] = newMetricSummary()
	}

	return &info
}

func (g *metricGlobalInfo) getMap(real_index uint64) *metricSummary {
	index := (real_index%g.MapNum + g.MapNum) % g.MapNum
	return g.IndexMap[index]
}

func (g *metricGlobalInfo) getAvailableMapIndex() []uint64 {
	indexList := make([]uint64, 0)
	for k, v := range g.IndexMap {
		if v.hasData() {
			indexList = append(indexList, k)
		}
	}
	return indexList
}

func (g *metricGlobalInfo) add(value *meta) {
	index := uint64(crc32.ChecksumIEEE(value.indexKey()))
	g.getMap(index).add(value.realKey(), value)
}

func (g *metricGlobalInfo) get(value *meta) *meta {
	index := uint64(crc32.ChecksumIEEE(value.indexKey()))
	return g.getMap(index).get(value.realKey())
}

type metricSummary struct {
	mu    sync.RWMutex
	cache map[string]*meta
}

func newMetricSummary() *metricSummary {
	return &metricSummary{cache: make(map[string]*meta)}
}

func (d *metricSummary) hasData() bool {
	d.mu.RLock()
	if len(d.cache) > 0 {
		d.mu.RUnlock()
		return true
	}
	d.mu.RUnlock()
	return false
}

func (d *metricSummary) getMapData() map[string]*meta {
	d.mu.Lock()
	metricList := d.cache
	d.cache = make(map[string]*meta, len(d.cache))
	d.mu.Unlock()
	return metricList
}
func (d *metricSummary) get(key string) *meta {
	d.mu.RLock()
	item, found := d.cache[key]
	d.mu.RUnlock()
	if found {
		return item
	}
	return nil
}

func (d *metricSummary) add(key string, value *meta) {
	d.mu.Lock()
	item, found := d.cache[key]
	if !found {
		d.cache[key] = value
	} else {
		d.merge(value, item)
	}
	d.mu.Unlock()
}

func (d *metricSummary) merge(inValue *meta, outValue *meta) {
	outValue.Ref = outValue.Ref + inValue.Ref

	if inValue.Del != metaDelete && inValue.Ref >= 0 {
		outValue.Timestamp = inValue.Timestamp
	}

	if inValue.Del == metaDelete && outValue.Ref <= 0 {

		outValue.Del = metaDelete
	}
	if inValue.FileSize > 0 {
		outValue.FileSize = inValue.FileSize
	}
	for key, val := range inValue.RefSandBoxIDs {
		if _, ok := outValue.RefSandBoxIDs[key]; ok {
			if val.Del {
				delete(outValue.RefSandBoxIDs, key)
			}
		} else {
			if outValue.RefSandBoxIDs == nil {
				outValue.RefSandBoxIDs = make(map[string]RefSandBox)
			}
			outValue.RefSandBoxIDs[key] = val
		}
	}
}

func (m meta) indexKey() []byte {
	k1 := strconv.FormatInt(int64(m.fileType), 10)
	size := 1 + len(k1) + len(m.userID) + len(m.fileSha256)
	b := make([]byte, 0, size)
	buf := bytes.NewBuffer(b)
	buf.WriteString(k1)
	buf.WriteString(m.userID)
	buf.WriteString(m.fileSha256)
	return buf.Bytes()
}

func (m meta) realKey() string {
	size := 1 + len(m.userID) + len(m.fileSha256) + len("|")
	b := make([]byte, 0, size)
	buf := bytes.NewBuffer(b)
	buf.WriteString(m.userID)
	buf.WriteString("|")
	buf.WriteString(m.fileSha256)
	return slice2Str(buf.Bytes())
}

type needDeleteType []*meta

func (m needDeleteType) Len() int {
	return len(m)
}

func (m needDeleteType) Less(i int, j int) bool {

	return m[i].Timestamp < m[j].Timestamp
}

func (m needDeleteType) Swap(i int, j int) {
	tmp := m[i]
	m[i] = m[j]
	m[j] = tmp
}

func slice2Str(b []byte) string {
	bh := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	sh := reflect.StringHeader{
		Data: bh.Data,
		Len:  bh.Len,
	}
	return *(*string)(unsafe.Pointer(&sh))
}

func lessByLastUsedTime(e1, e2 *meta) bool {
	return e1.Timestamp < e2.Timestamp
}

func lessBySize(e1, e2 *meta) bool {
	return e1.FileSize > e2.FileSize
}
