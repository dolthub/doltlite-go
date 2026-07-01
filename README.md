# doltlite-go

Go library for interacting with [doltlite](https://github.com/dolthub/doltlite)
databases over doltlite's HTTP sync protocol.

doltlite is a SQLite fork whose storage engine is a content-addressed prolly-tree
chunk store. It syncs (clone/fetch/push) over a small HTTP protocol implemented in C
(`doltlite-remotesrv` / `doltlite_http_remote.c`). This module reimplements the pieces
of that protocol needed in Go.

## Packages

| Package | Purpose |
|---|---|
| `prollyhash` | doltlite chunk-address hashing: `Compute([]byte) Hash` = BLAKE3 truncated to 20 bytes. Matches doltlite's `prollyHashCompute`; validated against doltlite's own known-answer vectors. |
| `remoteproto` | The HTTP sync wire format: endpoint names, size limits, and encoders/decoders for every request/response body. Shared by client and server. |
| `litestore` | Server-side `Store` interface (content-addressed chunk get/put/has + an opaque refs blob with compare-and-swap) plus an in-memory implementation. |
| `remote` | Go client for the sync protocol (analog of doltlite's C `DoltliteRemote`), with optional TLS and bearer-token auth. |
| `litehttp` | `http.Handler` implementing the protocol against a pluggable `StoreProvider`, where a host supplies auth, repo-name translation, and the storage backend. |

## The protocol

Requests are `/{owner}/{repo}/{endpoint}` (the handler is transport/format-agnostic —
it stores opaque chunk bytes keyed by their BLAKE3-20 address and an opaque refs blob):

| Method & path | Body in | Body out |
|---|---|---|
| `POST /{repo}/has-chunks` | N×20-byte hashes | N presence bytes (1=present) |
| `GET  /{repo}/chunk/{hex}` | — | raw chunk bytes (404 if absent) |
| `POST /{repo}/chunks` | repeated `[20B hash][4B LE len][bytes]` | empty |
| `GET  /{repo}/refs` | — | opaque refs blob (404 if none) |
| `PUT  /{repo}/refs` | opaque refs blob | empty |
| `PUT  /{repo}/refs-if` | `[20B expected-refs-hash][blob]` | empty / 409 on mismatch |
| `POST /{repo}/commit` | — | empty |

## Usage

Serve doltlite databases:

```go
h := litehttp.NewHandler(litehttp.StoreProviderFunc(
    func(r *http.Request, owner, repo string, write bool) (litestore.Store, error) {
        // authenticate r, authorize write, resolve owner/repo -> a litestore.Store
        return myStoreFor(owner, repo), nil
    }))
http.ListenAndServe(":8080", h)
```

Push to a remote:

```go
cl := remote.New("https://host/owner/repo", remote.WithBearerToken(tok))
cl.PutChunks(ctx, chunks)
cl.SetRefsIf(ctx, prevRefsHash, refsBlob) // prevRefsHash is prollyhash.Compute(prevBlob), or zero
cl.Commit(ctx)
```

## Testing

```sh
go test ./...
```

`prollyhash` is pinned to doltlite's `test/blake3_kat_test.sh` vectors, and `litehttp`
runs a full client→handler→store push/pull cycle over a real HTTP server.

## Compatibility note

This tracks doltlite's remote protocol as defined by its C source. doltlite's own HTTP
client is currently plaintext-only and sends no auth header; this library's client adds
HTTPS + bearer-token support so it can drive an authenticated, TLS-terminated host such
as `doltremoteapi`.
