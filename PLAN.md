# Plan: doltlite support for doltremoteapi

A living plan we can revisit. Implemented in reviewable stages; each stage is its
own PR. The reusable pieces (protocol, hashing, the packed store + its blob-store seam)
live in **this repo** so third parties can self-host a doltlite remote; only
DoltHub-specific glue (auth, repo-ID translation, bucket/IAM config) lands in the `ld`
monorepo. **Stage 1 is implemented; Stage 2 is implemented in `ld`.**

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
3. **Storage: pack-per-push, immutable blobs + a CAS'd manifest pointer** — accumulate
   each push's chunks into uniquely-keyed (immutable) blob object(s) + an index, rather
   than one object per chunk. Two seams: a `BlobStore` for the immutable objects (opaque
   put/get/range/list, no conditional writes) and a tiny `ManifestStore` holding the one
   mutable per-repo pointer with a compare-and-swap. Mirrors dolt's AWS NBS split — table
   files in S3, the root CAS in DynamoDB — so S3 / GCS / R2 / MinIO / local (blobs) and
   DynamoDB / conditional-put / a DB row (the CAS) are just adapters.
4. **Integrity: verify** — recompute each uploaded chunk's BLAKE3-20 server-side.
5. **Reuse boundary** — everything generic (protocol, hashing, the packed store + blob
   seam, an in-memory blob adapter) lives in `doltlite-go`; only DoltHub glue lives in
   `ld`. Cloud SDKs stay out of the core module — the S3 adapter is isolated in its own
   submodule (own `go.mod`) so consumers who only want the protocol/hash/packed store
   don't transitively pull the AWS SDK. (This mirrors dolt's NBS, which is generic over a
   `blobstore.Blobstore` with S3/GCS/local backends.)

### Cross-repo prerequisite (DONE)
doltlite's HTTP client now speaks **HTTPS and sends an `Authorization` bearer header**,
and the test server verifies it (merged in doltlite, PRs #1563 + #1564). The Go server
and library here were built/tested independently via the Go client, so they were never
blocked. The credential contract is pinned by `authcompat` (its golden token is byte-for-
byte identical to doltlite's `test/doltlite_creds_kat.c`).

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
  seam. `ld` plugs in a blob-backed `litestore.Store` + auth/repo-ID here. Includes a
  full client→handler→in-memory round-trip test.
- **`authcompat`** — pins the DoltHub credential contract (EdDSA JWT: `kid`,
  `iss`/`sub`/`aud`, `exp=iat+30`) with a golden token identical to doltlite's C KAT, so
  `ld`'s existing `doltremoteauth` validator accepts doltlite-signed tokens.

Deliberate deviation: the handler recomputes the chunk hash and rejects a mismatch
against the client-claimed hash (stricter than the upstream C server, which silently
recomputes) — appropriate for a multi-tenant hosted service.

---

## Stage 2 — Regression safety in `ld` (implemented)

The existing `ChunkStoreService` RPCs had almost no unit coverage. Before touching shared
server wiring, added characterization tests over a local NBS built from a checked-in dolt
fixture (cloud-free: `AlwaysAuthed`, `SimpleRepoIDer`, an in-test `tokenenc` sealer) for
`GetRepoMetadata`, `Root`, `HasChunks`, `GetDownloadLocations`, `GetUploadLocations`, and
`Commit` input validation. A green baseline that fails if the dolt path regresses; passes
under both `go test` and `bazel test`. Landed on `bh/doltlite-remoteapi-stage2`.

## Stage 3 — Packed, blob-backed `litestore.Store` (mostly `doltlite-go`)

The reusable storage engine, split so the cloud SDK never touches the core module.

Two seams, matching how dolt's AWS NBS already splits storage (table files in S3, the
root/manifest CAS in DynamoDB):
- **`BlobStore`** — immutable, uniquely-keyed objects: `Put(key, bytes)`, `Get(key)`,
  `GetRange(key, off, len)`, `List(prefix)`. No conditional writes; pack blobs and index
  objects are written once per push under unique keys, so there are no overwrite races.
- **`ManifestStore`** — the one small mutable per-repo pointer (current refs blob hash +
  index location), with an atomic `Get()` + `CompareAndSwap(prev, next)`.

### 3a — generic packed store + both seams (`doltlite-go`, PR)

A generic `litestore.Store` implementation (the "pack store") composed over `BlobStore` +
`ManifestStore`:
- `Put(chunks)` verifies each chunk's BLAKE3-20, appends bytes into one uniquely-keyed blob
  object, and records a per-repo index (`hash → blobKey,offset,len`) as another blob.
- `HasMany`/`Get` read via the index.
- `GetRefs`/`SetRefs`/`SetRefsIf`/`Commit` update the manifest pointer; `SetRefsIf` and
  `Commit` are a `CompareAndSwap` on the `ManifestStore` (the store-level refs CAS from
  Stage 1).

Ships with **in-memory implementations of both seams** for tests — distinct from Stage 1's
in-memory `Store`: these exercise the real packing/index/manifest code and CAS-conflict
behavior over fakes. No cloud deps; stays in the core module.

### 3b — S3 blob adapter (`doltlite-go` submodule `blobs3`)

The `BlobStore` backed by S3 (`aws-sdk-go-v2`): `PutObject`, ranged `GetObject`,
`ListObjectsV2` — no conditional writes needed, since blobs are immutable. Lands in a
**separate submodule with its own `go.mod`** so the AWS SDK is never a dependency of
consumers who only want the protocol / hash / pack store. Tested against a bucket fake /
localstack and by swapping it under 3a's pack-store suite.

The `ManifestStore` (CAS) adapter is **not** in `blobs3` — DoltHub backs it with DynamoDB
(next stage), matching what dolt's NBS already does; a self-hoster can supply any CAS
backend behind the seam.

## Stage 4 — Wire into the service in `ld` (PR)

DoltHub-specific glue only. Implement `litehttp.StoreProvider`: extract JWT from
`Authorization`, authenticate via the existing `doltremoteauth` client, authorize
READ/WRITE per endpoint, translate `owner/repo` → internal repo ID via `RepoIDer`, then
construct the pack store (3a) over the S3 `BlobStore` (3b) plus a **DynamoDB
`ManifestStore`** for the CAS pointer — reusing the existing `cfg.AwsCfg.DynamoTable` /
`abs`-style session plumbing that dolt's `nbs.NewAWSStore` already uses. No `NBSCache`
(doltlite needs no in-memory index/quota). Serve `litehttp.NewHandler` from a new
`http.Server` on its own port in `runServer` (HTTP/1.1, cannot share the gRPC/HTTP-2
listener); add config + graceful shutdown. Factor the authenticate/authorize helper in
`storesrv/auth.go` so gRPC and HTTP share it.

## Stage 5 — Infra (separate)

k8s Service/ingress route + container port (`//k8s/services/doltremoteapi`), S3/IAM
prefix perms (terraform). (The cross-repo doltlite C client HTTPS + auth header is done —
see the prerequisite above.)

---

## Verification

- Stage 1: `go test ./...` in this module — golden hash vectors, wire round-trips,
  store CAS semantics, and a full protocol client→handler→store push/pull cycle
  including a stale-`refs-if` 409.
- Stage 2 (done): `go test` + `bazel test` of the `storesrv` characterization suite.
- Stage 3a: `go test ./...` — run the full `litestore.Store` conformance/CAS suite
  against the pack store over the in-memory `BlobStore`, exercising packing, index reads,
  and manifest compare-and-swap (concurrent `SetRefsIf` → `ErrConflict`).
- Stage 3b: the same pack-store suite run against the S3 adapter over a bucket
  fake / localstack.
- Stage 4: a Go-client end-to-end push/pull against the wired handler with `AuthDisabled`;
  auth allow/deny.
- Stage 5: real `bin/doltlite` push/pull against dev doltremoteapi (C client HTTPS+auth is
  already shipped).

## Risks / open items
- **Manifest CAS.** Only the per-repo manifest pointer is mutable (blobs are immutable).
  DoltHub does the CAS with a DynamoDB conditional write — exactly as dolt's
  `nbs.NewAWSStore` does today, reusing the existing table. The library keeps it behind the
  `ManifestStore` seam (in-memory for tests) so self-hosters can back it with DynamoDB, an
  S3 conditional put (`If-Match`/`If-None-Match`), a DB row, or a local lock — with no
  change to the pack store or the S3 blob adapter.
- **Submodule boundary.** The `blobs3` submodule keeps the AWS SDK out of the core, at the
  cost of an extra `go.mod` to version (and a `go.work` for local dev). Acceptable, and
  standard; revisit only if a second cloud adapter makes a shared parent module cleaner.
- Pack-per-push leaves some blob redundancy across overlapping pushes; a compaction/GC
  pass can follow (out of scope initially).
- We reimplement a protocol defined by C source: pin the doltlite commit targeted, keep
  the golden vectors + framing tests as the contract, add a version check if doltlite
  versions its remote protocol.
- `/root` deferred (unused by the sync client); revisit if a non-sync consumer needs it.
