package litestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/doltlite-go/blob"
	"github.com/dolthub/doltlite-go/prollyhash"
)

const (
	packKeyPrefix = "pack/"
	idxKeyPrefix  = "idx/"

	// manifestCASRetries bounds optimistic-concurrency retries on the refs
	// pointer before giving up with ErrConflict.
	manifestCASRetries = 20
)

// PackStore is a Store that persists chunks as immutable, content-addressed
// pack blobs (each with a sibling index blob) in a blob.BlobStore, and keeps the
// single mutable refs pointer in a blob.ManifestStore guarded by compare-and-swap.
// This is the "pack-per-push" backend: each Put writes one immutable pack; chunk
// lookups discover packs by listing index blobs. It is storage-agnostic — plug in
// an S3 BlobStore + DynamoDB ManifestStore for hosted use, or the in-memory ones
// for tests.
type PackStore struct {
	blobs blob.BlobStore
	man   blob.ManifestStore
}

func NewPackStore(blobs blob.BlobStore, man blob.ManifestStore) *PackStore {
	return &PackStore{blobs: blobs, man: man}
}

var _ Store = (*PackStore)(nil)

// idxEntry locates one chunk within its pack blob. The pack key is derived from
// the index blob's key, so it need not be repeated per entry.
type idxEntry struct {
	H string `json:"h"`
	O int64  `json:"o"`
	L int64  `json:"l"`
}

type packManifest struct {
	Refs    []byte `json:"refs,omitempty"`
	RefsSet bool   `json:"refs_set"`
}

func (p *PackStore) Put(ctx context.Context, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	var pack []byte
	entries := make([]idxEntry, 0, len(chunks))
	for _, c := range chunks {
		if prollyhash.Compute(c.Data) != c.Hash {
			return fmt.Errorf("litestore: chunk %s failed integrity check", c.Hash)
		}
		entries = append(entries, idxEntry{H: c.Hash.String(), O: int64(len(pack)), L: int64(len(c.Data))})
		pack = append(pack, c.Data...)
	}

	// Content-addressed pack key: identical batches collapse to one immutable
	// object, so re-uploads and retries are naturally idempotent.
	name := prollyhash.Compute(pack).String()
	if err := p.blobs.Put(ctx, packKeyPrefix+name, pack); err != nil {
		return err
	}
	idxBytes, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return p.blobs.Put(ctx, idxKeyPrefix+name, idxBytes)
}

type chunkLoc struct {
	packKey string
	off     int64
	length  int64
}

// buildIndex discovers every pack by listing index blobs and returns a map from
// chunk hash to its location. First writer wins on duplicate hashes.
func (p *PackStore) buildIndex(ctx context.Context) (map[prollyhash.Hash]chunkLoc, error) {
	keys, err := p.blobs.List(ctx, idxKeyPrefix)
	if err != nil {
		return nil, err
	}
	idx := make(map[prollyhash.Hash]chunkLoc)
	for _, k := range keys {
		data, err := p.blobs.Get(ctx, k)
		if err != nil {
			if errors.Is(err, blob.ErrNotFound) {
				continue
			}
			return nil, err
		}
		var entries []idxEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("litestore: corrupt index %s: %w", k, err)
		}
		packKey := packKeyPrefix + strings.TrimPrefix(k, idxKeyPrefix)
		for _, e := range entries {
			h, err := prollyhash.Parse(e.H)
			if err != nil {
				return nil, fmt.Errorf("litestore: corrupt index %s: %w", k, err)
			}
			if _, ok := idx[h]; !ok {
				idx[h] = chunkLoc{packKey: packKey, off: e.O, length: e.L}
			}
		}
	}
	return idx, nil
}

func (p *PackStore) HasMany(ctx context.Context, hashes []prollyhash.Hash) ([]bool, error) {
	idx, err := p.buildIndex(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(hashes))
	for i, h := range hashes {
		_, out[i] = idx[h]
	}
	return out, nil
}

func (p *PackStore) Get(ctx context.Context, h prollyhash.Hash) ([]byte, error) {
	idx, err := p.buildIndex(ctx)
	if err != nil {
		return nil, err
	}
	loc, ok := idx[h]
	if !ok {
		return nil, ErrNotFound
	}
	return p.blobs.GetRange(ctx, loc.packKey, loc.off, loc.length)
}

// Commit is a no-op: pack and index blobs are durable as soon as Put returns.
// The refs compare-and-swap is what publishes a push.
func (p *PackStore) Commit(_ context.Context) error { return nil }

func (p *PackStore) GetRefs(ctx context.Context) ([]byte, error) {
	m, _, err := p.loadManifest(ctx)
	if err != nil {
		return nil, err
	}
	if !m.RefsSet {
		return nil, ErrNotFound
	}
	return append([]byte(nil), m.Refs...), nil
}

func (p *PackStore) SetRefs(ctx context.Context, refs []byte) error {
	return p.updateRefs(ctx, func(packManifest) (packManifest, error) {
		return packManifest{Refs: refs, RefsSet: true}, nil
	})
}

func (p *PackStore) SetRefsIf(ctx context.Context, expected prollyhash.Hash, refs []byte) error {
	return p.updateRefs(ctx, func(cur packManifest) (packManifest, error) {
		var curHash prollyhash.Hash
		if cur.RefsSet {
			curHash = prollyhash.Compute(cur.Refs)
		}
		if curHash != expected {
			return packManifest{}, ErrConflict
		}
		return packManifest{Refs: refs, RefsSet: true}, nil
	})
}

func (p *PackStore) loadManifest(ctx context.Context) (packManifest, string, error) {
	data, version, err := p.man.Load(ctx)
	if err != nil {
		return packManifest{}, "", err
	}
	var m packManifest
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return packManifest{}, "", fmt.Errorf("litestore: corrupt manifest: %w", err)
		}
	}
	return m, version, nil
}

// updateRefs applies mutate to the current manifest and commits it with a
// version compare-and-swap, retrying on a lost race. mutate may return
// ErrConflict to reject its precondition (e.g. a stale SetRefsIf).
func (p *PackStore) updateRefs(ctx context.Context, mutate func(packManifest) (packManifest, error)) error {
	for i := 0; i < manifestCASRetries; i++ {
		cur, version, err := p.loadManifest(ctx)
		if err != nil {
			return err
		}
		next, err := mutate(cur)
		if err != nil {
			return err
		}
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		_, err = p.man.CompareAndSwap(ctx, version, data)
		if err == nil {
			return nil
		}
		if errors.Is(err, blob.ErrConflict) {
			continue // another writer advanced the manifest; reload and retry
		}
		return err
	}
	return ErrConflict
}
