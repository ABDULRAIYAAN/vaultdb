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
	if err := s.setKeepDeadlineLocked(key, strconv.FormatInt(n, 10)); err != nil {
		return 0, err
	}
	return n, nil
}

// setKeepDeadlineLocked writes v to key and keeps the key's deadline, if it
// has one, in the same log record. The caller holds the write lock and has
// already dropped key if it had expired.
func (s *Store) setKeepDeadlineLocked(key, v string) error {
	if at, ok := s.expires[key]; ok {
		return s.write(aol.Record{Op: aol.OpSetExpire, Key: key, Value: aol.SetExpireValue(at, v)})
	}
	return s.write(aol.Record{Op: aol.OpSet, Key: key, Value: v})
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

// Decimal counters
//
// IncrByFloat is IncrBy for numbers with a fractional part (INCRBYFLOAT): a
// balance, a running total of seconds, a score. Values are read and added as
// 64-bit floats, so results carry the usual float rounding (0.1 + 0.2 is
// 0.30000000000000004). Redis uses 80-bit floats and rounds to 17 digits, so
// the last digit can differ from what Redis would print.
//
// A value counts as a number when it is written in plain decimal: an
// optional sign, digits with an optional point, and an optional exponent
// ("10", "-3.5", ".5", "5.", "+1e3", "2.5E-3"). Spaces, "inf", "nan", hex
// and "_" are refused, and so is anything too big for a float64. Integer
// counters are numbers here too, so INCRBYFLOAT works on a key INCR made.
//
// The result is written in the shortest plain decimal that reads back as the
// same float64, never with an exponent and never as "-0". A whole result is
// written with no point ("3", not "3.0"), so INCR keeps working on it. A
// result that would be infinite or not a number is refused and nothing is
// written. As with IncrBy, the key's deadline is kept.

// ErrNotFloat is returned by IncrByFloat when the key holds something that
// isn't a decimal number.
var ErrNotFloat = errors.New("value is not a valid float")

// ErrFloatOverflow is returned by IncrByFloat when the result would be
// infinite or not a number.
var ErrFloatOverflow = errors.New("increment would produce NaN or Infinity")

// IncrByFloat adds delta to the number stored at key and returns the new
// value as it was stored. A missing key starts at 0.
func (s *Store) IncrByFloat(key string, delta float64) (string, error) {
	if key == "" {
		return "", ErrEmptyKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur float64
	if s.liveLocked(key) {
		v, _ := s.data.Get(key)
		f, ok := ParseDecimal(v)
		if !ok {
			return "", ErrNotFloat
		}
		cur = f
	}
	n := cur + delta
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return "", ErrFloatOverflow
	}
	v := FormatDecimal(n)
	if err := s.setKeepDeadlineLocked(key, v); err != nil {
		return "", err
	}
	return v, nil
}

// ParseDecimal reads v as a float64 if it is a finite number written in
// plain decimal (see IncrByFloat for the accepted forms).
func ParseDecimal(v string) (float64, bool) {
	if !isDecimal(v) {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// FormatDecimal writes f the way IncrByFloat stores it.
func FormatDecimal(f float64) string {
	if f == 0 {
		return "0" // and not "-0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// isDecimal reports whether v is [+-] digits [. digits] [(e|E) [+-] digits],
// with at least one digit before or after the point.
func isDecimal(v string) bool {
	i := 0
	if i < len(v) && (v[i] == '+' || v[i] == '-') {
		i++
	}
	digits := 0
	for i < len(v) && '0' <= v[i] && v[i] <= '9' {
		i++
		digits++
	}
	if i < len(v) && v[i] == '.' {
		i++
		for i < len(v) && '0' <= v[i] && v[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(v) && (v[i] == 'e' || v[i] == 'E') {
		i++
		if i < len(v) && (v[i] == '+' || v[i] == '-') {
			i++
		}
		exp := 0
		for i < len(v) && '0' <= v[i] && v[i] <= '9' {
			i++
			exp++
		}
		if exp == 0 {
			return false
		}
	}
	return i == len(v)
}
