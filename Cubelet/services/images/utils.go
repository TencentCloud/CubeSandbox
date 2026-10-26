// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/multilock"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/volumefile"
)

func getCalleeAction(ft volumefile.FileType) string {
	switch ft {
	case volumefile.FtCode:
		return volumeCodeMetric
	case volumefile.FtLayer:
		return volumeLayerMetric
	case volumefile.FtLang:
		return volumeLangMetric
	case volumefile.FtLangExt4:
		return volumeLangExt4Metric
	default:
		return volumeLayerMetric
	}
}

func getBucketName(ft volumefile.FileType) string {
	switch ft {
	case volumefile.FtCode:
		return bucketKeyCode
	case volumefile.FtLayer:
		return bucketKeyLayer
	case volumefile.FtLang:
		return bucketKeyLang
	case volumefile.FtLangExt4:
		return bucketKeyLangExt4
	default:
		return ""
	}
}

func (l *volumeLocal) getMultiLock(ft volumefile.FileType, squashfsSha256 string) multilock.RWLock {
	switch ft {
	case volumefile.FtCode:
		return l.codeMultiLock.Get(squashfsSha256)
	case volumefile.FtLang, volumefile.FtLangExt4:
		return l.langMultiLock.Get(squashfsSha256)
	case volumefile.FtLayer:
		return l.layerMultiLock.Get(squashfsSha256)
	default:
		return l.codeMultiLock.Get(squashfsSha256)
	}
}

func (l *volumeLifetime) getMultiLock(ft volumefile.FileType, squashfsSha256 string) multilock.RWLock {
	return l.volumeLocalPtr.getMultiLock(ft, squashfsSha256)
}

func (l *volumeLifetime) getVolumeDir(ft volumefile.FileType, userID, sha256 string) string {
	return filepath.Join(l.volumeLocalPtr.baseVolumeDir(ft), userID, sha256)
}
