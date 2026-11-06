// Package aol is VaultDB's append-only log: the file that makes data survive
// a restart.
//
// The idea: never edit data in place. Every change (SET, DEL, CLEAR) is
// appended to the end of one file as a record. On startup, the database reads
// the file from the beginning and replays every record, rebuilding the
// in-memory map exactly as it was. Redis (AOF), Bitcask and the write-ahead
// logs in Postgres and SQLite all build on this same idea.
//
// File layout (format version 2):
//
//	+------------------+---------+---------+---------+-----
//	| "VAULTDB\0" + v2 | record  | record  | record  | ...
//	+------------------+---------+---------+---------+-----
//	  9-byte file header
//
// Record layout (big-endian):
//
//	+-------------+--------+------------+------------+--------------+-----+-------+
//	| hdrCRC (4)  | op (1) | keyLen (4) | valLen (4) | bodyCRC (4)  | key | value |
//	+-------------+--------+------------+------------+--------------+-----+-------+
//	 hdrCRC  = CRC-32C of op, keyLen, valLen and bodyCRC
//	 bodyCRC = CRC-32C of key and value
//
// Why two checksums? If one checksum covered the whole record, a flipped bit
// in a length field would make the reader trust a wrong length before it
// could check anything. It might then read past the real end of the record,
// decide the file was "cut off", and throw away every valid record after it.
// Checking the small fixed-size header first means a length is only ever
// used after it has been verified.
//
// What replay does with damage:
//   - A record cut off at the end of the file (power lost mid-write) is a
//     "torn write". It is removed and the database starts normally.
//   - A checksum failure in the middle of the file is real corruption. The
//     database refuses to start rather than load wrong data, unless it is run
//     in repair mode, which keeps everything before the damage.
package aol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
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

const (
	magic         = "VAULTDB\x00"
	formatVersion = 2

	// FileHeaderSize is the size of the magic bytes plus version byte.
	FileHeaderSize = len(magic) + 1

	recordHeaderSize = 4 + 1 + 4 + 4 + 4 // hdrCRC, op, keyLen, valLen, bodyCRC
)

// MaxFieldSize caps a key or value at 64 MB.
const MaxFieldSize = 64 << 20

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

var (
	// ErrCorrupt means the log contains damaged data that is not a simple
	// torn write at the end.
	ErrCorrupt = errors.New("log file is corrupt")

	// ErrTooLarge is returned when a key or value is over MaxFieldSize.
	ErrTooLarge = fmt.Errorf("key or value larger than %d bytes", MaxFieldSize)

	// ErrNeedsRewrite is returned by Append on a log still in the old format.
	// The store rewrites (compacts) such a log right after opening it.
	ErrNeedsRewrite = errors.New("log is in the old format and must be rewritten first")

	errTorn = errors.New("torn record")
)

// Record is one change to the database.
type Record struct {
	Op    Op
	Key   string
	Value string
}

// RecordSize is how many bytes a SET of key and value takes on disk.
func RecordSize(key, value string) int64 {
	return int64(recordHeaderSize + len(key) + len(value))
}

// Log is an open append-only log file.
type Log struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	size   int64
	sync   bool
	legacy bool // file is in format version 1; Append refuses until Rewrite
	replay bool // Replay has run
}

// Open opens (or creates) the log at path. If syncWrites is true, every Append
// waits until the data is physically on disk (fsync).
//
// Call Replay before the first Append.
func Open(path string, syncWrites bool) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// A leftover temp file means a compaction was interrupted before it
	// finished. The original log is still complete, so the temp file is junk.
	if err := os.Remove(tempPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, path: path, sync: syncWrites}, nil
}

// openFile opens path for reading and writing. Deliberately not O_APPEND: on
// Windows that mode forbids Truncate, which Replay needs. We seek ourselves.
func openFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}

func tempPath(path string) string { return path + ".compact" }

// ReplayOptions changes how Replay handles damage.
type ReplayOptions struct {
	// Repair keeps every valid record before the first corrupt one and cuts
	// off the rest, instead of refusing to start.
	Repair bool
}

// ReplayResult reports what Replay found.
type ReplayResult struct {
	Records       int   // complete records replayed
	TornBytes     int64 // bytes of a half-written record removed from the end
	RepairedBytes int64 // bytes cut off by repair mode
	Legacy        bool  // file was in the old (version 1) format
	BytesOnDisk   int64 // size of the log after replay
}

// Replay reads every record from the start of the file and calls apply for
// each one, in order. Afterwards the log is positioned at the end, ready for
// Append.
func (l *Log) Replay(apply func(Record), opts ReplayOptions) (ReplayResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var res ReplayResult
	info, err := l.f.Stat()
	if err != nil {
		return res, err
	}
	fileSize := info.Size()

	head := make([]byte, FileHeaderSize)
	n, err := l.f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return res, err
	}
	head = head[:n]

	var end int64
	switch {
	case fileSize == 0 || (fileSize < int64(FileHeaderSize) && bytes.HasPrefix([]byte(magic), head)):
		// New file, or a crash while writing the very first header.
		if err := l.writeFileHeader(); err != nil {
			return res, err
		}
		end = int64(FileHeaderSize)

	case bytes.HasPrefix(head, []byte(magic)):
		if v := head[len(magic)]; v != formatVersion {
			return res, fmt.Errorf("log format version %d is not supported (this build reads version %d)", v, formatVersion)
		}
		end, err = l.replayV2(apply, fileSize, opts, &res)
		if err != nil {
			return res, err
		}

	default:
		// No magic bytes: a log written by VaultDB 0.2 (format version 1).
		end, err = l.replayV1(apply, fileSize, &res)
		if err != nil {
			return res, err
		}
		res.Legacy = true
		l.legacy = true
	}

	if _, err := l.f.Seek(end, io.SeekStart); err != nil {
		return res, err
	}
	l.size = end
	l.replay = true
	res.BytesOnDisk = end
	return res, nil
}

func (l *Log) writeFileHeader() error {
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	hdr := append([]byte(magic), formatVersion)
	if _, err := l.f.WriteAt(hdr, 0); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *Log) replayV2(apply func(Record), fileSize int64, opts ReplayOptions, res *ReplayResult) (int64, error) {
	if _, err := l.f.Seek(int64(FileHeaderSize), io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReaderSize(l.f, 64*1024)
	offset := int64(FileHeaderSize) // end of the last good record
	hdr := make([]byte, recordHeaderSize)

	for {
		rec, n, err := readRecord(r, hdr, offset, fileSize)
		switch {
		case err == nil:
			apply(rec)
			offset += n
			res.Records++
			continue
		case err == io.EOF:
			return offset, nil
		case errors.Is(err, errTorn):
			res.TornBytes = fileSize - offset
		case errors.Is(err, ErrCorrupt) && opts.Repair:
			res.RepairedBytes = fileSize - offset
		default:
			return 0, err
		}
		// Torn or repaired: cut the file back to the last good record.
		if err := l.f.Truncate(offset); err != nil {
			return 0, fmt.Errorf("truncating log: %w", err)
		}
		return offset, nil
	}
}

// readRecord reads one version-2 record starting at offset. It returns
// io.EOF at a clean end of file, errTorn for a record cut off at the end, and
// an ErrCorrupt-wrapped error for damage.
func readRecord(r *bufio.Reader, hdr []byte, offset, fileSize int64) (Record, int64, error) {
	if _, err := io.ReadFull(r, hdr); err != nil {
		if err == io.ErrUnexpectedEOF {
			return Record{}, 0, errTorn
		}
		return Record{}, 0, err // io.EOF or a read error
	}

	storedHdrCRC := binary.BigEndian.Uint32(hdr[0:4])
	if crc32.Checksum(hdr[4:], castagnoli) != storedHdrCRC {
		return Record{}, 0, fmt.Errorf("%w: record header checksum mismatch at byte %d", ErrCorrupt, offset)
	}
	op := Op(hdr[4])
	keyLen := binary.BigEndian.Uint32(hdr[5:9])
	valLen := binary.BigEndian.Uint32(hdr[9:13])
	bodyCRC := binary.BigEndian.Uint32(hdr[13:17])
	if !op.valid() || keyLen > MaxFieldSize || valLen > MaxFieldSize {
		return Record{}, 0, fmt.Errorf("%w: invalid record header at byte %d", ErrCorrupt, offset)
	}

	body := make([]byte, int(keyLen)+int(valLen))
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Record{}, 0, errTorn // header is valid, so the length is right: body was cut off
		}
		return Record{}, 0, err
	}
	n := int64(recordHeaderSize + len(body))

	if crc32.Checksum(body, castagnoli) != bodyCRC {
		if offset+n == fileSize {
			// Last record in the file: the classic result of a crash mid-write.
			return Record{}, 0, errTorn
		}
		return Record{}, 0, fmt.Errorf("%w: data checksum mismatch at byte %d", ErrCorrupt, offset)
	}

	return Record{Op: op, Key: string(body[:keyLen]), Value: string(body[keyLen:])}, n, nil
}

// encode turns a record into its on-disk bytes.
func encode(rec Record) ([]byte, error) {
	if !rec.Op.valid() {
		return nil, fmt.Errorf("invalid op %d", rec.Op)
	}
	if len(rec.Key) > MaxFieldSize || len(rec.Value) > MaxFieldSize {
		return nil, ErrTooLarge
	}
	buf := make([]byte, recordHeaderSize+len(rec.Key)+len(rec.Value))
	body := buf[recordHeaderSize:]
	copy(body, rec.Key)
	copy(body[len(rec.Key):], rec.Value)

	buf[4] = byte(rec.Op)
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(rec.Key)))
	binary.BigEndian.PutUint32(buf[9:13], uint32(len(rec.Value)))
	binary.BigEndian.PutUint32(buf[13:17], crc32.Checksum(body, castagnoli))
	binary.BigEndian.PutUint32(buf[0:4], crc32.Checksum(buf[4:recordHeaderSize], castagnoli))
	return buf, nil
}

// Append writes rec to the end of the log. The whole record goes out in a
// single Write call, and with syncWrites on it is fsynced before returning.
func (l *Log) Append(rec Record) error {
	buf, err := encode(rec)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.f == nil:
		return os.ErrClosed
	case !l.replay:
		return errors.New("aol: Replay must be called before Append")
	case l.legacy:
		return ErrNeedsRewrite
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

// Rewrite atomically replaces the whole log with the records that snapshot
// emits. This is how compaction works:
//
//  1. Write the new log to a temporary file next to the real one.
//  2. fsync it, so it is fully on disk.
//  3. Rename it over the real log. A rename within one folder is atomic: at
//     every instant the path holds either the complete old log or the
//     complete new one, never a mix.
//
// If the computer crashes before step 3, the old log is untouched and Open
// deletes the leftover temp file. After step 3, the new log is in place.
func (l *Log) Rewrite(snapshot func(emit func(Record) error) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return os.ErrClosed
	}

	tmp := tempPath(l.path)
	if err := writeLogFile(tmp, snapshot); err != nil {
		os.Remove(tmp)
		return err
	}

	// Windows cannot rename over a file that is still open, so close first.
	l.f.Close()
	l.f = nil
	renameErr := os.Rename(tmp, l.path)
	if renameErr != nil {
		os.Remove(tmp)
	} else {
		syncDir(filepath.Dir(l.path))
	}

	// Reopen whichever file is now at the path (new on success, old on failure).
	f, err := openFile(l.path)
	if err != nil {
		return err
	}
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return err
	}
	l.f = f
	l.size = end
	if renameErr != nil {
		return renameErr
	}
	l.legacy = false
	return nil
}

// writeLogFile writes a complete, fsynced version-2 log file to path.
func writeLogFile(path string, snapshot func(emit func(Record) error) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 256*1024)
	writeErr := func() error {
		if _, err := w.Write(append([]byte(magic), formatVersion)); err != nil {
			return err
		}
		err := snapshot(func(rec Record) error {
			buf, err := encode(rec)
			if err != nil {
				return err
			}
			_, err = w.Write(buf)
			return err
		})
		if err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
		return f.Sync() // always sync: a compacted log must be fully on disk
	}()
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// syncDir makes a rename durable on Linux and macOS by syncing the folder.
// Windows does not allow opening a folder this way and does not need it.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
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
