# Single-Trip Direct GET - Design Note

**Status:** Draft / discussion notes

**Summary:** Buckit currently serves a normal shard-backed GET in two storage
phases: first read `xl.meta` across the erasure set to choose the visible version
and layout, then read shard bytes from the selected data files. This note proposes
adding **direct data paths** on top of the current metadata model: `xl.meta`
remains the canonical version index, while the physical shard directory for the
latest version is named `current` and older versions are named by version ID. The
shard files carry a small checked header before the existing bitrot-protected
shard bytes. A plain latest GET can open
`bucket/object/current/part.1`, read header then shard in one trip, and fall back
to today's `xl.meta` path whenever the fast path is missing, stale, inconsistent,
or not applicable.

This is not a replacement for `xl.meta`, and `current` is not an extra copy of
the latest shard data. There is one physical data location for a version:
`current` while it is latest, then `versions/<versionId>` after it is superseded.
The first target is plain latest GET; the same mechanism can also accelerate
explicit version GET.

Read `request-flow.md` first; this note assumes its vocabulary (set, shard,
`xl.meta`, `DataDir`, quorum, `ErasureDist`).

---

## 1. Context and motivation

### 1.1 Why this matters: making HDD more viable

The architecture Buckit inherited from MinIO is optimized for flash media. HDDs
are cheaper per terabyte and strong at sequential I/O, but weak at random metadata
IOPS. This matters for capacity-oriented deployments such as backups, archives,
media stores, and data lakes.

The HDD issue is broader than GET alone: small-object workloads, listing,
versioning, healing, scanning, and degraded reads can all create random I/O. This
proposal targets one concrete hot-path contributor: latest GET currently pays a
metadata-file read phase before it can open shard files.

### 1.2 Current GET path

For a healthy 16-disk set with `EC:4` (M=12 data, N=4 parity), a normal large
object GET generally does:

1. **Metadata fan-out:** read `xl.meta` from disks in the set to establish quorum
   on the visible version and learn its layout.
2. **Shard read:** open the selected `DataDir/part.N` files and Reed-Solomon
   decode from M available shard streams, pulling extra shards only on failures.

For large shard-backed objects, this is roughly one metadata-file seek plus one
data-file seek on the disks that participate in the read. On HDD, those seeks and
the second storage request phase dominate first-byte latency. On flash, the seek
cost is small, but the extra request phase still costs latency.

This statement has important exceptions:

- Small objects can already be returned from the metadata read path when inline
  data is present or when `ReadData` inlines a small single-part shard.
- Zero-byte and transitioned objects take special paths.
- The decoder does not eagerly read all M+N shards; it starts with M readers per
  stripe and falls back to more only when needed.

The target is therefore not "all GETs." The first target is the common large,
local, shard-backed latest GET, with explicit version GET as a natural extension
when the version ID maps directly to a stable data directory.

---

## 2. Core design: `xl.meta` canonical, direct data paths

The proposed layout adds stable directly-openable data directories alongside the
canonical metadata file:

```
bucket/object/xl.meta                  # canonical version chain and metadata
bucket/object/current/part.1           # physical data for the latest version
bucket/object/versions/<versionId-1>/part.1  # physical data for older version
bucket/object/versions/<versionId-2>/part.1
```

For non-versioned or null-version objects, the direct version path needs an
encoded reserved name rather than the literal empty/null value.

There is no duplicate `part.1` under both `current` and `versions/<versionId>` for
the same version. When a latest version is superseded, its data directory is
renamed or exchanged out of `current` into its deterministic version path.

`xl.meta` remains the source of truth for:

- version existence and ordering;
- delete markers;
- fallback for explicit `GET ?versionId=...`;
- listing and version listing;
- lifecycle, replication, healing, scanner, and repair decisions;
- compatibility with existing objects.

`current` is the direct path for plain GET without `versionId`.
`versions/<versionId>` is the direct path for explicit GET of non-current
versions. If a direct path is missing or disagrees with quorum, the request falls
back to the existing `xl.meta`-driven path. Repair can later reconstruct missing
or corrupt direct data from erasure quorum, with `xl.meta` deciding what should
exist.

### 2.1 Current shard file format

Each direct-path `part.N` file starts with a small header, then the existing shard
payload:

```
current/part.1  (one disk's current shard for one S3 part)
+--------------+-----------------------------------------------------------+
|  HEADER      |  SHARD DATA                                               |
|  (metadata)  |  hash_0 | block_0 | hash_1 | block_1 | ... | hash_k | block_k |
+--------------+-----------------------------------------------------------+
  ^ length-prefixed, checksummed       ^ existing per-block bitrot layout
```

The header carries enough information for the landing node to validate and decode
the fast path:

- object name, version ID, modtime, etag, object size;
- whether the current entry is an object or delete marker;
- direct path identity (`current` or `versions/<versionId>`) and canonical
  version identity;
- `ErasureM`, `ErasureN`, `ErasureIndex`, `ErasureDist`, block size;
- part number, part size, actual shard file size, checksum algorithm;
- version/header signature used for cross-disk quorum comparison;
- format version and header checksum.

The shard-data region should remain byte-for-byte compatible with the existing
bitrot block layout after accounting for the header offset. Existing bitrot
verification and erasure decode should not need semantic changes, but the reader
must know that shard offsets are relative to the payload start, not file offset 0.

### 2.2 Delete-marker current

Latest can be a delete marker. A plain GET must return not found when the quorum
latest entry is a delete marker, even if older data versions exist.

Therefore `current` must also represent "latest is deleted." Options:

- a small `current/marker` file with the same checked header and no shard data;
- a reserved `current/part.1` header marked delete-marker with empty payload;
- no `current` data plus a separate marker file.

The fast path must never silently fall through to older shard data when latest is
a delete marker. If the delete-marker fast path is missing or inconsistent, fall
back to `xl.meta`.

---

## 3. Fast GET protocol

### 3.1 Plain latest GET

For `GET /bucket/object` without `versionId`:

1. The landing node opens `current/part.N` on each disk in the set for the S3 part
   needed by the request.
2. Each disk returns the checked header first.
3. The landing node compares headers and establishes quorum on the same current
   version/signature.
4. If the quorum current is a delete marker, return not found.
5. If the quorum current is object data, stream from the selected M shard readers.
6. If any check fails, fall back to the existing `xl.meta` path and queue repair
   of the direct data path.

The common healthy path removes the separate `xl.meta` read phase from latest
GET. The correctness rule is simple: `current` can accelerate the answer, but it
cannot override `xl.meta`.

### 3.2 Header-pause vs speculative stream

For large objects, the safest protocol is header-pause:

```
each disk -> stream HEADER -> pause
landing node:
   collect headers and establish quorum
   continue selected M shard streams
   stop the rest
```

For small objects or low-latency flash deployments, the implementation may choose
to stream header and data speculatively and cancel losers after quorum selection.
This trades some wasted bandwidth for lower control-plane complexity.

The existing decoder already starts with M readers per stripe and reads additional
shards only after missing/corrupt reads. The new protocol should preserve that
behavior rather than eagerly reading all M+N shards.

### 3.3 Explicit version GET

Explicit version GET can use the same direct-path mechanism, but the lookup must
account for the no-duplicate rule:

```
GET /bucket/object?versionId=<v>
-> try bucket/object/current/part.N and accept it only if the header says <v>
-> otherwise try bucket/object/versions/<v>/part.N
-> read header, establish quorum for <v>, stream selected M shards
```

This removes the `xl.meta` lookup for healthy explicit version GETs when the
requested version is either current or already under `versions/<v>`. If the direct
paths are missing, corrupt, stale, or not supported by an old object, fall back to
today's `xl.meta` path and queue repair if enough shard data exists.

The directory name must be a safe encoding of the S3 version ID. The header still
needs to carry and validate the canonical version ID; the path name alone is not
sufficient for correctness.

### 3.4 Multipart and range GET

In the current layout, `part.N` is the S3 multipart part number, not the erasure
shard number. Most single-part objects only have `part.1`; multipart objects may
have `part.2`, `part.3`, and so on.

Today's multipart read flow is:

1. read `xl.meta` and choose the visible version;
2. use that version's part list to map the requested object byte range to a start
   part, offset within that part, and end part;
3. for each needed S3 part, open `bucket/object/<DataDir>/part.N` across disks;
4. Reed-Solomon decode that part's shard streams, then continue to the next
   `part.N` until the requested range is complete.

Range GET complicates the fast path because the starting byte may map to a later
S3 part. To keep the fast path correct, every direct-path `part.N` that can be
opened directly must carry enough header information to validate the requested
version and that part's layout.

In the direct-path layout, a multipart current version would look like:

```
bucket/object/current/part.1
bucket/object/current/part.2
bucket/object/current/part.3
```

Each file needs its own header so a range GET that starts in `part.3` can validate
the version and part layout without first reading `part.1` or `xl.meta`.

Initial implementation can reasonably limit the fast path to:

- full-object or range reads that begin in `part.1`;
- single-part objects;
- non-transitioned shard-backed data.

Other requests fall back to `xl.meta` until the multipart/range protocol is
explicitly designed.

---

## 4. Write and crash consistency

### 4.1 Canonical commit

PUT/DELETE correctness remains based on today's canonical metadata commit:

1. write shard data to temporary files;
2. commit the new latest data to `current`;
3. if an older latest version existed, move that old `current` directory to
   `versions/<oldVersionId>`;
4. update `xl.meta` as the canonical visible version chain.

The ordering can be adjusted as long as recovery has a deterministic rule. The
important invariant is that `xl.meta` decides which versions are visible and which
direct path should contain each version. If a crash leaves a mismatch, fast GET
falls back to `xl.meta`; repair either reconstructs the missing direct path from
erasure quorum or removes uncommitted data.

If a crash leaves `current` newer than `xl.meta`, `xl.meta` wins. The fast path
must detect that by quorum/header validation or by falling back when the `current`
headers do not form a valid committed quorum.

### 4.2 Updating `current`

A stable `current` directory is attractive because latest GET can open:

```
bucket/object/current/part.1
```

without resolving an opaque `DataDir` through `xl.meta`.

The important rule is to replace `current` by rename/exchange, not by overwriting
shard bytes in place. A reader that already opened `current/part.1` holds a file
descriptor to the old inode, so it can keep streaming even after the `current`
path is atomically swapped to a new directory. The swapped-out old directory then
becomes the immutable `versions/<oldVersionId>` directory; it is not a temporary
duplicate.

Updating that directory is not identical to today's `RenameData` flow. Today a
fresh immutable `DataDir` is committed under an opaque name and then made visible
through `xl.meta`. In the new layout, the latest version's physical data must land
at:

```
bucket/object/current
```

and the previous latest data must be preserved under:

```
bucket/object/versions/<oldVersionId>
```

There is no second copy. The filesystem operation is a rename/exchange of
directories, not a copy into `current`.

On Linux, `renameat2(RENAME_EXCHANGE)` can atomically swap two existing pathnames,
including directories on filesystems that support it:

```
bucket/object/current      <-> bucket/object/.staging-new
```

After the exchange:

```
bucket/object/current      # new latest data
bucket/object/.staging-new # old latest data, to rename to versions/<oldVersionId>
```

This can make latest replacement atomic at the per-disk namespace level, but it is
not portable POSIX behavior and still needs:

- startup capability detection for kernel and filesystem support;
- correct file and parent-directory fsync ordering;
- recovery of the old swapped-out directory into `versions/<oldVersionId>`;
- handling when latest is a delete marker;
- fallback for platforms/filesystems without `RENAME_EXCHANGE`.

The post-exchange rename of old data to `versions/<oldVersionId>` is another
crash point. Recovery must be able to recognize a swapped-out old-current staging
directory from its header and finish the rename or discard it if `xl.meta` says it
is not a committed version.

---

## 5. Durability risks and remediations

The new direct paths do not weaken erasure coding, but they add crash states that
the current opaque-`DataDir` layout does not have. The safe rule is:

```
xl.meta remains canonical.
Direct paths are served only when their headers form quorum for a committed version.
Staging/current/version mismatches are reconciled against xl.meta.
```

### 5.1 Crash-state risks

| Risk | Example state | Remediation |
|---|---|---|
| `current` ahead of `xl.meta` | `current` header says `v2`, but `xl.meta` still says latest is `v1` | Fast GET falls back to `xl.meta`; scanner/repair quarantines or removes uncommitted `v2` unless quorum metadata later confirms it. |
| `xl.meta` ahead of `current` | `xl.meta` says latest is `v2`, but `current` still contains `v1` | Fast GET detects header mismatch and falls back; repair moves/reconstructs `v2` into `current`. |
| old latest stranded in staging | after exchange, `.staging-new` contains `v1`, but `versions/v1` is missing | Recovery reads the staging header, verifies `v1` is committed in `xl.meta`, and renames it to `versions/v1`. |
| delete marker torn update | `xl.meta` latest is delete marker, but `current` still contains old data | Fast GET must not serve old data unless `current` headers form quorum for the committed latest; fallback returns 404 and repair installs the delete-marker current state. |
| partial per-disk commit | some disks have `current=v2`, some `current=v1`, some staging leftovers | Landing node groups headers by signature; serve only a committed quorum, otherwise fall back and queue per-disk repair. |
| missing fsync after rename/exchange | syscall returned success, but crash loses a directory entry | Follow strict fsync ordering for files and parent directories; recovery treats missing direct paths as repairable if `xl.meta` has quorum. |
| stale or orphan direct dirs | directory contains a version not present in `xl.meta` | Scanner quarantines or deletes after confirming it is not referenced by canonical metadata. |

### 5.2 Request-time repair trigger

The fast GET path can detect direct-path divergence cheaply from headers:

1. The landing node asks all disks for `current/part.N` or `versions/<id>/part.N`.
2. It groups returned headers by version/signature.
3. It reads from the quorum group if the group is valid for the request.
4. Disks whose headers are missing, corrupt, or outside the quorum group are
   excluded from the read.
5. The landing node queues a repair for those disk/object paths.

The landing node should not tell a disk "your version is wrong" based only on the
header mismatch. It should tell the disk to reconcile that object against
canonical `xl.meta`. The disk-local repair then decides whether its direct path is
ahead, behind, stranded in staging, or uncommitted.

### 5.3 Disk-local reconciliation algorithm

Per-disk repair extends the existing healing/scanner model:

1. Read local `xl.meta`.
2. Build the expected direct-path map:
   `current` -> latest committed object version or delete-marker marker, and
   `versions/<id>` -> each committed non-current object version.
3. Inspect `current`, `versions/*`, and staging directories by reading their
   checked headers.
4. Keep directories whose header matches the version expected at that path.
5. If a staging/current/version directory contains a committed version expected
   elsewhere, rename it into the expected path.
6. If a directory contains an uncommitted version not present in `xl.meta`,
   quarantine/delete it.
7. If committed shard data is missing or corrupt but recoverable from other disks,
   invoke normal erasure healing to reconstruct it.

This is the same authority model Buckit uses today: metadata decides what should
exist; shard directories are repaired or cleaned up to match it.

---

## 6. Expected performance

For a healthy 16-disk EC:4 set serving large shard-backed latest GETs, the current
path is roughly:

```
16 xl.meta reads + 12 shard reads
```

The fast path is roughly:

```
16 current header reads, then M shard streams from the same opened files
```

On HDD, the optimistic seek-bound ceiling is about:

```
28 / 16 ~= 1.75x
```

In practice, expected gains are lower:

- seek-bound small/medium shard-backed GET throughput: up to about 1.5x-1.75x;
- first-byte latency on HDD: roughly 30%-50% lower in the healthy fast path;
- large sequential transfers: usually 0%-10% total transfer improvement, mostly
  faster first byte;
- flash/NVMe: mostly one fewer storage request phase, often a smaller latency win.

These are estimates, not measured results. The win shrinks or disappears when the
request falls back to `xl.meta`, targets old objects that have not been migrated
to direct paths, targets transitioned objects, needs multipart/range behavior not
covered by the fast path, or runs from warm metadata cache where the `xl.meta`
seek is already cheap.

---

## 7. Fallback and repair rules

Fast GET must fall back to the current implementation when:

- the direct path (`current` or `versions/<versionId>`) is missing on too many
  disks;
- headers do not form quorum on version/signature;
- a header checksum fails;
- the current entry is ambiguous relative to delete-marker semantics;
- requested range maps to a `part.N` not supported by the fast path;
- bitrot verification fails and quorum cannot be reconstructed from available
  fast-path shards;
- the object is transitioned, restored, or otherwise not local shard-backed data.

After fallback succeeds through `xl.meta`, the system should queue a repair to
finish any interrupted rename/exchange, reconstruct missing shards from erasure
quorum, or delete uncommitted direct data.

---

## 8. Open decisions

1. **Direct-path layout:** `current/part.N` for the latest version plus
   `versions/<versionId>/part.N` for non-current versions, stable regular files,
   or another no-duplicate path scheme.
2. **Current update protocol:** `renameat2(RENAME_EXCHANGE)` vs. staged
   rename/recovery fallback for filesystems without atomic directory exchange.
3. **Delete-marker representation:** marker file, empty current shard header, or
   separate current state file.
4. **Multipart/range scope:** whether v1 supports only single-part `part.1` reads
   or duplicates headers into every direct-path `part.N`.
5. **Transport:** header-pause control protocol vs. speculative stream/cancel.
6. **Migration:** old objects simply miss direct paths and use `xl.meta`; scanner
   or write/read repair can migrate them opportunistically.

---

## 9. References

- `docs/request-flow.md` - current GET/PUT/DELETE flows, `xl.meta` format,
  quorum, and local-vs-distributed transport split.
- `cmd/erasure-object.go` - `GetObjectNInfo`, `getObjectFileInfo`, and the decode
  loop.
- `cmd/erasure-decode.go` - M-reader decode behavior and degraded fallback.
- `cmd/xl-storage.go` - `ReadXL`, `ReadVersion`, inline-data behavior, and
  `ReadFileStream`.
- `cmd/storage-interface.go` - storage API signatures for metadata and file
  reads.
