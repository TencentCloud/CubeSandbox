// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"
	"time"

	jsoniter "github.com/json-iterator/go"
	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/urfave/cli/v2"
	"go.etcd.io/bbolt"
)

const nfsImageBucketName = "nfsimage/v1"

var (
	cubeletPath string
	statePath   string
)

var Fix = &cli.Command{
	Name:        "fix",
	Usage:       "CUBEMNT=1 cubecli image fix",
	UsageText:   "CUBEMNT=1 cubecli image fix, fix broken image.",
	Description: "fix broken image",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "skip-cfs",
			Usage: "skip to fix cfs image",
		},
		&cli.StringFlag{
			Name:  "cubelet-path",
			Usage: "cubelet work path",
			Value: "/data/cubelet",
		},
	},
	Action: func(context *cli.Context) error {
		_, ok := os.LookupEnv("CUBEMNT")
		if !ok {
			return fmt.Errorf("env CUBEMNT=1 not set")
		}
		cubeletPath = context.String("cubelet-path")
		statePath = path.Join(cubeletPath, "state", "io.cubelet.internal.v1.images")
		var (
			db  *utils.CubeStore
			err error
		)

		dbspath := filepath.Join(statePath, "db")
		dbdpath := filepath.Join(statePath, "db-tmp")
		err = exec.Command("cp", "-rf", dbspath, dbdpath).Run()
		if err != nil {
			return fmt.Errorf("cp db failed %s", err.Error())
		}
		defer os.RemoveAll(dbdpath)

		dbopt := &bbolt.Options{
			Timeout:   30 * time.Second,
			ReadOnly:  true,
			MmapFlags: syscall.MAP_SHARED,
		}
		if db, err = utils.NewCubeStoreExt(dbdpath, "meta.db", 10, dbopt); err != nil {
			return fmt.Errorf("init db %s failed %s", dbdpath, err.Error())
		}
		nfsImageMap, err := db.ReadAll(nfsImageBucketName)
		if err != nil {
			return fmt.Errorf("read nfs image failed %s", err.Error())
		}
		fmt.Printf("read nfs image success, total %d\n", len(nfsImageMap))

		for _, data := range nfsImageMap {
			img := &imagestore.Image{}
			err = jsoniter.Unmarshal(data, img)
			if err != nil {
				fmt.Printf("unmarshal nfs image failed %s\n", err.Error())
				continue
			}
			err = fixCfsImage(img)
			if err != nil {
				fmt.Printf("fix cfs image %s failed %s\n", img.ID, err.Error())
				continue
			}
		}
		fmt.Println("fix cfs image success")

		return err
	},
}

func fixCfsImage(img *imagestore.Image) error {
	if img.NfsRootfs == "" {
		return nil
	}

	if img.NfsRootfs == img.UidFiles {
		return nil
	}
	ufDir := img.UidFiles
	err := copySourceToDest(filepath.Join(img.NfsRootfs, "etc", "group"), filepath.Join(ufDir, "etc", "group"))
	if err != nil {
		return fmt.Errorf("copy group file failed: %s", err.Error())
	}
	err = copySourceToDest(filepath.Join(img.NfsRootfs, "etc", "passwd"), filepath.Join(ufDir, "etc", "passwd"))
	if err != nil {
		return fmt.Errorf("copy passwd file failed %s", err.Error())
	}
	return nil
}

func copySourceToDest(src, dest string) error {
	_, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {

			return nil
		}

		return fmt.Errorf("stat source file %s failed %s", src, err.Error())
	}
	_, err = os.Stat(dest)
	if err != nil {
		if os.IsNotExist(err) {

			if err := utils.SafeCopyFile(dest, src); err != nil {
				return fmt.Errorf("copy file %s to %s failed: %s", src, dest, err.Error())
			}
			fmt.Printf("copy file %s to %s success\n", src, dest)
			return nil
		}
		return fmt.Errorf("stat dest file %s failed %s", dest, err.Error())
	}
	return nil
}
