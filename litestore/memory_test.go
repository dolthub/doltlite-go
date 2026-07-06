package litestore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dolthub/doltlite-go/prollyhash"
)

func chunk(s string) Chunk {
	d := []byte(s)
	return Chunk{Hash: prollyhash.Compute(d), Data: d}
}

func TestMemStoreChunks(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()

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
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, a.Data) {
		t.Fatalf("Get returned %q, want %q", got, a.Data)
	}

	if _, err := m.Get(ctx, b.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(absent) = %v, want ErrNotFound", err)
	}
}

func TestMemStoreGetReturnsCopy(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
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
}

func TestMemStoreRefsCAS(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()

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

	if err := m.SetRefsIf(ctx, prollyhash.Hash{}, []byte("refs-v2-stale")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale SetRefsIf = %v, want ErrConflict", err)
	}

	cur := prollyhash.Compute(first)
	second := []byte("refs-v2")
	if err := m.SetRefsIf(ctx, cur, second); err != nil {
		t.Fatalf("SetRefsIf with current hash: %v", err)
	}
	got, _ = m.GetRefs(ctx)
	if !bytes.Equal(got, second) {
		t.Fatalf("GetRefs after update = %q, want %q", got, second)
	}
}

func TestMemStoreSetRefsUnconditional(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
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
}
