/*
Copyright 2022 The Numaproj Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package service is built for querying metadata and to expose it over daemon service.
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/numaproj/numaflow/pkg/apis/numaflow/v1alpha1"
	"github.com/numaproj/numaflow/pkg/apis/proto/daemon"
	"github.com/numaproj/numaflow/pkg/isbsvc"
	"github.com/numaproj/numaflow/pkg/watermark/fetch"
)

// GetEdgeWatermarkFetchers returns a map of the watermark fetchers, where key is the buffer name,
// value is a list of fetchers to the buffers.
func GetEdgeWatermarkFetchers(ctx context.Context, pipeline *v1alpha1.Pipeline, isbSvcClient isbsvc.ISBService) (map[v1alpha1.Edge][]fetch.Fetcher, error) {
	var wmFetchers = make(map[v1alpha1.Edge][]fetch.Fetcher)
	if pipeline.Spec.Watermark.Disabled {
		return wmFetchers, nil
	}

	for _, edge := range pipeline.ListAllEdges() {
		bucketName := v1alpha1.GenerateEdgeBucketName(pipeline.Namespace, pipeline.Name, edge.From, edge.To)
		isReduce := pipeline.GetVertex(edge.To).IsReduceUDF()
		partitionCount := pipeline.GetVertex(edge.To).GetPartitionCount()
		wmFetcherList, err := isbSvcClient.CreateWatermarkFetcher(ctx, bucketName, partitionCount, isReduce)
		if err != nil {
			return nil, fmt.Errorf("failed to create watermark fetcher  %w", err)
		}
		wmFetchers[edge] = wmFetcherList
	}
	return wmFetchers, nil
}

// GetPipelineWatermarks is used to return the head watermarks for a given pipeline.
func (ps *pipelineMetadataQuery) GetPipelineWatermarks(ctx context.Context, request *daemon.GetPipelineWatermarksRequest) (*daemon.GetPipelineWatermarksResponse, error) {
	resp := new(daemon.GetPipelineWatermarksResponse)
	isWatermarkEnabled := !ps.pipeline.Spec.Watermark.Disabled

	// If watermark is not enabled, return time zero
	if ps.pipeline.Spec.Watermark.Disabled {
		timeZero := time.Unix(0, 0).UnixMilli()
		watermarkArr := make([]*daemon.EdgeWatermark, len(ps.watermarkFetchers))
		i := 0
		for k := range ps.watermarkFetchers {
			edgeName := k.GetEdgeName()
			watermarks := make([]int64, len(ps.watermarkFetchers[k]))
			for idx := range watermarks {
				watermarks[idx] = timeZero
			}
			watermarkArr[i] = &daemon.EdgeWatermark{
				Pipeline:           &ps.pipeline.Name,
				Edge:               &edgeName,
				Watermarks:         watermarks,
				IsWatermarkEnabled: &isWatermarkEnabled,
			}
			i++
		}
		resp.PipelineWatermarks = watermarkArr
		return resp, nil
	}

	// Watermark is enabled
	watermarkArr := make([]*daemon.EdgeWatermark, len(ps.watermarkFetchers))
	i := 0
	for k, edgeFetchers := range ps.watermarkFetchers {
		var latestWatermarks []int64
		for _, fetcher := range edgeFetchers {
			if ps.pipeline.GetVertex(k.To).IsReduceUDF() {
				watermark := fetcher.GetHeadWatermark(0).UnixMilli()
				latestWatermarks = append(latestWatermarks, watermark)
			} else {
				for idx := 0; idx < ps.pipeline.GetVertex(k.To).GetPartitionCount(); idx++ {
					watermark := fetcher.GetHeadWatermark(int32(idx)).UnixMilli()
					latestWatermarks = append(latestWatermarks, watermark)
				}
			}
		}

		edgeName := k.GetEdgeName()
		watermarkArr[i] = &daemon.EdgeWatermark{
			Pipeline:           &ps.pipeline.Name,
			Edge:               &edgeName,
			Watermarks:         latestWatermarks,
			IsWatermarkEnabled: &isWatermarkEnabled,
		}
		i++
	}
	resp.PipelineWatermarks = watermarkArr
	return resp, nil
}
