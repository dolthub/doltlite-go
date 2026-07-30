package litehttp

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dolthub/doltlite-go/litestore"
	"github.com/dolthub/doltlite-go/prollyhash"
	"github.com/dolthub/doltlite-go/remoteproto"
)

var (
	ErrRepoNotFound = errors.New("litehttp: repository not found")

	ErrUnauthorized = errors.New("litehttp: unauthorized")

	ErrForbidden = errors.New("litehttp: forbidden")
)

type StoreProvider interface {
	Store(r *http.Request, owner, repo string, write bool) (litestore.Store, error)
}

type StoreProviderFunc func(r *http.Request, owner, repo string, write bool) (litestore.Store, error)

func (f StoreProviderFunc) Store(r *http.Request, owner, repo string, write bool) (litestore.Store, error) {
	return f(r, owner, repo, write)
}

func NewHandler(p StoreProvider) http.Handler {
	return &handler{p: p}
}

type handler struct {
	p StoreProvider
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner, repo, endpoint, tail, ok := parsePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	write, known, methodOK := classify(r.Method, endpoint)
	if !known {
		http.NotFound(w, r)
		return
	}
	if !methodOK {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, tooLarge, err := readBody(r)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if tooLarge {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	store, err := h.p.Store(r, owner, repo, write)
	if err != nil {

		if errors.Is(err, ErrRepoNotFound) && endpoint == remoteproto.EndpointHasChunks {
			h.hasChunksMissing(w, body)
			return
		}
		writeProviderError(w, err)
		return
	}

	ctx := r.Context()
	switch endpoint {
	case remoteproto.EndpointHasChunks:
		hashes, derr := remoteproto.DecodeHashes(body)
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusBadRequest)
			return
		}
		present, herr := store.HasMany(ctx, hashes)
		if herr != nil {
			serverError(w, herr)
			return
		}
		writeOK(w, remoteproto.EncodePresence(present))

	case remoteproto.EndpointChunk:
		hsh, perr := prollyhash.Parse(tail)
		if perr != nil {
			http.Error(w, perr.Error(), http.StatusBadRequest)
			return
		}
		data, gerr := store.Get(ctx, hsh)
		if errors.Is(gerr, litestore.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if gerr != nil {
			serverError(w, gerr)
			return
		}
		writeOK(w, data)

	case remoteproto.EndpointChunks:
		chunks, derr := remoteproto.DecodeChunks(body)
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusBadRequest)
			return
		}

		for i := range chunks {
			if prollyhash.Compute(chunks[i].Data) != chunks[i].Hash {
				http.Error(w, "chunk hash does not match contents", http.StatusBadRequest)
				return
			}
		}
		if perr := store.Put(ctx, chunks); perr != nil {
			serverError(w, perr)
			return
		}
		writeOK(w, nil)

	case remoteproto.EndpointRefs:
		if r.Method == http.MethodGet {
			blob, gerr := store.GetRefs(ctx)
			if errors.Is(gerr, litestore.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if gerr != nil {
				serverError(w, gerr)
				return
			}
			writeOK(w, blob)
			return
		}

		// PUT /refs is an unconditional set. The branch/force prefix is parsed
		// off the body; the store keeps a single opaque refs blob, so the branch
		// scope is not yet enforced (see TODO below).
		_, _, blob, derr := remoteproto.DecodeRefs(body)
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusBadRequest)
			return
		}
		if len(blob) == 0 {
			http.Error(w, "empty refs blob", http.StatusBadRequest)
			return
		}
		if serr := store.SetRefs(ctx, blob); serr != nil {
			serverError(w, serr)
			return
		}
		writeOK(w, nil)

	case remoteproto.EndpointRefsIf:
		// TODO: the branch prefix declares which branch the push targets; a
		// future change could reject updates that touch any other branch. The
		// store's compare-and-swap is over the whole refs blob for now.
		_, force, expected, blob, derr := remoteproto.DecodeRefsIf(body)
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusBadRequest)
			return
		}
		var serr error
		if force {
			// A forced push skips the compare-and-swap.
			serr = store.SetRefs(ctx, blob)
		} else {
			serr = store.SetRefsIf(ctx, expected, blob)
		}
		if errors.Is(serr, litestore.ErrConflict) {
			http.Error(w, "refs precondition failed", http.StatusConflict)
			return
		}
		if serr != nil {
			serverError(w, serr)
			return
		}
		writeOK(w, nil)

	case remoteproto.EndpointCommit:
		if cerr := store.Commit(ctx); cerr != nil {
			serverError(w, cerr)
			return
		}
		writeOK(w, nil)

	case remoteproto.EndpointRoot:

		http.Error(w, "root endpoint not implemented", http.StatusNotImplemented)
	}
}

func (h *handler) hasChunksMissing(w http.ResponseWriter, body []byte) {
	hashes, err := remoteproto.DecodeHashes(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeOK(w, remoteproto.EncodePresence(make([]bool, len(hashes))))
}

func parsePath(p string) (owner, repo, endpoint, tail string, ok bool) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) < 3 || segs[0] == "" || segs[1] == "" || segs[2] == "" {
		return "", "", "", "", false
	}
	owner, repo, endpoint = segs[0], segs[1], segs[2]
	if len(segs) > 3 {
		tail = strings.Join(segs[3:], "/")
	}
	return owner, repo, endpoint, tail, true
}

func classify(method, endpoint string) (write, known, methodOK bool) {
	switch endpoint {
	case remoteproto.EndpointHasChunks:
		return false, true, method == http.MethodPost
	case remoteproto.EndpointChunk:
		return false, true, method == http.MethodGet
	case remoteproto.EndpointChunks:
		return true, true, method == http.MethodPost
	case remoteproto.EndpointRefs:
		return method == http.MethodPut, true, method == http.MethodGet || method == http.MethodPut
	case remoteproto.EndpointRefsIf:
		return true, true, method == http.MethodPut
	case remoteproto.EndpointCommit:
		return true, true, method == http.MethodPost
	case remoteproto.EndpointRoot:
		return false, true, method == http.MethodGet
	default:
		return false, false, false
	}
}

func readBody(r *http.Request) (body []byte, tooLarge bool, err error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, remoteproto.MaxRequestBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > remoteproto.MaxRequestBytes {
		return nil, true, nil
	}
	return b, false, nil
}

func writeOK(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func writeProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrRepoNotFound):
		http.Error(w, "repository not found", http.StatusNotFound)
	case errors.Is(err, ErrUnauthorized):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case errors.Is(err, ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		serverError(w, err)
	}
}

func serverError(w http.ResponseWriter, _ error) {
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
