// Package remoteproto defines the wire format of doltlite's HTTP sync protocol:
// endpoint names, size limits, and the encoders/decoders for each request and
// response body. It is shared by the client (package remote) and the server
// (package litehttp) so both sides agree on the framing.
//
// The framing mirrors doltlite's C implementation
// (github.com/dolthub/doltlite, src/doltlite_remotesrv.c and
// src/doltlite_http_remote.c):
//
//   - has-chunks request:  N concatenated 20-byte hashes.
//   - has-chunks response: N bytes, one per requested hash, non-zero == present.
//   - chunks request:      repeated records of [20-byte hash][4-byte LE length][bytes].
//   - refs-if request:     [20-byte expected refs hash][refs blob].
package remoteproto

import (
	"encoding/binary"
	"fmt"

	"github.com/dolthub/doltlite-go/litestore"
	"github.com/dolthub/doltlite-go/prollyhash"
)

// Endpoint path segments, relative to a repository's base path.
const (
	EndpointRoot      = "root"
	EndpointHasChunks = "has-chunks"
	EndpointChunk     = "chunk" // followed by /{hex}
	EndpointChunks    = "chunks"
	EndpointRefs      = "refs"
	EndpointRefsIf    = "refs-if"
	EndpointCommit    = "commit"
)

// Size limits, matching doltlite's server (doltlite_remotesrv.c).
const (
	// MaxChunkBytes is the largest single chunk accepted in a chunks request.
	MaxChunkBytes = 64 * 1024 * 1024
	// MaxRequestBytes is the largest total request body accepted.
	MaxRequestBytes = 128 * 1024 * 1024
)

// chunkLenSize is the width of the little-endian length prefix in the chunks
// framing.
const chunkLenSize = 4

// EncodeHashes concatenates hashes into a has-chunks request body.
func EncodeHashes(hashes []prollyhash.Hash) []byte {
	out := make([]byte, 0, len(hashes)*prollyhash.Size)
	for _, h := range hashes {
		out = append(out, h[:]...)
	}
	return out
}

// DecodeHashes parses a has-chunks request body into hashes. The body length
// must be a multiple of the hash size.
func DecodeHashes(body []byte) ([]prollyhash.Hash, error) {
	if len(body)%prollyhash.Size != 0 {
		return nil, fmt.Errorf("remoteproto: has-chunks body length %d not a multiple of %d", len(body), prollyhash.Size)
	}
	n := len(body) / prollyhash.Size
	out := make([]prollyhash.Hash, n)
	for i := 0; i < n; i++ {
		copy(out[i][:], body[i*prollyhash.Size:(i+1)*prollyhash.Size])
	}
	return out, nil
}

// EncodePresence encodes a has-chunks response: one byte per hash, 1 == present.
func EncodePresence(present []bool) []byte {
	out := make([]byte, len(present))
	for i, p := range present {
		if p {
			out[i] = 1
		}
	}
	return out
}

// DecodePresence decodes a has-chunks response of the given expected length.
func DecodePresence(body []byte, expect int) ([]bool, error) {
	if len(body) != expect {
		return nil, fmt.Errorf("remoteproto: has-chunks response length %d, want %d", len(body), expect)
	}
	out := make([]bool, expect)
	for i := range body {
		out[i] = body[i] != 0
	}
	return out, nil
}

// EncodeChunks encodes chunks into a chunks request body. Each record is
// [20-byte hash][4-byte LE length][bytes]. The hash is included for framing;
// the server recomputes and verifies it (see package litehttp).
func EncodeChunks(chunks []litestore.Chunk) []byte {
	size := 0
	for _, c := range chunks {
		size += prollyhash.Size + chunkLenSize + len(c.Data)
	}
	out := make([]byte, 0, size)
	var lenBuf [chunkLenSize]byte
	for _, c := range chunks {
		out = append(out, c.Hash[:]...)
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(c.Data)))
		out = append(out, lenBuf[:]...)
		out = append(out, c.Data...)
	}
	return out
}

// DecodeChunks parses a chunks request body into chunks. Each Chunk.Hash is the
// hash claimed on the wire; callers must verify it against the data. Chunks
// larger than MaxChunkBytes, or a record that runs past the end of the body,
// are rejected.
//
// This matches the C server's loop, which stops once a full record no longer
// fits (a trailing partial record is ignored) — but unlike the C server, which
// silently recomputes and ignores the wire hash, we surface the claimed hash so
// the caller can verify integrity.
func DecodeChunks(body []byte) ([]litestore.Chunk, error) {
	var chunks []litestore.Chunk
	off := 0
	for off+prollyhash.Size+chunkLenSize <= len(body) {
		var h prollyhash.Hash
		copy(h[:], body[off:off+prollyhash.Size])
		off += prollyhash.Size

		n := binary.LittleEndian.Uint32(body[off : off+chunkLenSize])
		off += chunkLenSize

		if n > MaxChunkBytes {
			return nil, fmt.Errorf("remoteproto: chunk length %d exceeds max %d", n, MaxChunkBytes)
		}
		if int64(n) > int64(len(body)-off) {
			return nil, fmt.Errorf("remoteproto: chunk length %d runs past end of body", n)
		}
		data := append([]byte(nil), body[off:off+int(n)]...)
		off += int(n)
		chunks = append(chunks, litestore.Chunk{Hash: h, Data: data})
	}
	return chunks, nil
}

// EncodeRefsIf encodes a refs-if request body: [expected hash][blob].
func EncodeRefsIf(expected prollyhash.Hash, blob []byte) []byte {
	out := make([]byte, 0, prollyhash.Size+len(blob))
	out = append(out, expected[:]...)
	out = append(out, blob...)
	return out
}

// DecodeRefsIf parses a refs-if request body into the expected hash and the
// refs blob. The body must be longer than a hash (the C server rejects a body
// of exactly the hash size, which would carry an empty blob).
func DecodeRefsIf(body []byte) (prollyhash.Hash, []byte, error) {
	var expected prollyhash.Hash
	if len(body) <= prollyhash.Size {
		return expected, nil, fmt.Errorf("remoteproto: refs-if body length %d too short", len(body))
	}
	copy(expected[:], body[:prollyhash.Size])
	blob := append([]byte(nil), body[prollyhash.Size:]...)
	return expected, blob, nil
}
