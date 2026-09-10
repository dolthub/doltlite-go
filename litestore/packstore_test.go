package litestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/doltlite-go/blob"
	"github.com/dolthub/doltlite-go/prollyhash"
)

type countingBlobStore struct {
	blob.BlobStore
	lists      atomic.Int64
	gets       atomic.Int64
	rangeReads atomic.Int64
}

func (s *countingBlobStore) List(ctx context.Context, prefix string) ([]string, error) {
	s.lists.Add(1)
	return s.BlobStore.List(ctx, prefix)
}

func (s *countingBlobStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets.Add(1)
	return s.BlobStore.Get(ctx, key)
}

func (s *countingBlobStore) GetRange(ctx context.Context, key string, off, length int64) ([]byte, error) {
	s.rangeReads.Add(1)
	return s.BlobStore.GetRange(ctx, key, off, length)
}

type blockingBlobStore struct {
	blob.BlobStore
	blockGet   bool
	blockRange bool
	started    chan struct{}
	release    chan struct{}
	active     atomic.Int64
	maxActive  atomic.Int64
}

type reorderedIndexBlobStore struct {
	blob.BlobStore
	secondDone chan struct{}
}

func (s *reorderedIndexBlobStore) Get(ctx context.Context, key string) ([]byte, error) {
	if key == idxKeyPrefix+"a" {
		select {
		case <-s.secondDone:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	data, err := s.BlobStore.Get(ctx, key)
	if key == idxKeyPrefix+"b" {
		close(s.secondDone)
	}
	return data, err
}

func (s *blockingBlobStore) wait(ctx context.Context) error {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		old := s.maxActive.Load()
		if n <= old || s.maxActive.CompareAndSwap(old, n) {
			break
		}
	}
	s.started <- struct{}{}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *blockingBlobStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.blockGet {
		if err := s.wait(ctx); err != nil {
			return nil, err
		}
	}
	return s.BlobStore.Get(ctx, key)
}

func (s *blockingBlobStore) GetRange(ctx context.Context, key string, off, length int64) ([]byte, error) {
	if s.blockRange {
		if err := s.wait(ctx); err != nil {
			return nil, err
		}
	}
	return s.BlobStore.GetRange(ctx, key, off, length)
}

func waitForConcurrentReads(t *testing.T, store *blockingBlobStore, done <-chan error) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for i := 0; i < 32; i++ {
		select {
		case <-store.started:
		case err := <-done:
			close(store.release)
			t.Fatalf("operation finished after %d concurrent reads: %v", i, err)
		case <-timer.C:
			close(store.release)
			<-done
			t.Fatalf("only %d reads started concurrently", i)
		}
	}
	select {
	case <-store.started:
		close(store.release)
		<-done
		t.Fatal("more than 32 reads started concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := store.maxActive.Load(); got != 32 {
		t.Fatalf("maximum concurrent reads = %d, want 32", got)
	}
}

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

func TestPackStoreGetManyBuildsIndexOnce(t *testing.T) {
	ctx := context.Background()
	bs := &countingBlobStore{BlobStore: blob.NewMemBlobStore()}
	p := NewPackStore(bs, blob.NewMemManifestStore())

	a, b := chunk("first"), chunk("second")
	if err := p.Put(ctx, []Chunk{a}); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, []Chunk{b}); err != nil {
		t.Fatal(err)
	}
	missing := prollyhash.Compute([]byte("missing"))
	got, err := p.GetMany(ctx, []prollyhash.Hash{a.Hash, missing, b.Hash, a.Hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || !bytes.Equal(got[0], a.Data) || got[1] != nil || !bytes.Equal(got[2], b.Data) || !bytes.Equal(got[3], a.Data) {
		t.Fatalf("GetMany = %q", got)
	}
	if bs.lists.Load() != 1 || bs.gets.Load() != 2 || bs.rangeReads.Load() != 2 {
		t.Fatalf("blob reads = lists:%d gets:%d ranges:%d, want 1, 2, 2", bs.lists.Load(), bs.gets.Load(), bs.rangeReads.Load())
	}
}

func TestPackStoreBuildIndexReadsConcurrently(t *testing.T) {
	ctx := context.Background()
	base := blob.NewMemBlobStore()
	seed := NewPackStore(base, blob.NewMemManifestStore())
	hashes := make([]prollyhash.Hash, 40)
	for i := range hashes {
		c := chunk(fmt.Sprintf("index-concurrency-%d", i))
		hashes[i] = c.Hash
		if err := seed.Put(ctx, []Chunk{c}); err != nil {
			t.Fatal(err)
		}
	}

	blocking := &blockingBlobStore{
		BlobStore: base,
		blockGet:  true,
		started:   make(chan struct{}, len(hashes)),
		release:   make(chan struct{}),
	}
	p := NewPackStore(blocking, blob.NewMemManifestStore())
	done := make(chan error, 1)
	go func() {
		_, err := p.GetMany(ctx, hashes)
		done <- err
	}()

	waitForConcurrentReads(t, blocking, done)
}

func TestPackStoreGetManyReadsConcurrently(t *testing.T) {
	ctx := context.Background()
	base := blob.NewMemBlobStore()
	seed := NewPackStore(base, blob.NewMemManifestStore())
	hashes := make([]prollyhash.Hash, 40)
	for i := range hashes {
		c := chunk(fmt.Sprintf("range-concurrency-%d", i))
		hashes[i] = c.Hash
		if err := seed.Put(ctx, []Chunk{c}); err != nil {
			t.Fatal(err)
		}
	}

	blocking := &blockingBlobStore{
		BlobStore:  base,
		blockRange: true,
		started:    make(chan struct{}, len(hashes)),
		release:    make(chan struct{}),
	}
	p := NewPackStore(blocking, blob.NewMemManifestStore())
	done := make(chan error, 1)
	go func() {
		_, err := p.GetMany(ctx, hashes)
		done <- err
	}()

	waitForConcurrentReads(t, blocking, done)
}

func TestPackStoreGetManyHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := NewPackStore(blob.NewMemBlobStore(), blob.NewMemManifestStore())
	if _, err := p.GetMany(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetMany with canceled context = %v, want context.Canceled", err)
	}
}

func TestPackStoreBuildIndexPreservesListOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	base := blob.NewMemBlobStore()
	h := prollyhash.Compute([]byte("duplicate"))
	for _, key := range []string{"a", "b"} {
		data, err := json.Marshal([]idxEntry{{H: h.String(), O: 0, L: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if err := base.Put(ctx, idxKeyPrefix+key, data); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPackStore(&reorderedIndexBlobStore{BlobStore: base, secondDone: make(chan struct{})}, blob.NewMemManifestStore())
	idx, err := p.buildIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx[h].packKey; got != packKeyPrefix+"a" {
		t.Fatalf("duplicate hash resolved to %q, want %q", got, packKeyPrefix+"a")
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
