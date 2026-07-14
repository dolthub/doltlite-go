// Package blobtest provides a reusable conformance suite for blob.BlobStore
// implementations, so every backend (in-memory, S3, ...) is held to the same
// contract. Import it from a _test.go file and call RunConformance.
package blobtest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dolthub/doltlite-go/blob"
)

// RunConformance exercises the blob.BlobStore contract against a fresh, empty
// store. It only issues valid sub-ranges (the pack store never asks for a range
// outside an object), so backends need not agree on out-of-range behavior.
func RunConformance(t *testing.T, s blob.BlobStore) {
	t.Helper()
	ctx := context.Background()

	if _, err := s.Get(ctx, "missing"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Get(absent) = %v, want ErrNotFound", err)
	}
	if _, err := s.GetRange(ctx, "missing", 0, 1); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("GetRange(absent) = %v, want ErrNotFound", err)
	}

	data := []byte("0123456789")
	if err := s.Put(ctx, "pack/a", data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ctx, "pack/a")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v; want %q", got, err, data)
	}

	// Get must return a copy: mutating the result cannot corrupt the store.
	got[0] ^= 0xff
	again, _ := s.Get(ctx, "pack/a")
	if !bytes.Equal(again, data) {
		t.Fatal("Get must return a copy; store was mutated via the returned slice")
	}

	for _, c := range []struct {
		off, length int64
		want        string
	}{
		{0, 3, "012"},
		{4, 2, "45"},
		{0, 10, "0123456789"},
		{7, -1, "789"}, // negative length reads to end
	} {
		r, err := s.GetRange(ctx, "pack/a", c.off, c.length)
		if err != nil || string(r) != c.want {
			t.Fatalf("GetRange(%d,%d) = %q,%v; want %q", c.off, c.length, r, err, c.want)
		}
	}

	for _, k := range []string{"idx/1", "idx/2", "other/x"} {
		if err := s.Put(ctx, k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.List(ctx, "idx/")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	if !set["idx/1"] || !set["idx/2"] || set["other/x"] || set["pack/a"] {
		t.Fatalf("List(idx/) = %v, want exactly {idx/1, idx/2}", keys)
	}
}
