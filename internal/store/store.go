// Package store holds VaultDB's key-value data.
//
// Reads are served from an in-memory B-tree, so they never touch the disk,
// and keys are always kept in sorted order for range and prefix queries.
// Writes follow the write-ahead rule: the change is appended to the log on
// disk first, and only then applied to the map. If the log write fails, the
// map is left untouched, so memory never holds data the disk doesn't.
//
// The log only ever grows, so the store also compacts it: when most of the
// file is old, overwritten data, it rewrites the log with just one SET per
// live key.
//
// A sync.RWMutex guards everything: many readers at once, one writer at a
// time. Holding the write lock across "append to log, then update map" also
// keeps the log and the map in the same order.
package store

import (
	"errors"
	"strings"
	"sync"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
	"github.com/ABDULRAIYAAN/vaultdb/internal/btree"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("key not found")

// ErrEmptyKey is returned when a caller passes an empty key.
var ErrEmptyKey = errors.New("key must not be empty")

// DefaultCompactMinBytes is the smallest log that auto-compaction will touch.
// Below this, rewriting is not worth the effort.
const DefaultCompactMinBytes = 1 << 20 // 1 MB

// Options configures a persistent store.
type Options struct {
	// Sync fsyncs every write before it returns.
	Sync bool
	// Repair starts even if the log is corrupt, keeping the data before the
	// damage. Without it, a corrupt log stops the database from opening.
	Repair bool
	// AutoCompact rewrites the log automatically once it is at least
	// CompactMinBytes and more than half of it is garbage.
	AutoCompact     bool
	CompactMinBytes int64
}

// DefaultOptions is safe, self-maintaining behaviour.
func DefaultOptions() Options {
	return Options{Sync: true, AutoCompact: true, CompactMinBytes: DefaultCompactMinBytes}
}

// Store is a thread-safe key-value store, optionally backed by a log file.
type Store struct {
	mu   sync.RWMutex
	data *btree.BTree // sorted key -> value
	log  *aol.Log     // nil for a purely in-memory store
	opts Options

	// liveBytes is what the log would weigh if it held only current data:
	// the file header plus one SET record per key. The difference between
	// the real log size and this is garbage that compaction can remove.
	liveBytes      int64
	compactions    int
	lastCompactErr error
}

// New creates an empty in-memory Store. Nothing is saved to disk.
func New() *Store {
	return &Store{data: btree.New(btree.DefaultDegree), liveBytes: int64(aol.FileHeaderSize)}
}

// OpenInfo describes what Open loaded from disk.
type OpenInfo struct {
	Path          string
	Records       int   // log records replayed
	Keys          int   // keys in the database after replay
	TornBytes     int64 // bytes of a half-written record dropped after a crash
	RepairedBytes int64 // bytes cut off by repair mode
	Upgraded      bool  // an old-format log was converted to the current format
	LogBytes      int64 // log file size after opening
}

// Open creates a Store backed by the log file at path, replaying the file to
// rebuild the data.
func Open(path string, opts Options) (*Store, OpenInfo, error) {
	if opts.CompactMinBytes <= 0 {
		opts.CompactMinBytes = DefaultCompactMinBytes
	}
	l, err := aol.Open(path, opts.Sync)
	if err != nil {
		return nil, OpenInfo{}, err
	}
	s := New()
	s.log = l
	s.opts = opts

	res, err := l.Replay(s.apply, aol.ReplayOptions{Repair: opts.Repair})
	if err != nil {
		l.Close()
		return nil, OpenInfo{}, err
	}
	info := OpenInfo{
		Path:          path,
		Records:       res.Records,
		Keys:          s.data.Len(),
		TornBytes:     res.TornBytes,
		RepairedBytes: res.RepairedBytes,
	}

	// An old-format log can't be appended to. Rewriting it converts it.
	if res.Legacy {
		if err := s.compactLocked(); err != nil {
			l.Close()
			return nil, OpenInfo{}, err
		}
		info.Upgraded = true
	}
	info.LogBytes = l.Size()
	return s, info, nil
}

// apply changes the map for one record and keeps liveBytes in step. The
// caller holds the lock (or, during replay, nobody else can see the store).
func (s *Store) apply(r aol.Record) {
	switch r.Op {
	case aol.OpSet:
		if old, ok := s.data.Get(r.Key); ok {
			s.liveBytes -= aol.RecordSize(r.Key, old)
		}
		s.data.Set(r.Key, r.Value)
		s.liveBytes += aol.RecordSize(r.Key, r.Value)
	case aol.OpDelete:
		if old, ok := s.data.Get(r.Key); ok {
			s.liveBytes -= aol.RecordSize(r.Key, old)
			s.data.Delete(r.Key)
		}
	case aol.OpClear:
		s.data.Clear()
		s.liveBytes = int64(aol.FileHeaderSize)
	}
}

// write logs r (if persistent), applies it, and compacts if the log has
// become mostly garbage. Caller holds s.mu.
func (s *Store) write(r aol.Record) error {
	if s.log != nil {
		if err := s.log.Append(r); err != nil {
			return err
		}
	}
	s.apply(r)
	if s.shouldAutoCompact() {
		// The write itself already succeeded and is safely on disk, so a
		// failed compaction is reported through INFO rather than as an error.
		s.lastCompactErr = s.compactLocked()
	}
	return nil
}

func (s *Store) shouldAutoCompact() bool {
	if s.log == nil || !s.opts.AutoCompact {
		return false
	}
	size := s.log.Size()
	return size >= s.opts.CompactMinBytes && size > 2*s.liveBytes
}

// CompactResult reports what a compaction did.
type CompactResult struct {
	BytesBefore int64
	BytesAfter  int64
}

// Compact rewrites the log so it holds exactly one SET record per key.
func (s *Store) Compact() (CompactResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return CompactResult{}, errors.New("nothing to compact: the database is in-memory")
	}
	before := s.log.Size()
	err := s.compactLocked()
	return CompactResult{BytesBefore: before, BytesAfter: s.log.Size()}, err
}

// compactLocked does the rewrite. Caller holds s.mu, so no write can slip in
// between taking the snapshot and swapping the files.
func (s *Store) compactLocked() error {
	// The B-tree hands keys over in sorted order, so compacted files are
	// always identical for identical data.
	err := s.log.Rewrite(func(emit func(aol.Record) error) error {
		var emitErr error
		s.data.Ascend("", "", func(k, v string) bool {
			emitErr = emit(aol.Record{Op: aol.OpSet, Key: k, Value: v})
			return emitErr == nil
		})
		return emitErr
	})
	if err == nil {
		s.compactions++
	}
	return err
}

// Set stores value under key, replacing any existing value.
func (s *Store) Set(key, value string) error {
	if key == "" {
		return ErrEmptyKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(aol.Record{Op: aol.OpSet, Key: key, Value: value})
}

// Get returns the value stored under key.
func (s *Store) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data.Get(key)
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Delete removes key and reports whether it existed. Deleting a missing key
// writes nothing to the log.
func (s *Store) Delete(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Get(key); !ok {
		return false, nil
	}
	if err := s.write(aol.Record{Op: aol.OpDelete, Key: key}); err != nil {
		return false, err
	}
	return true, nil
}

// Exists reports whether key is present.
func (s *Store) Exists(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data.Get(key)
	return ok
}

// Keys returns every key in sorted order.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, s.data.Len())
	s.data.Ascend("", "", func(k, _ string) bool {
		keys = append(keys, k)
		return true
	})
	return keys
}

// KV is one key and its value.
type KV struct {
	Key, Value string
}

// Scan returns up to limit pairs with from <= key < to, in sorted order. An
// empty from means "from the start", an empty to means "to the end", and a
// limit of 0 or less means no limit. more reports whether pairs were left out
// because of the limit.
func (s *Store) Scan(from, to string, limit int) (pairs []KV, more bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.data.Ascend(from, to, func(k, v string) bool {
		if limit > 0 && len(pairs) == limit {
			more = true
			return false
		}
		pairs = append(pairs, KV{k, v})
		return true
	})
	return pairs, more
}

// Prefix returns up to limit pairs whose key starts with prefix, in sorted
// order. All such keys sit next to each other in the B-tree, starting at
// prefix itself, so the walk stops at the first key without the prefix.
func (s *Store) Prefix(prefix string, limit int) (pairs []KV, more bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.data.Ascend(prefix, "", func(k, v string) bool {
		if !strings.HasPrefix(k, prefix) {
			return false
		}
		if limit > 0 && len(pairs) == limit {
			more = true
			return false
		}
		pairs = append(pairs, KV{k, v})
		return true
	})
	return pairs, more
}

// Len returns the number of keys stored.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Len()
}

// Clear removes every key.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(aol.Record{Op: aol.OpClear})
}

// Stats is a snapshot of the store's state, for INFO.
type Stats struct {
	Persistent     bool
	Keys           int
	LogBytes       int64 // size of the log file
	LiveBytes      int64 // size of the current data (what a compacted log would weigh)
	Compactions    int   // compactions since the store was opened
	AutoCompact    bool
	LastCompactErr error
}

// Stats returns current statistics.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{
		Persistent:     s.log != nil,
		Keys:           s.data.Len(),
		Compactions:    s.compactions,
		AutoCompact:    s.opts.AutoCompact,
		LastCompactErr: s.lastCompactErr,
	}
	st.LiveBytes = s.liveBytes
	if s.log != nil {
		st.LogBytes = s.log.Size()
	}
	return st
}

// Close closes the log file. The store must not be used afterwards.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return nil
	}
	return s.log.Close()
}
