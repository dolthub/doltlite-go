// Package prollyhash computes doltlite chunk addresses.
//
// A doltlite chunk address ("prolly hash") is the BLAKE3 hash of the chunk
// bytes, truncated to its first 20 bytes. This matches doltlite's
// prollyHashCompute (github.com/dolthub/doltlite, src/prolly_hash.c), which
// initializes a BLAKE3 hasher, updates it with the chunk bytes, and finalizes
// into a 20-byte output.
//
// BLAKE3's finalize produces the first N bytes of an extendable output stream,
// so the first 20 bytes of the standard 32-byte BLAKE3 digest are byte-for-byte
// identical to a 20-byte finalize. We therefore compute the 32-byte digest and
// slice the first 20 bytes; the golden vectors in the tests (taken from
// doltlite's own blake3_kat_test.sh) pin this equivalence.
//
// Note: the xxhash used elsewhere in doltlite is the rolling hash for
// content-defined chunking, not the chunk address; it is intentionally not
// implemented here.
package prollyhash

import (
	"encoding/hex"
	"fmt"

	"github.com/zeebo/blake3"
)

// Size is the length in bytes of a prolly hash (doltlite PROLLY_HASH_SIZE).
const Size = 20

// Hash is a 20-byte doltlite chunk address.
type Hash [Size]byte

// Compute returns the doltlite chunk address of data.
func Compute(data []byte) Hash {
	sum := blake3.Sum256(data)
	var h Hash
	copy(h[:], sum[:Size])
	return h
}

// String returns the lowercase hex encoding of the hash, matching doltlite's
// hex representation (e.g. from dolt_hashof_bytes).
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// IsEmpty reports whether the hash is all zero bytes. doltlite uses the
// all-zero hash as its sentinel for "no hash" (prollyHashIsEmpty), notably as
// the expected-refs hash when a repository has no refs yet.
func (h Hash) IsEmpty() bool {
	return h == Hash{}
}

// Parse decodes a hex-encoded prolly hash. The input must be exactly Size*2 hex
// characters.
func Parse(s string) (Hash, error) {
	var h Hash
	if len(s) != Size*2 {
		return h, fmt.Errorf("prollyhash: invalid hex length %d, want %d", len(s), Size*2)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("prollyhash: %w", err)
	}
	copy(h[:], b)
	return h, nil
}
