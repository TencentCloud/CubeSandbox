// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package backup

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-multierror"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type Local struct {
	BackupPeriod time.Duration
	jobs         []BackupFilePair

	triggerCh chan struct{}
}

type BackupFilePair struct {
	Locker    func(func())
	Source    string
	TargetDir string
}

func (l *Local) Backup() {
	CubeLog.Debugf("Trigger backup to run")
	select {
	case l.triggerCh <- struct{}{}:
	default:

	}
}

func (l *Local) Run(ctx context.Context) {
	t := time.NewTicker(l.BackupPeriod)
	for {
		select {
		case <-t.C:
		case <-l.triggerCh:
		case <-ctx.Done():
			return
		}

		err := l.backup()
		if err != nil {
			log.G(ctx).Errorf("backup: %v", err)
		} else {
			log.G(ctx).Infof("Successfully backup")
		}
	}
}

func (l *Local) backup() error {
	defer utils.Recover()
	var result *multierror.Error
	for _, p := range l.jobs {
		var err error
		p.Locker(func() {
			dstFile := filepath.Join(p.TargetDir, path.Base(p.Source))

			err = utils.SafeCopyFile(dstFile, p.Source)
		})
		if err != nil {
			result = multierror.Append(result, fmt.Errorf("backup %v: %v", p.Source, err))
		}
	}

	return result.ErrorOrNil()
}
