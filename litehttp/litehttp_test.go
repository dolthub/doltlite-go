package litehttp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dolthub/doltlite-go/litehttp"
	"github.com/dolthub/doltlite-go/litestore"
	"github.com/dolthub/doltlite-go/prollyhash"
	"github.com/dolthub/doltlite-go/remote"
)

// testProvider is a minimal StoreProvider: one MemStore per owner/repo, an
// optional required bearer token, and an optional set of "missing" repos used
// to exercise the not-found paths.
type testProvider struct {
	mu       sync.Mutex
	stores   map[string]*litestore.MemStore
	token    string          // if non-empty, required as "Bearer <token>"
	missing  map[string]bool // repos that report ErrRepoNotFound
	readOnly map[string]bool // repos that forbid writes
}

func newTestProvider() *testProvider {
	return &testProvider{
		stores:   map[string]*litestore.MemStore{},
		missing:  map[string]bool{},
		readOnly: map[string]bool{},
	}
}

func (p *testProvider) Store(r *http.Request, owner, repo string, write bool) (litestore.Store, error) {
	if p.token != "" && r.Header.Get("Authorization") != "Bearer "+p.token {
		return nil, litehttp.ErrUnauthorized
	}
	key := owner + "/" + repo
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.missing[key] {
		return nil, litehttp.ErrRepoNotFound
	}
	if write && p.readOnly[key] {
		return nil, litehttp.ErrForbidden
	}
	s, ok := p.stores[key]
	if !ok {
		s = litestore.NewMemStore()
		p.stores[key] = s
	}
	return s, nil
}

func chunk(s string) litestore.Chunk {
	d := []byte(s)
	return litestore.Chunk{Hash: prollyhash.Compute(d), Data: d}
}

// TestPushPullRoundTrip walks a full push then a full pull through the real HTTP
// stack: remote client -> httptest server -> litehttp handler -> MemStore.
func TestPushPullRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(litehttp.NewHandler(newTestProvider()))
	defer srv.Close()

	cl := remote.New(srv.URL + "/acme/widgets")

	c1, c2 := chunk("first chunk"), chunk("second chunk")

	// Before anything is pushed, neither chunk is present.
	present, err := cl.HasChunks(ctx, []prollyhash.Hash{c1.Hash, c2.Hash})
	if err != nil {
		t.Fatal(err)
	}
	if present[0] || present[1] {
		t.Fatalf("expected both absent, got %v", present)
	}

	// Push chunks, then set refs (first push: expected = zero hash), then commit.
	if err := cl.PutChunks(ctx, []litestore.Chunk{c1, c2}); err != nil {
		t.Fatal(err)
	}
	refs := []byte("refs blob v1")
	if err := cl.SetRefsIf(ctx, prollyhash.Hash{}, refs); err != nil {
		t.Fatal(err)
	}
	if err := cl.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Pull side: chunks now present, fetchable, and refs readable.
	present, err = cl.HasChunks(ctx, []prollyhash.Hash{c1.Hash, c2.Hash})
	if err != nil {
		t.Fatal(err)
	}
	if !present[0] || !present[1] {
		t.Fatalf("expected both present, got %v", present)
	}
	got, err := cl.GetChunk(ctx, c1.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first chunk" {
		t.Fatalf("GetChunk = %q", got)
	}
	gotRefs, err := cl.GetRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotRefs) != string(refs) {
		t.Fatalf("GetRefs = %q, want %q", gotRefs, refs)
	}
}

func TestGetMissingChunkAndRefs(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(litehttp.NewHandler(newTestProvider()))
	defer srv.Close()
	cl := remote.New(srv.URL + "/acme/widgets")

	if _, err := cl.GetChunk(ctx, prollyhash.Compute([]byte("nope"))); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("GetChunk(absent) = %v, want ErrNotFound", err)
	}
	if _, err := cl.GetRefs(ctx); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("GetRefs(none) = %v, want ErrNotFound", err)
	}
}

// TestRefsIfConflict simulates two clients racing on refs: the second, holding a
// stale expected hash, must get a 409 -> ErrConflict.
func TestRefsIfConflict(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(litehttp.NewHandler(newTestProvider()))
	defer srv.Close()
	cl := remote.New(srv.URL + "/acme/widgets")

	if err := cl.SetRefsIf(ctx, prollyhash.Hash{}, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	// Another writer already advanced refs; this call still thinks refs are empty.
	if err := cl.SetRefsIf(ctx, prollyhash.Hash{}, []byte("v2")); !errors.Is(err, remote.ErrConflict) {
		t.Fatalf("stale SetRefsIf = %v, want ErrConflict", err)
	}
}

// TestHasChunksOnMissingRepo verifies the doltlite behavior that has-chunks
// against a not-yet-created repository returns all-absent rather than 404.
func TestHasChunksOnMissingRepo(t *testing.T) {
	ctx := context.Background()
	p := newTestProvider()
	p.missing["acme/ghost"] = true
	srv := httptest.NewServer(litehttp.NewHandler(p))
	defer srv.Close()
	cl := remote.New(srv.URL + "/acme/ghost")

	present, err := cl.HasChunks(ctx, []prollyhash.Hash{prollyhash.Compute([]byte("x"))})
	if err != nil {
		t.Fatalf("HasChunks on missing repo: %v", err)
	}
	if len(present) != 1 || present[0] {
		t.Fatalf("expected [false], got %v", present)
	}

	// A read of an actual chunk on a missing repo is a 404, not all-absent.
	if _, err := cl.GetChunk(ctx, prollyhash.Compute([]byte("x"))); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("GetChunk on missing repo = %v, want ErrNotFound", err)
	}
}

func TestAuth(t *testing.T) {
	ctx := context.Background()
	p := newTestProvider()
	p.token = "s3cret"
	srv := httptest.NewServer(litehttp.NewHandler(p))
	defer srv.Close()

	// No token: unauthorized.
	anon := remote.New(srv.URL + "/acme/widgets")
	if err := anon.PutChunks(ctx, []litestore.Chunk{chunk("x")}); err == nil {
		t.Fatal("expected unauthorized error without token")
	}

	// With token: allowed.
	authed := remote.New(srv.URL+"/acme/widgets", remote.WithBearerToken("s3cret"))
	if err := authed.PutChunks(ctx, []litestore.Chunk{chunk("x")}); err != nil {
		t.Fatalf("authed PutChunks: %v", err)
	}
}

func TestWriteForbiddenReadAllowed(t *testing.T) {
	ctx := context.Background()
	p := newTestProvider()
	p.readOnly["acme/widgets"] = true
	srv := httptest.NewServer(litehttp.NewHandler(p))
	defer srv.Close()
	cl := remote.New(srv.URL + "/acme/widgets")

	// Read is fine (empty repo -> all absent).
	if _, err := cl.HasChunks(ctx, []prollyhash.Hash{prollyhash.Compute([]byte("x"))}); err != nil {
		t.Fatalf("read on read-only repo: %v", err)
	}
	// Write is forbidden.
	if err := cl.PutChunks(ctx, []litestore.Chunk{chunk("x")}); err == nil {
		t.Fatal("expected forbidden error on write to read-only repo")
	}
}

// TestChunkHashVerification confirms the handler rejects a chunk whose claimed
// hash does not match its bytes (stricter than upstream doltlite).
func TestChunkHashVerification(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(litehttp.NewHandler(newTestProvider()))
	defer srv.Close()
	cl := remote.New(srv.URL + "/acme/widgets")

	bad := litestore.Chunk{
		Hash: prollyhash.Compute([]byte("the real bytes")),
		Data: []byte("different bytes"),
	}
	if err := cl.PutChunks(ctx, []litestore.Chunk{bad}); err == nil {
		t.Fatal("expected error for mismatched chunk hash")
	}
}
