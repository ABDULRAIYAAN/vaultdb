package sql

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Row format
//
// Every row is stored as one value in the key-value store. Up to v1.0.0 that
// value was JSON text, like [1,"Ali",3.5,true]. Decoding JSON for every row a
// query touches was most of the cost of a full table scan, so rows are now
// written in a small binary format instead:
//
//	+------+-------------+---------+---------+-----
//	| 0x01 | count       | value 1 | value 2 | ...
//	+------+-------------+---------+---------+-----
//	 format  uvarint
//
// Each value is one tag byte followed by its data:
//
//	tagNull, tagFalse, tagTrue   nothing (the tag is the value)
//	tagInt                       signed varint (zig-zag, small numbers take 1 byte)
//	tagFloat                     8 bytes, the IEEE 754 bits, little-endian
//	tagText                      uvarint length, then the raw bytes
//
// A JSON row always starts with '[' and a binary row with 0x01, so the first
// byte says which decoder to use. Old rows keep working with no migration
// step, and each one is rewritten in the new format the next time an UPDATE
// touches it. The same trick (a version byte in front of every record) is how
// Postgres and SQLite can read pages written by older versions.
//
// The binary format also stores text exactly. JSON has to replace bytes that
// are not valid UTF-8 with U+FFFD, so a value could change on its way through
// the store.

const rowFormatBinary byte = 0x01

const (
	tagNull byte = iota
	tagFalse
	tagTrue
	tagInt
	tagFloat
	tagText
)

var errDamagedRow = errors.New("stored row is damaged")

// encodeRow turns a row into its stored form. Values must already have been
// through coerce, so each is nil, int64, float64, string or bool.
func encodeRow(row []any) (string, error) {
	size := 1 + binary.MaxVarintLen64
	for _, v := range row {
		size++
		switch v := v.(type) {
		case int64:
			size += binary.MaxVarintLen64
		case float64:
			size += 8
		case string:
			size += binary.MaxVarintLen64 + len(v)
		}
	}
	buf := make([]byte, 0, size)
	buf = append(buf, rowFormatBinary)
	buf = binary.AppendUvarint(buf, uint64(len(row)))
	for _, v := range row {
		switch v := v.(type) {
		case nil:
			buf = append(buf, tagNull)
		case bool:
			if v {
				buf = append(buf, tagTrue)
			} else {
				buf = append(buf, tagFalse)
			}
		case int64:
			buf = append(buf, tagInt)
			buf = binary.AppendVarint(buf, v)
		case float64:
			buf = append(buf, tagFloat)
			buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(v))
		case string:
			buf = append(buf, tagText)
			buf = binary.AppendUvarint(buf, uint64(len(v)))
			buf = append(buf, v...)
		default:
			return "", fmt.Errorf("cannot store a value of type %T", v)
		}
	}
	return string(buf), nil
}

// decodeRow reads a stored row in either format and checks that it has one
// value per column and that each value has the column's type.
func decodeRow(raw string, s *schema) ([]any, error) {
	if len(raw) > 0 && raw[0] == rowFormatBinary {
		return decodeBinaryRow(raw, s)
	}
	return decodeJSONRow(raw, s)
}

func decodeBinaryRow(raw string, s *schema) ([]any, error) {
	r := rowReader{raw: raw, pos: 1}
	n, ok := r.uvarint()
	if !ok || n != uint64(len(s.Columns)) {
		return nil, errDamagedRow
	}
	vals := make([]any, len(s.Columns))
	for i, c := range s.Columns {
		tag, ok := r.byte()
		if !ok {
			return nil, errDamagedRow
		}
		switch {
		case tag == tagNull:
			// vals[i] is already nil
		case (tag == tagFalse || tag == tagTrue) && c.Type == TypeBool:
			vals[i] = tag == tagTrue
		case tag == tagInt && c.Type == TypeInt:
			u, ok := r.uvarint()
			if !ok {
				return nil, errDamagedRow
			}
			vals[i] = int64(u>>1) ^ -int64(u&1) // undo zig-zag, as binary.Varint does
		case tag == tagFloat && c.Type == TypeFloat:
			bits, ok := r.uint64()
			if !ok {
				return nil, errDamagedRow
			}
			vals[i] = math.Float64frombits(bits)
		case tag == tagText && c.Type == TypeText:
			n, ok := r.uvarint()
			if !ok || n > uint64(len(r.raw)-r.pos) {
				return nil, errDamagedRow
			}
			// Slicing a string shares its memory, so text costs no copy.
			vals[i] = r.raw[r.pos : r.pos+int(n)]
			r.pos += int(n)
		default:
			return nil, errDamagedRow
		}
	}
	if r.pos != len(r.raw) {
		return nil, errDamagedRow
	}
	return vals, nil
}

// rowReader walks through a binary row. It works on the string directly
// instead of converting it to []byte, which would copy every row.
type rowReader struct {
	raw string
	pos int
}

func (r *rowReader) byte() (byte, bool) {
	if r.pos >= len(r.raw) {
		return 0, false
	}
	b := r.raw[r.pos]
	r.pos++
	return b, true
}

// uvarint reads an unsigned varint: 7 bits per byte, low bits first, the top
// bit set on every byte except the last.
func (r *rowReader) uvarint() (uint64, bool) {
	var x uint64
	for shift := uint(0); shift < 64; shift += 7 {
		b, ok := r.byte()
		if !ok {
			return 0, false
		}
		if shift == 63 && b > 1 {
			return 0, false // more than 64 bits
		}
		x |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return x, true
		}
	}
	return 0, false
}

func (r *rowReader) uint64() (uint64, bool) {
	if len(r.raw)-r.pos < 8 {
		return 0, false
	}
	var x uint64
	for i := 7; i >= 0; i-- {
		x = x<<8 | uint64(r.raw[r.pos+i])
	}
	r.pos += 8
	return x, true
}

// decodeJSONRow reads a row written before the binary format existed.
func decodeJSONRow(raw string, s *schema) ([]any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber() // keep 42 and 42.0 apart until we know the column type
	var vals []any
	if err := dec.Decode(&vals); err != nil || len(vals) != len(s.Columns) {
		return nil, errDamagedRow
	}
	for i, v := range vals {
		if num, ok := v.(json.Number); ok {
			if s.Columns[i].Type == TypeInt {
				vals[i], _ = num.Int64()
			} else {
				vals[i], _ = num.Float64()
			}
		}
	}
	return vals, nil
}
