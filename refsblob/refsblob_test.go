package refsblob

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dolthub/doltlite-go/prollyhash"
)

// The testdata blobs were served by doltlite-remotesrv (GET /{db}/refs) for
// databases built with the doltlite CLI:
//
//	basic.bin:    CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT);
//	              insert 2 rows; dolt_commit; dolt_branch('feature');
//	              dolt_tag('v1','-m','first release');
//	              dolt_remote('add','origin','http://127.0.0.1:1/x.db');
//	              insert 1 row; dolt_commit.
//	explicit.bin: one commit; dolt_branch('dev'); dolt_default_branch('dev').
//	clone.bin:    dolt_clone of basic.db (remote + tracking branches).

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func h(t *testing.T, s string) prollyhash.Hash {
	t.Helper()
	if s == "" {
		return prollyhash.Hash{}
	}
	v, err := prollyhash.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDecodeFixtures(t *testing.T) {
	const (
		second = "fcb09b514793640553cb168b5f8dfaa600e2371e"
		first  = "5710b5d42739ee9d5c7ff2a0f176dd0ec1bb2c07"
	)
	v1 := Tag{Name: "v1", Commit: h(t, first), Tagger: "doltlite", Timestamp: 0x6add5fb6, Message: "first release"}
	seqs := []Sequence{{Table: "t", Value: 3}}
	cases := []struct {
		file string
		want *Refs
	}{
		{"basic.bin", &Refs{
			DefaultBranch: "main",
			Branches: []Branch{
				{Name: "main", Commit: h(t, second), WorkingSet: h(t, "185be366437c14d7cf354e702f2c17983726c9f0")},
				{Name: "feature", Commit: h(t, first)},
			},
			Tags:      []Tag{v1},
			Remotes:   []Remote{{Name: "origin", URL: "http://127.0.0.1:1/x.db"}},
			Tracking:  []Tracking{},
			Sequences: seqs,
		}},
		{"clone.bin", &Refs{
			DefaultBranch: "main",
			Branches: []Branch{
				{Name: "main", Commit: h(t, second), WorkingSet: h(t, "8cad43223c03d2cb931537b9aeabb59a884874d4")},
			},
			Tags:    []Tag{v1},
			Remotes: []Remote{{Name: "origin", URL: "http://127.0.0.1:18731/basic.db"}},
			Tracking: []Tracking{
				{Remote: "origin", Branch: "main", Commit: h(t, second)},
				{Remote: "origin", Branch: "feature", Commit: h(t, first)},
			},
			Sequences: seqs,
		}},
		{"explicit.bin", &Refs{
			DefaultBranch:   "dev",
			DefaultExplicit: true,
			Branches: []Branch{
				{Name: "main", Commit: h(t, "bbc1cadf221ccd9539bb9139fd11a99824e04260"), WorkingSet: h(t, "b76eb4d866a52fa00af9e22720e44b0f05b337df")},
				{Name: "dev", Commit: h(t, "bbc1cadf221ccd9539bb9139fd11a99824e04260")},
			},
			Tags:      []Tag{},
			Remotes:   []Remote{},
			Tracking:  []Tracking{},
			Sequences: []Sequence{},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			got, err := Decode(load(t, tc.file))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Decode mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	r, err := Decode(load(t, "basic.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := r.Branch("feature"); !ok || b.Commit.String() != "5710b5d42739ee9d5c7ff2a0f176dd0ec1bb2c07" {
		t.Fatalf("Branch(feature) = %+v, %v", b, ok)
	}
	if _, ok := r.Branch("nope"); ok {
		t.Fatal("Branch(nope) found")
	}
	if tg, ok := r.Tag("v1"); !ok || tg.Message != "first release" {
		t.Fatalf("Tag(v1) = %+v, %v", tg, ok)
	}
}

func TestDecodeEmptyBranchList(t *testing.T) {
	// No branches: the default branch need not exist.
	b := []byte{7}
	b = appendStr(b, "main")
	for i := 0; i < 5; i++ {
		b = binary.LittleEndian.AppendUint32(b, 0)
	}
	r, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.DefaultBranch != "main" || r.DefaultExplicit || len(r.Branches) != 0 {
		t.Fatalf("unexpected %+v", r)
	}
}

func appendStr(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

func TestDecodeCorrupt(t *testing.T) {
	good := load(t, "basic.bin")

	mustCorrupt := func(t *testing.T, b []byte) {
		t.Helper()
		if _, err := Decode(b); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Decode err = %v, want ErrCorrupt", err)
		}
	}

	t.Run("nil", func(t *testing.T) { mustCorrupt(t, nil) })
	t.Run("version", func(t *testing.T) {
		b := append([]byte(nil), good...)
		b[0] = 6
		mustCorrupt(t, b)
	})
	t.Run("trailing", func(t *testing.T) {
		mustCorrupt(t, append(append([]byte(nil), good...), 0))
	})
	t.Run("truncated", func(t *testing.T) {
		for n := 0; n < len(good); n++ {
			if _, err := Decode(good[:n]); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("prefix %d: err = %v, want ErrCorrupt", n, err)
			}
		}
	})
	t.Run("missing default", func(t *testing.T) {
		b := append([]byte(nil), good...)
		copy(b[5:9], "nope") // default name "main" -> "nope"
		mustCorrupt(t, b)
	})
	t.Run("explicit pad mismatch", func(t *testing.T) {
		// explicit.bin pads both the default and the dev entry; drop the pad
		// from the default only and the raw names no longer match.
		ex := load(t, "explicit.bin")
		b := []byte{7}
		b = appendStr(b, "dev")
		b = append(b, ex[1+4+4:]...)
		mustCorrupt(t, b)
	})
	t.Run("huge count", func(t *testing.T) {
		b := []byte{7}
		b = appendStr(b, "main")
		b = binary.LittleEndian.AppendUint32(b, 1000)
		mustCorrupt(t, b)
	})
	t.Run("length over int32", func(t *testing.T) {
		b := []byte{7}
		b = binary.LittleEndian.AppendUint32(b, 0x80000000)
		mustCorrupt(t, b)
	})
}

func TestEncodeRoundTrip(t *testing.T) {
	for _, name := range []string{"basic.bin", "explicit.bin", "clone.bin"} {
		t.Run(name, func(t *testing.T) {
			blob := load(t, name)
			refs, err := Decode(blob)
			if err != nil {
				t.Fatal(err)
			}
			if got := Encode(refs); !bytes.Equal(got, blob) {
				t.Fatalf("Encode(Decode(%s)) differs from the original", name)
			}
		})
	}
}

// doltlite keeps an empty default branch name it decoded, so Encode writes it
// back rather than substituting main.
func TestEncodeEmptyDefault(t *testing.T) {
	blob := []byte{Version}
	blob = appendStr(blob, "")
	for i := 0; i < 5; i++ {
		blob = binary.LittleEndian.AppendUint32(blob, 0)
	}
	refs, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	if refs.DefaultBranch != "" {
		t.Fatalf("default = %q", refs.DefaultBranch)
	}
	if got := Encode(refs); !bytes.Equal(got, blob) {
		t.Fatal("Encode(Decode(blob)) changed an empty default branch")
	}
}

// Encode writes strings as doltlite's strlen-based serializer does: up to the
// first 0 byte. A hand-built value with an embedded 0 encodes to what doltlite
// would have written, and decodes to the same visible strings.
func TestEncodeCutsAtNUL(t *testing.T) {
	refs, err := Decode(load(t, "explicit.bin"))
	if err != nil {
		t.Fatal(err)
	}
	want := Encode(refs)

	withNUL := *refs
	withNUL.DefaultBranch += "\x00suffix"
	withNUL.Branches = append([]Branch(nil), refs.Branches...)
	for i := range withNUL.Branches {
		withNUL.Branches[i].Name += "\x00suffix"
	}
	if got := Encode(&withNUL); !bytes.Equal(got, want) {
		t.Fatal("embedded 0 bytes reached the encoded names")
	}

	withNUL.SetRemote("origin", "https://host/repo\x00suffix")
	again, err := Decode(Encode(&withNUL))
	if err != nil {
		t.Fatal(err)
	}
	if r := again.Remotes[len(again.Remotes)-1]; r.URL != "https://host/repo" {
		t.Fatalf("remote url = %q", r.URL)
	}
}

func TestSetRemote(t *testing.T) {
	refs, err := Decode(load(t, "clone.bin"))
	if err != nil {
		t.Fatal(err)
	}
	n := len(refs.Remotes)
	refs.SetRemote("dolthub", "http://127.0.0.1:9480/acme/widgets")
	refs.SetRemote("dolthub", "http://127.0.0.1:9480/acme/gadgets")
	again, err := Decode(Encode(refs))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Remotes) != n+1 {
		t.Fatalf("remotes = %v", again.Remotes)
	}
	if last := again.Remotes[n]; last.Name != "dolthub" || last.URL != "http://127.0.0.1:9480/acme/gadgets" {
		t.Fatalf("added remote = %+v", last)
	}
}
