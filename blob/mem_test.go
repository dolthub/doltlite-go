package blob_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dolthub/doltlite-go/blob"
	"github.com/dolthub/doltlite-go/blob/blobtest"
)

func TestMemBlobStoreConformance(t *testing.T) {
	blobtest.RunConformance(t, blob.NewMemBlobStore())
}

// MemBlobStore is lenient about out-of-range reads (returns empty rather than
// erroring). This is mem-specific and deliberately not part of the shared
// conformance, since object stores like S3 answer 416 instead.
func TestMemBlobStoreRangeBeyondEnd(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMemBlobStore()
	_ = s.Put(ctx, "k", []byte("0123456789"))
	for _, off := range []int64{10, 20} {
		got, err := s.GetRange(ctx, "k", off, 5)
		if err != nil || len(got) != 0 {
			t.Fatalf("GetRange(%d,5) = %q,%v; want empty", off, got, err)
		}
	}
}

func TestMemManifestStoreCAS(t *testing.T) {
	ctx := context.Background()
	m := blob.NewMemManifestStore()

	data, version, err := m.Load(ctx)
	if err != nil || data != nil || version != "" {
		t.Fatalf("Load(empty) = %q, %q, %v", data, version, err)
	}

	// A stale create (expecting an existing version) must conflict.
	if _, err := m.CompareAndSwap(ctx, "7", []byte("x")); !errors.Is(err, blob.ErrConflict) {
		t.Fatalf("CAS(wrong version on empty) = %v, want ErrConflict", err)
	}

	// Create requires the empty version.
	v1, err := m.CompareAndSwap(ctx, "", []byte("v1"))
	if err != nil {
		t.Fatalf("initial CAS: %v", err)
	}

	// A second writer holding the stale empty version loses.
	if _, err := m.CompareAndSwap(ctx, "", []byte("racer")); !errors.Is(err, blob.ErrConflict) {
		t.Fatalf("CAS(stale empty) = %v, want ErrConflict", err)
	}

	data, version, _ = m.Load(ctx)
	if !bytes.Equal(data, []byte("v1")) || version != v1 {
		t.Fatalf("Load after create = %q, %q (want v1, %q)", data, version, v1)
	}
	if _, err := m.CompareAndSwap(ctx, v1, []byte("v2")); err != nil {
		t.Fatalf("CAS(current version): %v", err)
	}
	data, _, _ = m.Load(ctx)
	if !bytes.Equal(data, []byte("v2")) {
		t.Fatalf("Load after update = %q, want v2", data)
	}
}
