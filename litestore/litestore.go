package litestore

import (
	"context"
	"errors"

	"github.com/dolthub/doltlite-go/prollyhash"
)

var ErrNotFound = errors.New("litestore: not found")

var ErrConflict = errors.New("litestore: refs precondition failed")

type Chunk struct {
	Hash prollyhash.Hash
	Data []byte
}

type Store interface {
	HasMany(ctx context.Context, hashes []prollyhash.Hash) ([]bool, error)

	Get(ctx context.Context, h prollyhash.Hash) ([]byte, error)

	Put(ctx context.Context, chunks []Chunk) error

	GetRefs(ctx context.Context) ([]byte, error)

	SetRefs(ctx context.Context, blob []byte) error

	SetRefsIf(ctx context.Context, expected prollyhash.Hash, blob []byte) error

	Commit(ctx context.Context) error
}
