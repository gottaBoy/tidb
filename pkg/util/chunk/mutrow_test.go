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

package chunk

import (
	"testing"
	"time"

	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx/stmtctx"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/collate"
	"github.com/stretchr/testify/require"
)

func TestMutRow(t *testing.T) {
	allTypes := newAllTypes()
	mutRow := MutRowFromTypes(allTypes)
	row := mutRow.ToRow()
	sc := stmtctx.NewStmtCtx()
	for i := 0; i < row.Len(); i++ {
		val := zeroValForType(allTypes[i])
		d := row.GetDatum(i, allTypes[i])
		d2 := types.NewDatum(val)
		cmp, err := d.Compare(sc, &d2, collate.GetCollator(allTypes[i].GetCollate()))
		require.NoError(t, err)
		require.Equal(t, 0, cmp)
	}

	mutRow = MutRowFromValues("abc", 123)
	require.False(t, row.IsNull(0))
	require.Equal(t, "abc", mutRow.ToRow().GetString(0))
	require.False(t, row.IsNull(1))
	require.Equal(t, int64(123), mutRow.ToRow().GetInt64(1))
	mutRow.SetValues("abcd", 456)
	row = mutRow.ToRow()
	require.Equal(t, "abcd", row.GetString(0))
	require.False(t, row.IsNull(0))
	require.Equal(t, int64(456), row.GetInt64(1))
	require.False(t, row.IsNull(1))
	mutRow.SetDatums(types.NewStringDatum("defgh"), types.NewIntDatum(33))
	require.False(t, row.IsNull(0))
	require.Equal(t, "defgh", row.GetString(0))
	require.False(t, row.IsNull(1))
	require.Equal(t, int64(33), row.GetInt64(1))

	mutRow.SetRow(MutRowFromValues("foobar", nil).ToRow())
	row = mutRow.ToRow()
	require.False(t, row.IsNull(0))
	require.True(t, row.IsNull(1))

	nRow := MutRowFromValues(nil, 111).ToRow()
	require.True(t, nRow.IsNull(0))
	require.False(t, nRow.IsNull(1))
	mutRow.SetRow(nRow)
	row = mutRow.ToRow()
	require.True(t, row.IsNull(0))
	require.False(t, row.IsNull(1))

	j, err := types.ParseBinaryJSONFromString("true")
	time := types.NewTime(types.FromDate(2000, 1, 1, 1, 0, 0, 0), mysql.TypeDatetime, types.MaxFsp)
	require.NoError(t, err)
	mutRow = MutRowFromValues(j, time)
	row = mutRow.ToRow()
	require.Equal(t, j, row.GetJSON(0))
	require.Equal(t, time, row.GetTime(1))

	retTypes := []*types.FieldType{types.NewFieldType(mysql.TypeDuration)}
	chk := New(retTypes, 1, 1)
	dur, _, err := types.ParseDuration(sc, "01:23:45", 0)
	require.NoError(t, err)
	chk.AppendDuration(0, dur)
	mutRow = MutRowFromTypes(retTypes)
	mutRow.SetValue(0, dur)
	require.Equal(t, mutRow.c.columns[0].data, chk.columns[0].data)
	mutRow.SetDatum(0, types.NewDurationDatum(dur))
	require.Equal(t, mutRow.c.columns[0].data, chk.columns[0].data)
}

func TestIssue29947(t *testing.T) {
	allTypes := newAllTypes()
	mutRow := MutRowFromTypes(allTypes)
	nilDatum := types.NewDatum(nil)

	dataBefore := make([][]byte, 0, len(mutRow.c.columns))
	elemBufBefore := make([][]byte, 0, len(mutRow.c.columns))
	for _, col := range mutRow.c.columns {
		dataBefore = append(dataBefore, col.data)
		elemBufBefore = append(elemBufBefore, col.elemBuf)
	}
	for i, col := range mutRow.c.columns {
		mutRow.SetDatum(i, nilDatum)
		require.Equal(t, col.IsNull(0), true)
		for _, off := range col.offsets {
			require.Equal(t, off, int64(0))
		}
		require.Equal(t, col.data, dataBefore[i])
		require.Equal(t, col.elemBuf, elemBufBefore[i])
	}
}

func BenchmarkMutRowSetRow(b *testing.B) {
	b.ReportAllocs()
	rowChk := newChunk(8, 0)
	rowChk.AppendInt64(0, 1)
	rowChk.AppendString(1, "abcd")
	row := rowChk.GetRow(0)
	mutRow := MutRowFromValues(1, "abcd")
	for i := 0; i < b.N; i++ {
		mutRow.SetRow(row)
	}
}

func BenchmarkMutRowSetDatums(b *testing.B) {
	b.ReportAllocs()
	mutRow := MutRowFromValues(1, "abcd")
	datums := []types.Datum{types.NewDatum(1), types.NewDatum("abcd")}
	for i := 0; i < b.N; i++ {
		mutRow.SetDatums(datums...)
	}
}

func BenchmarkMutRowSetValues(b *testing.B) {
	b.ReportAllocs()
	mutRow := MutRowFromValues(1, "abcd")
	for i := 0; i < b.N; i++ {
		mutRow.SetValues(1, "abcd")
	}
}

func BenchmarkMutRowFromTypes(b *testing.B) {
	b.ReportAllocs()
	tps := []*types.FieldType{
		types.NewFieldType(mysql.TypeLonglong),
		types.NewFieldType(mysql.TypeVarchar),
	}
	for i := 0; i < b.N; i++ {
		MutRowFromTypes(tps)
	}
}

func BenchmarkMutRowFromDatums(b *testing.B) {
	b.ReportAllocs()
	datums := []types.Datum{types.NewDatum(1), types.NewDatum("abc")}
	for i := 0; i < b.N; i++ {
		MutRowFromDatums(datums)
	}
}

func BenchmarkMutRowFromValues(b *testing.B) {
	b.ReportAllocs()
	values := []interface{}{1, "abc"}
	for i := 0; i < b.N; i++ {
		MutRowFromValues(values)
	}
}

func TestMutRowShallowCopyPartialRow(t *testing.T) {
	colTypes := make([]*types.FieldType, 0, 3)
	colTypes = append(colTypes, types.NewFieldType(mysql.TypeVarString))
	colTypes = append(colTypes, types.NewFieldType(mysql.TypeLonglong))
	colTypes = append(colTypes, types.NewFieldType(mysql.TypeTimestamp))

	mutRow := MutRowFromTypes(colTypes)
	row := MutRowFromValues("abc", 123, types.ZeroTimestamp).ToRow()
	mutRow.ShallowCopyPartialRow(0, row)
	require.Equal(t, mutRow.ToRow().GetString(0), row.GetString(0))
	require.Equal(t, mutRow.ToRow().GetInt64(1), row.GetInt64(1))
	require.Equal(t, mutRow.ToRow().GetTime(2), row.GetTime(2))

	row.c.Reset()
	d := types.NewStringDatum("dfg")
	row.c.AppendDatum(0, &d)
	d = types.NewIntDatum(567)
	row.c.AppendDatum(1, &d)
	d = types.NewTimeDatum(types.NewTime(types.FromGoTime(time.Now()), mysql.TypeTimestamp, 6))
	row.c.AppendDatum(2, &d)

	require.Equal(t, mutRow.ToRow().GetTime(2), d.GetMysqlTime())
	require.Equal(t, mutRow.ToRow().GetString(0), row.GetString(0))
	require.Equal(t, mutRow.ToRow().GetInt64(1), row.GetInt64(1))
	require.Equal(t, mutRow.ToRow().GetTime(2), row.GetTime(2))
}

var rowsNum = 1024

func BenchmarkMutRowShallowCopyPartialRow(b *testing.B) {
	b.ReportAllocs()
	colTypes := make([]*types.FieldType, 0, 8)
	colTypes = append(colTypes, types.NewFieldType(mysql.TypeVarString))
	colTypes = append(colTypes, types.NewFieldType(mysql.TypeVarString))
	colTypes = append(colTypes, types.NewFieldType(mysql.TypeLonglong))
	colTypes = append(colTypes, types.NewFieldType(mysql.TypeLonglong))
	colTypes = append(colTypes, types.NewFieldType(mysql.TypeDatetime))

	mutRow := MutRowFromTypes(colTypes)
	row := MutRowFromValues("abc", "abcdefg", 123, 456, types.ZeroDatetime).ToRow()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < rowsNum; j++ {
			mutRow.ShallowCopyPartialRow(0, row)
		}
	}
}

func BenchmarkChunkAppendPartialRow(b *testing.B) {
	b.ReportAllocs()
	chk := newChunkWithInitCap(rowsNum, 0, 0, 8, 8, sizeTime)
	row := MutRowFromValues("abc", "abcdefg", 123, 456, types.ZeroDatetime).ToRow()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chk.Reset()
		for j := 0; j < rowsNum; j++ {
			chk.AppendPartialRow(0, row)
		}
	}
}
