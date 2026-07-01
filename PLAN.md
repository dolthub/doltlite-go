# Plan: doltlite support for doltremoteapi

A living plan we can revisit. Implemented in reviewable stages; each stage is its
own PR. **Stage 1 (this repo) is implemented.** Later stages land in the `ld`
monorepo.

## Context / why

`doltremoteapi` (in `ld`: `go/services/doltremoteapi`) is DoltHub's gRPC service that
stores **dolt** databases pushed/pulled by the `dolt` CLI. It implements dolt's
`ChunkStoreService`, built around dolt's NBS model: clients exchange whole **table
files** via signed S3 URLs and the server serves byte-range chunk locations from an
in-memory `NomsBlockStore`. It is hardcoded to `types.Format_DOLT`.

We want the same hosted service to also store **doltlite** databases. doltlite
(`github.com/dolthub/doltlite`) is a SQLite fork whose storage is a single-file,
content-addressed **prolly-tree** chunk store. It does **not** speak dolt's gRPC
protocol — it has its own small **HTTP/1.1 sync protocol** (`doltlite_http_remote.c`,
`doltlite_remotesrv.c`):

| Method & path | Body in | Body out |
|---|---|---|
| `POST /{repo}/has-chunks` | N×20-byte hashes | N presence bytes (1=present) |
| `GET  /{repo}/chunk/{hex}` | — | raw chunk bytes (404 if absent) |
| `POST /{repo}/chunks` | repeated `[20B hash][4B LE len][bytes]` | empty (store, then durable) |
| `GET  /{repo}/refs` | — | opaque refs blob (404 if none) |
| `PUT  /{repo}/refs` | opaque refs blob | empty |
| `PUT  /{repo}/refs-if` | `[20B expected-refs-hash][blob]` | empty / **409** on mismatch |
| `POST /{repo}/commit` | — | empty |
| `GET  /{repo}/root` | — | 20-byte root hash (**unused by sync client**) |

Key facts that make this tractable:
- **Chunk address = BLAKE3(chunk bytes) truncated to 20 bytes** (doltlite
  `prollyHashCompute`, `src/prolly_hash.c`). Standard BLAKE3; first 20 bytes of the
  32-byte digest. (xxhash in doltlite is only the content-defined-chunking rolling
  hash, not the chunk address.)
- The server side is **storage-format-agnostic**: it only needs content-addressed
  get/put/has of chunks + an opaque refs blob with a compare-and-swap. It never parses
  prolly trees. The refs-CAS hash is `BLAKE3-20(refs blob)`.
- The sync client treats the refs blob as opaque and **never calls `/root`**, so the
  hosted server needs no doltlite format knowledge on the push/pull path.
- doltlite's store is C; there is no Go implementation to reuse — hence this library.

### Confirmed decisions
1. **Target: hosted DoltHub** — S3-backed, JWT auth, repo-ID translation, TLS.
2. **Topology: one binary** — a doltlite HTTP handler alongside the existing gRPC server.
3. **Storage: pack-per-push** — accumulate each push's chunks into blob object(s) + an
   index, rather than one S3 object per chunk.
4. **Integrity: verify** — recompute each uploaded chunk's BLAKE3-20 server-side.

### Cross-repo prerequisite (tracked separately, outside `ld`)
doltlite's HTTP client today only does `http://` and sends no auth header
(`doltlite_http_remote.c`). Real hosted push/pull needs C-side changes for **HTTPS + an
`Authorization` bearer header**. The Go server and library here are built and tested
independently of that (via the Go client in this module), so they are not blocked.

---

## Stage 1 — `doltlite-go` library (THIS REPO — implemented)

A standalone Go module `github.com/dolthub/doltlite-go` that `ld` will import. Pure
Go, no cloud dependencies, fully unit-tested.

- **`prollyhash`** — doltlite chunk-address hashing: `Compute([]byte) Hash` (BLAKE3-20),
  `Hash` (20 bytes) with `String`/`Parse`/`IsEmpty`. Golden tests use doltlite's own
  KAT vectors (`test/blake3_kat_test.sh`). *(Plan step 1a.)*
- **`remoteproto`** — the HTTP sync protocol: endpoint constants, size limits
  (`MaxChunkBytes`=64 MiB, `MaxRequestBytes`=128 MiB), and encode/decode of every body
  framing (has-chunks, chunks, refs-if, presence). Shared by client and server.
- **`litestore`** — server-side `Store` interface (the operations the handler needs)
  plus an in-memory implementation and `Chunk`/`ErrNotFound`/`ErrConflict`. The
  refs-CAS compares `BLAKE3-20(current refs)` to the caller's expected hash.
- **`remote`** — Go client for the sync protocol (analog of `DoltliteRemote`), with an
  optional bearer token. Used for end-to-end tests and future Go tooling.
- **`litehttp`** — `http.Handler` implementing the protocol against a `StoreProvider`
  seam. `ld` plugs in an S3-backed `litestore.Store` + auth/repo-ID here. Includes a
  full client→handler→in-memory round-trip test.

Deliberate deviation: the handler recomputes the chunk hash and rejects a mismatch
against the client-claimed hash (stricter than the upstream C server, which silently
recomputes) — appropriate for a multi-tenant hosted service.

---

## Stage 2 — Regression safety in `ld` (PR)

The existing `ChunkStoreService` RPCs have almost no unit coverage. Before touching
shared server wiring, add characterization tests over a local NBS using existing infra
(`TestNBSFactory`, `newTestNBSCache`, `AlwaysAuthed`, `SimpleRepoIDer`, testdata dolt
dbs) for `GetRepoMetadata`, `Root`, `HasChunks`, `GetUploadLocations`, `Commit`,
`GetDownloadLocations`. Goal: a green baseline that fails if the dolt path regresses.

## Stage 3 — S3-backed `litestore.Store` in `ld` (PR)

Implement the pack-per-push backend: `PutChunks` writes verified chunk bytes into one
S3 blob + a per-repo index (`hash → blobKey,offset,len`); a per-repo manifest tracks
blobs, the index, and the current refs blob + its hash for CAS. Reuse the AWS
session/bucket plumbing already used by `regRepoService`. No `NBSCache` (doltlite needs
no in-memory index/quota). In-memory `litestore` from Stage 1 backs its tests.

## Stage 4 — Wire into the service in `ld` (PR)

Implement `litehttp.StoreProvider`: extract JWT from `Authorization`, authenticate via
the existing `doltremoteauth` client, authorize READ/WRITE per endpoint, translate
`owner/repo` → internal repo ID via `RepoIDer`, return the S3 `litestore.Store`. Serve
`litehttp.NewHandler` from a new `http.Server` on its own port in `runServer`
(HTTP/1.1, cannot share the gRPC/HTTP-2 listener); add config + graceful shutdown.
Factor the authenticate/authorize helper in `storesrv/auth.go` so gRPC and HTTP share it.

## Stage 5 — Infra + cross-repo (separate)

k8s Service/ingress route + container port (`//k8s/services/doltremoteapi`), S3/IAM
prefix perms (terraform). Cross-repo: doltlite C client HTTPS + auth header.

---

## Verification

- Stage 1: `go test ./...` in this module — golden hash vectors, wire round-trips,
  store CAS semantics, and a full protocol client→handler→store push/pull cycle
  including a stale-`refs-if` 409.
- Stage 3/4: S3-fake store tests; a Go-client end-to-end push/pull against the wired
  handler with `AuthDisabled`; auth allow/deny.
- Stage 5: real `bin/doltlite` push/pull against dev doltremoteapi once the C client
  gains HTTPS+auth.

## Risks / open items
- Pack-per-push leaves some blob redundancy across overlapping pushes; a compaction/GC
  pass can follow (out of scope initially).
- We reimplement a protocol defined by C source: pin the doltlite commit targeted, keep
  the golden vectors + framing tests as the contract, add a version check if doltlite
  versions its remote protocol.
- `/root` deferred (unused by the sync client); revisit if a non-sync consumer needs it.
