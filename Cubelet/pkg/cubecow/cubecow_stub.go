// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !cgo

package cubecow

import "fmt"

const nativeBuildHint = "cubecow native support requires cgo; rebuild with CGO enabled after generating Cubelet/third_party/cubecow artifacts"

func Init(string) (*Engine, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}

func InitWithoutLogging(string) (*Engine, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}

func InitFromJSON(string) (*Engine, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}

func InitWithoutLoggingFromJSON(string) (*Engine, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}

func (e *Engine) Close() {}

func (e *Engine) ResetNodeStorage() error {
	return fmt.Errorf(nativeBuildHint)
}

func (e *Engine) CreateVolume(string, uint64) (string, error) {
	return "", fmt.Errorf(nativeBuildHint)
}

func (e *Engine) DeleteVolume(string) error {
	return fmt.Errorf(nativeBuildHint)
}

func (e *Engine) ResizeVolume(string, uint64) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf(nativeBuildHint)
}

func (e *Engine) GetVolumeInfo(string) (*Volume, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}

func (e *Engine) GetVolumeBlockInfo(string) (*VolumeBlockInfo, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}

func (e *Engine) ListVolumes(uint64, string) (*ListVolumesResult, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}

func (e *Engine) CreateSnapshot(string, string, bool) (string, error) {
	return "", fmt.Errorf(nativeBuildHint)
}

func (e *Engine) ActivateVolume(string) (string, error) {
	return "", fmt.Errorf(nativeBuildHint)
}

func (e *Engine) DeactivateVolume(string) error {
	return fmt.Errorf(nativeBuildHint)
}

func (e *Engine) DeleteSnapshot(string) error {
	return fmt.Errorf(nativeBuildHint)
}

func (e *Engine) ListSnapshots(string, uint64, string) (*ListSnapshotsResult, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}

func (e *Engine) GetMetrics() (map[string]uint64, error) {
	return nil, fmt.Errorf(nativeBuildHint)
}
