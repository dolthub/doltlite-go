package remoteproto

import (
	"encoding/binary"
	"fmt"

	"github.com/dolthub/doltlite-go/litestore"
	"github.com/dolthub/doltlite-go/prollyhash"
)

const (
	EndpointRoot      = "root"
	EndpointHasChunks = "has-chunks"
	EndpointChunk     = "chunk"
	EndpointChunks    = "chunks"
	EndpointRefs      = "refs"
	EndpointRefsIf    = "refs-if"
	EndpointCommit    = "commit"
)

const (
	MaxChunkBytes = 64 * 1024 * 1024

	MaxRequestBytes = 128 * 1024 * 1024
)

const chunkLenSize = 4

func EncodeHashes(hashes []prollyhash.Hash) []byte {
	out := make([]byte, 0, len(hashes)*prollyhash.Size)
	for _, h := range hashes {
		out = append(out, h[:]...)
	}
	return out
}

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

func EncodePresence(present []bool) []byte {
	out := make([]byte, len(present))
	for i, p := range present {
		if p {
			out[i] = 1
		}
	}
	return out
}

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

func EncodeRefsIf(expected prollyhash.Hash, blob []byte) []byte {
	out := make([]byte, 0, prollyhash.Size+len(blob))
	out = append(out, expected[:]...)
	out = append(out, blob...)
	return out
}

func DecodeRefsIf(body []byte) (prollyhash.Hash, []byte, error) {
	var expected prollyhash.Hash
	if len(body) <= prollyhash.Size {
		return expected, nil, fmt.Errorf("remoteproto: refs-if body length %d too short", len(body))
	}
	copy(expected[:], body[:prollyhash.Size])
	blob := append([]byte(nil), body[prollyhash.Size:]...)
	return expected, blob, nil
}
