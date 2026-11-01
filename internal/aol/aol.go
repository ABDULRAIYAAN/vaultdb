// Package aol is VaultDB's append-only log: the file that makes data survive
// a restart.
//
// The idea: never edit data in place. Every change (SET, DEL, CLEAR) is
// appended to the end of one file as a record. On startup, the database reads
// the file from the beginning and replays every record, rebuilding the
// in-memory map exactly as it was. Redis (AOF), Bitcask and the write-ahead
// logs in Postgres and SQLite all build on this same idea.
//
// Record layout on disk (big-endian):
//
//	+--------+-------------+-------------+-----------+-------------+
//	| op (1) | keyLen (4)  | valLen (4)  | key bytes | value bytes |
//	+--------+-------------+-------------+-----------+-------------+
//	 9-byte header                         variable    variable
//
// If the computer loses power halfway through writing a record, the file ends
// with a partial ("torn") record. Replay detects this, cuts the file back to
// the last complete record and carries on, so a crash never stops the
// database from starting. Milestone 3 adds checksums to catch other damage.
package aol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Op identifies what a record does.
type Op byte

const (
	OpSet    Op = 1
	OpDelete Op = 2
	OpClear  Op = 3
)

func (o Op) valid() bool { return o >= OpSet && o <= OpClear }

const headerSize = 1 + 4 + 4

// MaxFieldSize caps a key or value at 64 MB. A length above this in the file
// can only mean corruption, and the cap stops us allocating huge buffers for it.
const MaxFieldSize = 64 << 20

// ErrCorrupt means the log holds bytes that are not a valid record and are not
// a simple torn write at the end. VaultDB refuses to start rather than guess.
var ErrCorrupt = errors.New("log file is corrupt")

// ErrTooLarge is returned when a key or value is over MaxFieldSize.
var ErrTooLarge = fmt.Errorf("key or value larger than %d bytes", MaxFieldSize)

// Record is one change to the database.
type Record struct {
	Op    Op
	Key   string
	Value string
}

// Log is an open append-only log file.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	path string
	size int64
	sync bool
}

// Open opens (or creates) the log at path. If syncWrites is true, every Append
// waits until the data is physically on disk (fsync). That is slower but means
// a power cut can never lose a write that already returned "OK".
//
// Call Replay before the first Append.
func Open(path string, syncWrites bool) (*Log, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// Deliberately not O_APPEND: on Windows that mode forbids Truncate, which
	// Replay needs to cut off a torn record. We seek to the end ourselves.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, path: path, sync: syncWrites}, nil
}

// ReplayResult reports what Replay found.
type ReplayResult struct {
	Records     int   // complete records replayed
	TornBytes   int64 // bytes of a partial record cut from the end (0 if none)
	BytesOnDisk int64 // size of the log after replay
}

// Replay reads every record from the start of the file and calls apply for
// each one, in order. Afterwards the log is positioned at the end, ready for
// Append.
func (l *Log) Replay(apply func(Record)) (ReplayResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var res ReplayResult
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return res, err
	}
	info, err := l.f.Stat()
	if err != nil {
		return res, err
	}
	fileSize := info.Size()

	r := bufio.NewReaderSize(l.f, 64*1024)
	var offset int64 // end of the last complete record
	header := make([]byte, headerSize)

	for {
		rec, n, err := readRecord(r, header)
		if err == io.EOF {
			break // clean end of file
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// Torn write at the end: drop the partial record.
			res.TornBytes = fileSize - offset
			if err := l.f.Truncate(offset); err != nil {
				return res, fmt.Errorf("truncating torn record: %w", err)
			}
			break
		}
		if err != nil {
			return res, fmt.Errorf("%w at byte %d: %v", ErrCorrupt, offset, err)
		}
		apply(rec)
		offset += n
		res.Records++
	}

	if _, err := l.f.Seek(offset, io.SeekStart); err != nil {
		return res, err
	}
	l.size = offset
	res.BytesOnDisk = offset
	return res, nil
}

// readRecord reads one record. It returns io.EOF only when no bytes at all
// were left, and io.ErrUnexpectedEOF when a record was cut off part way.
func readRecord(r *bufio.Reader, header []byte) (Record, int64, error) {
	if _, err := io.ReadFull(r, header); err != nil {
		return Record{}, 0, err // io.EOF if empty, io.ErrUnexpectedEOF if partial
	}
	op := Op(header[0])
	keyLen := binary.BigEndian.Uint32(header[1:5])
	valLen := binary.BigEndian.Uint32(header[5:9])

	if !op.valid() {
		return Record{}, 0, fmt.Errorf("unknown op %d", op)
	}
	if keyLen > MaxFieldSize || valLen > MaxFieldSize {
		return Record{}, 0, fmt.Errorf("field length %d/%d over limit", keyLen, valLen)
	}

	body := make([]byte, int(keyLen)+int(valLen))
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF // header present, body missing: torn
		}
		return Record{}, 0, err
	}
	rec := Record{
		Op:    op,
		Key:   string(body[:keyLen]),
		Value: string(body[keyLen:]),
	}
	return rec, int64(headerSize + len(body)), nil
}

// encode turns a record into its on-disk bytes.
func encode(rec Record) ([]byte, error) {
	if len(rec.Key) > MaxFieldSize || len(rec.Value) > MaxFieldSize {
		return nil, ErrTooLarge
	}
	buf := make([]byte, headerSize+len(rec.Key)+len(rec.Value))
	buf[0] = byte(rec.Op)
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(rec.Key)))
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(rec.Value)))
	copy(buf[headerSize:], rec.Key)
	copy(buf[headerSize+len(rec.Key):], rec.Value)
	return buf, nil
}

// Append writes rec to the end of the log. The whole record goes out in a
// single Write call, and with syncWrites on it is fsynced before returning.
func (l *Log) Append(rec Record) error {
	if !rec.Op.valid() {
		return fmt.Errorf("invalid op %d", rec.Op)
	}
	buf, err := encode(rec)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return os.ErrClosed
	}
	prev := l.size
	if _, err := l.f.Write(buf); err != nil {
		// A failed write (disk full, say) may have left part of the record
		// behind. Cut it off so the next record doesn't land after garbage.
		l.f.Truncate(prev)
		l.f.Seek(prev, io.SeekStart)
		return err
	}
	l.size = prev + int64(len(buf))
	if l.sync {
		return l.f.Sync()
	}
	return nil
}

// Size returns the current size of the log in bytes.
func (l *Log) Size() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.size
}

// Path returns the file path of the log.
func (l *Log) Path() string { return l.path }

// Close flushes and closes the file. Calling it twice is safe.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	syncErr := l.f.Sync()
	closeErr := l.f.Close()
	l.f = nil
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
