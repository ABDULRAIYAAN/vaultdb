// Package store holds VaultDB's key-value data.
//
// Reads are served from an in-memory map, so they never touch the disk.
// Writes follow the write-ahead rule: the change is appended to the log on
// disk first, and only then applied to the map. If the log write fails, the
// map is left untouched, so memory never holds data the disk doesn't.
//
// A sync.RWMutex guards everything: many readers at once, one writer at a
// time. Holding the write lock across "append to log, then update map" also
// keeps the log and the map in the same order.
package store

import (
	"errors"
	"sort"
	"sync"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("key not found")

// ErrEmptyKey is returned when a caller passes an empty key.
var ErrEmptyKey = errors.New("key must not be empty")

// Store is a thread-safe key-value store, optionally backed by a log file.
type Store struct {
	mu   sync.RWMutex
	data map[string]string
	log  *aol.Log // nil for a purely in-memory store
}

// New creates an empty in-memory Store. Nothing is saved to disk.
func New() *Store {
	return &Store{data: make(map[string]string)}
}

// OpenInfo describes what Open loaded from disk.
type OpenInfo struct {
	Path      string
	Records   int   // log records replayed
	Keys      int   // keys in the database after replay
	TornBytes int64 // bytes of a half-written record dropped after a crash
	LogBytes  int64 // log file size
}

// Open creates a Store backed by the log file at path, replaying the file to
// rebuild the data. With syncWrites, every write is fsynced to disk before it
// returns.
func Open(path string, syncWrites bool) (*Store, OpenInfo, error) {
	l, err := aol.Open(path, syncWrites)
	if err != nil {
		return nil, OpenInfo{}, err
	}
	s := &Store{data: make(map[string]string), log: l}

	res, err := l.Replay(s.apply)
	if err != nil {
		l.Close()
		return nil, OpenInfo{}, err
	}
	info := OpenInfo{
		Path:      path,
		Records:   res.Records,
		Keys:      len(s.data),
		TornBytes: res.TornBytes,
		LogBytes:  res.BytesOnDisk,
	}
	return s, info, nil
}

// apply changes the map for one record. The caller holds the lock (or, during
// replay, nobody else can see the store yet).
func (s *Store) apply(r aol.Record) {
	switch r.Op {
	case aol.OpSet:
		s.data[r.Key] = r.Value
	case aol.OpDelete:
		delete(s.data, r.Key)
	case aol.OpClear:
		s.data = make(map[string]string)
	}
}

// write logs r (if persistent) and then applies it. Caller holds s.mu.
func (s *Store) write(r aol.Record) error {
	if s.log != nil {
		if err := s.log.Append(r); err != nil {
			return err
		}
	}
	s.apply(r)
	return nil
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
	v, ok := s.data[key]
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
	if _, ok := s.data[key]; !ok {
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
	_, ok := s.data[key]
	return ok
}

// Keys returns every key in sorted order.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Len returns the number of keys stored.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// Clear removes every key.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(aol.Record{Op: aol.OpClear})
}

// Persistent reports whether the store saves to disk.
func (s *Store) Persistent() bool { return s.log != nil }

// LogSize returns the log file size in bytes (0 for an in-memory store).
func (s *Store) LogSize() int64 {
	if s.log == nil {
		return 0
	}
	return s.log.Size()
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
