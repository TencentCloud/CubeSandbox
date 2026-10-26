// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"fmt"

	"k8s.io/client-go/tools/cache"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

var (
	templateIDIndexerKey = "TemplateID"

	imageNamespaceIDIndexerKey = "ImageNamespaceID"

	baseBlockNamespaceIDIndexerKey = "BaseBlockNamespaceID"

	snapshotIDIndexerKey = "BaseSnapshotID"

	indexer = cache.Indexers{
		templateIDIndexerKey:           templateIdIndex,
		imageNamespaceIDIndexerKey:     imageIDIndex,
		baseBlockNamespaceIDIndexerKey: baseBlockIDIndex,
		snapshotIDIndexerKey:           snapshotIDIndex,
	}
)

func taskIDKeyFunc(obj interface{}) (string, error) {
	o := obj.(*templatetypes.LocalRunTemplate)
	if o.DistributionTaskID == "" {
		return "", fmt.Errorf("template id is empty")
	}
	return o.DistributionTaskID, nil
}

func templateIdIndex(obj interface{}) ([]string, error) {
	o, ok := obj.(*templatetypes.LocalRunTemplate)
	if !ok {
		return nil, fmt.Errorf("obj is not *templatetypes.LocalRunTemplate")
	}
	return []string{o.TemplateID}, nil
}

func imageIDIndex(obj interface{}) ([]string, error) {
	o, ok := obj.(*templatetypes.LocalRunTemplate)
	if !ok {
		return nil, fmt.Errorf("obj is not *templatetypes.LocalRunTemplate")
	}
	var imageNamesapceIDs []string
	for _, image := range o.Images {

		imageNamesapceIDs = append(imageNamesapceIDs, o.Namespace+"/"+image.Image.ID)
	}
	return imageNamesapceIDs, nil
}

func baseBlockIDIndex(obj interface{}) ([]string, error) {
	o, ok := obj.(*templatetypes.LocalRunTemplate)
	if !ok {
		return nil, fmt.Errorf("obj is not *templatetypes.LocalRunTemplate")
	}
	var blockIDs []string
	for _, v := range o.Volumes {

		if v.Volume.BaseBlockSource.ID != "" {
			blockIDs = append(blockIDs, o.Namespace+"/"+v.Volume.BaseBlockSource.ID)
		}
	}
	return blockIDs, nil
}

func snapshotIDIndex(obj interface{}) ([]string, error) {
	o, ok := obj.(*templatetypes.LocalRunTemplate)
	if !ok {
		return nil, fmt.Errorf("obj is not *templatetypes.LocalRunTemplate")
	}
	return []string{o.Snapshot.Snapshot.ID}, nil
}
