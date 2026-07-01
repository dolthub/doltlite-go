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
	// Empty is valid (zero hashes).
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
	// Zero-length chunk records are emitted and decoded, so counts match.
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
	// Drop the last byte: the declared length now runs past the body.
	if _, err := DecodeChunks(body[:len(body)-1]); err == nil {
		t.Fatal("expected error for truncated chunk body")
	}
}

func TestRefsIfRoundTrip(t *testing.T) {
	expected := prollyhash.Compute([]byte("prev refs"))
	blob := []byte("new refs blob")
	gotExpected, gotBlob, err := DecodeRefsIf(EncodeRefsIf(expected, blob))
	if err != nil {
		t.Fatal(err)
	}
	if gotExpected != expected || !bytes.Equal(gotBlob, blob) {
		t.Fatal("refs-if round trip mismatch")
	}
	// A body of exactly the hash size (empty blob) is rejected.
	if _, _, err := DecodeRefsIf(make([]byte, prollyhash.Size)); err == nil {
		t.Fatal("expected error for empty blob")
	}
}
