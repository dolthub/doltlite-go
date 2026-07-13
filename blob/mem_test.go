package blob

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestMemBlobStorePutGet(t *testing.T) {
	ctx := context.Background()
	s := NewMemBlobStore()

	if err := s.Put(ctx, "pack/a", []byte("hello world")); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "pack/a")
	if err != nil || !bytes.Equal(got, []byte("hello world")) {
		t.Fatalf("Get = %q, %v", got, err)
	}

	if _, err := s.Get(ctx, "pack/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(absent) = %v, want ErrNotFound", err)
	}
}

func TestMemBlobStoreGetReturnsCopy(t *testing.T) {
	ctx := context.Background()
	s := NewMemBlobStore()
	_ = s.Put(ctx, "k", []byte("data"))
	got, _ := s.Get(ctx, "k")
	got[0] ^= 0xff
	again, _ := s.Get(ctx, "k")
	if !bytes.Equal(again, []byte("data")) {
		t.Fatal("Get must return a copy; store was mutated by caller")
	}
}

func TestMemBlobStoreGetRange(t *testing.T) {
	ctx := context.Background()
	s := NewMemBlobStore()
	_ = s.Put(ctx, "k", []byte("0123456789"))

	cases := []struct {
		off, length int64
		want        string
	}{
		{0, 3, "012"},
		{4, 2, "45"},
		{7, -1, "789"},         // negative length reads to end
		{0, 100, "0123456789"}, // length past end clamps
		{10, 5, ""},            // off at end
		{20, 5, ""},            // off past end
	}
	for _, c := range cases {
		got, err := s.GetRange(ctx, "k", c.off, c.length)
		if err != nil {
			t.Fatalf("GetRange(%d,%d): %v", c.off, c.length, err)
		}
		if string(got) != c.want {
			t.Fatalf("GetRange(%d,%d) = %q, want %q", c.off, c.length, got, c.want)
		}
	}

	if _, err := s.GetRange(ctx, "missing", 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRange(absent) = %v, want ErrNotFound", err)
	}
}

func TestMemBlobStoreList(t *testing.T) {
	ctx := context.Background()
	s := NewMemBlobStore()
	for _, k := range []string{"idx/2", "idx/1", "pack/1", "pack/2"} {
		_ = s.Put(ctx, k, []byte("x"))
	}
	got, err := s.List(ctx, "idx/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"idx/1", "idx/2"} // sorted, prefix-filtered
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List(idx/) = %v, want %v", got, want)
	}
}

func TestMemManifestStoreCAS(t *testing.T) {
	ctx := context.Background()
	m := NewMemManifestStore()

	// Empty manifest.
	data, version, err := m.Load(ctx)
	if err != nil || data != nil || version != "" {
		t.Fatalf("Load(empty) = %q, %q, %v", data, version, err)
	}

	// A stale create (expecting an existing version) must conflict.
	if _, err := m.CompareAndSwap(ctx, "7", []byte("x")); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS(wrong version on empty) = %v, want ErrConflict", err)
	}

	// Create requires the empty version.
	v1, err := m.CompareAndSwap(ctx, "", []byte("v1"))
	if err != nil {
		t.Fatalf("initial CAS: %v", err)
	}

	// A second writer holding the stale empty version loses.
	if _, err := m.CompareAndSwap(ctx, "", []byte("racer")); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS(stale empty) = %v, want ErrConflict", err)
	}

	// Load reflects v1, and CAS with the current version succeeds.
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
