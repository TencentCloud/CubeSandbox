// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package shimapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/apis/shimapi/shimtypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type ChDiskAPI interface {
	AddDisk(ctx context.Context, req *shimtypes.ChAddDiskRequest) (*shimtypes.ChAddDiskResponse, error)

	DelDisk(ctx context.Context, serial string) error
}

const (
	chAPIPrefix = "http://localhost/api/v1/vm."
)

func (csc *cubeShimControl) GetChAPIClient() *http.Client {

	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", csc.getChSockPath())
			},
		},
	}
}

func (csc *cubeShimControl) AddDisk(ctx context.Context, req *shimtypes.ChAddDiskRequest) (*shimtypes.ChAddDiskResponse, error) {
	if csc.cubebox.HotPlugDisk == nil {
		csc.cubebox.HotPlugDisk = make(map[string]*shimtypes.ChDiskDevice)
	}
	if _, ok := csc.cubebox.HotPlugDisk[req.Serial]; ok {
		return &csc.cubebox.HotPlugDisk[req.Serial].ChAddDiskResponse, nil
	}

	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("error marshaling request body: %v", err)
	}
	ipath := "vm.add-disk"
	resp, err := csc.chRequest(ctx, "add-disk", requestBody)
	if err != nil {
		return nil, fmt.Errorf("error sending request to %s: %v", ipath, err)
	}

	res := &shimtypes.ChAddDiskResponse{}
	err = json.Unmarshal([]byte(resp), res)
	if err != nil {
		return nil, fmt.Errorf("%s error unmarshaling response: %v", ipath, err)
	}

	csc.cubebox.HotPlugDisk[req.Serial] = &shimtypes.ChDiskDevice{
		ChAddDiskRequest:  *req,
		ChAddDiskResponse: *res,
	}
	return res, nil
}

func (csc *cubeShimControl) DelDisk(ctx context.Context, serial string) error {
	if csc.cubebox.HotPlugDisk == nil {
		csc.cubebox.HotPlugDisk = make(map[string]*shimtypes.ChDiskDevice)
	}
	dev, ok := csc.cubebox.HotPlugDisk[serial]
	if !ok {
		return fmt.Errorf("disk %s not found", serial)
	}

	requestBody, err := json.Marshal(dev.ChAddDiskResponse)
	if err != nil {
		return fmt.Errorf("error marshaling request body: %v", err)
	}
	ipath := "vm.remove-device"
	_, err = csc.chRequest(ctx, "remove-device", requestBody)
	if err != nil {
		return fmt.Errorf("error sending request to %s: %v", ipath, err)
	}

	return nil
}

func (csc *cubeShimControl) chRequest(ctx context.Context, path string, reqBody []byte) ([]byte, error) {
	url := chAPIPrefix + path

	hreq, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("Content-Type", "application/json")

	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"func":    path,
		"reqBody": string(reqBody),
		"chsock":  csc.getChSockPath(),
		"url":     url,
	})

	resp, err := csc.GetChAPIClient().Do(hreq)
	if err != nil {
		stepLog.Errorf("failed to request vm")
		return nil, fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %v", err)
	}
	stepLog.WithField("resp", string(body)).Debugf("request vm success")
	return body, nil
}
