// Copyright 2015 PingCAP, Inc.
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

package codec

import (
	"bytes"
	"encoding/binary"
	"hash"
	"io"
	"time"
	"unsafe"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/planner/cascades/base"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/collate"
	"github.com/pingcap/tidb/pkg/util/hack"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"go.uber.org/zap"
)

// First byte in the encoded value which specifies the encoding type.
const (
	NilFlag           byte = 0
	bytesFlag         byte = 1
	compactBytesFlag  byte = 2
	intFlag           byte = 3
	uintFlag          byte = 4
	floatFlag         byte = 5
	decimalFlag       byte = 6
	durationFlag      byte = 7
	varintFlag        byte = 8
	uvarintFlag       byte = 9
	jsonFlag          byte = 10
	vectorFloat32Flag byte = 20
	maxFlag           byte = 250
)

// IntHandleFlag is only used to encode int handle key.
const IntHandleFlag = intFlag

const (
	sizeUint64  = unsafe.Sizeof(uint64(0))
	sizeUint8   = unsafe.Sizeof(uint8(0))
	sizeUint32  = unsafe.Sizeof(uint32(0))
	sizeFloat64 = unsafe.Sizeof(float64(0))
)

func preRealloc(b []byte, vals []types.Datum, comparable1 bool) []byte {
	var size int
	for i := range vals {
		switch vals[i].Kind() {
		case types.KindInt64, types.KindUint64, types.KindMysqlEnum, types.KindMysqlSet, types.KindMysqlBit, types.KindBinaryLiteral:
			size += sizeInt(comparable1)
		case types.KindString, types.KindBytes:
			size += sizeBytes(vals[i].GetBytes(), comparable1)
		case types.KindMysqlTime, types.KindMysqlDuration, types.KindFloat32, types.KindFloat64:
			size += 9
		case types.KindNull, types.KindMinNotNull, types.KindMaxValue:
			size++
		case types.KindMysqlJSON:
			size += 2 + len(vals[i].GetBytes())
		case types.KindVectorFloat32:
			size += 1 + vals[i].GetVectorFloat32().SerializedSize()
		case types.KindMysqlDecimal:
			size += 1 + types.MyDecimalStructSize
		default:
			return b
		}
	}
	return reallocBytes(b, size)
}

// encode will encode a datum and append it to a byte slice. If comparable1 is true, the encoded bytes can be sorted as it's original order.
// If hash is true, the encoded bytes can be checked equal as it's original value.
func encode(loc *time.Location, b []byte, vals []types.Datum, comparable1 bool) (_ []byte, err error) {
	b = preRealloc(b, vals, comparable1)
	for i, length := 0, len(vals); i < length; i++ {
		switch vals[i].Kind() {
		case types.KindInt64:
			b = encodeSignedInt(b, vals[i].GetInt64(), comparable1)
		case types.KindUint64:
			b = encodeUnsignedInt(b, vals[i].GetUint64(), comparable1)
		case types.KindFloat32, types.KindFloat64:
			b = append(b, floatFlag)
			b = EncodeFloat(b, vals[i].GetFloat64())
		case types.KindString:
			b = encodeString(b, vals[i], comparable1)
		case types.KindBytes:
			b = encodeBytes(b, vals[i].GetBytes(), comparable1)
		case types.KindMysqlTime:
			b = append(b, uintFlag)
			b, err = EncodeMySQLTime(loc, vals[i].GetMysqlTime(), mysql.TypeUnspecified, b)
			if err != nil {
				return b, err
			}
		case types.KindMysqlDuration:
			// duration may have negative value, so we cannot use String to encode directly.
			b = append(b, durationFlag)
			b = EncodeInt(b, int64(vals[i].GetMysqlDuration().Duration))
		case types.KindMysqlDecimal:
			b = append(b, decimalFlag)
			b, err = EncodeDecimal(b, vals[i].GetMysqlDecimal(), vals[i].Length(), vals[i].Frac())
		case types.KindMysqlEnum:
			b = encodeUnsignedInt(b, vals[i].GetMysqlEnum().Value, comparable1)
		case types.KindMysqlSet:
			b = encodeUnsignedInt(b, vals[i].GetMysqlSet().Value, comparable1)
		case types.KindMysqlBit, types.KindBinaryLiteral:
			// We don't need to handle errors here since the literal is ensured to be able to store in uint64 in convertToMysqlBit.
			var val uint64
			val, err = vals[i].GetBinaryLiteral().ToInt(types.StrictContext)
			terror.Log(errors.Trace(err))
			b = encodeUnsignedInt(b, val, comparable1)
		case types.KindMysqlJSON:
			b = append(b, jsonFlag)
			j := vals[i].GetMysqlJSON()
			b = append(b, j.TypeCode)
			b = append(b, j.Value...)
		case types.KindVectorFloat32:
			// Always do a small deser + ser for sanity check
			b = append(b, vectorFloat32Flag)
			v := vals[i].GetVectorFloat32()
			b = v.SerializeTo(b)
		case types.KindNull:
			b = append(b, NilFlag)
		case types.KindMinNotNull:
			b = append(b, bytesFlag)
		case types.KindMaxValue:
			b = append(b, maxFlag)
		default:
			return b, errors.Errorf("unsupport encode type %d", vals[i].Kind())
		}
	}

	return b, errors.Trace(err)
}

// EstimateValueSize uses to estimate the value  size of the encoded values.
func EstimateValueSize(typeCtx types.Context, val types.Datum) (int, error) {
	l := 0
	switch val.Kind() {
	case types.KindInt64:
		l = valueSizeOfSignedInt(val.GetInt64())
	case types.KindUint64:
		l = valueSizeOfUnsignedInt(val.GetUint64())
	case types.KindFloat32, types.KindFloat64, types.KindMysqlTime, types.KindMysqlDuration:
		l = 9
	case types.KindString, types.KindBytes:
		l = valueSizeOfBytes(val.GetBytes())
	case types.KindMysqlDecimal:
		var err error
		l, err = valueSizeOfDecimal(val.GetMysqlDecimal(), val.Length(), val.Frac())
		if err != nil {
			return 0, err
		}
		l = l + 1
	case types.KindMysqlEnum:
		l = valueSizeOfUnsignedInt(val.GetMysqlEnum().Value)
	case types.KindMysqlSet:
		l = valueSizeOfUnsignedInt(val.GetMysqlSet().Value)
	case types.KindMysqlBit, types.KindBinaryLiteral:
		val, err := val.GetBinaryLiteral().ToInt(typeCtx)
		terror.Log(errors.Trace(err))
		l = valueSizeOfUnsignedInt(val)
	case types.KindMysqlJSON:
		l = 2 + len(val.GetMysqlJSON().Value)
	case types.KindVectorFloat32:
		v := val.GetVectorFloat32()
		l = 1 + v.SerializedSize()
	case types.KindNull, types.KindMinNotNull, types.KindMaxValue:
		l = 1
	default:
		return l, errors.Errorf("unsupported encode type %d", val.Kind())
	}
	return l, nil
}

// EncodeMySQLTime encodes datum of `KindMysqlTime` to []byte.
func EncodeMySQLTime(loc *time.Location, t types.Time, tp byte, b []byte) (_ []byte, err error) {
	// Encoding timestamp need to consider timezone. If it's not in UTC, transform to UTC first.
	// This is compatible with `PBToExpr > convertTime`, and coprocessor assumes the passed timestamp is in UTC as well.
	if tp == mysql.TypeUnspecified {
		tp = t.Type()
	}
	if tp == mysql.TypeTimestamp && loc != time.UTC {
		err = t.ConvertTimeZone(loc, time.UTC)
		if err != nil {
			return nil, err
		}
	}
	var v uint64
	v, err = t.ToPackedUint()
	if err != nil {
		return nil, err
	}
	b = EncodeUint(b, v)
	return b, nil
}

func encodeString(b []byte, val types.Datum, comparable1 bool) []byte {
	if collate.NewCollationEnabled() && comparable1 {
		return encodeBytes(b, collate.GetCollator(val.Collation()).Key(val.GetString()), true)
	}
	return encodeBytes(b, val.GetBytes(), comparable1)
}

func encodeBytes(b []byte, v []byte, comparable1 bool) []byte {
	if comparable1 {
		b = append(b, bytesFlag)
		b = EncodeBytes(b, v)
	} else {
		b = append(b, compactBytesFlag)
		b = EncodeCompactBytes(b, v)
	}
	return b
}

func valueSizeOfBytes(v []byte) int {
	return valueSizeOfSignedInt(int64(len(v))) + len(v)
}

func sizeBytes(v []byte, comparable1 bool) int {
	if comparable1 {
		reallocSize := (len(v)/encGroupSize + 1) * (encGroupSize + 1)
		return 1 + reallocSize
	}
	reallocSize := binary.MaxVarintLen64 + len(v)
	return 1 + reallocSize
}

func encodeSignedInt(b []byte, v int64, comparable1 bool) []byte {
	if comparable1 {
		b = append(b, intFlag)
		b = EncodeInt(b, v)
	} else {
		b = append(b, varintFlag)
		b = EncodeVarint(b, v)
	}
	return b
}

func valueSizeOfSignedInt(v int64) int {
	if v < 0 {
		v = 0 - v - 1
	}
	// flag occupy 1 bit and at lease 1 bit.
	size := 2
	v = v >> 6
	for v > 0 {
		size++
		v = v >> 7
	}
	return size
}

func encodeUnsignedInt(b []byte, v uint64, comparable1 bool) []byte {
	if comparable1 {
		b = append(b, uintFlag)
		b = EncodeUint(b, v)
	} else {
		b = append(b, uvarintFlag)
		b = EncodeUvarint(b, v)
	}
	return b
}

func valueSizeOfUnsignedInt(v uint64) int {
	// flag occupy 1 bit and at lease 1 bit.
	size := 2
	v = v >> 7
	for v > 0 {
		size++
		v = v >> 7
	}
	return size
}

func sizeInt(comparable1 bool) int {
	if comparable1 {
		return 9
	}
	return 1 + binary.MaxVarintLen64
}

// EncodeKey appends the encoded values to byte slice b, returns the appended
// slice. It guarantees the encoded value is in ascending order for comparison.
// For decimal type, datum must set datum's length and frac.
func EncodeKey(loc *time.Location, b []byte, v ...types.Datum) ([]byte, error) {
	return encode(loc, b, v, true)
}

// EncodeValue appends the encoded values to byte slice b, returning the appended
// slice. It does not guarantee the order for comparison.
func EncodeValue(loc *time.Location, b []byte, v ...types.Datum) ([]byte, error) {
	return encode(loc, b, v, false)
}

func encodeHashChunkRowIdx(typeCtx types.Context, row chunk.Row, tp *types.FieldType, idx int) (flag byte, b []byte, err error) {
	if row.IsNull(idx) {
		flag = NilFlag
		return
	}
	switch tp.GetType() {
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong, mysql.TypeYear:
		flag = uvarintFlag
		if !mysql.HasUnsignedFlag(tp.GetFlag()) && row.GetInt64(idx) < 0 {
			flag = varintFlag
		}
		b = row.GetRaw(idx)
	case mysql.TypeFloat:
		flag = floatFlag
		f := float64(row.GetFloat32(idx))
		// For negative zero. In memory, 0 is [0, 0, 0, 0, 0, 0, 0, 0] and -0 is [0, 0, 0, 0, 0, 0, 0, 128].
		// It makes -0's hash val different from 0's.
		if f == 0 {
			f = 0
		}
		b = unsafe.Slice((*byte)(unsafe.Pointer(&f)), unsafe.Sizeof(f))
	case mysql.TypeDouble:
		flag = floatFlag
		f := row.GetFloat64(idx)
		// For negative zero. In memory, 0 is [0, 0, 0, 0, 0, 0, 0, 0] and -0 is [0, 0, 0, 0, 0, 0, 0, 128].
		// It makes -0's hash val different from 0's.
		if f == 0 {
			f = 0
		}
		b = unsafe.Slice((*byte)(unsafe.Pointer(&f)), unsafe.Sizeof(f))
	case mysql.TypeVarchar, mysql.TypeVarString, mysql.TypeString, mysql.TypeBlob, mysql.TypeTinyBlob, mysql.TypeMediumBlob, mysql.TypeLongBlob:
		flag = compactBytesFlag
		b = row.GetBytes(idx)
		b = ConvertByCollation(b, tp)
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp:
		flag = uintFlag
		t := row.GetTime(idx)

		var v uint64
		v, err = t.ToPackedUint()
		if err != nil {
			return
		}
		b = unsafe.Slice((*byte)(unsafe.Pointer(&v)), unsafe.Sizeof(v))
	case mysql.TypeDuration:
		flag = durationFlag
		// duration may have negative value, so we cannot use String to encode directly.
		b = row.GetRaw(idx)
	case mysql.TypeNewDecimal:
		flag = decimalFlag
		// If hash is true, we only consider the original value of this decimal and ignore it's precision.
		dec := row.GetMyDecimal(idx)
		b, err = dec.ToHashKey()
		if err != nil {
			return
		}
	case mysql.TypeEnum:
		if mysql.HasEnumSetAsIntFlag(tp.GetFlag()) {
			flag = uvarintFlag
			v := row.GetEnum(idx).Value
			b = unsafe.Slice((*byte)(unsafe.Pointer(&v)), sizeUint64)
		} else {
			flag = compactBytesFlag
			v := row.GetEnum(idx).Value
			str := ""
			if enum, err := types.ParseEnumValue(tp.GetElems(), v); err == nil {
				// str will be empty string if v out of definition of enum.
				str = enum.Name
			}
			b = ConvertByCollation(hack.Slice(str), tp)
		}
	case mysql.TypeSet:
		flag = compactBytesFlag
		s, err := types.ParseSetValue(tp.GetElems(), row.GetSet(idx).Value)
		if err != nil {
			return 0, nil, err
		}
		b = ConvertByCollation(hack.Slice(s.Name), tp)
	case mysql.TypeBit:
		// We don't need to handle errors here since the literal is ensured to be able to store in uint64 in convertToMysqlBit.
		flag = uvarintFlag
		v, err1 := types.BinaryLiteral(row.GetBytes(idx)).ToInt(typeCtx)
		terror.Log(errors.Trace(err1))
		b = unsafe.Slice((*byte)(unsafe.Pointer(&v)), unsafe.Sizeof(v))
	case mysql.TypeJSON:
		flag = jsonFlag
		json := row.GetJSON(idx)
		b = json.HashValue(b)
	case mysql.TypeTiDBVectorFloat32:
		flag = vectorFloat32Flag
		v := row.GetVectorFloat32(idx)
		b = v.SerializeTo(nil)
	default:
		return 0, nil, errors.Errorf("unsupport column type for encode %d", tp.GetType())
	}
	return
}

// SerializeMode is for some special cases during serialize key
type SerializeMode int

const (
	// Normal means serialize in the normal way
	Normal SerializeMode = iota
	// NeedSignFlag when serialize integer column, if the join key is <signed, signed> or <unsigned, unsigned>,
	// the unsigned flag can be ignored, if the join key is <unsigned, signed> or <signed, unsigned>
	// the unsigned flag can not be ignored, if the unsigned flag can not be ignored, the key can not be inlined
	NeedSignFlag
	// KeepVarColumnLength when serialize var-length column, whether record the column length or not. If the join key only contains one var-length
	// column, and the key is not inlined, then no need to record the column length, otherwise, always need to record the column length
	KeepVarColumnLength
)

// SerializeKeys is used in join
func SerializeKeys(typeCtx types.Context, chk *chunk.Chunk, tp *types.FieldType, colIdx int, usedRows []int, filterVector []bool, nullVector []bool, serializeMode SerializeMode, serializedKeysVector [][]byte) (err error) {
	column := chk.Column(colIdx)
	canSkip := func(index int) bool {
		if column.IsNull(index) {
			nullVector[index] = true
		}
		return (filterVector != nil && !filterVector[index]) || (nullVector != nil && nullVector[index])
	}
	var jsonHashBuffer []byte
	if tp.GetType() == mysql.TypeJSON {
		jsonHashBuffer = make([]byte, 0)
	}
	switch tp.GetType() {
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong, mysql.TypeYear:
		i64s := column.Int64s()
		for logicalRowIndex, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
			if serializeMode == NeedSignFlag {
				if !mysql.HasUnsignedFlag(tp.GetFlag()) && i64s[physicalRowindex] < 0 {
					serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], intFlag)
				} else {
					serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], uintFlag)
				}
			}
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], column.GetRaw(physicalRowindex)...)
		}
	case mysql.TypeFloat:
		f32s := column.Float32s()
		for logicalRowIndex, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
			d := float64(f32s[physicalRowindex])
			// For negative zero. In memory, 0 is [0, 0, 0, 0, 0, 0, 0, 0] and -0 is [0, 0, 0, 0, 0, 0, 0, 128].
			// It makes -0's hash val different from 0's.
			if d == 0 {
				d = 0
			}
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&d)), sizeFloat64)...)
		}
	case mysql.TypeDouble:
		f64s := column.Float64s()
		for logicalRowIndex, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
			// For negative zero. In memory, 0 is [0, 0, 0, 0, 0, 0, 0, 0] and -0 is [0, 0, 0, 0, 0, 0, 0, 128].
			// It makes -0's hash val different from 0's.
			f := f64s[physicalRowindex]
			if f == 0 {
				f = 0
			}
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&f)), sizeFloat64)...)
		}
	case mysql.TypeVarchar, mysql.TypeVarString, mysql.TypeString, mysql.TypeBlob, mysql.TypeTinyBlob, mysql.TypeMediumBlob, mysql.TypeLongBlob:
		for logicalRowIndex, physicalRowIndex := range usedRows {
			if canSkip(physicalRowIndex) {
				continue
			}
			data := ConvertByCollation(column.GetBytes(physicalRowIndex), tp)
			size := uint32(len(data))
			if serializeMode == KeepVarColumnLength {
				serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&size)), sizeUint32)...)
			}
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], data...)
		}
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp:
		ts := column.Times()
		for logicalRowIndex, physicalRowIndex := range usedRows {
			if canSkip(physicalRowIndex) {
				continue
			}
			var v uint64
			v, err = ts[physicalRowIndex].ToPackedUint()
			if err != nil {
				return err
			}
			// don't need to check serializeMode since date/datetime/timestamp must be compared with date/datetime/timestamp, so the serializeMode must be normal
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&v)), sizeUint64)...)
		}
	case mysql.TypeDuration:
		for logicalRowIndex, physicalRowIndex := range usedRows {
			if canSkip(physicalRowIndex) {
				continue
			}
			// don't need to check serializeMode since duration must be compared with duration, so the serializeMode must be normal
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], column.GetRaw(physicalRowIndex)...)
		}
	case mysql.TypeNewDecimal:
		ds := column.Decimals()
		for logicalRowIndex, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
			var b []byte
			b, err = ds[physicalRowindex].ToHashKey()
			if err != nil {
				return err
			}
			if serializeMode == KeepVarColumnLength {
				// for decimal, the size must be less than uint8.MAX, so use uint8 here
				size := uint8(len(b))
				serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&size)), sizeUint8)...)
			}
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], b...)
		}
	case mysql.TypeEnum:
		for logicalRowIndex, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
			v := column.GetEnum(physicalRowindex).Value
			if mysql.HasEnumSetAsIntFlag(tp.GetFlag()) {
				// check serializeMode here because enum maybe compare to integer type directly
				if serializeMode == NeedSignFlag {
					serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], uintFlag)
				}
				serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&v)), sizeUint64)...)
			} else {
				str := ""
				if enum, err := types.ParseEnumValue(tp.GetElems(), v); err == nil {
					str = enum.Name
				}
				b := ConvertByCollation(hack.Slice(str), tp)
				if serializeMode == KeepVarColumnLength {
					// for enum, the size must be less than uint32.MAX, so use uint32 here
					size := uint32(len(b))
					serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&size)), sizeUint32)...)
				}
				serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], b...)
			}
		}
	case mysql.TypeSet:
		for logicalRowIndex, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
			s, err := types.ParseSetValue(tp.GetElems(), column.GetSet(physicalRowindex).Value)
			if err != nil {
				return err
			}
			b := ConvertByCollation(hack.Slice(s.Name), tp)
			if serializeMode == KeepVarColumnLength {
				// for enum, the size must be less than uint32.MAX, so use uint32 here
				size := uint32(len(b))
				serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&size)), sizeUint32)...)
			}
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], b...)
		}
	case mysql.TypeBit:
		for logicalRowIndex, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
			v, err1 := types.BinaryLiteral(column.GetBytes(physicalRowindex)).ToInt(typeCtx)
			terror.Log(errors.Trace(err1))
			// check serializeMode here because bit maybe compare to integer type directly
			if serializeMode == NeedSignFlag {
				serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], uintFlag)
			}
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&v)), sizeUint64)...)
		}
	case mysql.TypeJSON:
		for logicalRowIndex, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
			jsonHashBuffer = jsonHashBuffer[:0]
			jsonHashBuffer = column.GetJSON(physicalRowindex).HashValue(jsonHashBuffer)
			if serializeMode == KeepVarColumnLength {
				size := uint32(len(jsonHashBuffer))
				serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], unsafe.Slice((*byte)(unsafe.Pointer(&size)), sizeUint32)...)
			}
			serializedKeysVector[logicalRowIndex] = append(serializedKeysVector[logicalRowIndex], jsonHashBuffer...)
		}
	case mysql.TypeNull:
		for _, physicalRowindex := range usedRows {
			if canSkip(physicalRowindex) {
				continue
			}
		}
	default:
		return errors.Errorf("unsupport column type for encode %d", tp.GetType())
	}
	return
}

// HashChunkColumns writes the encoded value of each row's column, which of index `colIdx`, to h.
func HashChunkColumns(typeCtx types.Context, h []hash.Hash64, chk *chunk.Chunk, tp *types.FieldType, colIdx int, buf []byte, isNull []bool) (err error) {
	return HashChunkSelected(typeCtx, h, chk, tp, colIdx, buf, isNull, nil, false)
}

// HashChunkSelected writes the encoded value of selected row's column, which of index `colIdx`, to h.
// sel indicates which rows are selected. If it is nil, all rows are selected.
func HashChunkSelected(typeCtx types.Context, h []hash.Hash64, chk *chunk.Chunk, tp *types.FieldType, colIdx int, buf []byte,
	isNull, sel []bool, ignoreNull bool) (err error) {
	var b []byte
	column := chk.Column(colIdx)
	rows := chk.NumRows()
	switch tp.GetType() {
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong, mysql.TypeYear:
		i64s := column.Int64s()
		for i, v := range i64s {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = uvarintFlag
				if !mysql.HasUnsignedFlag(tp.GetFlag()) && v < 0 {
					buf[0] = varintFlag
				}
				b = column.GetRaw(i)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeFloat:
		f32s := column.Float32s()
		for i, f := range f32s {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = floatFlag
				d := float64(f)
				// For negative zero. In memory, 0 is [0, 0, 0, 0, 0, 0, 0, 0] and -0 is [0, 0, 0, 0, 0, 0, 0, 128].
				// It makes -0's hash val different from 0's.
				if d == 0 {
					d = 0
				}
				b = unsafe.Slice((*byte)(unsafe.Pointer(&d)), sizeFloat64)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeDouble:
		f64s := column.Float64s()
		for i := range f64s {
			f := f64s[i]
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = floatFlag
				// For negative zero. In memory, 0 is [0, 0, 0, 0, 0, 0, 0, 0] and -0 is [0, 0, 0, 0, 0, 0, 0, 128].
				// It makes -0's hash val different from 0's.
				if f == 0 {
					f = 0
				}
				b = unsafe.Slice((*byte)(unsafe.Pointer(&f)), sizeFloat64)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeVarchar, mysql.TypeVarString, mysql.TypeString, mysql.TypeBlob, mysql.TypeTinyBlob, mysql.TypeMediumBlob, mysql.TypeLongBlob:
		for i := range rows {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = compactBytesFlag
				b = column.GetBytes(i)
				b = ConvertByCollation(b, tp)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp:
		ts := column.Times()
		for i, t := range ts {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = uintFlag

				var v uint64
				v, err = t.ToPackedUint()
				if err != nil {
					return
				}
				b = unsafe.Slice((*byte)(unsafe.Pointer(&v)), sizeUint64)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeDuration:
		for i := range rows {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = durationFlag
				// duration may have negative value, so we cannot use String to encode directly.
				b = column.GetRaw(i)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeNewDecimal:
		ds := column.Decimals()
		for i, d := range ds {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = decimalFlag
				// If hash is true, we only consider the original value of this decimal and ignore it's precision.
				b, err = d.ToHashKey()
				if err != nil {
					return
				}
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeEnum:
		for i := range rows {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else if mysql.HasEnumSetAsIntFlag(tp.GetFlag()) {
				buf[0] = uvarintFlag
				v := column.GetEnum(i).Value
				b = unsafe.Slice((*byte)(unsafe.Pointer(&v)), sizeUint64)
			} else {
				buf[0] = compactBytesFlag
				v := column.GetEnum(i).Value
				str := ""
				if enum, err := types.ParseEnumValue(tp.GetElems(), v); err == nil {
					// str will be empty string if v out of definition of enum.
					str = enum.Name
				}
				b = ConvertByCollation(hack.Slice(str), tp)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeSet:
		for i := range rows {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = compactBytesFlag
				s, err := types.ParseSetValue(tp.GetElems(), column.GetSet(i).Value)
				if err != nil {
					return err
				}
				b = ConvertByCollation(hack.Slice(s.Name), tp)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeBit:
		for i := range rows {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				// We don't need to handle errors here since the literal is ensured to be able to store in uint64 in convertToMysqlBit.
				buf[0] = uvarintFlag
				v, err1 := types.BinaryLiteral(column.GetBytes(i)).ToInt(typeCtx)
				terror.Log(errors.Trace(err1))
				b = unsafe.Slice((*byte)(unsafe.Pointer(&v)), sizeUint64)
			}

			// As the golang doc described, `Hash.Write` never returns an error.
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeJSON:
		for i := range rows {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = jsonFlag
				json := column.GetJSON(i)
				b = b[:0]
				b = json.HashValue(b)
			}

			// As the golang doc described, `Hash.Write` never returns an error..
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeTiDBVectorFloat32:
		for i := range rows {
			if sel != nil && !sel[i] {
				continue
			}
			if column.IsNull(i) {
				buf[0], b = NilFlag, nil
				isNull[i] = !ignoreNull
			} else {
				buf[0] = vectorFloat32Flag
				v := column.GetVectorFloat32(i)
				b = v.SerializeTo(nil)
			}

			// As the golang doc described, `Hash.Write` never returns an error..
			// See https://golang.org/pkg/hash/#Hash
			_, _ = h[i].Write(buf)
			_, _ = h[i].Write(b)
		}
	case mysql.TypeNull:
		for i := range rows {
			if sel != nil && !sel[i] {
				continue
			}
			isNull[i] = !ignoreNull
			buf[0] = NilFlag
			_, _ = h[i].Write(buf)
		}
	default:
		return errors.Errorf("unsupport column type for encode %d", tp.GetType())
	}
	return
}

// HashChunkRow writes the encoded values to w.
// If two rows are logically equal, it will generate the same bytes.
func HashChunkRow(typeCtx types.Context, w io.Writer, row chunk.Row, allTypes []*types.FieldType, colIdx []int, buf []byte) (err error) {
	var b []byte
	for i, idx := range colIdx {
		buf[0], b, err = encodeHashChunkRowIdx(typeCtx, row, allTypes[i], idx)
		if err != nil {
			return errors.Trace(err)
		}
		_, err = w.Write(buf)
		if err != nil {
			return
		}
		_, err = w.Write(b)
		if err != nil {
			return
		}
	}
	return err
}

// EqualChunkRow returns a boolean reporting whether row1 and row2
// with their types and column index are logically equal.
func EqualChunkRow(typeCtx types.Context,
	row1 chunk.Row, allTypes1 []*types.FieldType, colIdx1 []int,
	row2 chunk.Row, allTypes2 []*types.FieldType, colIdx2 []int,
) (bool, error) {
	if len(colIdx1) != len(colIdx2) {
		return false, errors.Errorf("Internal error: Hash columns count mismatch, col1: %d, col2: %d", len(colIdx1), len(colIdx2))
	}
	for i := range colIdx1 {
		idx1, idx2 := colIdx1[i], colIdx2[i]
		flag1, b1, err := encodeHashChunkRowIdx(typeCtx, row1, allTypes1[i], idx1)
		if err != nil {
			return false, errors.Trace(err)
		}
		flag2, b2, err := encodeHashChunkRowIdx(typeCtx, row2, allTypes2[i], idx2)
		if err != nil {
			return false, errors.Trace(err)
		}
		if !(flag1 == flag2 && bytes.Equal(b1, b2)) {
			return false, nil
		}
	}
	return true, nil
}

// Decode decodes values from a byte slice generated with EncodeKey or EncodeValue
// before.
// size is the size of decoded datum slice.
func Decode(b []byte, size int) ([]types.Datum, error) {
	if len(b) < 1 {
		return nil, errors.New("invalid encoded key")
	}

	var (
		err    error
		values = make([]types.Datum, 0, size)
	)

	for len(b) > 0 {
		var d types.Datum
		b, d, err = DecodeOne(b)
		if err != nil {
			return nil, errors.Trace(err)
		}

		values = append(values, d)
	}

	return values, nil
}

// DecodeRange decodes the range values from a byte slice that generated by EncodeKey.
// It handles some special values like `MinNotNull` and `MaxValueDatum`.
// loc can be nil and only used in when the corresponding type is `mysql.TypeTimestamp`.
func DecodeRange(b []byte, size int, idxColumnTypes []byte, loc *time.Location) ([]types.Datum, []byte, error) {
	if len(b) < 1 {
		return nil, b, errors.New("invalid encoded key: length of key is zero")
	}

	var (
		err    error
		values = make([]types.Datum, 0, size)
	)

	i := 0
	for len(b) > 1 {
		var d types.Datum
		if idxColumnTypes == nil {
			b, d, err = DecodeOne(b)
		} else {
			if i >= len(idxColumnTypes) {
				return values, b, errors.New("invalid length of index's columns")
			}
			if types.IsTypeTime(idxColumnTypes[i]) {
				// handle datetime values specially since they are encoded to int and we'll get int values if using DecodeOne.
				b, d, err = DecodeAsDateTime(b, idxColumnTypes[i], loc)
			} else if types.IsTypeFloat(idxColumnTypes[i]) {
				b, d, err = DecodeAsFloat32(b, idxColumnTypes[i])
			} else {
				b, d, err = DecodeOne(b)
			}
		}
		if err != nil {
			return values, b, errors.Trace(err)
		}
		values = append(values, d)
		i++
	}

	if len(b) == 1 {
		switch b[0] {
		case NilFlag:
			values = append(values, types.Datum{})
		case bytesFlag:
			values = append(values, types.MinNotNullDatum())
		// `maxFlag + 1` for PrefixNext
		case maxFlag, maxFlag + 1:
			values = append(values, types.MaxValueDatum())
		default:
			return values, b, errors.Errorf("invalid encoded key flag %v", b[0])
		}
	}
	return values, nil, nil
}

// DecodeOne decodes on datum from a byte slice generated with EncodeKey or EncodeValue.
func DecodeOne(b []byte) (remain []byte, d types.Datum, err error) {
	if len(b) < 1 {
		return nil, d, errors.New("invalid encoded key")
	}
	flag := b[0]
	b = b[1:]
	switch flag {
	case intFlag:
		var v int64
		b, v, err = DecodeInt(b)
		d.SetInt64(v)
	case uintFlag:
		var v uint64
		b, v, err = DecodeUint(b)
		d.SetUint64(v)
	case varintFlag:
		var v int64
		b, v, err = DecodeVarint(b)
		d.SetInt64(v)
	case uvarintFlag:
		var v uint64
		b, v, err = DecodeUvarint(b)
		d.SetUint64(v)
	case floatFlag:
		var v float64
		b, v, err = DecodeFloat(b)
		d.SetFloat64(v)
	case bytesFlag:
		var v []byte
		b, v, err = DecodeBytes(b, nil)
		d.SetBytes(v)
	case compactBytesFlag:
		var v []byte
		b, v, err = DecodeCompactBytes(b)
		d.SetBytes(v)
	case decimalFlag:
		var (
			dec             *types.MyDecimal
			precision, frac int
		)
		b, dec, precision, frac, err = DecodeDecimal(b)
		if err == nil {
			d.SetMysqlDecimal(dec)
			d.SetLength(precision)
			d.SetFrac(frac)
		}
	case durationFlag:
		var r int64
		b, r, err = DecodeInt(b)
		if err == nil {
			// use max fsp, let outer to do round manually.
			v := types.Duration{Duration: time.Duration(r), Fsp: types.MaxFsp}
			d.SetMysqlDuration(v)
		}
	case jsonFlag:
		var size int
		size, err = types.PeekBytesAsJSON(b)
		if err != nil {
			return b, d, err
		}
		j := types.BinaryJSON{TypeCode: b[0], Value: b[1:size]}
		d.SetMysqlJSON(j)
		b = b[size:]
	case vectorFloat32Flag:
		v, remaining, err := types.ZeroCopyDeserializeVectorFloat32(b)
		if err != nil {
			return b, d, errors.Trace(err)
		}
		d.SetVectorFloat32(v)
		b = remaining
	case NilFlag:
	default:
		return b, d, errors.Errorf("invalid encoded key flag %v", flag)
	}
	if err != nil {
		return b, d, errors.Trace(err)
	}
	return b, d, nil
}

// DecodeAsDateTime decodes on datum from []byte of `KindMysqlTime`.
func DecodeAsDateTime(b []byte, tp byte, loc *time.Location) (remain []byte, d types.Datum, err error) {
	if len(b) < 1 {
		return nil, d, errors.New("invalid encoded key")
	}
	flag := b[0]
	b = b[1:]
	var v uint64
	switch flag {
	case uintFlag:
		b, v, err = DecodeUint(b)
	case uvarintFlag:
		// Datetime can be encoded as Uvarint
		b, v, err = DecodeUvarint(b)
	case NilFlag:
		// null value should also be decoded out.
		return b, d, nil

	default:
		return b, d, errors.Errorf("invalid encoded key flag %v", flag)
	}
	if err != nil {
		return b, d, err
	}
	t := types.NewTime(types.ZeroCoreTime, tp, 0)
	err = t.FromPackedUint(v)
	if err != nil {
		return b, d, errors.Trace(err)
	}
	if tp == mysql.TypeTimestamp && !t.IsZero() && loc != nil {
		err = t.ConvertTimeZone(time.UTC, loc)
		if err != nil {
			return b, d, err
		}
	}
	d.SetMysqlTime(t)
	return b, d, nil
}

// DecodeAsFloat32 decodes value for mysql.TypeFloat
func DecodeAsFloat32(b []byte, tp byte) (remain []byte, d types.Datum, err error) {
	if len(b) < 1 || tp != mysql.TypeFloat {
		return nil, d, errors.New("invalid encoded key")
	}
	flag := b[0]
	b = b[1:]
	if flag != floatFlag {
		return b, d, errors.Errorf("invalid encoded key flag %v for DecodeAsFloat32", flag)
	}
	var v float64
	b, v, err = DecodeFloat(b)
	if err != nil {
		return nil, d, err
	}
	d.SetFloat32FromF64(v)
	return b, d, nil
}

// CutOne cuts the first encoded value from b.
// It will return the first encoded item and the remains as byte slice.
func CutOne(b []byte) (data []byte, remain []byte, err error) {
	l, err := peek(b)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	return b[:l], b[l:], nil
}

// CutColumnID cuts the column ID from b.
// It will return the remains as byte slice and column ID
func CutColumnID(b []byte) (remain []byte, n int64, err error) {
	if len(b) < 1 {
		return nil, 0, errors.New("invalid encoded key")
	}
	// skip the flag
	b = b[1:]
	return DecodeVarint(b)
}

// SetRawValues set raw datum values from a row data.
func SetRawValues(data []byte, values []types.Datum) error {
	for i := range values {
		l, err := peek(data)
		if err != nil {
			return errors.Trace(err)
		}
		values[i].SetRaw(data[:l:l])
		data = data[l:]
	}
	return nil
}

// peek peeks the first encoded value from b and returns its length.
func peek(b []byte) (length int, err error) {
	originLength := len(b)
	if len(b) < 1 {
		return 0, errors.New("invalid encoded key")
	}
	flag := b[0]
	length++
	b = b[1:]
	var l int
	switch flag {
	case NilFlag:
	case intFlag, uintFlag, floatFlag, durationFlag:
		// Those types are stored in 8 bytes.
		l = 8
	case bytesFlag:
		l, err = peekBytes(b)
	case compactBytesFlag:
		l, err = peekCompactBytes(b)
	case decimalFlag:
		l, err = types.DecimalPeak(b)
	case varintFlag:
		l, err = peekVarint(b)
	case uvarintFlag:
		l, err = peekUvarint(b)
	case jsonFlag:
		l, err = types.PeekBytesAsJSON(b)
	case vectorFloat32Flag:
		l, err = types.PeekBytesAsVectorFloat32(b)
	default:
		return 0, errors.Errorf("invalid encoded key flag %v", flag)
	}
	if err != nil {
		return 0, errors.Trace(err)
	}
	length += l
	if length <= 0 {
		return 0, errors.New("invalid encoded key")
	} else if length > originLength {
		return 0, errors.Errorf("invalid encoded key, "+
			"expected length: %d, actual length: %d", length, originLength)
	}
	return
}

func peekBytes(b []byte) (int, error) {
	offset := 0
	for {
		if len(b) < offset+encGroupSize+1 {
			return 0, errors.New("insufficient bytes to decode value")
		}
		// The byte slice is encoded into many groups.
		// For each group, there are 8 bytes for data and 1 byte for marker.
		marker := b[offset+encGroupSize]
		padCount := encMarker - marker
		offset += encGroupSize + 1
		// When padCount is not zero, it means we get the end of the byte slice.
		if padCount != 0 {
			break
		}
	}
	return offset, nil
}

func peekCompactBytes(b []byte) (int, error) {
	// Get length.
	v, n := binary.Varint(b)
	vi := int(v)
	if n < 0 {
		return 0, errors.New("value larger than 64 bits")
	} else if n == 0 {
		return 0, errors.New("insufficient bytes to decode value")
	}
	if len(b) < vi+n {
		return 0, errors.Errorf("insufficient bytes to decode value, expected length: %v", n)
	}
	return n + vi, nil
}

func peekVarint(b []byte) (int, error) {
	_, n := binary.Varint(b)
	if n < 0 {
		return 0, errors.New("value larger than 64 bits")
	}
	return n, nil
}

func peekUvarint(b []byte) (int, error) {
	_, n := binary.Uvarint(b)
	if n < 0 {
		return 0, errors.New("value larger than 64 bits")
	}
	return n, nil
}

// Decoder is used to decode value to chunk.
type Decoder struct {
	chk      *chunk.Chunk
	timezone *time.Location

	// buf is only used for DecodeBytes to avoid the cost of makeslice.
	buf []byte
}

// NewDecoder creates a Decoder.
func NewDecoder(chk *chunk.Chunk, timezone *time.Location) *Decoder {
	return &Decoder{
		chk:      chk,
		timezone: timezone,
	}
}

// DecodeOne decodes one value to chunk and returns the remained bytes.
func (decoder *Decoder) DecodeOne(b []byte, colIdx int, ft *types.FieldType) (remain []byte, err error) {
	if len(b) < 1 {
		return nil, errors.New("invalid encoded key")
	}
	chk := decoder.chk
	flag := b[0]
	b = b[1:]
	switch flag {
	case intFlag:
		var v int64
		b, v, err = DecodeInt(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		appendIntToChunk(v, chk, colIdx, ft)
	case uintFlag:
		var v uint64
		b, v, err = DecodeUint(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		err = appendUintToChunk(v, chk, colIdx, ft, decoder.timezone)
	case varintFlag:
		var v int64
		b, v, err = DecodeVarint(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		appendIntToChunk(v, chk, colIdx, ft)
	case uvarintFlag:
		var v uint64
		b, v, err = DecodeUvarint(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		err = appendUintToChunk(v, chk, colIdx, ft, decoder.timezone)
	case floatFlag:
		var v float64
		b, v, err = DecodeFloat(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		appendFloatToChunk(v, chk, colIdx, ft)
	case bytesFlag:
		b, decoder.buf, err = DecodeBytes(b, decoder.buf)
		if err != nil {
			return nil, errors.Trace(err)
		}
		chk.AppendBytes(colIdx, decoder.buf)
	case compactBytesFlag:
		var v []byte
		b, v, err = DecodeCompactBytes(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		chk.AppendBytes(colIdx, v)
	case decimalFlag:
		var dec *types.MyDecimal
		var frac int
		b, dec, _, frac, err = DecodeDecimal(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if ft.GetDecimal() != types.UnspecifiedLength && frac > ft.GetDecimal() {
			to := new(types.MyDecimal)
			err := dec.Round(to, ft.GetDecimal(), types.ModeHalfUp)
			if err != nil {
				return nil, errors.Trace(err)
			}
			dec = to
		}
		chk.AppendMyDecimal(colIdx, dec)
	case durationFlag:
		var r int64
		b, r, err = DecodeInt(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		v := types.Duration{Duration: time.Duration(r), Fsp: ft.GetDecimal()}
		chk.AppendDuration(colIdx, v)
	case jsonFlag:
		var size int
		size, err = types.PeekBytesAsJSON(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		chk.AppendJSON(colIdx, types.BinaryJSON{TypeCode: b[0], Value: b[1:size]})
		b = b[size:]
	case vectorFloat32Flag:
		v, remaining, err := types.ZeroCopyDeserializeVectorFloat32(b)
		if err != nil {
			return nil, errors.Trace(err)
		}
		chk.AppendVectorFloat32(colIdx, v)
		b = remaining
	case NilFlag:
		chk.AppendNull(colIdx)
	default:
		return nil, errors.Errorf("invalid encoded key flag %v", flag)
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	return b, nil
}

func appendIntToChunk(val int64, chk *chunk.Chunk, colIdx int, ft *types.FieldType) {
	switch ft.GetType() {
	case mysql.TypeDuration:
		v := types.Duration{Duration: time.Duration(val), Fsp: ft.GetDecimal()}
		chk.AppendDuration(colIdx, v)
	default:
		chk.AppendInt64(colIdx, val)
	}
}

func appendUintToChunk(val uint64, chk *chunk.Chunk, colIdx int, ft *types.FieldType, loc *time.Location) error {
	switch ft.GetType() {
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp:
		t := types.NewTime(types.ZeroCoreTime, ft.GetType(), ft.GetDecimal())
		var err error
		err = t.FromPackedUint(val)
		if err != nil {
			return errors.Trace(err)
		}
		if ft.GetType() == mysql.TypeTimestamp && !t.IsZero() {
			err = t.ConvertTimeZone(time.UTC, loc)
			if err != nil {
				return errors.Trace(err)
			}
		}
		chk.AppendTime(colIdx, t)
	case mysql.TypeEnum:
		// ignore error deliberately, to read empty enum value.
		enum, err := types.ParseEnumValue(ft.GetElems(), val)
		if err != nil {
			enum = types.Enum{}
		}
		chk.AppendEnum(colIdx, enum)
	case mysql.TypeSet:
		set, err := types.ParseSetValue(ft.GetElems(), val)
		if err != nil {
			return errors.Trace(err)
		}
		chk.AppendSet(colIdx, set)
	case mysql.TypeBit:
		byteSize := (ft.GetFlen() + 7) >> 3
		chk.AppendBytes(colIdx, types.NewBinaryLiteralFromUint(val, byteSize))
	default:
		chk.AppendUint64(colIdx, val)
	}
	return nil
}

func appendFloatToChunk(val float64, chk *chunk.Chunk, colIdx int, ft *types.FieldType) {
	if ft.GetType() == mysql.TypeFloat {
		chk.AppendFloat32(colIdx, float32(val))
	} else {
		chk.AppendFloat64(colIdx, val)
	}
}

// HashGroupKey encodes each row of this column and append encoded data into buf.
// Only use in the aggregate executor.
func HashGroupKey(loc *time.Location, n int, col *chunk.Column, buf [][]byte, ft *types.FieldType) ([][]byte, error) {
	var err error
	switch ft.EvalType() {
	case types.ETInt:
		i64s := col.Int64s()
		for i := range n {
			if col.IsNull(i) {
				buf[i] = append(buf[i], NilFlag)
			} else {
				buf[i] = encodeSignedInt(buf[i], i64s[i], false)
			}
		}
	case types.ETReal:
		f64s := col.Float64s()
		for i := range n {
			if col.IsNull(i) {
				buf[i] = append(buf[i], NilFlag)
			} else {
				buf[i] = append(buf[i], floatFlag)
				buf[i] = EncodeFloat(buf[i], f64s[i])
			}
		}
	case types.ETDecimal:
		ds := col.Decimals()
		for i := range n {
			if col.IsNull(i) {
				buf[i] = append(buf[i], NilFlag)
			} else {
				buf[i] = append(buf[i], decimalFlag)
				buf[i], err = EncodeDecimal(buf[i], &ds[i], ft.GetFlen(), ft.GetDecimal())
				if err != nil {
					return buf, err
				}
			}
		}
	case types.ETDatetime, types.ETTimestamp:
		ts := col.Times()
		for i := range n {
			if col.IsNull(i) {
				buf[i] = append(buf[i], NilFlag)
			} else {
				buf[i] = append(buf[i], uintFlag)
				buf[i], err = EncodeMySQLTime(loc, ts[i], mysql.TypeUnspecified, buf[i])
				if err != nil {
					return buf, err
				}
			}
		}
	case types.ETDuration:
		ds := col.GoDurations()
		for i := range n {
			if col.IsNull(i) {
				buf[i] = append(buf[i], NilFlag)
			} else {
				buf[i] = append(buf[i], durationFlag)
				buf[i] = EncodeInt(buf[i], int64(ds[i]))
			}
		}
	case types.ETJson:
		for i := range n {
			if col.IsNull(i) {
				buf[i] = append(buf[i], NilFlag)
			} else {
				buf[i] = append(buf[i], jsonFlag)
				buf[i] = col.GetJSON(i).HashValue(buf[i])
			}
		}
	case types.ETString:
		for i := range n {
			if col.IsNull(i) {
				buf[i] = append(buf[i], NilFlag)
			} else {
				buf[i] = encodeBytes(buf[i], ConvertByCollation(col.GetBytes(i), ft), false)
			}
		}
	case types.ETVectorFloat32:
		for i := range n {
			if col.IsNull(i) {
				buf[i] = append(buf[i], NilFlag)
			} else {
				buf[i] = col.GetVectorFloat32(i).SerializeTo(buf[i])
			}
		}
	default:
		return nil, errors.Errorf("unsupported type %s during evaluation", ft.EvalType())
	}
	return buf, nil
}

// ConvertByCollation converts these bytes according to its collation.
func ConvertByCollation(raw []byte, tp *types.FieldType) []byte {
	collator := collate.GetCollator(tp.GetCollate())
	return collator.Key(string(hack.String(raw)))
}

// ConvertByCollationStr converts this string according to its collation.
func ConvertByCollationStr(str string, tp *types.FieldType) string {
	collator := collate.GetCollator(tp.GetCollate())
	return string(hack.String(collator.Key(str)))
}

// Hash64 is for datum hash64 calculation.
func Hash64(h base.Hasher, d *types.Datum) {
	// let h.cache to receive datum hash value, which is potentially expendable.
	// clean the cache before using it.
	b := h.Cache()[:0]
	b = HashCode(b, *d)
	h.HashBytes(b)
	h.SetCache(b)
}

func init() {
	types.Hash64ForDatum = Hash64
}

// HashCode encodes a Datum into a unique byte slice.
// It is mostly the same as EncodeValue, but it doesn't contain truncation or verification logic in order to make the encoding lossless.
func HashCode(b []byte, d types.Datum) []byte {
	switch d.Kind() {
	case types.KindInt64:
		b = encodeSignedInt(b, d.GetInt64(), false)
	case types.KindUint64:
		b = encodeUnsignedInt(b, d.GetUint64(), false)
	case types.KindFloat32, types.KindFloat64:
		b = append(b, floatFlag)
		b = EncodeFloat(b, d.GetFloat64())
	case types.KindString:
		b = encodeString(b, d, false)
	case types.KindBytes:
		b = encodeBytes(b, d.GetBytes(), false)
	case types.KindMysqlTime:
		b = append(b, uintFlag)
		t := d.GetMysqlTime().CoreTime()
		b = encodeUnsignedInt(b, uint64(t), true)
	case types.KindMysqlDuration:
		// duration may have negative value, so we cannot use String to encode directly.
		b = append(b, durationFlag)
		b = EncodeInt(b, int64(d.GetMysqlDuration().Duration))
	case types.KindMysqlDecimal:
		b = append(b, decimalFlag)
		decStr := d.GetMysqlDecimal().ToString()
		b = encodeBytes(b, decStr, false)
	case types.KindMysqlEnum:
		b = encodeUnsignedInt(b, d.GetMysqlEnum().Value, false)
	case types.KindMysqlSet:
		b = encodeUnsignedInt(b, d.GetMysqlSet().Value, false)
	case types.KindMysqlBit, types.KindBinaryLiteral:
		val := d.GetBinaryLiteral()
		b = encodeBytes(b, val, false)
	case types.KindMysqlJSON:
		b = append(b, jsonFlag)
		j := d.GetMysqlJSON()
		b = append(b, j.TypeCode)
		b = append(b, j.Value...)
	case types.KindVectorFloat32:
		b = append(b, vectorFloat32Flag)
		v := d.GetVectorFloat32()
		b = v.SerializeTo(b)
	case types.KindNull:
		b = append(b, NilFlag)
	case types.KindMinNotNull:
		b = append(b, bytesFlag)
	case types.KindMaxValue:
		b = append(b, maxFlag)
	default:
		logutil.BgLogger().Warn("trying to calculate HashCode of an unexpected type of Datum",
			zap.Uint8("Datum Kind", d.Kind()),
			zap.Stack("stack"))
	}
	return b
}
