package aol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Batch records
//
// An OpBatch record carries several changes that must happen together or
// not at all. Its Key is empty and its Value is the changes packed one after
// another:
//
//	+--------+------------+------------+-----+-------+--------+-----
//	| op (1) | keyLen (4) | valLen (4) | key | value | op (1) | ...
//	+--------+------------+------------+-----+-------+--------+-----
//
// The point is crash safety. Writing MSET a 1 b 2 as two SET records means a
// crash between them leaves a on disk but not b. As one record, the batch is
// covered by one pair of checksums: replay either finds the whole thing or
// treats it as a torn write and drops all of it.
//
// A batch can't hold another batch. Compaction never writes batches (it
// writes one record per key), so a compacted log has none.

// batchEntryHeaderSize is the op, keyLen and valLen in front of each change.
const batchEntryHeaderSize = 1 + 4 + 4

// ErrBadBatch is returned by SplitBatch for a Value that isn't a well-formed
// batch.
var ErrBadBatch = errors.New("malformed batch record")

// BatchValue packs recs into the Value of an OpBatch record. It refuses an
// empty batch, a nested batch, and a result over MaxFieldSize.
func BatchValue(recs []Record) (string, error) {
	if len(recs) == 0 {
		return "", errors.New("aol: empty batch")
	}
	size := 0
	for _, r := range recs {
		if !r.Op.valid() || r.Op == OpBatch {
			return "", fmt.Errorf("aol: op %d can't go in a batch", r.Op)
		}
		// Checking each part first keeps size from overflowing below.
		if len(r.Key) > MaxFieldSize || len(r.Value) > MaxFieldSize {
			return "", ErrTooLarge
		}
		size += batchEntryHeaderSize + len(r.Key) + len(r.Value)
		if size > MaxFieldSize {
			return "", ErrTooLarge
		}
	}
	b := make([]byte, 0, size)
	for _, r := range recs {
		b = append(b, byte(r.Op))
		b = binary.BigEndian.AppendUint32(b, uint32(len(r.Key)))
		b = binary.BigEndian.AppendUint32(b, uint32(len(r.Value)))
		b = append(b, r.Key...)
		b = append(b, r.Value...)
	}
	return string(b), nil
}

// SplitBatch unpacks an OpBatch Value into its changes, in order. Anything
// that doesn't parse exactly (a length running past the end, bytes left
// over, an unknown or nested op, no changes at all) is ErrBadBatch.
func SplitBatch(v string) ([]Record, error) {
	if v == "" {
		return nil, ErrBadBatch
	}
	var recs []Record
	for len(v) > 0 {
		if len(v) < batchEntryHeaderSize {
			return nil, ErrBadBatch
		}
		op := Op(v[0])
		keyLen := uint64(binary.BigEndian.Uint32([]byte(v[1:5])))
		valLen := uint64(binary.BigEndian.Uint32([]byte(v[5:9])))
		v = v[batchEntryHeaderSize:]
		// uint64, so keyLen+valLen can't wrap around on a 32-bit build.
		if !op.valid() || op == OpBatch || keyLen+valLen > uint64(len(v)) {
			return nil, ErrBadBatch
		}
		recs = append(recs, Record{Op: op, Key: v[:keyLen], Value: v[keyLen : keyLen+valLen]})
		v = v[keyLen+valLen:]
	}
	return recs, nil
}
