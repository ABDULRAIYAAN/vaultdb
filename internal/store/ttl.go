package store

import (
	"errors"
	"strconv"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

// Expiring keys
//
// A key can be given a deadline with Expire (a duration from now) or
// ExpireAt (a moment), and Deadline reads it back. Until then it behaves like any
// other key; from the deadline on it reads as missing everywhere (Get,
// Exists, Keys, Scan, Prefix, Len), as if it had been deleted at that exact
// moment.
//
// The deadline is saved as an EXPIRE record in the log, right after the key's
// SET, holding the absolute time in Unix milliseconds. Saving the absolute
// time rather than "60 seconds" means a restart doesn't give the key a fresh
// minute: replay rebuilds the key and its deadline, and Open then drops every
// key whose deadline passed while the database was closed.
//
// Removing an expired key from memory is lazy, the way Redis does it. Reads
// can't change anything (they only hold the read lock), so they just skip
// expired keys. The memory is freed:
//   - when a write touches that key (SET, DEL, EXPIRE, PERSIST, GETDEL,
//     GETEX),
//   - after every write, by checking up to reapBatch keys that have a
//     deadline and dropping the expired ones,
//   - at compaction, which also leaves them out of the new log,
//   - and when the database is opened.
//
// None of these write a DEL record. The log already says when the key runs
// out, so replaying it always arrives at the same answer, as long as the
// clock doesn't go backwards between runs.
//
// A plain SET replaces a key's value and clears its deadline, like Redis.
// SetWith can set a value and a deadline together (SET ... EX), which is one
// SETEX record in the log, so a crash can never keep the value but lose the
// deadline. Compaction writes every key that has a deadline as a SETEX too.

// MaxTTL is the longest time to live a key can be given, so that a deadline
// always fits in a time.Duration and an int64 of milliseconds.
const MaxTTL = 100 * 365 * 24 * time.Hour

// ErrTTLTooLong is returned by Expire for a duration over MaxTTL.
var ErrTTLTooLong = errors.New("time to live is too long (the limit is 100 years)")

// reapBatch is how many keys with a deadline each write checks.
const reapBatch = 20

// Expire gives key a deadline d from now, replacing any earlier one, and
// reports whether the key exists. A d of zero or less deletes the key
// straight away. Durations under a millisecond round up to one.
func (s *Store) Expire(key string, d time.Duration) (bool, error) {
	if d > MaxTTL {
		return false, ErrTTLTooLong
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.liveLocked(key) {
		return false, nil
	}
	if d <= 0 {
		return true, s.write(aol.Record{Op: aol.OpDelete, Key: key})
	}
	return true, s.expireLocked(key, s.nowMs()+ceilMs(d))
}

// ExpireAt is Expire with an absolute deadline (EXPIREAT, PEXPIREAT). A
// moment that has already come deletes the key straight away, and one more
// than MaxTTL away is refused. Times between two milliseconds round up.
func (s *Store) ExpireAt(key string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if at.Sub(now) > MaxTTL {
		return false, ErrTTLTooLong
	}
	if !s.liveLocked(key) {
		return false, nil
	}
	if !at.After(now) {
		return true, s.write(aol.Record{Op: aol.OpDelete, Key: key})
	}
	return true, s.expireLocked(key, ceilUnixMs(at))
}

// expireLocked writes key's new deadline. The key is live and the deadline
// is in the future. Caller holds the write lock.
func (s *Store) expireLocked(key string, at int64) error {
	// Compaction will write the key as one SETEX, deadline first, so the
	// value has to leave room for it.
	if v, _ := s.data.Get(key); len(v) > aol.MaxFieldSize-aol.DeadlineSize {
		return aol.ErrTooLarge
	}
	return s.write(aol.Record{Op: aol.OpExpire, Key: key, Value: deadlineString(at)})
}

// SetOptions changes what SetWith does. The zero value is a plain Set.
type SetOptions struct {
	// TTL, if above zero, makes the key expire this long from now. Under a
	// millisecond rounds up to one.
	TTL time.Duration
	// At, if not the zero time, makes the key expire at that moment (EXAT,
	// PXAT). A moment that has already come leaves the key deleted, the way
	// Redis does it. Times between two milliseconds round up.
	At time.Time
	// KeepTTL keeps the key's current deadline instead of clearing it.
	KeepTTL bool
	// IfMissing only writes if the key doesn't exist (NX); IfExists only
	// if it does (XX).
	IfMissing, IfExists bool
}

// ErrBadSetOptions is returned by SetWith for options that contradict each
// other.
var ErrBadSetOptions = errors.New("bad SET options: a negative TTL, more than one of TTL, At and KeepTTL, or IfMissing with IfExists")

// SetWith stores value under key like Set, with the extras in o, and reports
// whether the SET went ahead (false when IfMissing or IfExists said no).
//
// A value with a deadline is written as a single SETEX record, so the value
// and the deadline reach the disk together or not at all.
func (s *Store) SetWith(key, value string, o SetOptions) (bool, error) {
	_, _, wrote, err := s.SetWithOld(key, value, o)
	return wrote, err
}

// SetWithOld is SetWith that also returns the value the key held just
// before (SET ... GET). existed is false if the key was missing or had
// expired. Reading the old value and writing the new one happen under the
// same lock, so no other write can land in between, and the old value is
// returned even when IfMissing stops the write.
func (s *Store) SetWithOld(key, value string, o SetOptions) (old string, existed, wrote bool, err error) {
	hasAt := !o.At.IsZero()
	switch {
	case key == "":
		return "", false, false, ErrEmptyKey
	case o.TTL < 0, o.TTL > 0 && (o.KeepTTL || hasAt), hasAt && o.KeepTTL, o.IfMissing && o.IfExists:
		return "", false, false, ErrBadSetOptions
	case o.TTL > MaxTTL:
		return "", false, false, ErrTTLTooLong
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if hasAt && o.At.Sub(now) > MaxTTL {
		return "", false, false, ErrTTLTooLong
	}
	existed = s.liveLocked(key)
	if existed {
		old, _ = s.data.Get(key)
	}
	if (o.IfMissing && existed) || (o.IfExists && !existed) {
		return old, existed, false, nil
	}
	var at int64 // deadline, 0 for none
	switch {
	case o.TTL > 0:
		at = s.nowMs() + ceilMs(o.TTL)
	case hasAt && !o.At.After(now):
		// Already past: the new value would be expired the moment it's
		// stored, so the key just ends up gone. A missing key has nothing
		// to delete and nothing needs logging.
		if existed {
			err = s.write(aol.Record{Op: aol.OpDelete, Key: key})
		}
		return old, existed, err == nil, err
	case hasAt:
		at = ceilUnixMs(o.At)
	case o.KeepTTL && existed:
		at = s.expires[key] // 0 if it has none
	}
	if at == 0 {
		err = s.write(aol.Record{Op: aol.OpSet, Key: key, Value: value})
		return old, existed, err == nil, err
	}
	if len(value) > aol.MaxFieldSize-aol.DeadlineSize {
		return old, existed, false, aol.ErrTooLarge // checked here too for in-memory stores
	}
	err = s.write(aol.Record{Op: aol.OpSetExpire, Key: key, Value: aol.SetExpireValue(at, value)})
	return old, existed, err == nil, err
}

// Persist removes key's deadline, so it lives until deleted. It reports
// whether there was a deadline to remove.
func (s *Store) Persist(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.liveLocked(key) {
		return false, nil
	}
	if _, ok := s.expires[key]; !ok {
		return false, nil
	}
	return true, s.write(aol.Record{Op: aol.OpExpire, Key: key})
}

// GetExOptions says what GetEx does to the key's expiry. The zero value
// leaves it alone, which makes GetEx a plain Get.
type GetExOptions struct {
	// TTL, if above zero, makes the key expire this long from now (EX, PX).
	// Under a millisecond rounds up to one.
	TTL time.Duration
	// At, if not the zero time, makes the key expire at that moment (EXAT,
	// PXAT). A moment that has already come deletes the key, after reading
	// it. Times between two milliseconds round up.
	At time.Time
	// Persist removes the key's deadline (PERSIST).
	Persist bool
}

// ErrBadGetExOptions is returned by GetEx for options that contradict each
// other.
var ErrBadGetExOptions = errors.New("bad GETEX options: a negative TTL, or more than one of TTL, At and Persist")

// GetEx returns key's value and changes its expiry as o says (GETEX), as
// one step under the write lock. A session read that also pushes its
// timeout back is the usual use: done as GET then EXPIRE, the key could run
// out in between and the EXPIRE would find nothing. A missing (or expired)
// key returns ErrNotFound and writes nothing.
func (s *Store) GetEx(key string, o GetExOptions) (string, error) {
	hasAt := !o.At.IsZero()
	n := 0
	for _, set := range []bool{o.TTL != 0, hasAt, o.Persist} {
		if set {
			n++
		}
	}
	switch {
	case o.TTL < 0, n > 1:
		return "", ErrBadGetExOptions
	case o.TTL > MaxTTL:
		return "", ErrTTLTooLong
	case n == 0:
		return s.Get(key) // changes nothing, so the read lock is enough
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if hasAt && o.At.Sub(now) > MaxTTL {
		return "", ErrTTLTooLong
	}
	if !s.liveLocked(key) {
		return "", ErrNotFound
	}
	v, _ := s.data.Get(key)
	var err error
	switch {
	case o.TTL > 0:
		err = s.expireLocked(key, s.nowMs()+ceilMs(o.TTL))
	case hasAt && !o.At.After(now):
		err = s.write(aol.Record{Op: aol.OpDelete, Key: key})
	case hasAt:
		err = s.expireLocked(key, ceilUnixMs(o.At))
	case o.Persist:
		if _, ok := s.expires[key]; ok {
			err = s.write(aol.Record{Op: aol.OpExpire, Key: key})
		} // no deadline: nothing to remove, nothing to log
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// TTL returns how long key has left. hasDeadline is false for a key that
// never expires, and err is ErrNotFound if the key doesn't exist (or has
// already expired).
func (s *Store) TTL(key string) (left time.Duration, hasDeadline bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.nowMs()
	if _, ok := s.data.Get(key); !ok || s.expired(key, now) {
		return 0, false, ErrNotFound
	}
	at, ok := s.expires[key]
	if !ok {
		return 0, false, nil
	}
	return time.Duration(at-now) * time.Millisecond, true, nil
}

// Deadline returns the moment key expires (EXPIRETIME, PEXPIRETIME), to the
// millisecond. hasDeadline is false for a key that never expires, and err is
// ErrNotFound if the key doesn't exist (or has already expired).
func (s *Store) Deadline(key string) (at time.Time, hasDeadline bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.data.Get(key); !ok || s.expired(key, s.nowMs()) {
		return time.Time{}, false, ErrNotFound
	}
	ms, ok := s.expires[key]
	if !ok {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(ms), true, nil
}

func (s *Store) nowMs() int64 { return s.now().UnixMilli() }

// ceilMs is d in whole milliseconds, rounded up.
func ceilMs(d time.Duration) int64 { return int64((d + time.Millisecond - 1) / time.Millisecond) }

// ceilUnixMs is t as Unix milliseconds, rounded up.
func ceilUnixMs(t time.Time) int64 {
	ms := t.UnixMilli() // rounds down, also before 1970
	if t.After(time.UnixMilli(ms)) {
		ms++
	}
	return ms
}

func deadlineString(at int64) string { return strconv.FormatInt(at, 10) }

// expired reports whether key has a deadline that has passed. Caller holds
// s.mu (read or write).
func (s *Store) expired(key string, now int64) bool {
	at, ok := s.expires[key]
	return ok && at <= now
}

// expiredCount is how many keys are past their deadline but still in
// memory. Caller holds s.mu.
func (s *Store) expiredCount(now int64) int {
	n := 0
	for _, at := range s.expires {
		if at <= now {
			n++
		}
	}
	return n
}

// liveLocked reports whether key exists and hasn't expired, dropping it from
// memory if it has. Caller holds the write lock.
func (s *Store) liveLocked(key string) bool {
	if _, ok := s.data.Get(key); !ok {
		return false
	}
	if s.expired(key, s.nowMs()) {
		s.drop(key)
		return false
	}
	return true
}

// reapSomeLocked checks up to reapBatch keys that have a deadline and drops
// the expired ones. Go starts each map walk at a random place, so over many
// writes every key gets checked. Caller holds the write lock.
func (s *Store) reapSomeLocked() {
	now, n := s.nowMs(), 0
	for k, at := range s.expires {
		if n == reapBatch {
			return
		}
		n++
		if at <= now {
			s.drop(k)
		}
	}
}

// reapAllLocked drops every expired key. Caller holds the write lock (or,
// during Open, nobody else can see the store).
func (s *Store) reapAllLocked() {
	now := s.nowMs()
	for k, at := range s.expires {
		if at <= now {
			s.drop(k)
		}
	}
}
