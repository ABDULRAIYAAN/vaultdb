package aol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Reading logs written by VaultDB 0.2 (format version 1).
//
// Version 1 had no file header and no checksums:
//
//	| op (1) | keyLen (4) | valLen (4) | key | value |
//
// Such a log is replayed once, then the store immediately rewrites it in the
// current format, so old data carries over without any manual step.

const v1HeaderSize = 1 + 4 + 4

func (l *Log) replayV1(apply func(Record), fileSize int64, res *ReplayResult) (int64, error) {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReaderSize(l.f, 64*1024)
	var offset int64
	hdr := make([]byte, v1HeaderSize)

	for {
		rec, n, err := readV1Record(r, hdr)
		if err == io.EOF {
			return offset, nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			res.TornBytes = fileSize - offset
			return offset, nil // the rewrite that follows drops the partial bytes
		}
		if err != nil {
			return 0, fmt.Errorf("%w (old format) at byte %d: %v", ErrCorrupt, offset, err)
		}
		apply(rec)
		offset += n
		res.Records++
	}
}

func readV1Record(r *bufio.Reader, hdr []byte) (Record, int64, error) {
	if _, err := io.ReadFull(r, hdr); err != nil {
		return Record{}, 0, err
	}
	op := Op(hdr[0])
	keyLen := binary.BigEndian.Uint32(hdr[1:5])
	valLen := binary.BigEndian.Uint32(hdr[5:9])
	if !op.valid() || keyLen > MaxFieldSize || valLen > MaxFieldSize {
		return Record{}, 0, fmt.Errorf("invalid record header")
	}
	body := make([]byte, int(keyLen)+int(valLen))
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Record{}, 0, err
	}
	return Record{Op: op, Key: string(body[:keyLen]), Value: string(body[keyLen:])}, int64(v1HeaderSize + len(body)), nil
}
