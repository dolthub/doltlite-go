package blob

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// MemBlobStore is an in-memory BlobStore for tests.
type MemBlobStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func NewMemBlobStore() *MemBlobStore {
	return &MemBlobStore{objs: make(map[string][]byte)}
}

var _ BlobStore = (*MemBlobStore)(nil)

func (m *MemBlobStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = append([]byte(nil), data...)
	return nil
}

func (m *MemBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (m *MemBlobStore) GetRange(_ context.Context, key string, off, length int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	if off < 0 {
		off = 0
	}
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	rest := data[off:]
	if length >= 0 && length < int64(len(rest)) {
		rest = rest[:length]
	}
	return append([]byte(nil), rest...), nil
}

func (m *MemBlobStore) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// MemManifestStore is an in-memory ManifestStore for tests. The version token
// is a monotonic counter rendered as a decimal string.
type MemManifestStore struct {
	mu      sync.Mutex
	data    []byte
	version uint64
	written bool
}

func NewMemManifestStore() *MemManifestStore {
	return &MemManifestStore{}
}

var _ ManifestStore = (*MemManifestStore)(nil)

func (m *MemManifestStore) Load(_ context.Context) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.written {
		return nil, "", nil
	}
	return append([]byte(nil), m.data...), strconv.FormatUint(m.version, 10), nil
}

func (m *MemManifestStore) CompareAndSwap(_ context.Context, expectedVersion string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := ""
	if m.written {
		cur = strconv.FormatUint(m.version, 10)
	}
	if expectedVersion != cur {
		return "", ErrConflict
	}
	m.version++
	m.data = append([]byte(nil), data...)
	m.written = true
	return strconv.FormatUint(m.version, 10), nil
}
