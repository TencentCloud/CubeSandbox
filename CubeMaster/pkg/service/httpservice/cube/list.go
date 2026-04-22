// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func handleListAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	rsp := &types.ListCubeSandboxRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	defer func() {
		rt.RetCode = int64(rsp.Ret.RetCode)
	}()
	req := &types.ListCubeSandboxReq{}

	err := utils.DecodeHttpBody(r.Body, req)
	if err != nil {
		if errors.Is(err, io.EOF) {
			queries := r.URL.Query()
			req.RequestID = queries.Get("requestID")
			req.HostID = queries.Get("host_id")
			req.InstanceType = queries.Get("instance_type")
			idx, err := strconv.ParseInt(queries.Get("start_idx"), 10, 64)
			if err == nil {
				req.StartIdx = int(idx)
			}
			num, err := strconv.ParseInt(queries.Get("size"), 10, 64)
			if err == nil {
				req.Size = int(num)
			}

			filters := make(map[string]string)
			filterParams := queries.Get("filter.label_selector")
			for _, labels := range strings.Split(filterParams, ",") {
				if len(labels) > 0 {
					kv := strings.Split(labels, "=")
					if len(kv) >= 2 {
						filters[kv[0]] = kv[1]
					}
				}
			}
			if len(filters) > 0 {
				req.Filter = &types.CubeSandboxFilter{
					LabelSelector: filters,
				}
			}
		} else {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = err.Error()
			return rsp
		}
	}

	rt.RequestID = req.RequestID
	if req.InstanceType == "" {
		req.InstanceType = cubebox.InstanceType_cubebox.String()
	}
	rt.InstanceType = req.InstanceType
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
	}))
	rsp = sandbox.ListSandbox(ctx, req)
	return rsp
}
