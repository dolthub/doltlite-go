// Package blob defines the object-storage seams the doltlite pack store sits on,
// plus in-memory implementations for tests. Cloud adapters (e.g. an S3 BlobStore
// or a DynamoDB ManifestStore) implement these interfaces without pulling any
// doltlite-go dependency beyond this package, so the AWS SDK never reaches the
// core module.
package blob

import (
	"context"
	"errors"
)

// ErrNotFound is returned by BlobStore.Get/GetRange for an absent key.
var ErrNotFound = errors.New("blob: object not found")

// ErrConflict is returned by ManifestStore.CompareAndSwap when the stored
// version does not match the expected version.
var ErrConflict = errors.New("blob: compare-and-swap version conflict")

// BlobStore is immutable, uniquely-keyed object storage. A key is written at
// most once (the pack store keys objects by content hash), so there are no
// conditional writes and reads are stable once a key exists.
type BlobStore interface {
	// Put stores data under key. Writing the same key with identical bytes is
	// idempotent; keys are never overwritten with different content.
	Put(ctx context.Context, key string, data []byte) error

	// Get returns the full object, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)

	// GetRange returns length bytes starting at off. A negative length reads from
	// off to the end; a zero length returns an empty slice. off and length must
	// denote a range within the object (as the pack store always guarantees);
	// behavior for an out-of-range request is backend-specific. Returns
	// ErrNotFound for an absent key.
	GetRange(ctx context.Context, key string, off, length int64) ([]byte, error)

	// List returns all keys with the given prefix.
	List(ctx context.Context, prefix string) ([]string, error)
}

// ManifestStore is a single small mutable value with atomic compare-and-swap.
// It holds the pack store's per-repo pointer (the current refs blob). DoltHub
// backs this with a DynamoDB conditional write; a self-hoster can use an S3
// conditional put, a DB row, or a lock file.
type ManifestStore interface {
	// Load returns the current value and its opaque version token. A manifest
	// that has never been written returns (nil, "", nil).
	Load(ctx context.Context) (data []byte, version string, err error)

	// CompareAndSwap writes data iff the stored version equals expectedVersion
	// (the empty string means "must not exist yet"), returning the new version.
	// It returns ErrConflict when the version does not match.
	CompareAndSwap(ctx context.Context, expectedVersion string, data []byte) (version string, err error)
}
