package prollyhash

import "testing"

// seq returns n bytes where byte i == i mod 256, matching the buffers used by
// doltlite's blake3_kat_test.sh.
func seq(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 256)
	}
	return b
}

// TestComputeGolden pins Compute against the known-answer vectors from
// doltlite's own test/blake3_kat_test.sh. If these pass, our Go BLAKE3-20 port
// is byte-identical to doltlite's prollyHashCompute.
func TestComputeGolden(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", []byte{}, "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9"},
		{"abc", []byte("abc"), "6437b3ac38465133ffb63b75273a8db548c55846"},
		{"1024", seq(1024), "882179b8dbccd285cda241d968cfcccb3156c5ed"},
		{"4096", seq(4096), "0b3dda6fbfe01c93d79388632f66c5c1fa781382"},
		{"16384", seq(16384), "d49d367e4b0011a34510a28a1eb0caeb3e51e77f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Compute(tc.data).String()
			if got != tc.want {
				t.Fatalf("Compute(%s) = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestEmptyHashIsNotSentinel guards the distinction between "hash of empty
// input" (a real BLAKE3 digest) and IsEmpty (the all-zero sentinel).
func TestEmptyHashIsNotSentinel(t *testing.T) {
	if Compute(nil).IsEmpty() {
		t.Fatal("hash of empty input must not be the zero sentinel")
	}
	var zero Hash
	if !zero.IsEmpty() {
		t.Fatal("zero hash must be empty")
	}
}

func TestParseRoundTrip(t *testing.T) {
	orig := Compute([]byte("round trip"))
	got, err := Parse(orig.String())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != orig {
		t.Fatalf("Parse round trip mismatch: %s != %s", got, orig)
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse("abc"); err == nil {
		t.Fatal("expected error for short hex")
	}
	// Correct length, invalid hex characters.
	bad := ""
	for i := 0; i < Size*2; i++ {
		bad += "z"
	}
	if _, err := Parse(bad); err == nil {
		t.Fatal("expected error for non-hex characters")
	}
}
