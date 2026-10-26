// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func sysWrite(filename string, data []byte) error {
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0200)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err1 := f.Close(); err1 != nil && err == nil {
		err = err1
	}
	return err
}

func DetachVirtioDevice(ctx context.Context, pciBus string) error {
	return sysWrite(fmt.Sprintf("/sys/bus/pci/devices/%s/driver/unbind", pciBus), []byte(pciBus))
}

func AttachVFIODriver(ctx context.Context, pciBus string) error {
	output, errmsg, err := Exec(fmt.Sprintf("lspci -n -s %s | awk '{print $NF}' | awk -F: '{print $1,$2}'", pciBus), 1000)
	if err != nil {
		return fmt.Errorf("AttachVFIODriver failed: %s, %s, %w", output, errmsg, err)
	}
	return sysWrite("/sys/bus/pci/drivers/vfio-pci/new_id", []byte(strings.TrimSpace(output)))
}

func DetachVFIODriver(ctx context.Context, pciBus string) error {
	err := sysWrite("/sys/bus/pci/drivers/vfio-pci/unbind", []byte(pciBus))
	if err != nil {
		return fmt.Errorf("detach vfio driver failed: %w", err)
	}
	return sysWrite("/sys/bus/pci/drivers_probe", []byte(pciBus))
}

const (
	Vfio_pci   = "vfio-pci"
	Virtio_pci = "virtio-pci"
)

func InitDriverId(ctx context.Context, driver string) error {
	netVendorID := "1af4 1000"
	blkVendorID := "1af4 1001"
	err := sysWrite(fmt.Sprintf("/sys/bus/pci/drivers/%s/new_id", driver), []byte(netVendorID))
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("sysWrite new_id failed: %w", err)
	}
	err = sysWrite(fmt.Sprintf("/sys/bus/pci/drivers/%s/new_id", driver), []byte(blkVendorID))
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("sysWrite new_id failed: %w", err)
	}
	return nil
}

func IsDriverAttached(ctx context.Context, pciBus string, driver string) (eq bool, exist bool, err error) {
	oldDriver, err := os.Readlink(fmt.Sprintf("/sys/bus/pci/devices/%s/driver", pciBus))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false, nil
		}
		return false, false, err
	}

	realName := filepath.Base(oldDriver)
	if realName == driver {
		return true, true, nil
	}
	return false, true, nil
}

func BindDriver(ctx context.Context, pciBus string, driver string) error {
	same, exist, err := IsDriverAttached(ctx, pciBus, driver)
	if err != nil {
		return err
	} else if same {
		return nil
	}

	if exist {
		err := sysWrite(fmt.Sprintf("/sys/bus/pci/devices/%s/driver/unbind", pciBus), []byte(pciBus))
		if err != nil {
			return fmt.Errorf("sysWrite unbind failed: %w", err)
		}
	}

	err = sysWrite(fmt.Sprintf("/sys/bus/pci/drivers/%s/bind", driver), []byte(pciBus))
	if err != nil {
		return fmt.Errorf("sysWrite bind(%s) failed: %w", driver, err)
	}
	return nil
}
