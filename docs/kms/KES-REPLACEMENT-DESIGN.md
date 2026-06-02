# KES Replacement — Design & Cost Analysis

**Status:** Draft / discussion notes

**Summary:** MinIO KES is deprecated, and its successors are proprietary. This
document proposes an open-source replacement: Buckit caches a short-lived
**per-cluster encryption key** to keep costs low, and a small **stateless proxy**
holds the cloud KMS credentials so they never live inside Buckit.

---

## 1. Background

### 1.1 Why we need a replacement

MinIO KES was archived in June 2025 and is no longer maintained. The replacements
MinIO points to — Enterprise KES and MinKMS — are **proprietary**, so they are not
options for an open-source Buckit. KES still works in Buckit today, but building on
unmaintained software long-term is not viable. We need our own approach.

### 1.2 How encryption works in Buckit today

Buckit uses **envelope encryption**, which keeps the KMS out of the data path. It
works through a small hierarchy of keys:

- **Master key** — held by the key service (KES today). It is fetched once from
  the root KMS and reused; its only job is to wrap and unwrap the keys below it.
- **Data Encryption Key (DEK)** — a ~32-byte key the key service produces on
  request. Crucially, it is **generated locally** by whoever holds the master key —
  not fetched from the root KMS each time. It comes in two forms: a plaintext copy
  Buckit uses immediately, and a sealed copy (wrapped by the master key) that is
  safe to store on disk.
- **Object key** — derived inside Buckit from the DEK plus fresh randomness. This
  is what actually encrypts the object's bytes, and it is unique for every object.

```
Master key   (in KMS; fetched once, then cached and reused)
   │   wraps / unwraps DEKs locally — no root-KMS call per DEK
   ▼
DEK          (generated locally per request; can be cached)
   │   derives
   ▼
Object key   (unique per object; encrypts the data)
```

The important consequences:

- **The KMS only handles tiny keys, never object data.** All bulk encryption
  happens inside Buckit.
- **Per-object key material is stored with the object**, in its metadata. The KMS
  stores only the master key. This means switching KMS backends requires no
  per-object migration — only the master key moves.

### 1.3 Buckit does not cache keys today

Every encrypted upload asks the key service (KES) for a fresh DEK, and every
encrypted download asks it to unwrap one. Buckit itself caches nothing. KES answers
those requests locally using the master key it already holds, so it rarely needs to
call the root KMS — that local caching is what makes encryption affordable today.
When we remove KES, we remove the only cache in the chain, and the replacement has
to provide one.

---

## 2. The cost problem

Because the KMS is contacted once per object operation, the bill scales with
traffic when nothing caches. Using AWS KMS pricing ($1/key/month, $3 per million
requests):

| Object ops/sec | KMS requests/month | Monthly cost |
|---|---|---|
| 100 | ~260 M | ~$780 |
| 1,000 | ~2.6 B | **~$7,800** |
| 10,000 | ~26 B | **~$78,000** |

At higher rates you also hit AWS KMS rate limits and get throttled.

A cache changes the cost driver entirely: the KMS is contacted per *key per time
window* instead of per *object*, which drops the bill to a few dollars a month.
**This caching is the entire value KES provided, and any replacement must keep
it.**

---

## 3. Recommended design

The design has two parts, each placed where it naturally belongs:

| Part | Lives in | Job |
|---|---|---|
| **Hot key cache (L1)** | Buckit | avoid a network call on every object (the cost/latency win) |
| **Proxy + history cache (L2)** | long-running service (Fargate) | hold the cloud credentials, and cache all historical keys so cold reads stay fast |

**How this maps to KES.** It helps to see the design as redistributing the jobs KES
does today:

| KES job today | Moves to | Notes |
|---|---|---|
| Hold the cloud credentials and the master key | **Proxy (Fargate)** | fetches and caches the master key under its IAM role |
| Generate keys under the master key | **Proxy (Fargate)** | generation must happen where the master key lives |
| Per-object key derivation | **Buckit** | unchanged — Buckit already does this today |
| *(a reusable key cache)* | **Buckit (L1) + Fargate (L2)** | new — KES answers a fresh key per request and caches none |

The genuinely new piece is the **cache**. Today the Buckit↔KES call is local and
cheap, so KES never needed to cache — it just answers every request. In the new
design the Buckit↔proxy call is remote and crosses a trust boundary, so Buckit
caches keys (L1) and reuses them for many objects, making that call rare instead of
per-object; Fargate keeps a full history (L2) so even cold reads stay fast.

### 3.1 A cached per-cluster key

Instead of contacting the key service for every object, Buckit obtains one
**short-lived cluster key**, caches it, and derives every object's DEK from it
locally. The proxy and KMS are only contacted when that cluster key needs to be
created or, on reads, reconstructed. This is the same idea as AWS's S3 Bucket Keys —
but scoped to the whole cluster rather than per bucket.

**Why per cluster, not per bucket.** AWS scopes its bucket key to each bucket. For
Buckit that scaling is wrong: an on-prem deployment can have thousands of buckets,
and a per-bucket key would multiply both cost and key count by the number of
buckets. A single per-cluster key removes that multiplier and needs only one master
key. It also fits how Buckit does multi-tenancy — each tenant runs a **separate
Buckit server**, so a per-cluster key already isolates tenants from one another.
Separating users *within* a cluster is the access-control system's job, not the
encryption key's.

**Rotation happens only on writes.** The rotation window controls how often *new
uploads* begin using a fresh cluster key. Reads never rotate: an object is tied to
the key version recorded in its metadata, and a read only ever reconstructs that
version. Rotating the write key never makes old objects unreadable — it just
starts a new version going forward.

**Choosing the window.** A shorter window means the key spends less time in memory
but costs slightly more and creates more historical versions to reconstruct later.
A window of **5 minutes to 1 hour** is a sensible range; **15 minutes** is a good
default. Even aggressive 5-minute rotation costs only about $26/month at 1,000
writers, so cost rarely forces the decision — the exposure window does.

**The cache has two parts** (detailed in §3.2). Inside Buckit, one slot holds the
single active write key and a small bounded set of recently-used read keys. The
full history of older keys is kept by the proxy. A cold scan reconstructs one key
per *time window* of history it touches — not one per object — so even large scans
stay cheap.

**The KMS still stores only the master key.** Old cluster keys are never kept in
the KMS; they are wrapped and stored alongside the objects, exactly as DEKs are
today, and reconstructed from object metadata when needed.

This is a contained change. Buckit's encryption code already separates "where the
key comes from" from "how it seals an object," so the cluster key simply becomes a
new source feeding the existing sealing machinery. The work touches two call sites
(encrypt and decrypt), adds a key-version field to object metadata, and adds the
cache and configuration. The decrypt path — reconstructing the right historical key
— is the part to implement and test most carefully, because errors there mean data
loss or key reuse.

### 3.2 The proxy, and a two-tier cache

The proxy is a service whose job is to hold the cloud credentials (an IAM role) and
call the KMS on Buckit's behalf, so **Buckit holds no cloud credentials at all**. A
long-running container — **AWS Fargate** is a good fit — is preferable to a Lambda
function here: it has no cold-start latency, has flat predictable cost, and, most
usefully, it can hold a large in-memory cache of its own.

That leads to a **two-tier cache**, like a CPU cache hierarchy applied to keys:

| Tier | Where | Holds | Size |
|---|---|---|---|
| **L1** | Buckit | the active write key + recently-used read keys | sub-MB |
| **L2** | Fargate proxy | every cluster-key version ever generated | ~90 MB for 10 years |
| source | root KMS | only the master key | — |

```
upload / download
   │
   ├─ L1 hit (Buckit)      → derive DEK locally          → no network at all
   ├─ L1 miss → L2 hit     → Fargate returns the key     → one fast hop, no KMS
   └─ L1 & L2 miss (rare)  → Fargate reconstructs        → one KMS-ish op, then cached
```

Each tier earns its place:

- **L1 keeps the hot path local.** Common reads and writes never leave Buckit, so
  there is no per-object network call and no latency added.
- **L2 makes cold reads fast.** A read of old data whose key has aged out of L1 hits
  Fargate's warm in-memory map and gets the key back immediately — no root-KMS call,
  no cryptographic reconstruction. Because Fargate holds the full history, L2 misses
  effectively never happen after warm-up. (Sizing is a non-issue: keys are 32 bytes,
  so a decade of 15-minute rotation is ~350,000 keys ≈ 90 MB.)

This is strictly better than caching in only one place: keeping everything in Buckit
made cold reads slow, while keeping everything in Fargate would put a network hop on
*every* object. The two-tier split gives a local hot path **and** fast cold reads.

**How Buckit authenticates to the proxy.** The recommended method is a **signed
token (JWT)**: Buckit signs a short-lived token with its own private key, and the
proxy verifies it with the matching public key before serving a key. This is
preferred because Buckit often runs outside AWS, where AWS's own IAM authentication
would need extra machinery, whereas a signed token needs nothing AWS-specific on
Buckit's side. Use asymmetric keys so a leak of the proxy's configuration cannot
forge tokens, keep token lifetimes short, and have the proxy strictly validate
signature, expiry, and audience. (AWS IAM auth or mutual TLS also work if Buckit
runs inside AWS.)

**Latency on cold reads.** When a read does miss L1 and must ask the proxy, that
call can run **in parallel with fetching the object data** — the two are
independent and only the final decrypt step needs both. This hides the proxy
round-trip behind the storage read that was happening anyway.

**Credential rotation becomes a non-issue.** Buckit holds no cloud credentials, so
there is nothing to rotate there. The proxy's IAM role issues short-lived tokens
that the cloud rotates automatically. The only secret on Buckit's side is its own
token-signing key, which it rotates on its own schedule.

**One caveat to be clear about:** the two-tier cache is a latency-and-cost
optimization, not a security change. Because L1 still holds plaintext keys inside
Buckit, key material lives in the data plane — see §5.

---

## 4. Cost with this design

Because the key is per cluster, the KMS cost depends only on how many writers there
are and how often the key rotates — **not on how many objects or buckets exist**.
One shared master key costs $1/month, and the proxy itself is negligible.

Write-side cost at 1,000 concurrent writers:

| Rotation window | KMS requests/month | Monthly cost |
|---|---|---|
| 5 minutes | ~8.6 M | ~$26 |
| 15 minutes (default) | ~2.9 M | ~$8.6 |
| 1 hour | ~720 K | ~$2.2 |

Reads add cost only when they reconstruct an uncached historical key, and that
depends on how much *history* a read sweeps, not how many objects. A full scan of a
year's data reconstructs a few thousand keys at most — a few cents, once, then
cached.

Compared with the ~$7,800/month an uncached deployment would pay at 1,000 ops/sec,
the cache turns KMS cost into a rounding error.

---

## 5. Security

**What the design achieves:**

- Cloud credentials never enter Buckit, so a compromise of Buckit cannot steal
  them. This is the property KES gave us, now from components we own.
- Cost stays low because the cache lives in the long-running server.
- No proprietary dependency.
- The proxy is a natural place to scope permissions, rate-limit, and audit every
  KMS call.

**The limit to be honest about.** This design isolates *credentials*, not *key
material*. A compromised Buckit can still read whatever keys are currently in its
cache, and can still ask the proxy to decrypt anything the proxy's role allows. So
the worst case shrinks from "attacker gains permanent cloud KMS access" to
"attacker abuses a scoped, audited, rate-limited service while they remain inside
Buckit" — a large improvement, but not immunity.

If a deployment requires that key material **never** reside in Buckit at all, this
design is not enough. That stricter goal requires a separate key-broker process
that holds the cache itself — essentially maintaining an open-source KES — with the
ongoing cost of owning that project.

One concrete hardening note: Buckit does not currently wipe plaintext keys from
memory after use. With a cache holding keys longer, that becomes worth addressing
through short cache lifetimes, memory locking, or explicit zeroization.

---

## 6. Open decisions

1. **Proxy auth** — signed token (recommended), AWS IAM, or mutual TLS?
2. **Rotation window** — the trade-off between cost and how long a key stays in
   memory.
3. **Is an in-Buckit (L1) cache acceptable?** It puts plaintext keys in the data
   plane. If full key-material isolation is mandatory, the L1 cache must be dropped
   — which means a network call per object and pushes this toward a separate
   key-broker design.
4. **Backend scope** — AWS only at first, or also Vault, GCP, and Azure later?

**Suggested order:** build the cached per-cluster key first (it stands alone if you
accept Buckit holding scoped KMS credentials), then add the proxy to remove those
credentials from Buckit.

---

## 7. Code references

For implementers, the relevant code:

- `internal/kms/config.go` — how Buckit connects to a KMS backend
- `internal/kms/conn.go` — the backend interface and the `DEK` type
- `internal/kms/kms.go` — the KMS API (`GenerateKey`, `Decrypt`)
- `internal/kms/secret-key.go` — in-process key derivation and sealing (reusable
  for the cluster-key logic)
- `internal/crypto/key.go` — object-key generation, sealing, and unsealing
- `internal/crypto/sse.go` — the `sio`/DARE library that encrypts object data
- `internal/crypto/metadata.go` — the per-object sealed-key metadata fields
- `cmd/encryption-v1.go` — the encrypt and decrypt paths to branch (the two call
  sites that fetch keys today)
