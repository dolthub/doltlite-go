// Package refsblob decodes and encodes doltlite's refs blob, the chunk a
// doltlite remote's root hash points at. Decode mirrors csDecodeRefsV7 in
// doltlite's src/chunk_refs.c byte for byte, including its corruption checks,
// so a blob this package accepts is exactly a blob doltlite accepts. Encode
// writes the same bytes doltlite's serializer does.
//
// Layout (all integers little-endian; strings are [u32 len][bytes]):
//
//	u8 version (7)
//	string default branch (a trailing 0 byte marks an explicit dolt_default_branch)
//	u32 n, n * { string name, 20B commit, 20B working set }      branches
//	u32 n, n * { string name, 20B commit, string tagger,
//	             string email, i64 timestamp, string message }   tags
//	u32 n, n * { string name, string url }                       remotes
//	u32 n, n * { string remote, string branch, 20B commit }      tracking branches
//	u32 n, n * { string table, i64 value }                       sequences
package refsblob

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/dolthub/doltlite-go/prollyhash"
)

// Version is the only refs blob format version doltlite reads.
const Version = 7

// ErrCorrupt is returned (wrapped) for any blob doltlite would reject.
var ErrCorrupt = errors.New("refsblob: corrupt refs blob")

type Branch struct {
	Name       string
	Commit     prollyhash.Hash
	WorkingSet prollyhash.Hash
}

type Tag struct {
	Name   string
	Commit prollyhash.Hash
	Tagger string
	Email  string
	// Timestamp is the tag time in seconds since the Unix epoch.
	Timestamp int64
	Message   string
}

type Remote struct {
	Name string
	URL  string
}

// Tracking is a remote-tracking branch (refs/remotes/<Remote>/<Branch>).
type Tracking struct {
	Remote string
	Branch string
	Commit prollyhash.Hash
}

// Sequence is a non-versioned AUTOINCREMENT high-water mark.
type Sequence struct {
	Table string
	Value int64
}

type Refs struct {
	DefaultBranch string
	// DefaultExplicit is set when dolt_default_branch chose DefaultBranch.
	DefaultExplicit bool
	Branches        []Branch
	Tags            []Tag
	Remotes         []Remote
	Tracking        []Tracking
	Sequences       []Sequence
}

// Branch returns the named branch.
func (r *Refs) Branch(name string) (Branch, bool) {
	for _, b := range r.Branches {
		if b.Name == name {
			return b, true
		}
	}
	return Branch{}, false
}

// Tag returns the named tag.
func (r *Refs) Tag(name string) (Tag, bool) {
	for _, t := range r.Tags {
		if t.Name == name {
			return t, true
		}
	}
	return Tag{}, false
}

// Decode parses a version 7 refs blob. It rejects anything doltlite's decoder
// rejects: a wrong version byte, truncated fields, counts that cannot fit in
// the remaining bytes, a non-empty branch list missing the default branch, and
// trailing bytes.
func Decode(data []byte) (*Refs, error) {
	r := &reader{p: data}
	v, err := r.take(1)
	if err != nil {
		return nil, err
	}
	if v[0] != Version {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, v[0])
	}
	out := &Refs{}

	defRaw, err := r.bytes()
	if err != nil {
		return nil, err
	}
	out.DefaultBranch = cString(defRaw)
	out.DefaultExplicit = len(defRaw) > 0 && defRaw[len(defRaw)-1] == 0

	n, err := r.count(4 + 2*prollyhash.Size)
	if err != nil {
		return nil, err
	}
	hasDefault := false
	out.Branches = make([]Branch, n)
	for i := range out.Branches {
		b := &out.Branches[i]
		raw, err := r.bytes()
		if err != nil {
			return nil, err
		}
		// Compared on the raw bytes, explicit-default pad included, as C does.
		if bytes.Equal(raw, defRaw) {
			hasDefault = true
		}
		b.Name = cString(raw)
		if b.Commit, err = r.hash(); err != nil {
			return nil, err
		}
		if b.WorkingSet, err = r.hash(); err != nil {
			return nil, err
		}
	}
	if n > 0 && !hasDefault {
		return nil, fmt.Errorf("%w: default branch %q not in branch list", ErrCorrupt, out.DefaultBranch)
	}

	if n, err = r.count(24 + prollyhash.Size); err != nil {
		return nil, err
	}
	out.Tags = make([]Tag, n)
	for i := range out.Tags {
		t := &out.Tags[i]
		if t.Name, err = r.str(); err != nil {
			return nil, err
		}
		if t.Commit, err = r.hash(); err != nil {
			return nil, err
		}
		if t.Tagger, err = r.str(); err != nil {
			return nil, err
		}
		if t.Email, err = r.str(); err != nil {
			return nil, err
		}
		if t.Timestamp, err = r.i64(); err != nil {
			return nil, err
		}
		if t.Message, err = r.str(); err != nil {
			return nil, err
		}
	}

	if n, err = r.count(8); err != nil {
		return nil, err
	}
	out.Remotes = make([]Remote, n)
	for i := range out.Remotes {
		rm := &out.Remotes[i]
		if rm.Name, err = r.str(); err != nil {
			return nil, err
		}
		if rm.URL, err = r.str(); err != nil {
			return nil, err
		}
	}

	if n, err = r.count(8 + prollyhash.Size); err != nil {
		return nil, err
	}
	out.Tracking = make([]Tracking, n)
	for i := range out.Tracking {
		t := &out.Tracking[i]
		if t.Remote, err = r.str(); err != nil {
			return nil, err
		}
		if t.Branch, err = r.str(); err != nil {
			return nil, err
		}
		if t.Commit, err = r.hash(); err != nil {
			return nil, err
		}
	}

	if n, err = r.count(12); err != nil {
		return nil, err
	}
	out.Sequences = make([]Sequence, n)
	for i := range out.Sequences {
		s := &out.Sequences[i]
		if s.Table, err = r.str(); err != nil {
			return nil, err
		}
		if s.Value, err = r.i64(); err != nil {
			return nil, err
		}
	}

	if len(r.p) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrCorrupt, len(r.p))
	}
	return out, nil
}

// cString returns b as doltlite's C code sees it: up to the first 0 byte.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

type reader struct {
	p []byte
}

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || n > len(r.p) {
		return nil, fmt.Errorf("%w: truncated", ErrCorrupt)
	}
	b := r.p[:n]
	r.p = r.p[n:]
	return b, nil
}

func (r *reader) u32() (int, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(b)
	if v > math.MaxInt32 {
		return 0, fmt.Errorf("%w: length %d out of range", ErrCorrupt, v)
	}
	return int(v), nil
}

// count reads an entry count, rejecting one that could not fit in the
// remaining bytes at minSize bytes per entry.
func (r *reader) count(minSize int) (int, error) {
	n, err := r.u32()
	if err != nil {
		return 0, err
	}
	if n > len(r.p)/minSize {
		return 0, fmt.Errorf("%w: count %d exceeds remaining bytes", ErrCorrupt, n)
	}
	return n, nil
}

func (r *reader) bytes() ([]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	return r.take(n)
}

func (r *reader) str() (string, error) {
	b, err := r.bytes()
	if err != nil {
		return "", err
	}
	return cString(b), nil
}

func (r *reader) hash() (prollyhash.Hash, error) {
	var h prollyhash.Hash
	b, err := r.take(prollyhash.Size)
	if err != nil {
		return h, err
	}
	copy(h[:], b)
	return h, nil
}

func (r *reader) i64() (int64, error) {
	b, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b)), nil
}

// Encode serializes refs exactly as doltlite's csSerializeRefsBlob does, so
// Encode(Decode(b)) == b for any blob doltlite wrote. An explicit default
// branch is recorded as a trailing 0 byte on the default name and on that
// branch's name.
func Encode(refs *Refs) []byte {
	def := refs.DefaultBranch
	if def == "" {
		def = "main"
	}
	w := &writer{}
	w.p = append(w.p, Version)
	w.padded(def, refs.DefaultExplicit)
	w.u32(len(refs.Branches))
	for _, b := range refs.Branches {
		w.padded(b.Name, refs.DefaultExplicit && b.Name == def)
		w.hash(b.Commit)
		w.hash(b.WorkingSet)
	}
	w.u32(len(refs.Tags))
	for _, t := range refs.Tags {
		w.str(t.Name)
		w.hash(t.Commit)
		w.str(t.Tagger)
		w.str(t.Email)
		w.i64(t.Timestamp)
		w.str(t.Message)
	}
	w.u32(len(refs.Remotes))
	for _, rm := range refs.Remotes {
		w.str(rm.Name)
		w.str(rm.URL)
	}
	w.u32(len(refs.Tracking))
	for _, t := range refs.Tracking {
		w.str(t.Remote)
		w.str(t.Branch)
		w.hash(t.Commit)
	}
	w.u32(len(refs.Sequences))
	for _, s := range refs.Sequences {
		w.str(s.Table)
		w.i64(s.Value)
	}
	return w.p
}

// SetRemote adds the named remote, or changes its URL if it exists. New
// remotes go last, as dolt_remote('add', ...) appends them.
func (r *Refs) SetRemote(name, url string) {
	for i := range r.Remotes {
		if r.Remotes[i].Name == name {
			r.Remotes[i].URL = url
			return
		}
	}
	r.Remotes = append(r.Remotes, Remote{Name: name, URL: url})
}

type writer struct {
	p []byte
}

func (w *writer) u32(n int) {
	w.p = binary.LittleEndian.AppendUint32(w.p, uint32(n))
}

func (w *writer) str(s string) {
	w.u32(len(s))
	w.p = append(w.p, s...)
}

func (w *writer) padded(s string, pad bool) {
	if !pad {
		w.str(s)
		return
	}
	w.u32(len(s) + 1)
	w.p = append(w.p, s...)
	w.p = append(w.p, 0)
}

func (w *writer) hash(h prollyhash.Hash) {
	w.p = append(w.p, h[:]...)
}

func (w *writer) i64(v int64) {
	w.p = binary.LittleEndian.AppendUint64(w.p, uint64(v))
}
