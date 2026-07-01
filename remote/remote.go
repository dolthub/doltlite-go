// Package remote is a Go client for doltlite's HTTP sync protocol — the
// analog of doltlite's C DoltliteRemote/doltliteHttpRemoteOpen. It is used for
// end-to-end testing of a server (package litehttp) and for Go tooling that
// pushes to or pulls from a doltlite remote.
//
// Unlike doltlite's current C client, this client supports https:// and an
// optional bearer token, so it can exercise an authenticated, TLS-terminated
// hosted server.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dolthub/doltlite-go/litestore"
	"github.com/dolthub/doltlite-go/prollyhash"
	"github.com/dolthub/doltlite-go/remoteproto"
)

// ErrNotFound is returned by GetChunk and GetRefs when the server responds 404.
var ErrNotFound = errors.New("remote: not found")

// ErrConflict is returned by SetRefsIf when the server responds 409, meaning
// the refs precondition failed and the caller should re-fetch refs and retry.
var ErrConflict = errors.New("remote: refs precondition failed")

// Client talks to a single doltlite repository's remote endpoint.
type Client struct {
	httpc     *http.Client
	baseURL   string // no trailing slash; e.g. https://host/owner/repo
	authToken string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the underlying HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpc = c }
}

// WithBearerToken sends "Authorization: Bearer <token>" on every request.
func WithBearerToken(token string) Option {
	return func(cl *Client) { cl.authToken = token }
}

// New returns a Client for the repository rooted at baseURL. baseURL should
// include the repository path (e.g. https://doltremoteapi.dolthub.com/owner/repo).
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		httpc:   http.DefaultClient,
		baseURL: strings.TrimRight(baseURL, "/"),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) do(ctx context.Context, method, endpoint string, body []byte) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+endpoint, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, remoteproto.MaxRequestBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// HasChunks reports, for each requested hash, whether the remote has the chunk.
func (c *Client) HasChunks(ctx context.Context, hashes []prollyhash.Hash) ([]bool, error) {
	status, body, err := c.do(ctx, http.MethodPost, remoteproto.EndpointHasChunks, remoteproto.EncodeHashes(hashes))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError("has-chunks", status)
	}
	return remoteproto.DecodePresence(body, len(hashes))
}

// GetChunk fetches a single chunk by hash, or returns ErrNotFound.
func (c *Client) GetChunk(ctx context.Context, h prollyhash.Hash) ([]byte, error) {
	status, body, err := c.do(ctx, http.MethodGet, remoteproto.EndpointChunk+"/"+h.String(), nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return body, nil
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, statusError("chunk", status)
	}
}

// PutChunks uploads chunks to the remote in a single request. The server stores
// them durably.
func (c *Client) PutChunks(ctx context.Context, chunks []litestore.Chunk) error {
	status, _, err := c.do(ctx, http.MethodPost, remoteproto.EndpointChunks, remoteproto.EncodeChunks(chunks))
	if err != nil {
		return err
	}
	if !okStatus(status) {
		return statusError("chunks", status)
	}
	return nil
}

// GetRefs fetches the remote's refs blob, or returns ErrNotFound.
func (c *Client) GetRefs(ctx context.Context) ([]byte, error) {
	status, body, err := c.do(ctx, http.MethodGet, remoteproto.EndpointRefs, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return body, nil
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, statusError("refs", status)
	}
}

// SetRefs unconditionally installs the refs blob on the remote.
func (c *Client) SetRefs(ctx context.Context, blob []byte) error {
	status, _, err := c.do(ctx, http.MethodPut, remoteproto.EndpointRefs, blob)
	if err != nil {
		return err
	}
	if !okStatus(status) {
		return statusError("refs", status)
	}
	return nil
}

// SetRefsIf installs the refs blob only if the remote's current refs hash equals
// expected, else returns ErrConflict. For a repository with no refs yet, pass
// the zero hash as expected.
func (c *Client) SetRefsIf(ctx context.Context, expected prollyhash.Hash, blob []byte) error {
	status, _, err := c.do(ctx, http.MethodPut, remoteproto.EndpointRefsIf, remoteproto.EncodeRefsIf(expected, blob))
	if err != nil {
		return err
	}
	switch {
	case okStatus(status):
		return nil
	case status == http.StatusConflict:
		return ErrConflict
	default:
		return statusError("refs-if", status)
	}
}

// Commit issues the protocol's commit barrier.
func (c *Client) Commit(ctx context.Context) error {
	status, _, err := c.do(ctx, http.MethodPost, remoteproto.EndpointCommit, nil)
	if err != nil {
		return err
	}
	if !okStatus(status) {
		return statusError("commit", status)
	}
	return nil
}

// Root returns the remote's root hash (the default branch tip). Note: doltlite's
// own sync client never calls this endpoint, and hosted servers may not
// implement it.
func (c *Client) Root(ctx context.Context) (prollyhash.Hash, error) {
	var h prollyhash.Hash
	status, body, err := c.do(ctx, http.MethodGet, remoteproto.EndpointRoot, nil)
	if err != nil {
		return h, err
	}
	if status != http.StatusOK {
		return h, statusError("root", status)
	}
	if len(body) != prollyhash.Size {
		return h, fmt.Errorf("remote: root response length %d, want %d", len(body), prollyhash.Size)
	}
	copy(h[:], body)
	return h, nil
}

func okStatus(status int) bool {
	return status == http.StatusOK || status == http.StatusNoContent
}

func statusError(op string, status int) error {
	return fmt.Errorf("remote: %s: unexpected status %d", op, status)
}
