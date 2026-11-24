package store

import (
	"errors"
	"math"
	"strconv"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

// Counters
//
// IncrBy treats a key's value as a 64-bit integer and adds to it, as one step
// under the write lock (INCR, DECR, INCRBY, DECRBY). Doing GET, add one, SET
// from a client loses updates when two clients do it at once: both read 41,
// both write 42. Here the read and the write can't be split, so a thousand
// INCRs from a hundred clients always add up to a thousand.
//
// Values are still plain strings. Only one form counts as an integer: the
// one strconv.FormatInt would write, so "42" and "-7" do, but "+42", "042",
// "-0", " 42" and "4.2" don't. That keeps the value IncrBy reads and the
// value it writes back in the same form, and it is what Redis accepts too.
//
// A missing (or expired) key counts as 0. A key's deadline is kept, like
// Redis: a rate-limit counter that resets every minute shouldn't live
// forever just because someone hit it. With a deadline the new value is
// written as one SETEX record, so a crash can't keep the value but lose the
// deadline.

// ErrNotInteger is returned by IncrBy when the key holds something that isn't
// a whole number that fits in 64 bits.
var ErrNotInteger = errors.New("value is not an integer or out of range")

// ErrOverflow is returned by IncrBy when the result would not fit in 64 bits.
var ErrOverflow = errors.New("increment or decrement would overflow")

// IncrBy adds delta to the integer stored at key and returns the result. A
// missing key starts at 0. When the value isn't an integer, or the sum would
// overflow, nothing is written.
func (s *Store) IncrBy(key string, delta int64) (int64, error) {
	if key == "" {
		return 0, ErrEmptyKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur int64
	if s.liveLocked(key) {
		v, _ := s.data.Get(key)
		n, ok := parseInteger(v)
		if !ok {
			return 0, ErrNotInteger
		}
		cur = n
	}
	if delta > 0 && cur > math.MaxInt64-delta || delta < 0 && cur < math.MinInt64-delta {
		return 0, ErrOverflow
	}
	n := cur + delta
	v := strconv.FormatInt(n, 10)
	var err error
	if at, ok := s.expires[key]; ok {
		err = s.write(aol.Record{Op: aol.OpSetExpire, Key: key, Value: aol.SetExpireValue(at, v)})
	} else {
		err = s.write(aol.Record{Op: aol.OpSet, Key: key, Value: v})
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}

// parseInteger reads v as an int64 if it is written exactly the way
// strconv.FormatInt would write it.
func parseInteger(v string) (int64, bool) {
	if len(v) == 0 || len(v) > 20 { // "-9223372036854775808" is 20 bytes
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != v {
		return 0, false
	}
	return n, true
}
