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
	EndpointGetChunks = "get-chunks"
)

// absentChunkLen marks an absent chunk in a get-chunks response (a length no
// real chunk can reach, since chunks are bounded by MaxChunkBytes).
const absentChunkLen = 0xFFFFFFFF

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

// EncodeGetChunks builds a /get-chunks response body. For each requested hash
// (in order) it writes a 4-byte big-endian length then that many payload bytes;
// a nil entry (chunk absent) is written as the length 0xFFFFFFFF with no
// payload. The request body is the concatenated hashes (see EncodeHashes).
func EncodeGetChunks(chunks [][]byte) []byte {
	size := 0
	for _, c := range chunks {
		size += chunkLenSize + len(c)
	}
	out := make([]byte, 0, size)
	var hdr [chunkLenSize]byte
	for _, c := range chunks {
		if c == nil {
			binary.BigEndian.PutUint32(hdr[:], absentChunkLen)
			out = append(out, hdr[:]...)
			continue
		}
		binary.BigEndian.PutUint32(hdr[:], uint32(len(c)))
		out = append(out, hdr[:]...)
		out = append(out, c...)
	}
	return out
}

// DecodeGetChunks parses a /get-chunks response body into n entries (n = the
// number of requested hashes). An absent chunk (length 0xFFFFFFFF) decodes to a
// nil entry; present chunks decode to their payload bytes.
func DecodeGetChunks(body []byte, n int) ([][]byte, error) {
	out := make([][]byte, n)
	off := 0
	for i := 0; i < n; i++ {
		if off+chunkLenSize > len(body) {
			return nil, fmt.Errorf("remoteproto: get-chunks response truncated at entry %d", i)
		}
		l := binary.BigEndian.Uint32(body[off : off+chunkLenSize])
		off += chunkLenSize
		if l == absentChunkLen {
			continue
		}
		if int64(off)+int64(l) > int64(len(body)) {
			return nil, fmt.Errorf("remoteproto: get-chunks entry %d length %d runs past end of body", i, l)
		}
		// Use a non-nil base so a present-but-empty chunk stays non-nil and is
		// not confused with an absent (nil) entry.
		out[i] = append([]byte{}, body[off:off+int(l)]...)
		off += int(l)
	}
	return out, nil
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

// The ref-update endpoints carry a branch-scope prefix so the server knows
// which branch the push targets and whether it is forced. The layout matches
// the doltlite C client (doltlite_http_remote.c):
//
//	PUT /refs     : [u16 branchLen LE][branch][u8 force][blob]
//	PUT /refs-if  : [u16 branchLen LE][branch][u8 force][20B expected][blob]
//
// force==1 requests an unconditional update (skip the compare-and-swap).

// EncodeRefs builds a PUT /refs body.
func EncodeRefs(branch string, force bool, blob []byte) []byte {
	out := make([]byte, 0, refsPrefixLen(branch)+len(blob))
	out = appendRefsPrefix(out, branch, force)
	out = append(out, blob...)
	return out
}

// DecodeRefs parses a PUT /refs body.
func DecodeRefs(body []byte) (branch string, force bool, blob []byte, err error) {
	branch, force, rest, err := decodeRefsPrefix(body)
	if err != nil {
		return "", false, nil, err
	}
	return branch, force, append([]byte(nil), rest...), nil
}

// EncodeRefsIf builds a PUT /refs-if body.
func EncodeRefsIf(branch string, force bool, expected prollyhash.Hash, blob []byte) []byte {
	out := make([]byte, 0, refsPrefixLen(branch)+prollyhash.Size+len(blob))
	out = appendRefsPrefix(out, branch, force)
	out = append(out, expected[:]...)
	out = append(out, blob...)
	return out
}

// DecodeRefsIf parses a PUT /refs-if body.
func DecodeRefsIf(body []byte) (branch string, force bool, expected prollyhash.Hash, blob []byte, err error) {
	branch, force, rest, err := decodeRefsPrefix(body)
	if err != nil {
		return "", false, expected, nil, err
	}
	if len(rest) < prollyhash.Size {
		return "", false, expected, nil, fmt.Errorf("remoteproto: refs-if body missing %d-byte expected hash", prollyhash.Size)
	}
	copy(expected[:], rest[:prollyhash.Size])
	blob = append([]byte(nil), rest[prollyhash.Size:]...)
	return branch, force, expected, blob, nil
}

func refsPrefixLen(branch string) int { return 2 + len(branch) + 1 }

func appendRefsPrefix(out []byte, branch string, force bool) []byte {
	n := len(branch)
	out = append(out, byte(n), byte(n>>8)) // u16 little-endian branch length
	out = append(out, branch...)
	var f byte
	if force {
		f = 1
	}
	return append(out, f)
}

// decodeRefsPrefix reads [u16 branchLen LE][branch][u8 force] and returns the
// bytes remaining after the prefix.
func decodeRefsPrefix(body []byte) (branch string, force bool, rest []byte, err error) {
	if len(body) < 3 { // 2-byte length + 1-byte force
		return "", false, nil, fmt.Errorf("remoteproto: refs body length %d too short", len(body))
	}
	n := int(body[0]) | int(body[1])<<8
	off := 2
	if off+n+1 > len(body) {
		return "", false, nil, fmt.Errorf("remoteproto: refs branch length %d runs past end of body", n)
	}
	branch = string(body[off : off+n])
	off += n
	force = body[off] != 0
	off++
	return branch, force, body[off:], nil
}
