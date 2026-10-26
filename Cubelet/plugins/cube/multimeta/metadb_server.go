// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package multimeta

import (
	"context"
	"fmt"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	multimetadb "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
)

func (s *multimeta) Register(server *grpc.Server) error {
	multimetadb.RegisterMultiMetaDBServerServer(server, s)
	return nil
}

func (s *multimeta) RegisterTCP(server *grpc.Server) error {
	return s.Register(server)
}

func (s *multimeta) GetBucketDefines(ctx context.Context, header *multimetadb.CommonRequestHeader) (*multimetadb.GetBucketDefinesResponse, error) {
	var defines []*multimetadb.BucketDefine
	lock.RLock()
	defer lock.RUnlock()

	for _, bucket := range bucketMap {
		defines = append(defines, bucket.BucketDefine)
	}
	return &multimetadb.GetBucketDefinesResponse{
		Header: &multimetadb.CommonResponseHeader{
			Code:      0,
			Message:   "success",
			RequestID: header.RequestID,
		},
		BucketDefines: defines,
	}, nil
}

func (s *multimeta) GetStreamData(req *multimetadb.GetDataRequest, response multimetadb.MultiMetaDBServer_GetStreamDataServer) error {
	if len(req.Buckets) == 0 {
		return fmt.Errorf("buckets is empty")
	}

	var dbInstance MetadataDBAPI = s.CubeStore

	if req.DbName != "" {
		lock.RLock()
		bucketDefine, ok := bucketMap[req.DbName+"-"+req.Buckets[0]]
		lock.RUnlock()
		if !ok {
			return fmt.Errorf("db %s bucket %s not found", req.DbName, req.Buckets[0])
		}

		if bucketDefine.CubeStore != nil {
			dbInstance = bucketDefine.CubeStore
		}
	}

	var buckets [][]byte
	for _, bucket := range req.Buckets {
		buckets = append(buckets, []byte(bucket))
	}
	if req.Key == "" {
		dataMap, err := dbInstance.ReadAllBs(buckets...)
		if err != nil {
			return fmt.Errorf("failed to get data from %v: %v", req.Buckets, err)
		}
		response.SendHeader(metadata.New(map[string]string{
			"size": strconv.Itoa(len(dataMap)),
		}))
		for key, value := range dataMap {
			response.Send(&multimetadb.DbData{
				Buckets: req.Buckets,
				Key:     key,
				Value:   value,
			})
		}
	} else {
		data, err := dbInstance.GetBs(req.Key, buckets...)
		if err != nil {
			return fmt.Errorf("failed to get data from %v / %s: %v", req.Buckets, req.Key, err)
		}

		response.Send(&multimetadb.DbData{
			Buckets: req.Buckets,
			Key:     req.Key,
			Value:   data,
		})
	}
	return nil
}
