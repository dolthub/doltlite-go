package litestore

import (
	"context"
	"sync"

	"github.com/dolthub/doltlite-go/prollyhash"
)

// MemStore is an in-memory Store, useful for tests and local development. It is
// safe for concurrent use.
type MemStore struct {
	mu      sync.Mutex
	chunks  map[prollyhash.Hash][]byte
	refs    []byte
	refsSet bool
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{chunks: make(map[prollyhash.Hash][]byte)}
}

var _ Store = (*MemStore)(nil)

func (m *MemStore) HasMany(_ context.Context, hashes []prollyhash.Hash) ([]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]bool, len(hashes))
	for i, h := range hashes {
		_, ok := m.chunks[h]
		out[i] = ok
	}
	return out, nil
}

func (m *MemStore) Get(_ context.Context, h prollyhash.Hash) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.chunks[h]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (m *MemStore) Put(_ context.Context, chunks []Chunk) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range chunks {
		if _, ok := m.chunks[c.Hash]; ok {
			continue
		}
		m.chunks[c.Hash] = append([]byte(nil), c.Data...)
	}
	return nil
}

func (m *MemStore) GetRefs(_ context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.refsSet {
		return nil, ErrNotFound
	}
	return append([]byte(nil), m.refs...), nil
}

func (m *MemStore) SetRefs(_ context.Context, blob []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setRefsLocked(blob)
	return nil
}

func (m *MemStore) SetRefsIf(_ context.Context, expected prollyhash.Hash, blob []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refsHashLocked() != expected {
		return ErrConflict
	}
	m.setRefsLocked(blob)
	return nil
}

func (m *MemStore) Commit(_ context.Context) error { return nil }

func (m *MemStore) setRefsLocked(blob []byte) {
	m.refs = append([]byte(nil), blob...)
	m.refsSet = true
}

// refsHashLocked returns the current refs hash: the zero sentinel when there
// are no refs, otherwise BLAKE3-20 of the refs blob. This mirrors doltlite's
// refsTableGetHash, which the C server compares against the client-supplied
// expected hash in handlePutRefsIf.
func (m *MemStore) refsHashLocked() prollyhash.Hash {
	if !m.refsSet {
		return prollyhash.Hash{}
	}
	return prollyhash.Compute(m.refs)
}
