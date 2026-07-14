package blobs3

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/dolthub/doltlite-go/blob/blobtest"
)

// fakeS3 is an in-process stand-in for the S3 API. It honors the Range header
// and Prefix filtering, so the Store's request construction (range strings, key
// prefixing, error mapping) is what's actually under test.
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
	// truncateWithoutToken makes ListObjectsV2 always claim more results but
	// return no continuation token, to test the loop guard.
	truncateWithoutToken bool
}

func newFakeS3() *fakeS3 { return &fakeS3{objs: map[string][]byte{}} }

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[aws.ToString(in.Key)] = append([]byte(nil), data...)
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	data, ok := f.objs[aws.ToString(in.Key)]
	f.mu.Unlock()
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	if in.Range != nil {
		data = applyRange(data, aws.ToString(in.Range))
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data))}, nil
}

// ListObjectsV2 returns one key per page (driving the caller's continuation
// loop) in sorted order for determinism.
func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.truncateWithoutToken {
		return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(true)}, nil
	}

	prefix := aws.ToString(in.Prefix)
	var matched []string
	for k := range f.objs {
		if strings.HasPrefix(k, prefix) {
			matched = append(matched, k)
		}
	}
	sort.Strings(matched)

	start := 0
	if in.ContinuationToken != nil {
		start, _ = strconv.Atoi(aws.ToString(in.ContinuationToken))
	}

	out := &s3.ListObjectsV2Output{}
	if start < len(matched) {
		key := matched[start]
		out.Contents = []types.Object{{Key: &key}}
	}
	if start+1 < len(matched) {
		next := strconv.Itoa(start + 1)
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = &next
	}
	return out, nil
}

// applyRange interprets an HTTP byte range ("bytes=start-end" inclusive, or
// "bytes=start-" to the end), as S3 does. The Store only issues valid ranges.
func applyRange(data []byte, hdr string) []byte {
	spec := strings.TrimPrefix(hdr, "bytes=")
	lo, hi, _ := strings.Cut(spec, "-")
	start, _ := strconv.ParseInt(lo, 10, 64)
	if start > int64(len(data)) {
		start = int64(len(data))
	}
	if hi == "" {
		return append([]byte(nil), data[start:]...)
	}
	end, _ := strconv.ParseInt(hi, 10, 64) // inclusive
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	return append([]byte(nil), data[start:end+1]...)
}

func TestConformance(t *testing.T) {
	s := New(newFakeS3(), "test-bucket", "repos/org-repo/")
	blobtest.RunConformance(t, s)
}

func TestKeyPrefixing(t *testing.T) {
	f := newFakeS3()
	s := New(f, "bucket", "repos/x") // no trailing slash; New should add it
	if err := s.Put(context.Background(), "pack/a", []byte("y")); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objs["repos/x/pack/a"]; !ok {
		got := make([]string, 0, len(f.objs))
		for k := range f.objs {
			got = append(got, k)
		}
		t.Fatalf("object stored under %v, want key repos/x/pack/a", got)
	}
}

func TestListPaginates(t *testing.T) {
	ctx := context.Background()
	s := New(newFakeS3(), "bucket", "repos/x/")
	for _, k := range []string{"idx/1", "idx/2", "idx/3", "pack/a"} {
		if err := s.Put(ctx, k, []byte("y")); err != nil {
			t.Fatal(err)
		}
	}
	// The fake returns one key per page, so List must follow continuation
	// tokens to collect all three idx/ keys.
	keys, err := s.List(ctx, "idx/")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	want := []string{"idx/1", "idx/2", "idx/3"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] || keys[2] != want[2] {
		t.Fatalf("List(idx/) = %v, want %v", keys, want)
	}
}

func TestListTruncatedWithoutTokenErrors(t *testing.T) {
	f := newFakeS3()
	f.truncateWithoutToken = true
	s := New(f, "bucket", "repos/x/")
	if _, err := s.List(context.Background(), "idx/"); err == nil {
		t.Fatal("List must error (not loop) when the server reports more results but omits a token")
	}
}
