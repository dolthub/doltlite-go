// Package blobs3 implements blob.BlobStore over Amazon S3 using aws-sdk-go-v2.
//
// It is a separate Go module so the AWS SDK never becomes a dependency of
// doltlite-go's core packages — consumers who only want the protocol, hashing,
// or the pack store pull none of it.
package blobs3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/dolthub/doltlite-go/blob"
)

// API is the subset of *s3.Client that Store uses. *s3.Client satisfies it;
// tests provide a fake.
type API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// Store is a blob.BlobStore backed by an S3 bucket. Every blob key is placed
// under an optional prefix, e.g. a per-repo path.
type Store struct {
	api    API
	bucket string
	prefix string
}

// New returns a Store writing under bucket/prefix. An empty prefix uses the
// bucket root; a non-empty prefix is normalized to end with "/".
func New(api API, bucket, prefix string) *Store {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Store{api: api, bucket: bucket, prefix: prefix}
}

var _ blob.BlobStore = (*Store)(nil)

func (s *Store) fullKey(key string) string { return s.prefix + key }

func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
		Body:   bytes.NewReader(data),
	})
	return err
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	return s.get(ctx, key, nil)
}

func (s *Store) GetRange(ctx context.Context, key string, off, length int64) ([]byte, error) {
	if length == 0 {
		return []byte{}, nil
	}
	var rng *string
	if length < 0 {
		rng = aws.String(fmt.Sprintf("bytes=%d-", off))
	} else {
		rng = aws.String(fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	}
	return s.get(ctx, key, rng)
}

func (s *Store) get(ctx context.Context, key string, rng *string) ([]byte, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
		Range:  rng,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, blob.ErrNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	full := s.fullKey(prefix)
	var keys []string
	var token *string
	for {
		out, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(full),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, o := range out.Contents {
			keys = append(keys, strings.TrimPrefix(aws.ToString(o.Key), s.prefix))
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		if aws.ToString(out.NextContinuationToken) == "" {
			return nil, fmt.Errorf("blobs3: list of %q reported more results but no continuation token", full)
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}

// isNotFound recognizes S3's absent-object errors, whether returned as a typed
// modeled error (real SDK) or a generic API error code (some S3-compatible
// servers).
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}
