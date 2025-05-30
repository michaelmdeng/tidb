// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mockcopr

import (
	"context"

	"github.com/golang/protobuf/proto"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/planner/core/resolve"
	"github.com/pingcap/tidb/pkg/statistics"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/codec"
	"github.com/pingcap/tidb/pkg/util/collate"
	"github.com/pingcap/tidb/pkg/util/rowcodec"
	"github.com/pingcap/tidb/pkg/util/timeutil"
	"github.com/pingcap/tipb/go-tipb"
)

func (h coprHandler) handleCopAnalyzeRequest(req *coprocessor.Request) *coprocessor.Response {
	resp := &coprocessor.Response{}
	if len(req.Ranges) == 0 {
		return resp
	}
	if req.GetTp() != kv.ReqTypeAnalyze {
		return resp
	}
	if err := h.CheckRequestContext(req.GetContext()); err != nil {
		resp.RegionError = err
		return resp
	}
	analyzeReq := new(tipb.AnalyzeReq)
	err := proto.Unmarshal(req.Data, analyzeReq)
	if err != nil {
		resp.OtherError = err.Error()
		return resp
	}
	if analyzeReq.Tp == tipb.AnalyzeType_TypeIndex {
		resp, err = h.handleAnalyzeIndexReq(req, analyzeReq)
	} else {
		resp, err = h.handleAnalyzeColumnsReq(req, analyzeReq)
	}
	if err != nil {
		resp.OtherError = err.Error()
	}
	return resp
}

func (h coprHandler) handleAnalyzeIndexReq(req *coprocessor.Request, analyzeReq *tipb.AnalyzeReq) (*coprocessor.Response, error) {
	ranges, err := h.extractKVRanges(req.Ranges, false)
	if err != nil {
		return nil, errors.Trace(err)
	}
	startTS := req.StartTs
	if startTS == 0 {
		startTS = analyzeReq.GetStartTsFallback()
	}
	e := &indexScanExec{
		colsLen:        int(analyzeReq.IdxReq.NumColumns),
		kvRanges:       ranges,
		startTS:        startTS,
		isolationLevel: h.GetIsolationLevel(),
		mvccStore:      h.GetMVCCStore(),
		IndexScan:      &tipb.IndexScan{Desc: false},
		execDetail:     new(execDetail),
		hdStatus:       tablecodec.HandleNotNeeded,
	}

	tz, err := timeutil.ConstructTimeZone("", int(analyzeReq.TimeZoneOffset))
	if err != nil {
		return nil, errors.Trace(err)
	}
	sctx := flagsAndTzToSessionContext(analyzeReq.Flags, tz)
	sc := sctx.GetSessionVars().StmtCtx
	statsBuilder := statistics.NewSortedBuilder(sc, analyzeReq.IdxReq.BucketSize, 0, types.NewFieldType(mysql.TypeBlob), statistics.Version1)
	var cms *statistics.CMSketch
	if analyzeReq.IdxReq.CmsketchDepth != nil && analyzeReq.IdxReq.CmsketchWidth != nil {
		cms = statistics.NewCMSketch(*analyzeReq.IdxReq.CmsketchDepth, *analyzeReq.IdxReq.CmsketchWidth)
	}
	ctx := context.TODO()
	var values [][]byte
	for {
		values, err = e.Next(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if values == nil {
			break
		}
		var value []byte
		for _, val := range values {
			value = append(value, val...)
			if cms != nil {
				cms.InsertBytes(value)
			}
		}
		err = statsBuilder.Iterate(types.NewBytesDatum(value))
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	hg := statistics.HistogramToProto(statsBuilder.Hist())
	var cm *tipb.CMSketch
	if cms != nil {
		cm = statistics.CMSketchToProto(cms, nil)
	}
	data, err := proto.Marshal(&tipb.AnalyzeIndexResp{Hist: hg, Cms: cm})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &coprocessor.Response{Data: data}, nil
}

type analyzeColumnsExec struct {
	tblExec *tableScanExec
	fields  []*resolve.ResultField
}

func (h coprHandler) handleAnalyzeColumnsReq(req *coprocessor.Request, analyzeReq *tipb.AnalyzeReq) (_ *coprocessor.Response, err error) {
	tz, err := timeutil.ConstructTimeZone("", int(analyzeReq.TimeZoneOffset))
	if err != nil {
		return nil, errors.Trace(err)
	}

	sctx := flagsAndTzToSessionContext(analyzeReq.Flags, tz)

	evalCtx := &evalContext{sctx: sctx}
	columns := analyzeReq.ColReq.ColumnsInfo
	evalCtx.setColumnInfo(columns)
	ranges, err := h.extractKVRanges(req.Ranges, false)
	if err != nil {
		return nil, errors.Trace(err)
	}
	startTS := req.StartTs
	if startTS == 0 {
		startTS = analyzeReq.GetStartTsFallback()
	}
	colInfos := make([]rowcodec.ColInfo, len(columns))
	for i := range columns {
		col := columns[i]
		colInfos[i] = rowcodec.ColInfo{
			ID:         col.ColumnId,
			Ft:         evalCtx.fieldTps[i],
			IsPKHandle: col.GetPkHandle(),
		}
	}
	defVal := func(i int) ([]byte, error) {
		col := columns[i]
		if col.DefaultVal == nil {
			return nil, nil
		}
		// col.DefaultVal always be  varint `[flag]+[value]`.
		if len(col.DefaultVal) < 1 {
			panic("invalid default value")
		}
		return col.DefaultVal, nil
	}
	rd := rowcodec.NewByteDecoder(colInfos, []int64{-1}, defVal, nil)
	e := &analyzeColumnsExec{
		tblExec: &tableScanExec{
			TableScan:      &tipb.TableScan{Columns: columns},
			kvRanges:       ranges,
			colIDs:         evalCtx.colIDs,
			startTS:        startTS,
			isolationLevel: h.GetIsolationLevel(),
			mvccStore:      h.GetMVCCStore(),
			execDetail:     new(execDetail),
			rd:             rd,
		},
	}
	e.fields = make([]*resolve.ResultField, len(columns))
	for i := range e.fields {
		rf := new(resolve.ResultField)
		rf.Column = new(model.ColumnInfo)
		ft := types.FieldType{}
		ft.SetType(mysql.TypeBlob)
		ft.SetFlen(mysql.MaxBlobWidth)
		ft.SetCharset(mysql.DefaultCharset)
		ft.SetCollate(mysql.DefaultCollationName)
		rf.Column.FieldType = ft
		e.fields[i] = rf
	}

	pkID := int64(-1)
	numCols := len(columns)
	if columns[0].GetPkHandle() {
		pkID = columns[0].ColumnId
		columns = columns[1:]
		numCols--
	}
	collators := make([]collate.Collator, numCols)
	fts := make([]*types.FieldType, numCols)
	for i, col := range columns {
		ft := fieldTypeFromPBColumn(col)
		fts[i] = ft
		if ft.EvalType() == types.ETString {
			collators[i] = collate.GetCollator(ft.GetCollate())
		}
	}
	colReq := analyzeReq.ColReq
	builder := statistics.SampleBuilder{
		Sc:              sctx.GetSessionVars().StmtCtx,
		RecordSet:       e,
		ColLen:          numCols,
		MaxBucketSize:   colReq.BucketSize,
		MaxFMSketchSize: colReq.SketchSize,
		MaxSampleSize:   colReq.SampleSize,
		Collators:       collators,
		ColsFieldType:   fts,
	}
	if pkID != -1 {
		builder.PkBuilder = statistics.NewSortedBuilder(sctx.GetSessionVars().StmtCtx, builder.MaxBucketSize, pkID, types.NewFieldType(mysql.TypeBlob), statistics.Version1)
	}
	if colReq.CmsketchWidth != nil && colReq.CmsketchDepth != nil {
		builder.CMSketchWidth = *colReq.CmsketchWidth
		builder.CMSketchDepth = *colReq.CmsketchDepth
	}
	collectors, pkBuilder, err := builder.CollectColumnStats()
	if err != nil {
		return nil, errors.Trace(err)
	}
	colResp := &tipb.AnalyzeColumnsResp{}
	if pkID != -1 {
		colResp.PkHist = statistics.HistogramToProto(pkBuilder.Hist())
	}
	for _, c := range collectors {
		colResp.Collectors = append(colResp.Collectors, statistics.SampleCollectorToProto(c))
	}
	data, err := proto.Marshal(colResp)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &coprocessor.Response{Data: data}, nil
}

// Fields implements the sqlexec.RecordSet Fields interface.
func (e *analyzeColumnsExec) Fields() []*resolve.ResultField {
	return e.fields
}

func (e *analyzeColumnsExec) getNext(ctx context.Context) ([]types.Datum, error) {
	values, err := e.tblExec.Next(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if values == nil {
		return nil, nil
	}
	datumRow := make([]types.Datum, 0, len(values))
	for _, val := range values {
		d := types.NewBytesDatum(val)
		if len(val) == 1 && val[0] == codec.NilFlag {
			d.SetNull()
		}
		datumRow = append(datumRow, d)
	}
	return datumRow, nil
}

func (e *analyzeColumnsExec) Next(ctx context.Context, req *chunk.Chunk) error {
	req.Reset()
	row, err := e.getNext(ctx)
	if row == nil || err != nil {
		return errors.Trace(err)
	}
	for i := range row {
		req.AppendDatum(i, &row[i])
	}
	return nil
}

func (e *analyzeColumnsExec) NewChunk(_ chunk.Allocator) *chunk.Chunk {
	fields := make([]*types.FieldType, 0, len(e.fields))
	for _, field := range e.fields {
		fields = append(fields, &field.Column.FieldType)
	}
	return chunk.NewChunkWithCapacity(fields, 1)
}

// Close implements the sqlexec.RecordSet Close interface.
func (e *analyzeColumnsExec) Close() error {
	return nil
}
