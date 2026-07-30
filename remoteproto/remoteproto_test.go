package remoteproto

import (
	"bytes"
	"testing"

	"github.com/dolthub/doltlite-go/litestore"
	"github.com/dolthub/doltlite-go/prollyhash"
)

func TestHashesRoundTrip(t *testing.T) {
	in := []prollyhash.Hash{
		prollyhash.Compute([]byte("a")),
		prollyhash.Compute([]byte("b")),
		prollyhash.Compute([]byte("c")),
	}
	out, err := DecodeHashes(EncodeHashes(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d hashes, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("hash %d mismatch", i)
		}
	}
}

func TestDecodeHashesBadLength(t *testing.T) {
	if _, err := DecodeHashes(make([]byte, prollyhash.Size+1)); err == nil {
		t.Fatal("expected error for non-multiple length")
	}

	out, err := DecodeHashes(nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("empty body: got %v, %v", out, err)
	}
}

func TestPresenceRoundTrip(t *testing.T) {
	in := []bool{true, false, true, true, false}
	out, err := DecodePresence(EncodePresence(in), len(in))
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("presence %d mismatch", i)
		}
	}
	if _, err := DecodePresence(EncodePresence(in), len(in)+1); err == nil {
		t.Fatal("expected length mismatch error")
	}
}

func TestChunksRoundTrip(t *testing.T) {
	mk := func(s string) litestore.Chunk {
		d := []byte(s)
		return litestore.Chunk{Hash: prollyhash.Compute(d), Data: d}
	}
	in := []litestore.Chunk{mk("hello"), mk(""), mk("a longer chunk of data")}
	out, err := DecodeChunks(EncodeChunks(in))
	if err != nil {
		t.Fatal(err)
	}

	if len(out) != len(in) {
		t.Fatalf("got %d chunks, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Hash != in[i].Hash || !bytes.Equal(out[i].Data, in[i].Data) {
			t.Fatalf("chunk %d mismatch", i)
		}
	}
}

func TestDecodeChunksTruncated(t *testing.T) {
	d := []byte("payload")
	body := EncodeChunks([]litestore.Chunk{{Hash: prollyhash.Compute(d), Data: d}})

	if _, err := DecodeChunks(body[:len(body)-1]); err == nil {
		t.Fatal("expected error for truncated chunk body")
	}
}

func TestRefsIfRoundTrip(t *testing.T) {
	expected := prollyhash.Compute([]byte("prev refs"))
	blob := []byte("new refs blob")
	gotBranch, gotForce, gotExpected, gotBlob, err := DecodeRefsIf(EncodeRefsIf("main", false, expected, blob))
	if err != nil {
		t.Fatal(err)
	}
	if gotBranch != "main" || gotForce || gotExpected != expected || !bytes.Equal(gotBlob, blob) {
		t.Fatalf("refs-if round trip mismatch: branch=%q force=%v", gotBranch, gotForce)
	}

	// A forced refs-if with an empty branch and empty blob still round-trips.
	gotBranch, gotForce, _, gotBlob, err = DecodeRefsIf(EncodeRefsIf("", true, prollyhash.Hash{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if gotBranch != "" || !gotForce || len(gotBlob) != 0 {
		t.Fatalf("forced refs-if round trip mismatch: branch=%q force=%v blob=%q", gotBranch, gotForce, gotBlob)
	}

	// Too short to hold the expected hash after the prefix.
	if _, _, _, _, err := DecodeRefsIf(EncodeRefs("main", false, nil)); err == nil {
		t.Fatal("expected error for refs-if body missing expected hash")
	}
}

func TestRefsRoundTrip(t *testing.T) {
	blob := []byte("refs blob")
	gotBranch, gotForce, gotBlob, err := DecodeRefs(EncodeRefs("feature/x", true, blob))
	if err != nil {
		t.Fatal(err)
	}
	if gotBranch != "feature/x" || !gotForce || !bytes.Equal(gotBlob, blob) {
		t.Fatalf("refs round trip mismatch: branch=%q force=%v blob=%q", gotBranch, gotForce, gotBlob)
	}

	if _, _, _, err := DecodeRefs([]byte{0x00}); err == nil {
		t.Fatal("expected error for truncated refs prefix")
	}
}
