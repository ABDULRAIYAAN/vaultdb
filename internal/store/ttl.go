package store

import (
	"errors"
	"strconv"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

// Expiring keys
//
// A key can be given a deadline with Expire. Until then it behaves like any
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
//   - when a write touches that key (SET, DEL, EXPIRE, PERSIST),
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
	ms := int64((d + time.Millisecond - 1) / time.Millisecond)
	at := s.nowMs() + ms
	return true, s.write(aol.Record{Op: aol.OpExpire, Key: key, Value: deadlineString(at)})
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

func (s *Store) nowMs() int64 { return s.now().UnixMilli() }

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
