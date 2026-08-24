package cube

import (
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func rejectCallerSuppliedQos(req *types.CreateCubeSandboxReq) error {
	if req == nil {
		return nil
	}
	for _, key := range []string{constants.CubeAnnotationsNetWork, constants.CubeAnnotationsBlkQos, constants.CubeAnnotationsFSQos} {
		if _, ok := req.Annotations[key]; ok {
			return fmt.Errorf("%s is template-managed and cannot be supplied directly", key)
		}
	}
	return nil
}

func sanitizeTemplateCreateRequest(req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxReq {
	if req == nil {
		return nil
	}
	cloned := *req
	if req.Annotations != nil {
		cloned.Annotations = make(map[string]string, len(req.Annotations))
		for key, value := range req.Annotations {
			cloned.Annotations[key] = value
		}
		delete(cloned.Annotations, constants.CubeAnnotationsNetWork)
		delete(cloned.Annotations, constants.CubeAnnotationsBlkQos)
		delete(cloned.Annotations, constants.CubeAnnotationsFSQos)
	}
	return &cloned
}
