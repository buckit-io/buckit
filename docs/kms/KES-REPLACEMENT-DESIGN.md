# KES Replacement — Design & Cost Analysis

**Status:** Draft / discussion notes

**Summary:** MinIO KES is deprecated, and its successors are proprietary. This
document proposes an open-source replacement. Buckit encrypts objects using a small
ladder of derived keys, and a separate stateless proxy holds the cloud credentials so
they never live inside Buckit. The **master key never leaves the KMS** — unlike KES
and MinKMS, which load it into memory — so no single compromise can ever expose it.
The result is near-zero KMS cost, fast performance, and a bounded blast radius if any
working key is exposed.

---

## 1. Background

### 1.1 Why we need a replacement

MinIO KES was archived in June 2025 and is no longer maintained. The replacements
MinIO points to — Enterprise KES and MinKMS — are **proprietary**, so they are not
options for an open-source Buckit. KES still works in Buckit today, but relying on
unmaintained software long-term is not viable. We need our own approach.

### 1.2 How encryption works in Buckit today

Buckit uses **envelope encryption**, which keeps the KMS out of the data path:

- A **master key** lives in the KMS and never leaves it. Its only job is to wrap and
  unwrap the smaller keys below it.
- For each object, the key service produces a small **data key**. It comes in two
  forms: a plaintext copy, used immediately to encrypt, and a sealed copy, wrapped by
  the master key and safe to store on disk next to the object.
- The plaintext data key derives a unique **object key** that actually encrypts the
  object's bytes.

So the KMS only ever handles tiny keys, never object data, and all the per-object key
material is stored with the object. The KMS holds nothing but the master key.

### 1.3 Buckit does not cache keys today

Every encrypted upload asks the key service (KES) for a fresh data key, and every
download asks it to unwrap one. Buckit itself caches nothing. KES answers these
requests locally from the master key it already holds, so it rarely calls the cloud
KMS — and that local caching is what makes encryption cheap today. Removing KES
removes the only cache in the chain, so the replacement has to provide one.

---

## 2. The cost problem

Without a cache, the KMS is contacted once per object operation, so the bill grows
with traffic. Using AWS KMS pricing ($1 per key per month, $3 per million requests):

| Object ops/sec | KMS requests/month | Monthly cost |
|---|---|---|
| 100 | ~260 M | ~$780 |
| 1,000 | ~2.6 B | **~$7,800** |
| 10,000 | ~26 B | **~$78,000** |

At higher rates you also hit AWS KMS rate limits and get throttled.

A cache changes this completely: the KMS is contacted per *time window* instead of
per *object*, dropping the bill to a few dollars a month. This caching was the entire
value of KES, and any replacement must keep it.

---

## 3. Recommended design

The design has two parts:

| Part | Lives in | Job |
|---|---|---|
| **Key cache** | Buckit | derive object keys locally, avoiding a network call per object |
| **KMS proxy** | a small long-running service | hold the cloud credentials and the key history, so Buckit holds neither |

### 3.1 A ladder of derived keys

Buckit encrypts each object using keys built from a short chain. The crucial property
is that **the master key never leaves the KMS** — not even the proxy ever holds it.
The KMS is asked once per time window to produce an *epoch key*; everything below the
epoch key is then derived locally with HKDF, which is a fast CPU operation that costs
nothing.

```
Master key   (inside the KMS / HSM; never exported)
   │  the KMS produces one epoch key per window  ← the only operation needing the KMS
   ▼
Epoch key    (one per time window, e.g. every 12 hours; cached in the proxy)
   │  derive locally:  HKDF(epoch key, "tenant + bucket")
   ▼
Bucket key   (one per tenant + bucket)
   │  wraps
   ▼
Data key     (random, one per object)
   │  derives
   ▼
Object key   (encrypts the object's bytes with AES-256-GCM)
```

Each layer has a clear purpose:

- **The epoch key** is produced by the KMS once per time window and reused for many
  objects. This is the only step that touches the KMS, and it happens per window — not
  per object — which is what makes the design cheap. See "Epoch-key derivation" below
  for exactly how the KMS produces it; the default is a deterministic MAC so the proxy
  can recover any past epoch key with one KMS call and stores nothing.
- **The bucket key** is derived from the epoch key for a specific tenant and bucket.
  This is the safety boundary: if a bucket key is ever exposed, it can decrypt only
  that one bucket for that one window — not other buckets, tenants, or other windows.
  Always include the tenant and bucket names in the derivation, even though Buckit
  runs one server per tenant today, so any future shared deployment is isolated from
  the start.
- **The data key** stays random per object, exactly as today. Random keys avoid the
  pitfalls of deterministic derivation and match standard envelope encryption.

**Why keep the master key in the KMS.** KES and the proprietary MinKMS both load the
master key into the key service's *memory* and do the crypto in software. That is
cheaper but means a compromise of that service exposes the master key — and with it,
the ability to derive every key, past and future, forever. Keeping the master inside
the KMS/HSM removes that single point of total failure: an attacker who compromises
the proxy gets only the epoch keys currently cached, never the master. The cost of
this is one KMS call per window (a few dollars a month — see §4), which is well worth
it. A real HSM (via PKCS#11, CloudHSM, or similar) can anchor the master key for
hardware-grade protection; a software KMS is the cheaper option.

**Epoch-key derivation (must be pinned before implementation).** "The KMS produces
the epoch key" needs a precise scheme, because standard cloud-KMS APIs are *not*
deterministic KDFs by default — `GenerateDataKey` returns a **random** key, and using
it naively would make epoch keys unrecoverable. Pick one of these two:

- **Default — deterministic MAC.** Use a KMS-backed HMAC operation:
  `epochKey = KMS_MAC(masterKey, "cluster=<id>,epoch=<window>")` (for example AWS KMS
  `GenerateMac` with an HMAC key, or the equivalent on other backends). The master key
  never leaves the KMS, and the same call always returns the same epoch key, so any
  past epoch is re-derivable on demand and **nothing is stored**.
- **Alternative — random, wrapped.** Generate a random epoch key once and store it
  wrapped under the master key:
  `wrappedEpochKey = KMS_Encrypt(masterKey, random())`. Recover it later with
  `KMS_Decrypt`. Simpler to reason about and works on any KMS, at the cost of storing
  one small wrapped record per epoch.

The two are interchangeable for the rest of the design; the deterministic MAC is
preferred because it stores nothing. Whichever is chosen, **it must be fixed in the
spec** so every implementation derives identical, recoverable epoch keys.

**Where each key is generated and stored.** Only two things are durable: the master
key, inside the KMS, and each object's wrapped data key, in that object's metadata.
Everything else is produced on demand and held only in memory.

| Key | Produced where | Stored where | Lifetime |
|---|---|---|---|
| Master key | inside the KMS | **KMS / HSM** (never exported) | permanent |
| Epoch key | by the KMS, from the master key + window | not stored — cached in proxy memory | one window |
| Bucket key | in the **proxy**, derived from the epoch key | not stored — cached in Buckit memory | one window |
| Data key | in **Buckit**, random per object | **wrapped, in the object's metadata** | life of the object |
| Object key | in **Buckit**, derived from the data key | not stored — re-derived each time | per request |

Because the epoch key is tied to the window number (recorded in each object's
metadata), any past key can be recovered on demand — re-derived from the master key
with the deterministic-MAC scheme, or unwrapped from its stored copy with the
random-and-wrap scheme (see "Epoch-key derivation" above). Switching KMS backends
moves only the master key.

**Rotation only affects new uploads.** The epoch window controls how often new
uploads start using a fresh epoch key. Reads are unaffected: each object records its
window in metadata and always re-derives the same key, so rotating forward never
makes old objects unreadable.

**Two separate windows — don't conflate them.** There are two independent knobs, and
keeping them separate is what makes the design both cheap and safe:

- **The epoch window** (how often the KMS mints a new epoch key) should be **long —
  a default of 12 hours**. Longer is better here: it means fewer KMS calls and far
  fewer historical epoch keys to track (12-hour epochs are ~7,300 over a decade,
  versus ~350,000 at 15 minutes). Cost barely moves with this knob, so optimize it
  for simplicity.
- **The Buckit cache lifetime** (how long a usable plaintext key lingers in a storage
  node) should be **short — a default of ~15 minutes, idle/sliding**. This is the
  real exposure window: it bounds how long a key sits in the data plane where a memory
  dump could catch it. An actively-used bucket keeps resetting the timer and stays
  cached; an idle bucket drops its key quickly.

So a 12-hour epoch does **not** mean keys live in Buckit for 12 hours — Buckit evicts
its copy after ~15 idle minutes and re-fetches from the proxy if the bucket is used
again, even though the epoch itself is still current.

This is a contained change. Buckit's crypto code already separates *where a key comes
from* from *how it seals an object*, so the bucket key simply becomes a new source
feeding the existing machinery. The work adds the derivation, a tenant/bucket/window
field to object metadata, and the cache. The decrypt path — re-deriving the right key
for old objects — is the part to build and test most carefully, since a mistake there
means data loss.

### 3.2 The proxy and its cache

The proxy's job is to hold the cloud credentials and call the KMS for Buckit, so
**Buckit holds no cloud credentials**. A long-running container — AWS Fargate is a
good fit — is better here than a Lambda function: no cold-start delay, flat cost, and
it can hold a large cache of its own.

That gives a **two-level cache**:

| Level | Where | Holds | Lifetime |
|---|---|---|---|
| Near cache | Buckit | bucket keys used recently (well under a megabyte) | ~15 min idle (§3.1) |
| Far cache | proxy | epoch keys currently in use (a handful at 12-hour epochs) | the epoch window |

A request flows like this:

```
upload / download
   │
   ├─ found in Buckit's cache   → encrypt/decrypt locally   → no network at all
   ├─ not in Buckit, ask proxy  → proxy derives bucket key   → one fast hop, no KMS
   └─ proxy lacks the epoch     → proxy asks the KMS          → one KMS call, then cached
```

Buckit's cache keeps the common case entirely local — no network, no latency. The
proxy's cache keeps cold reads fast: a read of older data is served from the proxy's
memory without touching the KMS. With 12-hour epochs there are very few epoch keys to
hold (a few thousand over a decade, all tiny), so after warm-up the proxy rarely has
to call the KMS at all — and when it does, the epoch key is recovered deterministically
from the master key, so nothing is ever stored.

**Only the bucket key is cached.** Buckit caches the bucket key because it unwraps
*every* object in that bucket. The per-object data key and object key are **not**
cached — each object's wrapped data key is read from its metadata and unwrapped with
the cached bucket key on every request, and the object key is derived from it. So a
single cached bucket key serves all of a bucket's objects locally, and the cache holds
a small number of bucket keys rather than one entry per object.

**The proxy derives the bucket keys.** Buckit never receives the epoch key — it asks
the proxy for a specific bucket's key, and the proxy derives and returns only that.
This keeps the blast radius small: if a Buckit node is compromised, only the buckets
that node actually served are exposed, never the epoch key or other buckets. The cost
is one proxy request per bucket per window instead of one per window, but these are
cheap local derivations on the proxy with no extra KMS cost, and Buckit caches each
bucket key after the first use.

**How Buckit proves who it is.** Buckit signs a short-lived token with its own
private key, and the proxy checks it with the matching public key before returning a
key. This is the recommended method because Buckit often runs outside AWS, where
using AWS's own authentication would need extra setup, while a signed token needs
nothing AWS-specific. (Mutual TLS or AWS IAM are alternatives when Buckit runs inside
AWS.) Buckit holds no cloud credentials, so there is nothing to rotate on its side;
its only secret is its own signing key.

**Hiding cold-read latency.** When a read does need the proxy, that request can run
**at the same time** as fetching the object's data from disk — they don't depend on
each other, and only the final decrypt step needs both. This hides the proxy round
trip behind the storage read that was happening anyway.

**Running multiple proxy instances.** The proxy scales horizontally with no
coordination, because key production is deterministic: every instance derives the
same epoch and bucket keys from the same master key and inputs, so any request can go
to any instance behind a load balancer. There is no shared key store and no leader.
Each instance simply warms its own small cache independently. The only state that
*must* be shared across instances is **security** state, not keys — the token
verification material and, in phase 2, the revocation list and current-epoch number.
Plan a small shared config source for those; the keys themselves need none.

---

## 4. Cost

The KMS is contacted only to produce an epoch key — once per epoch window, shared
across the whole proxy — so the cost depends only on the epoch window, **not on the
number of objects, buckets, or writers**. Everything below the epoch key is derived
locally for free.

At a **12-hour epoch**, that is about **60 KMS calls a month**, which is free under
the AWS KMS free tier. Cold reads of old data add one KMS call per historical epoch
they touch — and there are very few epochs (a few thousand over a decade), each
recovered with one call and then cached. So ongoing KMS cost is essentially just the
**$1/month** to store the master key.

| Epoch window | KMS calls/month (active key) | Monthly cost |
|---|---|---|
| 12 hours (default) | ~60 | ~$1 (storage only) |
| 1 hour | ~720 | ~$1 |
| 15 minutes | ~2,900 | ~$1 |

Compared with the ~$7,800/month an uncached deployment would pay at 1,000 ops/sec,
KMS cost effectively disappears. (The proxy is a small always-on container; its cost
is compute, not KMS.)

---

## 5. Security

**What the design achieves:**

- **The master key never leaves the KMS.** This is the central win. KES and MinKMS
  load the master key into the key service's memory, so compromising that service
  exposes the master — and the power to derive every key, past and future, forever.
  Here the master stays in the KMS (optionally a hardware HSM), so no compromise of
  the proxy or Buckit can ever yield it. A breach is bounded, never total.
- **Cloud credentials never enter Buckit**, so compromising a storage node cannot
  steal them.
- **Keys are scoped per tenant and bucket**, so a leaked working key can decrypt only
  that bucket for one window — not the deployment.
- No proprietary dependency.

**Tamper-proofing object metadata.** Each object's sealed key must be tied to the
object's identity so its metadata cannot be altered or rolled back. Buckit already
binds the sealed key to the bucket and object path; extend this to also cover the
tenant, the object version, and the key's window. This stops an attacker from
swapping key versions or replaying an old wrapped key to force a decrypt.

**The bounded worst case.** Working keys still pass through Buckit and the proxy, so a
compromise of either is not nothing — but it is *bounded*. A compromised Buckit node
can use the bucket keys in its cache (only for buckets it serves) and ask the proxy
for more, which the proxy can rate-limit and audit. A compromised proxy exposes the
epoch keys it currently holds. In **neither case does the attacker get the master
key** — so they cannot derive arbitrary past or future keys, and rotating the epoch
locks them out going forward. This is exactly the property the in-memory-master
designs (KES, MinKMS) give up.

**Accepted residual risk.** The design deliberately accepts one exposure: a leaked
*bucket key* can decrypt that one bucket for that one epoch window. This is a
conscious trade for the local-cache performance win, and it is dramatically smaller
than the alternative it replaces (a single key that unlocks the whole deployment).
Shortening the epoch window or the cache lifetime narrows it further.

**Memory hygiene.** Buckit does not currently wipe plaintext keys from memory after
use. Pair the short cache lifetime (§3.1) with a bounded cache size, zeroization on
eviction, memory locking, and disabled core dumps — partial but worthwhile
protections in a garbage-collected language.

---

## 6. Requirements and phasing

A few security properties are cheap to build in from the start and close sharp risks,
so they belong in the **MVP**, not in a later hardening pass. The rest is genuine
additional infrastructure that can follow.

### MVP requirements (build from the start)

- **Pinned epoch-key derivation** — the exact scheme from §3.1 (deterministic MAC, or
  random-and-wrap). This is a correctness requirement, not hardening: get it wrong and
  data becomes unrecoverable.
- **Authenticated metadata binding** — bind the object's identity into the encryption
  as AEAD additional data: tenant, bucket, object name, object version, epoch, and
  algorithm. Buckit already binds the bucket/object path, so this is a small extension,
  and it blocks metadata tampering, rollback, and epoch-substitution attacks.
- **Basic token scoping** — each token names the node and the tenant/bucket(s) it may
  request keys for, with a short lifetime, so a stolen token cannot become a
  decrypt-anything token. (Full replay/nonce infrastructure is phase 2.)
- **A minimal revocation path** — short cache lifetimes (a compromised node loses its
  keys within ~15 minutes) plus the ability to rotate the proxy's token-signing key to
  cut off a leaked node immediately. (A coordinated deny-list and cluster-wide cache
  flush are phase 2.)
- **Bounded historical cache** — cap the proxy's retained epoch keys by count and TTL,
  rather than retaining all history indefinitely.

### Open decisions

1. **How Buckit authenticates to the proxy** — signed token (recommended), mutual
   TLS, or AWS IAM.
2. **Epoch-key derivation scheme** — deterministic MAC (preferred, stores nothing) or
   random-and-wrap (§3.1).
3. **Epoch window and cache lifetime** — defaults of 12 hours and ~15 minutes (§3.1);
   tune the cache lifetime down for higher-isolation deployments.
4. **KMS backends and HSM anchoring** — AWS KMS first; Vault, GCP, Azure, or
   PKCS#11/CloudHSM later.

**Suggested order:** build the key ladder and cache (with the MVP requirements above)
first, then add the proxy to take credentials out of Buckit, then the phase-2
hardening.

### Phase-2 hardening

Real infrastructure that can follow the MVP:

- **Full token replay protection** — nonce/`jti` tracking and replay detection, with
  mutual TLS underneath the token.
- **Coordinated revocation** — a node deny-list and cluster-wide cache flush shared
  across proxy instances, beyond the minimal MVP kill switch.
- **The proxy owns epoch numbers** — nodes never invent their own window numbers; the
  proxy assigns them, avoiding clock-skew and partition bugs (also see multi-instance
  notes in §3.2).
- **Rate limiting and anomaly detection** at the proxy to bound mass-decrypt abuse.
- **Logging discipline** — never log keys, tokens, or wrapped keys; audit logs record
  only who did what, when, and with which key version.

---

## 7. Code references

For implementers, the relevant code:

- `internal/kms/config.go` — how Buckit connects to a KMS backend
- `internal/kms/conn.go` — the backend interface and the data-key type
- `internal/kms/kms.go` — the KMS API (`GenerateKey`, `Decrypt`)
- `internal/kms/secret-key.go` — in-process key derivation and sealing (reusable for
  the new key ladder)
- `internal/crypto/key.go` — object-key generation, sealing, and unsealing
- `internal/crypto/sse.go` — the library that encrypts object data
- `internal/crypto/metadata.go` — the per-object sealed-key metadata fields
- `cmd/encryption-v1.go` — the encrypt and decrypt paths to change (the two places
  that fetch keys today)
