// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package stream

import (
	"container/heap"
	"context"
	"sync"

	"go.uber.org/multierr"

	"github.com/apache/skywalking-banyandb/banyand/internal/storage"
	"github.com/apache/skywalking-banyandb/banyand/protector"
	"github.com/apache/skywalking-banyandb/pkg/cgroups"
	"github.com/apache/skywalking-banyandb/pkg/logger"
	pbv1 "github.com/apache/skywalking-banyandb/pkg/pb/v1"
	"github.com/apache/skywalking-banyandb/pkg/pool"
	"github.com/apache/skywalking-banyandb/pkg/query/model"
)

var _ model.StreamQueryResult = (*tsResult)(nil)

type tsResult struct {
	sm       *stream
	pm       *protector.Memory
	l        *logger.Logger
	segments []storage.Segment[*tsTable, option]
	series   []*pbv1.Series
	shards   []*model.StreamResult
	qo       queryOptions
	asc      bool
}

func (t *tsResult) Pull(ctx context.Context) *model.StreamResult {
	if len(t.segments) == 0 {
		return &model.StreamResult{}
	}
	if err := t.scanSegment(ctx); err != nil {
		return &model.StreamResult{Error: err}
	}
	var err error
	for i := range t.shards {
		if t.shards[i].Error != nil {
			err = multierr.Append(err, t.shards[i].Error)
		}
	}
	if err != nil {
		return &model.StreamResult{Error: err}
	}
	return model.MergeStreamResults(t.shards, t.qo.MaxElementSize, t.asc)
}

func (t *tsResult) scanSegment(ctx context.Context) error {
	var segment storage.Segment[*tsTable, option]
	if t.asc {
		segment = t.segments[len(t.segments)-1]
		t.segments = t.segments[:len(t.segments)-1]
	} else {
		segment = t.segments[0]
		t.segments = t.segments[1:]
	}

	bs := blockScanner{
		segment: segment,
		qo:      t.qo,
		series:  t.series,
		pm:      t.pm,
		l:       t.l,
	}
	defer bs.close()
	if err := bs.searchSeries(ctx); err != nil {
		return err
	}
	workerSize := cgroups.CPUs()
	var workerWg sync.WaitGroup
	batchCh := make(chan *blockScanResultBatch, workerSize)
	workerWg.Add(workerSize)
	if t.shards == nil {
		t.shards = make([]*model.StreamResult, workerSize)
		for i := range t.shards {
			t.shards[i] = model.NewStreamResult(t.qo.MaxElementSize, t.asc)
		}
	} else {
		for i := range t.shards {
			t.shards[i].Reset()
		}
	}
	for i := 0; i < workerSize; i++ {
		go func(workerID int) {
			tmpBlock := generateBlock()
			defer releaseBlock(tmpBlock)
			blockHeap := generateBlockCursorHeap(t.asc)
			defer releaseBlockCursorHeap(blockHeap)
			tmpResult := model.NewStreamResult(t.qo.MaxElementSize, t.asc)
			for batch := range batchCh {
				if batch.err != nil {
					t.shards[workerID].Error = batch.err
					releaseBlockScanResultBatch(batch)
					continue
				}
				for _, bs := range batch.bss {
					bc := generateBlockCursor()
					bc.init(bs.p, &bs.bm, bs.qo)
					if loadBlockCursor(bc, tmpBlock, bs.qo, t.sm) {
						if !t.asc {
							bc.idx = len(bc.timestamps) - 1
						}
						blockHeap.Push(bc)
					}
				}
				releaseBlockScanResultBatch(batch)
				heap.Init(blockHeap)
				result := blockHeap.merge(t.qo.MaxElementSize)
				t.shards[workerID].CopyFrom(tmpResult, result)
				blockHeap.reset()
			}
			workerWg.Done()
		}(i)
	}

	var scannerWg sync.WaitGroup
	finalizers := bs.scanShardsInParallel(ctx, &scannerWg, batchCh)
	scannerWg.Wait()
	close(batchCh)
	workerWg.Wait()
	for i := range finalizers {
		if finalizers[i] != nil {
			finalizers[i]()
		}
	}
	return nil
}

func loadBlockCursor(bc *blockCursor, tmpBlock *block, qo queryOptions, sm *stream) bool {
	tmpBlock.reset()
	if !bc.loadData(tmpBlock) {
		releaseBlockCursor(bc)
		return false
	}
	entityValues := qo.seriesToEntity[bc.bm.seriesID]

	tagFamilyMap := make(map[string]int)
	for idx, tagFamily := range bc.tagFamilies {
		tagFamilyMap[tagFamily.name] = idx + 1
	}
	for _, tagFamilyProj := range bc.tagProjection {
		for j, tagProj := range tagFamilyProj.Names {
			tagSpec := sm.tagMap[tagProj]
			if tagSpec.IndexedOnly {
				continue
			}
			entityPos := sm.entityMap[tagProj]
			if entityPos == 0 {
				continue
			}
			tagFamilyPos := tagFamilyMap[tagFamilyProj.Family]
			if tagFamilyPos == 0 {
				bc.tagFamilies[tagFamilyPos-1] = tagFamily{
					name: tagFamilyProj.Family,
					tags: make([]tag, 0),
				}
			}
			valueType := pbv1.MustTagValueToValueType(entityValues[entityPos-1])
			bc.tagFamilies[tagFamilyPos-1].tags[j] = tag{
				name:      tagProj,
				values:    mustEncodeTagValue(tagProj, tagSpec.GetType(), entityValues[entityPos-1], len(bc.timestamps)),
				valueType: valueType,
			}
		}
	}
	return true
}

func (t *tsResult) Release() {
	for i := range t.segments {
		t.segments[i].DecRef()
	}
}

type blockCursorHeap struct {
	bcc []*blockCursor
	asc bool
}

func (bch blockCursorHeap) Len() int {
	return len(bch.bcc)
}

func (bch blockCursorHeap) Less(i, j int) bool {
	leftIdx, rightIdx := bch.bcc[i].idx, bch.bcc[j].idx
	leftTS := bch.bcc[i].timestamps[leftIdx]
	rightTS := bch.bcc[j].timestamps[rightIdx]
	if bch.asc {
		return leftTS < rightTS
	}
	return leftTS > rightTS
}

func (bch *blockCursorHeap) Swap(i, j int) {
	bch.bcc[i], bch.bcc[j] = bch.bcc[j], bch.bcc[i]
}

func (bch *blockCursorHeap) Push(x interface{}) {
	bch.bcc = append(bch.bcc, x.(*blockCursor))
}

func (bch *blockCursorHeap) Pop() interface{} {
	old := bch.bcc
	n := len(old)
	x := old[n-1]
	bch.bcc = old[0 : n-1]
	releaseBlockCursor(x)
	return x
}

func (bch *blockCursorHeap) reset() {
	for i := range bch.bcc {
		releaseBlockCursor(bch.bcc[i])
	}
	bch.bcc = bch.bcc[:0]
}

func (bch *blockCursorHeap) merge(limit int) *model.StreamResult {
	step := -1
	if bch.asc {
		step = 1
	}
	result := &model.StreamResult{}

	for bch.Len() > 0 {
		topBC := bch.bcc[0]
		topBC.copyTo(result)
		if result.Len() >= limit {
			break
		}
		topBC.idx += step

		if bch.asc {
			if topBC.idx >= len(topBC.timestamps) {
				heap.Pop(bch)
			} else {
				heap.Fix(bch, 0)
			}
		} else {
			if topBC.idx < 0 {
				heap.Pop(bch)
			} else {
				heap.Fix(bch, 0)
			}
		}
	}

	return result
}

var blockCursorHeapPool = pool.Register[*blockCursorHeap]("stream-blockCursorHeap")

func generateBlockCursorHeap(asc bool) *blockCursorHeap {
	v := blockCursorHeapPool.Get()
	if v == nil {
		return &blockCursorHeap{
			asc: asc,
			bcc: make([]*blockCursor, 0, blockScannerBatchSize),
		}
	}
	v.asc = asc
	return v
}

func releaseBlockCursorHeap(bch *blockCursorHeap) {
	bch.reset()
	blockCursorHeapPool.Put(bch)
}
