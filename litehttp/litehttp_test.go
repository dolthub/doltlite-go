package litehttp_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dolthub/doltlite-go/litehttp"
	"github.com/dolthub/doltlite-go/litestore"
	"github.com/dolthub/doltlite-go/prollyhash"
	"github.com/dolthub/doltlite-go/remote"
)

// TestRefsIfBranchScopedFirstPush reproduces the doltlite C client's first push
// to a new repository at the raw wire level: a /refs-if body framed as
// [u16 branchLen LE]["main"][u8 force=0][20B zero expected][blob] against an
// empty store. The server must read the expected hash from the correct offset
// (after the branch/force prefix), see it matches the empty store's zero refs,
// and return 200 — not misread the prefix bytes as the expected hash and 409.
func TestRefsIfBranchScopedFirstPush(t *testing.T) {
	srv := httptest.NewServer(litehttp.NewHandler(newTestProvider()))
	defer srv.Close()

	branch := "main"
	blob := []byte("refs blob v1")
	body := &bytes.Buffer{}
	body.WriteByte(byte(len(branch)))      // branchLen low byte
	body.WriteByte(byte(len(branch) >> 8)) // branchLen high byte
	body.WriteString(branch)
	body.WriteByte(0)                         // force = false
	body.Write(make([]byte, prollyhash.Size)) // expected = zero (new repo)
	body.Write(blob)

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/acme/widgets/refs-if", bytes.NewReader(body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("first branch-scoped refs-if = %d (%s), want 200", resp.StatusCode, bytes.TrimSpace(msg))
	}

	// The stored refs must be exactly the blob, with the branch/force/expected
	// prefix stripped.
	got, err := remote.New(srv.URL + "/acme/widgets").GetRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("GetRefs = %q, want %q", got, blob)
	}
}

type testProvider struct {
	mu       sync.Mutex
	stores   map[string]*litestore.MemStore
	token    string
	missing  map[string]bool
	readOnly map[string]bool
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

func TestPushPullRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(litehttp.NewHandler(newTestProvider()))
	defer srv.Close()

	cl := remote.New(srv.URL + "/acme/widgets")

	c1, c2 := chunk("first chunk"), chunk("second chunk")

	present, err := cl.HasChunks(ctx, []prollyhash.Hash{c1.Hash, c2.Hash})
	if err != nil {
		t.Fatal(err)
	}
	if present[0] || present[1] {
		t.Fatalf("expected both absent, got %v", present)
	}

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

func TestGetChunksBatched(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(litehttp.NewHandler(newTestProvider()))
	defer srv.Close()
	cl := remote.New(srv.URL + "/acme/widgets")

	c1, c2 := chunk("first chunk"), chunk("second chunk")
	if err := cl.PutChunks(ctx, []litestore.Chunk{c1, c2}); err != nil {
		t.Fatal(err)
	}

	absent := prollyhash.Compute([]byte("nope"))
	got, err := cl.GetChunks(ctx, []prollyhash.Hash{c1.Hash, absent, c2.Hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("GetChunks returned %d entries, want 3", len(got))
	}
	if !bytes.Equal(got[0], []byte("first chunk")) {
		t.Fatalf("entry 0 = %q, want %q", got[0], "first chunk")
	}
	if got[1] != nil {
		t.Fatalf("entry 1 (absent) = %q, want nil", got[1])
	}
	if !bytes.Equal(got[2], []byte("second chunk")) {
		t.Fatalf("entry 2 = %q, want %q", got[2], "second chunk")
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

func TestRefsIfConflict(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(litehttp.NewHandler(newTestProvider()))
	defer srv.Close()
	cl := remote.New(srv.URL + "/acme/widgets")

	if err := cl.SetRefsIf(ctx, prollyhash.Hash{}, []byte("v1")); err != nil {
		t.Fatal(err)
	}

	if err := cl.SetRefsIf(ctx, prollyhash.Hash{}, []byte("v2")); !errors.Is(err, remote.ErrConflict) {
		t.Fatalf("stale SetRefsIf = %v, want ErrConflict", err)
	}
}

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

	anon := remote.New(srv.URL + "/acme/widgets")
	if err := anon.PutChunks(ctx, []litestore.Chunk{chunk("x")}); err == nil {
		t.Fatal("expected unauthorized error without token")
	}

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

	if _, err := cl.HasChunks(ctx, []prollyhash.Hash{prollyhash.Compute([]byte("x"))}); err != nil {
		t.Fatalf("read on read-only repo: %v", err)
	}

	if err := cl.PutChunks(ctx, []litestore.Chunk{chunk("x")}); err == nil {
		t.Fatal("expected forbidden error on write to read-only repo")
	}
}

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
