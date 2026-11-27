package store

import (
	"errors"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

// String commands
//
// Append, SetRange, StrLen and GetRange work on a value as a run of bytes
// (APPEND, SETRANGE, STRLEN, GETRANGE), the way Redis does. Lengths and offsets count bytes,
// not characters, so "café" is 5 bytes long and a range can cut a
// multi-byte character in half. That keeps them exact and cheap: no
// decoding, and an offset always means the same byte.
//
// Append and SetRange are writes and run under the write lock, so two
// clients appending to the same log line (or patching different parts of
// one value) at once both land, one after the other, and neither is lost the
// way a GET, edit, SET from the client would lose one. Like the counters,
// they keep the key's deadline, in the same log record as the new value.
//
// The log stores whole values, so each Append or SetRange writes the full
// new value, not just the changed part. Appending n small pieces to one key costs about n²/2
// bytes of log, which the usual compaction cleans up. That is fine for the
// log lines, audit trails and small buffers APPEND is for; a value that keeps
// growing for ever belongs in many keys (or a SQL table) instead.

// Append adds suffix to the end of the value at key and returns the value's
// new length in bytes. A missing (or expired) key starts as "", so Append
// creates it. A result over the size limit is refused and nothing is
// written.
func (s *Store) Append(key, suffix string) (int, error) {
	if key == "" {
		return 0, ErrEmptyKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur string
	existed := s.liveLocked(key)
	if existed {
		cur, _ = s.data.Get(key)
	}
	if len(suffix) > s.valueLimitLocked(key)-len(cur) {
		return 0, aol.ErrTooLarge
	}
	if suffix == "" && existed {
		return len(cur), nil // nothing changes, so nothing to log
	}
	v := cur + suffix
	if err := s.setKeepDeadlineLocked(key, v); err != nil {
		return 0, err
	}
	return len(v), nil
}

// StrLen returns the length in bytes of the value at key, or 0 if the key is
// missing (or expired), like Redis.
func (s *Store) StrLen(key string) int {
	v, err := s.Get(key)
	if err != nil {
		return 0
	}
	return len(v)
}

// GetRange returns the bytes of the value at key from start to end, both
// included. Negative offsets count back from the end, so -1 is the last
// byte and GetRange(k, 0, -1) is the whole value. Offsets past either end
// are pulled back to it, and a range that ends up empty (start after end,
// or starting past the last byte) returns "". A missing key returns "" too,
// as in Redis.
func (s *Store) GetRange(key string, start, end int64) string {
	v, err := s.Get(key)
	if err != nil {
		return ""
	}
	return byteRange(v, start, end)
}

// byteRange is the slicing half of GetRange, kept apart so it can be fuzzed.
func byteRange(v string, start, end int64) string {
	n := int64(len(v))
	if n == 0 {
		return ""
	}
	if start < 0 {
		start += n
	}
	if end < 0 {
		end += n
	}
	if start < 0 {
		start = 0
	}
	if end >= n {
		end = n - 1
	}
	// After clamping, a start past the last byte or an end before the
	// first one both leave start > end.
	if start > end {
		return ""
	}
	return v[start : end+1]
}

// ErrOffsetRange is returned by SetRange for a negative offset.
var ErrOffsetRange = errors.New("offset is out of range")

// SetRange overwrites the value at key with text, starting offset bytes in,
// and returns the value's new length (SETRANGE). If the value is shorter
// than offset, the gap is filled with zero bytes first, so SetRange on a
// missing key at offset 3 makes "\x00\x00\x00" + text. Bytes past the end
// of text are left as they were.
//
// Like Redis, an empty text changes nothing and writes nothing, not even for
// a missing key: it just reports the current length. A result over the size
// limit is refused, and the key's deadline is kept, as with Append.
func (s *Store) SetRange(key string, offset int64, text string) (int, error) {
	if key == "" {
		return 0, ErrEmptyKey
	}
	if offset < 0 {
		return 0, ErrOffsetRange
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur string
	existed := s.liveLocked(key)
	if existed {
		cur, _ = s.data.Get(key)
	}
	if text == "" {
		return len(cur), nil
	}
	v, err := overwriteAt(cur, offset, text, s.valueLimitLocked(key))
	if err != nil {
		return 0, err
	}
	if existed && v == cur {
		return len(v), nil // same bytes as before, so nothing to log
	}
	if err := s.setKeepDeadlineLocked(key, v); err != nil {
		return 0, err
	}
	return len(v), nil
}

// valueLimitLocked is the largest value key can hold. With a deadline the
// value shares its record with it, and compaction always writes a key with a
// deadline as one SETEX, so it has to leave room.
func (s *Store) valueLimitLocked(key string) int {
	if _, ok := s.expires[key]; ok {
		return aol.MaxFieldSize - aol.DeadlineSize
	}
	return aol.MaxFieldSize
}

// overwriteAt is the byte work of SetRange, kept apart so it can be fuzzed:
// cur with text written over it from offset on, zero-padded if cur is
// shorter than offset. A result longer than limit is refused with
// aol.ErrTooLarge before anything is allocated, so a huge offset can't make
// it try to build a huge value.
func overwriteAt(cur string, offset int64, text string, limit int) (string, error) {
	if offset < 0 {
		return "", ErrOffsetRange
	}
	// Checking offset first means offset+len(text) can't overflow below,
	// even for an offset near MaxInt64.
	if offset > int64(limit) {
		return "", aol.ErrTooLarge
	}
	n := len(cur)
	if end := int(offset) + len(text); end > n {
		n = end
	}
	// This also catches a cur that was already over the limit, say a
	// full-size value that has since been given a deadline.
	if n > limit {
		return "", aol.ErrTooLarge
	}
	b := make([]byte, n) // new bytes start as zero, which is the padding
	copy(b, cur)
	copy(b[offset:], text)
	return string(b), nil
}
