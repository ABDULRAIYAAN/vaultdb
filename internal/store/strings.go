package store

import (
	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

// String commands
//
// Append, StrLen and GetRange work on a value as a run of bytes (APPEND,
// STRLEN, GETRANGE), the way Redis does. Lengths and offsets count bytes,
// not characters, so "café" is 5 bytes long and a range can cut a
// multi-byte character in half. That keeps them exact and cheap: no
// decoding, and an offset always means the same byte.
//
// Append is a write and runs under the write lock, so two clients appending
// to the same log line at once both land, one after the other, and neither
// is lost the way a GET, concatenate, SET from the client would lose one.
// Like the counters, it keeps the key's deadline, in the same log record as
// the new value.
//
// The log stores whole values, so each Append writes the full new value, not
// just the added part. Appending n small pieces to one key costs about n²/2
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
	// With a deadline the value shares its record with it, and compaction
	// always writes a key with a deadline as one SETEX, so leave room.
	limit := aol.MaxFieldSize
	if _, ok := s.expires[key]; ok {
		limit -= aol.DeadlineSize
	}
	if len(suffix) > limit-len(cur) {
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
