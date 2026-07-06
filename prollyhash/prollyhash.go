package prollyhash

import (
	"encoding/hex"
	"fmt"

	"github.com/zeebo/blake3"
)

const Size = 20

type Hash [Size]byte

func Compute(data []byte) Hash {
	sum := blake3.Sum256(data)
	var h Hash
	copy(h[:], sum[:Size])
	return h
}

func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

func (h Hash) IsEmpty() bool {
	return h == Hash{}
}

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
