/*
Copyright 2023 The Numaproj Authors.

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

package fetch

import (
	"context"
	"math"
	"time"

	"github.com/numaproj/numaflow/pkg/isb"
	"github.com/numaproj/numaflow/pkg/shared/logging"
	"github.com/numaproj/numaflow/pkg/watermark/wmb"
	"go.uber.org/zap"
)

// a set of EdgeFetchers, incoming to a Vertex
// (In the case of a Join Vertex, there are multiple incoming Edges)
// key=name of From Vertex
type edgeFetcherSet struct {
	edgeFetchers map[string]Fetcher
	log          *zap.SugaredLogger
}

func NewEdgeFetcherSet(ctx context.Context, edgeFetchers map[string]Fetcher) Fetcher {
	return &edgeFetcherSet{
		edgeFetchers,
		logging.FromContext(ctx),
	}
}

func (efs *edgeFetcherSet) ProcessOffsetGetWatermark(inputOffset isb.Offset, fromPartitionIdx int32) wmb.Watermark {
	_ = efs.ProcessOffset(inputOffset, fromPartitionIdx) // even if it errored, we'll keep going
	return efs.GetWatermark()
}

// GetWatermark processes the Watermark for the given partition from the given offset
func (efs *edgeFetcherSet) ProcessOffset(inputOffset isb.Offset, fromPartitionIdx int32) error {
	var returnErr error
	for _, fetcher := range efs.edgeFetchers {
		err := fetcher.ProcessOffset(inputOffset, fromPartitionIdx)
		if err != nil {
			returnErr = err // instead of returning error immediately, first try processing offset for all edge fetchers
		}
	}
	return returnErr
}

// GetHeadWatermark returns the latest watermark among all processors for the given partition.
// This can be used in showing the watermark
// progression for a vertex when not consuming the messages directly (eg. UX, tests)
func (efs *edgeFetcherSet) GetHeadWatermark(fromPartitionIdx int32) wmb.Watermark {
	// get the most conservative time (minimum watermark) across all Edges
	var wm wmb.Watermark
	overallWatermark := wmb.Watermark(time.UnixMilli(math.MaxInt64))
	for fromVertex, fetcher := range efs.edgeFetchers {
		wm = fetcher.GetHeadWatermark(fromPartitionIdx)
		if wm == wmb.InitialWatermark { // unset
			continue
		}
		efs.log.Debugf("Got Edge Head Watermark from vertex=%q while processing partition %d: %v", fromVertex, fromPartitionIdx, wm.UnixMilli())
		if wm.BeforeWatermark(overallWatermark) {
			overallWatermark = wm
		}
	}
	return overallWatermark
}

// GetHeadWMB returns the latest idle WMB with the smallest watermark for the given partition
// Only returns one if all Publishers are idle and if it's the smallest one of any partitions
func (efs *edgeFetcherSet) GetHeadWMB(fromPartitionIdx int32) wmb.WMB {
	// if we get back one that's empty it means that there could be one that's not Idle, so we need to return empty

	// call GetHeadWMB() for all Edges and get the smallest one
	var watermarkBuffer, unsetWMB wmb.WMB
	var overallHeadWMB = wmb.WMB{
		// we find the head WMB based on watermark
		Offset:    math.MaxInt64,
		Watermark: math.MaxInt64,
	}

	for fromVertex, fetcher := range efs.edgeFetchers {
		watermarkBuffer = fetcher.GetHeadWMB(fromPartitionIdx)
		if watermarkBuffer == unsetWMB { // unset
			return wmb.WMB{}
		}
		efs.log.Debugf("Got Edge Head WMB from vertex=%q while processing partition %d: %v", fromVertex, fromPartitionIdx, watermarkBuffer)
		if watermarkBuffer.Watermark != -1 {
			// find the smallest head offset of the smallest WMB.watermark (though latest)
			if watermarkBuffer.Watermark < overallHeadWMB.Watermark {
				overallHeadWMB = watermarkBuffer
			} else if watermarkBuffer.Watermark == overallHeadWMB.Watermark && watermarkBuffer.Offset < overallHeadWMB.Offset {
				overallHeadWMB = watermarkBuffer
			}
		}
	}

	// we only consider idle watermark if it is smaller than or equal to min of all the last processed watermarks.

	if overallHeadWMB.Watermark > efs.GetWatermark().UnixMilli() {
		return wmb.WMB{}
	}
	return overallHeadWMB

}

func (efs *edgeFetcherSet) GetWatermark() wmb.Watermark {
	// get the most conservative time (minimum watermark) across all Edges
	var wm wmb.Watermark
	overallWatermark := wmb.Watermark(time.UnixMilli(math.MaxInt64))
	for fromVertex, fetcher := range efs.edgeFetchers {
		wm = fetcher.GetWatermark()
		efs.log.Debugf("Got Edge watermark from vertex=%q: %v", fromVertex, wm.UnixMilli())
		if wm.BeforeWatermark(overallWatermark) {
			overallWatermark = wm
		}
	}
	return overallWatermark
}

func (efs *edgeFetcherSet) Close() error {
	for _, fetcher := range efs.edgeFetchers {
		err := fetcher.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
