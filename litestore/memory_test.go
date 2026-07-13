package litestore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dolthub/doltlite-go/blob"
	"github.com/dolthub/doltlite-go/prollyhash"
)

func chunk(s string) Chunk {
	d := []byte(s)
	return Chunk{Hash: prollyhash.Compute(d), Data: d}
}

// runStoreConformance exercises the behavioral contract every Store must honor.
// It runs against each implementation so the pack store is held to exactly the
// same guarantees as the reference in-memory store.
func runStoreConformance(t *testing.T, newStore func() Store) {
	ctx := context.Background()

	t.Run("Chunks", func(t *testing.T) {
		m := newStore()
		a, b := chunk("alpha"), chunk("beta")
		if err := m.Put(ctx, []Chunk{a}); err != nil {
			t.Fatal(err)
		}
		present, err := m.HasMany(ctx, []prollyhash.Hash{a.Hash, b.Hash})
		if err != nil {
			t.Fatal(err)
		}
		if !present[0] || present[1] {
			t.Fatalf("HasMany = %v, want [true false]", present)
		}
		got, err := m.Get(ctx, a.Hash)
		if err != nil || !bytes.Equal(got, a.Data) {
			t.Fatalf("Get(a) = %q, %v", got, err)
		}
		if _, err := m.Get(ctx, b.Hash); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(absent) = %v, want ErrNotFound", err)
		}
	})

	t.Run("EmptyPutIsNoop", func(t *testing.T) {
		m := newStore()
		if err := m.Put(ctx, nil); err != nil {
			t.Fatalf("Put(nil): %v", err)
		}
	})

	t.Run("GetReturnsCopy", func(t *testing.T) {
		m := newStore()
		c := chunk("mutable")
		if err := m.Put(ctx, []Chunk{c}); err != nil {
			t.Fatal(err)
		}
		got, _ := m.Get(ctx, c.Hash)
		got[0] ^= 0xff
		again, _ := m.Get(ctx, c.Hash)
		if !bytes.Equal(again, c.Data) {
			t.Fatal("Get must return a copy; store was mutated by caller")
		}
	})

	t.Run("RefsCAS", func(t *testing.T) {
		m := newStore()
		if _, err := m.GetRefs(ctx); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetRefs on empty = %v, want ErrNotFound", err)
		}
		first := []byte("refs-v1")
		if err := m.SetRefsIf(ctx, prollyhash.Hash{}, first); err != nil {
			t.Fatalf("initial SetRefsIf: %v", err)
		}
		got, err := m.GetRefs(ctx)
		if err != nil || !bytes.Equal(got, first) {
			t.Fatalf("GetRefs = %q, %v", got, err)
		}
		if err := m.SetRefsIf(ctx, prollyhash.Hash{}, []byte("stale")); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale SetRefsIf = %v, want ErrConflict", err)
		}
		second := []byte("refs-v2")
		if err := m.SetRefsIf(ctx, prollyhash.Compute(first), second); err != nil {
			t.Fatalf("SetRefsIf with current hash: %v", err)
		}
		got, _ = m.GetRefs(ctx)
		if !bytes.Equal(got, second) {
			t.Fatalf("GetRefs after update = %q, want %q", got, second)
		}
	})

	t.Run("SetRefsUnconditional", func(t *testing.T) {
		m := newStore()
		if err := m.SetRefs(ctx, []byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := m.SetRefs(ctx, []byte("y")); err != nil {
			t.Fatal(err)
		}
		got, _ := m.GetRefs(ctx)
		if !bytes.Equal(got, []byte("y")) {
			t.Fatalf("GetRefs = %q, want y", got)
		}
	})
}

func TestMemStoreConformance(t *testing.T) {
	runStoreConformance(t, func() Store { return NewMemStore() })
}

func TestPackStoreConformance(t *testing.T) {
	runStoreConformance(t, func() Store {
		return NewPackStore(blob.NewMemBlobStore(), blob.NewMemManifestStore())
	})
}
