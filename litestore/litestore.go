// Package litestore defines the server-side view of a single doltlite
// database's content-addressed chunk store — exactly the operations needed to
// serve doltlite's HTTP sync protocol (see package remoteproto) — plus an
// in-memory implementation for tests.
//
// The store is intentionally format-agnostic: it stores and retrieves opaque
// chunk bytes keyed by their doltlite chunk address (BLAKE3-20, see package
// prollyhash) and an opaque "refs" blob guarded by a compare-and-swap. It does
// not parse doltlite prolly trees or refs structures.
package litestore

import (
	"context"
	"errors"

	"github.com/dolthub/doltlite-go/prollyhash"
)

// ErrNotFound is returned by Get and GetRefs when the requested content is
// absent. The HTTP layer maps it to 404.
var ErrNotFound = errors.New("litestore: not found")

// ErrConflict is returned by SetRefsIf when the expected refs hash does not
// match the store's current refs. The HTTP layer maps it to 409.
var ErrConflict = errors.New("litestore: refs precondition failed")

// Chunk is a stored chunk: its doltlite address and the bytes that hash to it.
// Callers passing chunks to Put are expected to have verified that
// prollyhash.Compute(Data) == Hash (the litehttp handler enforces this).
type Chunk struct {
	Hash prollyhash.Hash
	Data []byte
}

// Store is a single doltlite database's chunk store on the server side.
//
// Implementations must be safe for concurrent use. SetRefsIf must perform its
// compare-and-set atomically with respect to other SetRefs/SetRefsIf calls so
// that concurrent pushes to the same repository serialize correctly.
type Store interface {
	// HasMany reports, for each requested hash, whether the chunk is present.
	// The result slice is parallel to hashes.
	HasMany(ctx context.Context, hashes []prollyhash.Hash) ([]bool, error)

	// Get returns the bytes of the chunk with the given hash, or ErrNotFound.
	Get(ctx context.Context, h prollyhash.Hash) ([]byte, error)

	// Put durably stores the given chunks, keyed by their Hash. Storing a chunk
	// that already exists is a no-op.
	Put(ctx context.Context, chunks []Chunk) error

	// GetRefs returns the current opaque refs blob, or ErrNotFound if the
	// repository has no refs yet.
	GetRefs(ctx context.Context) ([]byte, error)

	// SetRefs unconditionally installs the refs blob.
	SetRefs(ctx context.Context, blob []byte) error

	// SetRefsIf installs the refs blob only if the store's current refs hash
	// equals expected (BLAKE3-20 of the current refs blob, or the zero hash
	// when there are no refs). Otherwise it returns ErrConflict.
	SetRefsIf(ctx context.Context, expected prollyhash.Hash, blob []byte) error

	// Commit flushes any pending state to durable storage. It corresponds to
	// the protocol's POST /commit barrier. Implementations whose other
	// operations are already durable may implement this as a no-op.
	Commit(ctx context.Context) error
}
