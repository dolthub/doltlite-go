package litestore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dolthub/doltlite-go/blob"
	"github.com/dolthub/doltlite-go/prollyhash"
)

func TestPackStoreRejectsCorruptChunk(t *testing.T) {
	ctx := context.Background()
	p := NewPackStore(blob.NewMemBlobStore(), blob.NewMemManifestStore())

	bad := Chunk{Hash: prollyhash.Compute([]byte("claimed")), Data: []byte("actual")}
	if err := p.Put(ctx, []Chunk{bad}); err == nil {
		t.Fatal("Put must reject a chunk whose data does not hash to its claimed address")
	}
}

func TestPackStoreDedupesIdenticalBatch(t *testing.T) {
	ctx := context.Background()
	bs := blob.NewMemBlobStore()
	p := NewPackStore(bs, blob.NewMemManifestStore())

	c := chunk("payload")
	for i := 0; i < 3; i++ {
		if err := p.Put(ctx, []Chunk{c}); err != nil {
			t.Fatal(err)
		}
	}
	// Content-addressed pack keys collapse the three identical uploads into one.
	packs, err := bs.List(ctx, packKeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 {
		t.Fatalf("got %d pack blobs, want 1 (content-addressed dedup)", len(packs))
	}
}

func TestPackStoreGetSpansMultiplePacks(t *testing.T) {
	ctx := context.Background()
	p := NewPackStore(blob.NewMemBlobStore(), blob.NewMemManifestStore())

	a, b, c := chunk("aaa"), chunk("bbbb"), chunk("ccccc")
	if err := p.Put(ctx, []Chunk{a, b}); err != nil { // pack 1
		t.Fatal(err)
	}
	if err := p.Put(ctx, []Chunk{c}); err != nil { // pack 2
		t.Fatal(err)
	}
	for _, want := range []Chunk{a, b, c} {
		got, err := p.Get(ctx, want.Hash)
		if err != nil || !bytes.Equal(got, want.Data) {
			t.Fatalf("Get(%s) = %q, %v", want.Hash, got, err)
		}
	}
}

// A push crosses several HTTP requests, each of which builds a fresh Store over
// the same backing blob + manifest stores. Chunks and refs written by one
// request must be visible to the next.
func TestPackStoreStatelessAcrossInstances(t *testing.T) {
	ctx := context.Background()
	bs, ms := blob.NewMemBlobStore(), blob.NewMemManifestStore()

	a := chunk("cross-request")
	if err := NewPackStore(bs, ms).Put(ctx, []Chunk{a}); err != nil {
		t.Fatal(err)
	}

	present, err := NewPackStore(bs, ms).HasMany(ctx, []prollyhash.Hash{a.Hash})
	if err != nil || !present[0] {
		t.Fatalf("HasMany from a fresh instance = %v, %v; want [true]", present, err)
	}

	refs := []byte("refs-blob")
	if err := NewPackStore(bs, ms).SetRefsIf(ctx, prollyhash.Hash{}, refs); err != nil {
		t.Fatal(err)
	}
	got, err := NewPackStore(bs, ms).GetRefs(ctx)
	if err != nil || !bytes.Equal(got, refs) {
		t.Fatalf("GetRefs from a fresh instance = %q, %v", got, err)
	}
}

// Two independent pushers race on the refs pointer; the one working from a stale
// view must lose with ErrConflict.
func TestPackStoreConcurrentRefsConflict(t *testing.T) {
	ctx := context.Background()
	bs, ms := blob.NewMemBlobStore(), blob.NewMemManifestStore()

	if err := NewPackStore(bs, ms).SetRefsIf(ctx, prollyhash.Hash{}, []byte("winner")); err != nil {
		t.Fatal(err)
	}
	// Second pusher still thinks refs are empty.
	err := NewPackStore(bs, ms).SetRefsIf(ctx, prollyhash.Hash{}, []byte("loser"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale SetRefsIf = %v, want ErrConflict", err)
	}
}
