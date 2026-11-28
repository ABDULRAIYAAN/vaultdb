package store

import (
	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

// Multi-key commands
//
// MSet, MSetNX and MGet work on many keys in one call (MSET, MSETNX, MGET),
// the way Redis does. Besides saving round trips, the writes are all or
// nothing, twice over:
//
//   - In memory, they run under one write lock, so no other client ever
//     sees some of the keys set and others not.
//   - On disk, all the SETs go to the log as one OpBatch record (see
//     aol/batch.go). One record has one pair of checksums, so a crash in the
//     middle of writing it loses the whole MSET on replay, never half of it.
//
// That makes MSET the right way to change values that only make sense
// together, like a user's name and email, or a balance moved between two
// keys.

// MSet stores every pair, replacing existing values and removing any
// expiry, like a SET on each. If a key comes up more than once, the last
// value wins. Either every pair is written or none is.
func (s *Store) MSet(pairs []KV) error {
	if err := checkKeys(pairs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msetLocked(pairs)
}

// MSetNX stores every pair only if none of the keys exist (MSETNX), and
// reports whether it did. One existing key means nothing is written, so it
// can claim a group of names at once: two callers racing for overlapping
// keys can't each end up with part of them.
func (s *Store) MSetNX(pairs []KV) (bool, error) {
	if err := checkKeys(pairs); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range pairs {
		if s.liveLocked(p.Key) {
			return false, nil
		}
	}
	if err := s.msetLocked(pairs); err != nil {
		return false, err
	}
	return true, nil
}

// MGet returns the value of each key, in the order given. found[i] is false
// for a key that is missing or expired, and values[i] is then "". Every key
// is read at the same moment, under one read lock, so an MSET can't land
// half way through.
func (s *Store) MGet(keys []string) (values []string, found []bool) {
	values = make([]string, len(keys))
	found = make([]bool, len(keys))
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.nowMs()
	for i, k := range keys {
		if v, ok := s.data.Get(k); ok && !s.expired(k, now) {
			values[i], found[i] = v, true
		}
	}
	return values, found
}

// checkKeys refuses an empty key anywhere in pairs, before anything is
// locked or written.
func checkKeys(pairs []KV) error {
	for _, p := range pairs {
		if p.Key == "" {
			return ErrEmptyKey
		}
	}
	return nil
}

// msetLocked writes pairs as one log record: a plain SET for a single pair,
// otherwise a batch of SETs. Caller holds the write lock.
func (s *Store) msetLocked(pairs []KV) error {
	switch len(pairs) {
	case 0:
		return nil
	case 1:
		return s.write(aol.Record{Op: aol.OpSet, Key: pairs[0].Key, Value: pairs[0].Value})
	}
	recs := make([]aol.Record, len(pairs))
	for i, p := range pairs {
		recs[i] = aol.Record{Op: aol.OpSet, Key: p.Key, Value: p.Value}
	}
	v, err := aol.BatchValue(recs)
	if err != nil {
		return err
	}
	return s.write(aol.Record{Op: aol.OpBatch, Value: v})
}
