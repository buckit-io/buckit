# Buckit Manager — UI Architecture

This document captures the cross-cutting implementation details for the
Buckit Manager web UI: how the tool runs on a developer's workstation,
where data comes from, how the browser talks to the local `bm` process,
and how `bm` talks to the remote Buckit clusters the operator manages.

It complements the wireframe-level Phase 1 UI spec in
[`phase1-web-ui.md`](./phase1-web-ui.md) and the implementation plan in
[`phase1-implementation.md`](./phase1-implementation.md).

Per-page details are kept in a single document so the cross-cutting
patterns (data flow, refresh model, History) are defined once and reused.
Pages are listed in priority order; new pages get appended under
[Per-page details](#per-page-details) as they are designed.

## Positioning

`bm` is a **personal desktop tool**, not a centralized cluster
management service. It runs on the operator's Mac or Windows machine,
the same way `mc`, `docker`, `gh`, or `jupyter notebook` do. There is
no shared multi-user backend; each operator runs their own `bm` against
their own copy of local state.

The web UI exists to make install, deploy, and migration workflows
ergonomic — those are too complex for a CLI alone. Day-to-day cluster
operations remain available through the CLI, mirroring `mc`.

| `bm` is like | `bm` is **not** like |
|---|---|
| `gh` (GitHub CLI + local helpers) | Rancher / Portainer / ArgoCD |
| `docker` (CLI + local daemon if any) | A SaaS cluster console |
| `jupyter notebook` (foreground process serving a local web UI) | A Kubernetes Operator running in-cluster |
| `mc` (the tool it intends to replace, plus a UI) | A CMDB or compliance system |

This framing rules out a number of designs that would otherwise be
defensible (multi-tenant auth, RBAC, audit retention policies,
high-availability backends) and rules in a number of others (per-user
config, opinionated defaults, optional remote-access mode).

## Audience

Engineers building the `bm` binary (Go backend or React frontend).
Operators using `bm` should look at the README and the Phase 1 web UI
spec instead.

## Process model

```
Operator's terminal:
$ bm web
   ⇒ binds 127.0.0.1:9443 by default
   ⇒ opens default browser to http://localhost:9443
   ⇒ runs in the foreground; logs to stdout
   ⇒ Ctrl-C exits (with a confirm prompt if any task is in flight)

Operator's browser:
   ⇒ talks only to the local bm process

Closing the browser tab does NOT stop the process.
Killing the terminal DOES stop the process and any in-flight task.
```

`bm web` is a foreground terminal process that hosts a local HTTP server
on `127.0.0.1`. It is not a system service. There is no `systemctl`,
no daemonization, no PID file management. Operators start it like
`jupyter notebook` and stop it with Ctrl-C.

`bm web` also schedules an automatic browser open on launch (suppressible
with `--no-browser`), and prints the URL + a one-line status to stdout.

The CLI side (`bm cluster ls`, `bm cluster restart`, etc.) is a separate
invocation of the same binary. It opens the same local bbolt file as
`bm web`. Coordination is by short-lived bbolt locks: each CLI call and
each `bm web` request opens the file briefly, runs its transaction, and
closes. Two `bm` processes can coexist; brief lock contention is the
worst case.

### Optional remote access

The operator can publish their `bm` to other devices on their network
(a tablet next to the laptop, a teammate occasionally borrowing the UI).
This is opt-in and gated by the operator's local **Manager Settings →
Remote access** screen. Enabling it requires:

- A bind address other than `127.0.0.1` (e.g. `0.0.0.0`, or a specific
  LAN address).
- A passcode of at least 8 characters.
- A TLS certificate. By default `bm` generates a self-signed cert and
  key on enable, written into the data directory; the operator can
  replace either with a custom cert/key path.

While remote access is on:

- The web UI requires the passcode on first request from a session
  (cookie set, time-limited).
- The CLI is unaffected: it still reaches the local socket without auth
  because it's the same OS user on the same machine.
- Anyone on the network can reach the URL, so the passcode + TLS pair
  is the only thing protecting state.

Disabling remote access (the default) returns the listener to
`127.0.0.1` and clears the passcode + sessions.

This is the only place auth or TLS appears in the design. In the
default localhost-only mode, the OS account is the trust boundary, the
same as `mc`'s `~/.mc/config.json` or `gh`'s `~/.config/gh/hosts.yml`.

## Architecture

```
                Operator's browser              Operator's terminal
                (localhost only by default)
                       │                                │
                       │  HTTP / 127.0.0.1:9443         │  bm cluster restart …
                       │  (HTTPS + passcode if          │
                       │   remote access enabled)       │
                       ▼                                ▼
            ┌──────────────────────────────────────────────────────┐
            │                  bm (this machine)                   │
            │                                                      │
            │   ┌────────────────────────────────────────────────┐ │
            │   │  HTTP API (chi)                                │ │
            │   │  /api/v1/clusters       (read bbolt)           │ │
            │   │  /api/v1/clusters/refresh (sync re-fetch)      │ │
            │   │  /api/v1/tasks/:id/events (SSE for in-flight)  │ │
            │   │  /api/v1/history        (CLI + UI history)     │ │
            │   └──────────┬─────────────────────────────────────┘ │
            │              │                                        │
            │     ┌────────▼─────────┐    ┌────────────────────┐   │
            │     │  bbolt           │    │  task engine       │   │
            │     │  ~/.config/bm/   │◀──▶│  in-process        │   │
            │     │    bm.db         │    │  log files on disk │   │
            │     │  clusters/       │    └─────────┬──────────┘   │
            │     │  node_facts/     │              │              │
            │     │  tasks/          │              │              │
            │     │  history/        │    ┌─────────▼──────────┐   │
            │     │  credentials/    │    │  shared core lib   │   │
            │     └─────────▲────────┘    │  (importable by    │   │
            │               │             │   API and CLI)     │   │
            │               └─────────────┴────────┬──────────┘    │
            └────────────────────────────────────┬─┴────────────────┘
                                                 │
                            HTTPS · admin creds (per cluster)
                            GET /minio/admin/v3/info
                            SSH · per-node facts
                                                 │
                                                 ▼
                              ┌────────────────────────────────────┐
                              │  Buckit server (per cluster)       │
                              │  - /minio/admin/v3/info            │
                              │  - /minio/health/live              │
                              │  - /minio/health/cluster           │
                              │  - SSH-exposed system facts        │
                              └────────────────────────────────────┘
```

### Trust boundary

There is exactly one: **the operator's OS account on this machine ↔
the remote Buckit clusters they have configured**. The boundary is
crossed by SSH (per-cluster keys/credentials) and by HTTPS to each
cluster's admin API (per-cluster root credentials).

Inside the machine, the browser-to-bm boundary is not a security
boundary in the default localhost mode — anyone with the OS account
can already read the bbolt file directly. Remote-access mode adds a
real boundary at the network listener, defended by the passcode and TLS.

## Data sources and caching

### One source of truth: bbolt

Every byte the UI renders is read from `~/.config/bm/bm.db`
(`%APPDATA%\bm\bm.db` on Windows). The browser does not call any
external admin endpoint. `bm` mediates all reachout to clusters because
the operator's machine may not be on the same network as its targets.

Bbolt buckets relevant to UI rendering:

| Bucket | Holds | Written by |
|---|---|---|
| `clusters` | One record per cluster: id, name, description, intended use, version pin, parity setting, lifecycle status, `lastFetchedAt`, `health`, `healthSummary`, `unreachableSince` | Wizard saves + `refresh` |
| `node_facts` | Per-node facts from the last fetch: OS, kernel, CPU, RAM, NIC, drives, drive states, sizes, used bytes, service unit state, existing services | `refresh` |
| `tasks` | Long-running operation records: kind, state, steps, durations, cluster id, retryable flag | Task engine |
| `history` | CLI command + UI action log. See [History](#cli--ui-history) | CLI dispatcher + API handlers |
| `credentials` | AES-GCM encrypted SSH and admin credentials | Wizard saves + rotation |
| `prefs` | App preferences: theme, default cluster, remote-access settings | Settings page |

Task **logs** never go in bbolt. They stream to flat files
(`~/.config/bm/tasks/<task-id>.log`) and are tailed for SSE.

### On-demand fetches, not a daemon loop

`bm` does **not** run a background goroutine continuously polling every
cluster. Instead:

1. The **first time** a cluster page or the cluster list is opened in a
   session, an API handler initiates a fetch of `/minio/admin/v3/info`
   plus SSH facts for that cluster, writes the result to bbolt, and
   returns it.
2. Subsequent renders within the next 30s read from bbolt without
   hitting the cluster (cache-fresh).
3. Older than 30s, the next render initiates a re-fetch in parallel
   with serving the cached data — the UI shows the old data instantly,
   then re-renders when the fresh data lands ("stale-while-revalidate").
4. The **Refresh button** on the Clusters list and Cluster Overview
   forces an immediate fetch and waits for the result.

This keeps the cost proportional to use: a developer who never opens
the UI never burns network or CPU on probes. When the UI is open, freshness
is bounded by the implicit 30s TTL plus the explicit Refresh affordance.

#### Health rule

```
draft cluster                                            → unknown
no nodes / never successfully fetched                    → unknown
two consecutive failed fetches                           → unknown
drive failures > parity per set OR
  offline nodes > parity                                 → critical
any node not fully online OR
  any drive not ready OR
  any long-running active op in flight                   → degraded
otherwise                                                → healthy
```

`activeOps` is sourced from the local `tasks` bucket (running tasks
with `kind ∈ {deploy, cutover, rolling_restart, rollback, finalize}`),
not from `/minio/admin/v3/info`. `health_probe` and equivalent kinds
are excluded as monitoring noise.

The reference implementation of `computeHealthSummary` and
`computeHealth` lives in `bm/web/src/mock/data.ts`; the real backend
ports the same logic to Go.

#### Failure handling

| Condition | Behaviour | UI display |
|---|---|---|
| Single failed fetch | Existing facts retained, `lastFetchedAt` unchanged, warning logged to stdout | Stale label ("Fetched 1m 30s ago") |
| Two consecutive failed fetches | `unreachableSince` set; `health = "unknown"` | Pill flips to Unknown; counts shown greyed |
| Reachable again | `unreachableSince` cleared; `health` recomputed | Updates on next refresh |
| Admin auth fails | Same as unreachable, error printed to stdout and surfaced on Cluster Settings | Pill shows Unknown; flag on credentials |
| Cluster lifecycle = `draft` | Fetch is skipped | Pill shows neutral `—`; counts shown `—` |
| Cluster lifecycle = `migrating` mid-cutover | Some nodes between binaries; partial facts retained | Pill stays Degraded; per-node table shows transient states |

### What a fetch actually does

Each fetch (whether triggered by opening a page or by Refresh) pulls from
**three independent sources** and merges them into one result. Failures
in any source are isolated — the Connectivity column on the cluster
page exists precisely so the operator can see which source failed for a
given node.

**Source A — Buckit admin API** (`GET /minio/admin/v3/info` on :9000):
one HTTPS call to any healthy node, authenticated with the cluster's
stored root credentials. Returns the bulk of cluster + per-node +
per-drive data in one round-trip. See [`metrics.md`](./metrics.md) for
the response shape; the same endpoint backs the per-cluster Buckit
console's Info tab.

**Source B — per-node connectivity probes**: four small attempts in
parallel against each node listed in source A:

- TCP dial to `:22` (or `:9000`) → `pingable`
- HTTP `GET :9000/minio/health/live` → `apiAccessible`
- HTTP `GET :9001/` → `consoleAccessible`
- SSH connect with stored credentials → `sshable`

These never block the admin-API path; if all four fail for a node we
keep its previous facts and mark only the probes as failed.

**Source C — SSH facts**: only attempted when `sshable=true`. Pulls
the small set of host-state signals admin API doesn't expose:

- `date +%s` → clock skew vs the operator's machine
- `systemctl is-active buckit` → service state cross-check

OS and kernel are **not** collected over SSH. They come from source A
once Buckit ships the field (see [Extending Buckit](#extending-buckit-to-return-os--kernel)
below); until then they show as `—` in the table. Keeping the
collection unified avoids a per-field fallback ladder and means SSH is
only load-bearing for the two small things it's actually unique at.

Phase 1 keeps SSH facts minimal. Wizards do deeper SSH probes during
discovery; the cluster-detail page only needs the lightweight set.

**Source-by-field map** for the cluster detail page:

| Field | A (admin API) | B (probes) | C (SSH) |
|---|:-:|:-:|:-:|
| `version`, `parity`, `nodeCount`, `poolCount` | ✓ | | |
| `usedBytes`, `rawBytes`, `usableBytes` | ✓ | | |
| Per-node `state`, `version`, `uptimeSec`, `pool` | ✓ | | |
| Per-node drives + states | ✓ | | |
| `pingable` | | ✓ | |
| `sshable` | | ✓ | |
| `apiAccessible` | | ✓ | |
| `consoleAccessible` | | ✓ | |
| `kernel`, `os` | ✓ (once Buckit ships it; see below) | | |
| `health`, `healthSummary` | computed from A + B + active-tasks bucket | | |

#### Pipeline

```
internal/app/refresh.go::RefreshCluster(ctx, clusterID)

  1. Load cluster record from bbolt
       hostnames + decrypted ssh creds + decrypted admin creds
       last-known node_facts (for fall-back on partial failure)

  2. Pick an admin entry-point
       try cluster.adminEndpoints in order until one responds

  3. ── A · GET https://<entry>:9000/minio/admin/v3/info ──▶  cluster + per-node bulk
       single HTTPS call, 5s timeout

  4. For each node returned by step 3, in parallel (worker pool ≤ 16):
       a. (B) TCP dial :22                               → pingable
       b. (B) HTTP GET :9000/minio/health/live           → apiAccessible
       c. (B) HTTP GET :9001/                            → consoleAccessible
       d. (B) SSH connect                                → sshable
       e. (C) if sshable: date +%s / is-active            → skew, svc

  5. Merge:
       - admin-API per-node row + connectivity + SSH facts
       - one bbolt write txn:
            clusters/<id>           (aggregate + health + lastFetchedAt)
            node_facts/<id>/<nodeId> (one row per node)

  6. Compute Cluster.health, Cluster.healthSummary

  7. Return updated cluster + nodes to the caller
       browser via the API handler that triggered the refresh, or
       CLI in-process if the operator ran `bm cluster ls --refresh`
```

Wall-clock budget on a healthy 10-node cluster: typically **300–600
ms** — admin call ~80 ms, then 10 nodes × max(TCP probe 50 ms, SSH
200 ms) in parallel.

#### Failure isolation

| Scenario | Behaviour |
|---|---|
| Admin API entry-point down | Rotate through `cluster.adminEndpoints`. If all fail, whole fetch fails: `unreachableSince` stamped, `health = "unknown"`. Per-node probes are skipped (no list to probe against). |
| Admin API ok, one node's probes time out | Keep that node's previous facts; new bbolt write zeroes only the probe fields for that node. The probe layer never blocks the admin-API path. |
| SSH down for a node, API up | Row shows ping ✓ / SSH ✗ / API ✓ / Console ✓ / kernel column = `—`. No retry within the same fetch. |
| Admin API ok but admin auth fails | Same as unreachable; logged to stdout and surfaced on Cluster Settings as a credential warning. |
| Cluster lifecycle = `draft` | Step 3 is skipped (not deployed yet). Only steps 1, 4a, 4d run. Page shows a "Cluster not yet deployed" state. |
| Mid-cutover (node briefly between binaries) | Admin API on the entry-point still works; that node's row shows API ✗ / Console ✗ until the buckit unit comes up. Cluster Health pill stays Degraded because of the active migration task. |
| Operator killed `bm` mid-fetch | The bbolt write in step 5 is one atomic transaction; partial state never lands. Next fetch starts cleanly from the previous successful state. |

#### Concurrency, timeouts, reuse

- **Per-probe timeouts:** TCP 2 s, HTTP 3 s, SSH 5 s, admin API 5 s.
- **Parallelism:** worker pool capped at 16 — keeps the operator's laptop sane on a 100-node cluster (6.25 batches × ~500 ms ≈ 3 s worst case).
- **SSH connection reuse:** Phase 1 doesn't pool SSH connections across fetches. A per-cluster SSH client cache with a 5-min idle timeout is a cheap follow-up if profiling shows it matters.
- **No background polling.** Fetches happen on page open or explicit Refresh. The operator's laptop never probes the cluster when the UI is closed.

#### Code layout (refers to the Phase 1 implementation plan)

```
bm/internal/app/refresh.go        orchestrates steps 1–7
bm/internal/cluster/minio.go      thin client around /minio/admin/v3/info
bm/internal/cluster/probe.go      TCP / HTTP probes (source B)
bm/internal/ssh/client.go         pooled SSH client per cluster
bm/internal/ssh/exec.go           Run / RunStream + sudo wrapping
bm/internal/store/clusters.go     bbolt typed accessors
bm/internal/store/nodes.go        bbolt typed accessors for node_facts
bm/internal/api/clusters.go       /api/v1/clusters/refresh handler
bm/internal/app/health.go         computeHealthSummary + computeHealth
                                  (Go port of the reference TS in
                                  bm/web/src/mock/data.ts)
```

The mocked `refreshAllClusters()` in `bm/web/src/mock/api.ts` is a
stand-in for what `internal/app/refresh.go` does in the real backend;
the React side stays identical.

#### Extending Buckit to return host info

Today, OS + kernel + host hardware facts aren't in `/admin/info` at
all. `madmin.ServerProperties` exposes some Go-runtime fields
(`NumCPU`, `GoMaxProcs`, `MemStats`) that are easy to misread as host
metrics — they describe the Buckit process, not the box it runs on.
We fix this by extending the per-server payload with a `Host`
substruct that carries the static + slow-changing host facts.

**Scope** — point-in-time, static-ish data only. Anything that needs
history or rate (CPU usage %, RAM usage over time, network bytes/s)
belongs in the metrics endpoint per [`metrics.md`](./metrics.md), not
here. `/admin/info` is the single-shot snapshot; the metrics endpoint
is the time-series source.

**Library** — `madmin.ServerProperties` (in
`github.com/minio/madmin-go/v3`) gains a `Host *HostInfo` field:

```go
type HostInfo struct {
    OS      *OSInfo    `json:"os,omitempty"`
    CPU     *CPUInfo   `json:"cpu,omitempty"`
    Memory  *MemInfo   `json:"memory,omitempty"`
    Network *NetInfo   `json:"network,omitempty"`
}

type OSInfo struct {
    Name    string `json:"name,omitempty"`     // "Ubuntu"
    Version string `json:"version,omitempty"`  // PRETTY_NAME from /etc/os-release
    Kernel  string `json:"kernel,omitempty"`   // uname -r
    Arch    string `json:"arch,omitempty"`     // runtime.GOARCH
}

type CPUInfo struct {
    Model    string `json:"model,omitempty"`     // "Intel Xeon Gold 6248R"
    Sockets  int    `json:"sockets,omitempty"`
    Cores    int    `json:"cores,omitempty"`     // physical
    Threads  int    `json:"threads,omitempty"`   // logical (SMT-aware)
    MaxMHz   int    `json:"maxMHz,omitempty"`
}

type MemInfo struct {
    TotalBytes uint64 `json:"totalBytes,omitempty"`
}

type NetInfo struct {
    Interfaces []NetInterface `json:"interfaces,omitempty"`
}

type NetInterface struct {
    Name      string `json:"name,omitempty"`
    SpeedMbps int    `json:"speedMbps,omitempty"`  // -1 if unknown
    State     string `json:"state,omitempty"`      // "online" / "offline"
}
```

Note: we deliberately omit `MemInfo.AvailableBytes`, CPU usage,
network throughput, and any other rate/utilization field. Those are
metrics-tab concerns; mixing a point-in-time sample in here would
mislead the operator.

The recommended dependency strategy is a Buckit-owned fork of
`madmin-go` referenced via `go.mod replace`:

```
replace github.com/minio/madmin-go/v3 => github.com/buckit-io/madmin-go/v3 v3.0.109-buckit.1
```

This keeps the import path unchanged in Buckit source while letting us
add Buckit-specific fields without depending on upstream MinIO accepting
the PR. Future Buckit-specific extensions (drive labels, custom probe
metadata) follow the same path.

**Server** — a one-shot `globalHostInfo = computeHostInfo()` runs at
boot. Sources per field:

| Field | Read from |
|---|---|
| `OS.Name`, `OS.Version` | `/etc/os-release` (`NAME`, `PRETTY_NAME`) |
| `OS.Kernel` | `syscall.Uname()` → `Release` |
| `OS.Arch` | `runtime.GOARCH` |
| `CPU.Model`, `CPU.MaxMHz` | `/proc/cpuinfo` (first physical CPU's `model name`, `cpu MHz`) |
| `CPU.Sockets` | distinct `physical id` count in `/proc/cpuinfo` |
| `CPU.Cores` | distinct `(physical id, core id)` pairs |
| `CPU.Threads` | `runtime.NumCPU()` (or count of `processor` lines) |
| `Memory.TotalBytes` | `/proc/meminfo` `MemTotal` |
| `Network.Interfaces[].Name`, `.State` | existing logic that populates `ServerProperties.Network` |
| `Network.Interfaces[].SpeedMbps` | `/sys/class/net/<name>/speed` |

Then `cmd/admin-server-info.go:96` adds one line to the existing
`madmin.ServerProperties{}` literal: `Host: globalHostInfo`. Cost: a
few /proc + /sys reads at boot, then nothing per request. Non-Linux
hosts (macOS / Windows) leave fields empty until platform-specific
probes are added; bm gracefully renders `—`.

**bm client** — `internal/cluster/minio.go` decodes the new field
automatically once it depends on the forked madmin. The merge in
`internal/app/refresh.go` step 5 reads host facts straight from admin
API:

```go
if h := adminInfo.Servers[i].Host; h != nil {
    if h.OS != nil {
        node.OS     = formatOS(h.OS)
        node.Kernel = h.OS.Kernel
    }
    if h.CPU != nil {
        node.CPUModel   = h.CPU.Model
        node.CPUCores   = h.CPU.Cores
        node.CPUThreads = h.CPU.Threads
        node.CPUMaxMHz  = h.CPU.MaxMHz
    }
    if h.Memory != nil {
        node.RAMBytes = h.Memory.TotalBytes
    }
    if h.Network != nil {
        node.NICs = h.Network.Interfaces
    }
}
```

If the field is empty (the operator is pointing bm at an older Buckit
build without the change), the affected columns show `—`. We don't try
to backfill via SSH — keeping each column wired to exactly one source
avoids a fallback ladder and keeps SSH load-bearing only for what it's
actually unique at (liveness, clock skew, service state). A short
upgrade window where some columns read `—` is a fair cost for a
simpler data path.

**What about the existing `ServerProperties` fields we keep?**
`NumCPU`, `MemStats`, `Network` (the flat `map[string]string`) stay
where they are for backward compatibility, but `bm` ignores them in
favour of the `Host` substruct. They describe the Buckit process, not
the host.

### What "Refresh" means

Refresh is a synchronous action, not a backgrounded task. The button
calls `POST /api/v1/clusters/refresh` (or
`POST /api/v1/clusters/:id/refresh` from the Overview page); the handler
fetches `/minio/admin/v3/info` for each non-draft cluster, writes results
to bbolt, and returns the updated list in the same response. The button
shows a spinner for the duration; on success the table re-renders.

There is no task created for Refresh — it's an interactive operation,
not a long-running orchestration. (Deploys, cutovers, and rolling
restarts are the things that need tasks.)

## REST API contract

All endpoints are JSON, mounted under `/api/v1/`, served on `127.0.0.1`
by default. Authentication and CSRF protection only apply when remote
access is enabled (see [Optional remote access](#optional-remote-access)).

### Clusters

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/clusters` | List clusters (cached from bbolt; auto-refresh if stale) |
| `POST` | `/clusters` | Create draft |
| `GET` | `/clusters/:id` | Detail |
| `PATCH` | `/clusters/:id` | Update draft fields |
| `DELETE` | `/clusters/:id` | Tear down (destructive; triggers a task) |
| `POST` | `/clusters/refresh` | Synchronously re-fetch all non-draft clusters |
| `POST` | `/clusters/:id/refresh` | Synchronously re-fetch a single cluster |

#### `GET /clusters` response

```jsonc
{
  "clusters": [
    {
      "id": "prod-east",
      "name": "prod-east",
      "description": "Customer-facing production",
      "intendedUse": "production",
      "version": "v1.0.0",
      "status": "active",
      "health": "degraded",
      "healthSummary": {
        "nodes":  { "online": 5, "degraded": 1, "offline": 0, "total": 6 },
        "drives": { "ready":  71, "healing":  1, "failed":  0, "total": 72 },
        "activeOps": []
      },
      "nodeCount": 6,
      "poolCount": 1,
      "driveCount": 72,
      "parity": 4,
      "usableBytes": 950737950310400,
      "rawBytes":    1267483933747200,
      "usedBytes":   452984832000000,
      "lastFetchedAt": "2026-05-14T19:01:32Z",
      "unreachableSince": null,
      "lastActivityAt": "2026-05-14T18:48:11Z",
      "createdAt": "2026-03-15T20:21:00Z"
      // migratedFrom: { product, version, finalizedAt } when applicable
    }
  ]
}
```

This is the bbolt-cached shape. Every field is computed by `bm` from
its last successful fetch.

### Nodes

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/clusters/:id/nodes` | List node facts for a cluster |
| `GET` | `/clusters/:id/nodes/:nodeId` | Node detail (the full `node_facts` record) |

### Tasks

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/tasks` | List, with `?clusterId=&state=` filters |
| `GET` | `/tasks/:id` | Detail (metadata + steps) |
| `GET` | `/tasks/:id/events` | **SSE** stream of progress + log lines |
| `POST` | `/tasks/:id/cancel` | Request cancel |
| `POST` | `/tasks/:id/pause` | Pause after current step |

Long-running orchestrations (wizard deploys, migration cutovers, rolling
restarts) return a task id. The UI never polls; it subscribes to
`/tasks/:id/events`.

### History

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/history` | List recent operations: `?kind=&clusterId=&since=&until=` |
| `DELETE` | `/history` | Clear history (with optional `?before=<ts>`) |

See [CLI + UI history](#cli--ui-history) for the row shape.

### Settings

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/settings` | App preferences + remote access state |
| `PATCH` | `/settings` | Theme, default cluster, remote access on/off, passcode, TLS paths |
| `GET` | `/clusters/:id/settings` | Per-cluster settings |
| `PATCH` | `/clusters/:id/settings` | SSH credentials, version pin |

### Errors

```jsonc
{
  "error": {
    "code": "cluster_unreachable",
    "message": "Could not reach any node in cluster prod-east",
    "details": { "clusterId": "prod-east", "lastError": "i/o timeout" }
  }
}
```

## CLI + UI history

`bm` keeps a running record of operations the operator has performed,
displayed in the **History** tab. The intent is closer to shell history
than to an audit log: a personal record the operator browses to recall
what they did, copy a previous CLI invocation, or jump to a related
task log.

### Sources

Three things contribute rows in the long term; only the third applies
in Phase 1.

| Source | Display | Phase |
|---|---|---|
| CLI command typed in a terminal | Verbatim command line, e.g. `bm cluster restart prod-east --concurrency sequential` | Phase 2 (when the write CLI lands) |
| CLI command typed inside a future web-UI terminal | Verbatim command line | Future phase |
| Web-UI button action | Short human-readable description, e.g. `Rolling restart on prod-east` | Phase 1 |

### Row shape

```ts
interface HistoryEntry {
  id: string                                  // ULID
  at: string                                  // wall-clock ISO timestamp
  kind: "cli" | "ui_action"                   // governs display + filter
  display: string                             // verbatim command OR description
  target?: string                             // cluster id, node id, etc.
  status: "succeeded" | "failed" | "running"
  durationSec?: number
  taskId?: string                             // jump-to-task link when applicable
  exitCode?: number                           // CLI only
}
```

### What is not History

- Read-only operations (`bm cluster ls`, browsing pages in the UI) are
  not recorded.
- Background fetches by the on-demand cache loop are not recorded.
- App lifecycle events (`bm web` started, settings changed) are not
  recorded — they go to stdout instead.

### Retention

Last 1000 entries by default; configurable in Manager Settings →
Preferences. Older rows are pruned in a sweep on next write. Manual
clear available via a button on the History page (with a confirm
modal).

## Frontend conventions

### Query layer

React Query owns the cache. Conventions:

- One query key per resource: `["clusters"]`, `["cluster", id]`,
  `["nodes", clusterId]`, `["task", id]`, `["tasks", { clusterId }]`,
  `["history", { kind, clusterId }]`.
- Mutations that change cluster state (`PATCH /clusters/:id`,
  `POST /clusters/.../refresh`, button-driven actions) invalidate the
  matching key(s) on success.
- Background polling is **off**. The Tasks page polls only while a task
  subscribed via SSE has not closed its event stream.
- `staleTime` is short (5s) so when the operator clicks Refresh the
  table re-renders even if the cached response would otherwise still be
  fresh.

### Staleness display

Anywhere the UI shows cached state (Clusters list, Cluster Overview,
Node Detail), it must show **when that state was last fetched** in a
subtle line near the header. Format:

```
Fetched 14s ago   ↻ Refresh
Fetched 2m 18s ago ↻ Refresh
```

There is no warning threshold colour — for a personal tool, an old
timestamp just means you haven't opened the page recently. Click
Refresh to update.

### Loading and error states

- **Skeleton rows** for tables on first load.
- **Connection-lost banner** at the top of the global shell only when
  the bm process itself has died (the browser can't reach
  `localhost:9443`). Auto-dismisses on reconnect.
- **Per-row error** for individual cluster fetch failures — last-known
  facts shown greyed with a small ⚠ icon and a tooltip detailing the
  failure.

### Real-time updates

The only real-time mechanism is SSE on task event streams. We do not
push cluster state changes to the browser. The reason: a single
operator clicks Refresh infrequently, the on-demand fetch handles the
rest, and a fan-out broadcast layer is unjustified complexity for a
desktop tool.

## Per-page details

### Clusters list — `/clusters`

The default landing page after the operator has at least one cluster
or draft.

**Data fetched**

- `useClusters()` → `GET /api/v1/clusters` → bbolt-cached list with
  `health`, `healthSummary`, `lastFetchedAt`. Auto-refreshes if any
  cluster's data is older than 30s.

**Columns**

| Column | Source field | Notes |
|---|---|---|
| Name | `name`, `description` | Link to `/clusters/:id` |
| Pools | `poolCount` | Single integer; `—` for drafts |
| Nodes | `healthSummary.nodes.online / .total` | Numerator turns warning colour if `<` denominator |
| Drives | `healthSummary.drives.ready / .total` | Same numerator-lag styling |
| Version | `version` | `v1.0.0` |
| Health | `health` | Rendered as `Pill` (Healthy / Degraded / Critical / Unknown / `—` for drafts) |
| Used | `usedBytes / usableBytes` | Friendly bytes; `—` for drafts |
| Status | `status` | Lifecycle pill: Active / Draft / Migrating / Failed |

The `lastActivityAt` field stays on the cluster record (still useful on the
Cluster Overview "Activity" card), but is intentionally not surfaced in the
list — the list focuses on identity, capacity, and current health.

**Refresh button**

In the header next to `+ New ▾`. Clicking calls
`POST /api/v1/clusters/refresh`, shows a spinner until the response
returns, and invalidates the `["clusters"]` react-query key.

A `Fetched Ns ago` line sits to the left, derived from the **oldest**
`lastFetchedAt` across the visible rows (worst-case freshness).

**Empty states**

- Zero clusters, zero drafts → redirect to `/welcome`.
- Drafts only → list with a single banner: "You have N draft clusters.
  Resume | Discard".

**Filters**

Chips above the table: `All / Active / Draft / Migrating / Failed`.
Client-side only.

### History — `/history`

Reverse-chronological table of CLI commands and UI actions.

**Columns**

| Column | Notes |
|---|---|
| Time | Relative ("2m ago"); tooltip shows full ISO |
| Source | Icon: terminal for `cli`, cursor for `ui_action` |
| Action | The `display` string. Mono font for `cli`, regular for `ui_action` |
| Target | Cluster name (links to `/clusters/:id`); `—` if global |
| Status | `Pill` (Succeeded / Failed / Running) |
| Duration | If known |
| | Copy button on `cli` rows; "View task" link on rows with `taskId` |

**Filters**

- Source: `All / CLI / UI`
- Cluster: dropdown of known clusters + "All"
- Date range picker
- Search box (substring match against `display`)

**Actions**

- "Clear history" button at the top, opens a typed-confirm modal.

### Cluster detail — `/clusters/:id`

A single page (no tabs) that focuses on monitoring the cluster's nodes
and performing operations on them. Per-node drill-in lives at
`/clusters/:id/nodes/:nodeId`; cluster Settings lives at
`/clusters/:id/settings`. There are no `/overview`, `/nodes`,
`/services`, or `/tasks` sub-routes — those concerns are merged into
this page or moved to the global Tasks center.

**Page structure**

1. **Header** — cluster name + health pill + lifecycle pill, meta line
   (version · node count · pool count · EC parity · "migrated from
   MinIO …" when applicable). Actions on the right:
   - `↗ Open Buckit console` — deep-link to the per-cluster Buckit
     console (data-plane operations live there, not in `bm`).
   - `Settings` — navigates to `/clusters/:id/settings`.
   - `Actions ▾` — dropdown with cluster-level operations (see below).

2. **Three summary cards**:
   - **Health** — pill, plus the `healthSummary` breakdown
     (online/degraded/offline nodes, ready/healing/failed drives, any
     active op).
   - **Capacity** — used / usable bytes, percentage bar.
   - **Pools** — per-pool rollup. One row per pool with a small health
     pill (Healthy / Degraded / Critical) and a node + drive breakdown
     for that pool (e.g. `5/6 nodes · 1 degraded`, `71/72 drives · 1
     healing`). The rollup uses the same severity rule as the
     cluster-level health but scoped to one pool's nodes/drives — so
     the operator can tell at a glance whether a problem is contained
     to one pool or fleet-wide.

     **Ordering and truncation:** pools are ordered by health severity
     (critical → degraded → healthy), then by pool number. When the
     cluster has more than two pools, only the worst two are shown by
     default with a `Show N more pools ▼` affordance below; clicking
     expands to all and the label flips to `Show fewer pools ▲`.
     Severity-first ordering means a problem can never be hidden in the
     collapsed view — if pool 7 is critical, it bubbles into the
     visible block before pool 1, no matter how many healthy pools sit
     in between.

     Probe rollups (Ping / SSH / S3 API / Console) are not surfaced at
     the cluster level — the operator gets that data per-node via the
     four probe columns in the table.

3. **Nodes table** — the centerpiece of the page.

**Cluster Actions menu** (header dropdown)

| Action | Notes |
|---|---|
| Rolling restart | Per-node restart with quorum-aware ordering |
| Rolling upgrade… | Opens version picker, then performs rolling upgrade |
| Stop all | Stops `buckit.service` on every node |
| Start all | Starts `buckit.service` on every node |
| Rotate root credentials | Rewrites `/etc/default/minio` with new creds |
| Tear down cluster… | Destructive; typed-confirm modal |

Each menu item creates a task and records a History entry.

**Nodes table**

The table is what the operator looks at most of the time. It supports
filtering, sorting, and multi-select with bulk actions.

| Column | Source | Notes |
|---|---|---|
| ☐ | client-side | Header checkbox toggles all visible rows |
| Host | `hostname` | Link to `/clusters/:id/nodes/:nodeId` |
| Pool | `pool` | Numeric, supports filter |
| State | `state` | Pill: Online / Degraded / Offline / Unknown |
| Version | `version` | Mono; supports filter |
| Ping | `pingable` | Green dot / red ✗ |
| SSH | `sshable` | Green dot / red ✗ |
| S3 API | `apiAccessible` | Green dot / red ✗ for `/minio/health/live` on :9000 |
| Console | `consoleAccessible` | Green dot / red ✗ for the Buckit web console on :9001 |
| Kernel | `kernel` | Mono; populated only when `sshable=true` |
| Uptime | `uptimeSec` | `3d 4h` style |

**Per-column filters**

Filter controls live in a second header row directly under the column
labels — not in a separate toolbar. This keeps the filter visually
attached to the column it acts on. Cells without filters (checkbox,
probe columns, uptime) are left empty.

| Column | Filter control |
|---|---|
| Host | Text input (substring match) |
| Pool | Dropdown (`All` + each distinct value) |
| State | Dropdown (`All / Online / Degraded / Offline / Unknown`) |
| Version | Dropdown (`All` + each distinct value) |
| Kernel | Text input (substring match) |

All filters are client-side; the table re-renders without a server
round trip. Selection state is preserved across filter changes.

**Sorting**

Every data column header is a button. First click sorts ascending,
second click toggles to descending. A small arrow next to the label
indicates the active sort key and direction (`↕` neutral, `↑` asc,
`↓` desc).

**Default sort:** Pool ascending, with hostname ascending as the
tiebreaker within each pool. The Pool header shows the active sort
arrow on first render so the operator can see what's happening.

**Tiebreaker:** all sorts apply `(pool asc, hostname asc)` as a stable
secondary ordering after the primary key. Clicking any column gives a
deterministic result even when many rows share the same primary value
(e.g. sorting by Version puts every `v1.0.0` row together, then within
that block keeps pool/hostname order).

Sort semantics per column:

- **State** uses a severity ranking (online → degraded → offline →
  unknown) so ascending puts healthy nodes at the top.
- **Probes** are boolean: ascending lists failures first (sorting `0`
  before `1`), so a single click on `SSH` brings the unreachable hosts
  to the top.
- **Kernel** sorts as text; rows with no kernel sort first under asc.
- All other columns use the natural string/number ordering.

**Always-on bulk action bar**

A bulk action bar sits between the cluster summary and the node table
at all times — not just when a selection exists. It contains
`Restart buckit.service`, `Redeploy software`, `Reboot host`, and
`Shut down host`. Buttons are disabled unless **both** conditions are
met:

1. SSH is configured for this cluster (`cluster.sshConfigured`).
2. At least one host is selected.

When buttons are disabled, the bar shows an inline hint
(`Configure SSH in Settings to enable host actions.` /
`Select one or more hosts above to enable actions.`) so the operator
knows what to do.

Keeping the bar always visible makes the per-host operations
discoverable even when no selection has been made — the operator sees
what's possible before clicking around.

Each button creates a task and records a History entry. Buttons that
operate on multiple hosts run in parallel SSH calls; the resulting
task log shows per-host progress.

**Why no Recent Tasks section here**

The original design had a "Recent tasks" panel. It is removed because:

- Tasks running for this cluster already surface in the global Tasks
  badge in the top bar and on `/tasks?clusterId=:id`.
- The page now leads with the node table, where operational state
  lives. A tasks panel below it pushed the most-clicked content too far
  down.

If we ever want a quick "recent activity" hint, it can go into the
Activity card alongside Health/Capacity, but the task list itself
belongs in the global Tasks center.

### Tasks center — `/tasks` and `/tasks/:id`

*To be documented.*

### New cluster wizard — `/clusters/new`

*To be documented.*

### MinIO migration wizard — `/clusters/migrate`

*To be documented.*

### Manager settings — `/settings`

The Settings page is intentionally lean. Three sections in Phase 1:

**Preferences**
- Theme (System / Light / Dark)
- Default cluster on launch (dropdown of known clusters; "Last viewed")
- History retention (rows kept; default 1000)

**Storage**
- Data directory (read-only display, with "Reveal in Finder/Explorer" button)
- Backups: schedule (off / daily / weekly), location, "Back up now" button

**Remote access** (off by default)
- Toggle: "Allow access from other devices on this network"
- When on:
  - Bind address (default `0.0.0.0:9443`)
  - Passcode (set / change / clear)
  - TLS certificate (auto-generated / custom path)
  - Last access log (last 10 IPs and times, for awareness)

No "Admin", no "TLS" as a top-level concern, no "Audit log retention".

## Open questions

- For very large clusters (50–100 nodes), should the on-demand fetch
  pull SSH facts from all nodes or sample? Sampling complicates the
  health rule but reduces network. Deferred until we hit pain.
- Should `bm web` support a `--detach` flag (background mode, with PID
  file) for users who want to leave it running across terminal restarts?
  Adds OS-specific complexity. Deferred until a user asks.
- Should the History tab support exporting selected rows to a `.sh`
  script (for replaying CLI commands)? Nice ergonomic add; not on the
  critical path.

## Change log

- 2026-05-14 — Major revision. Repositioned `bm` as a personal desktop
  tool; replaced auth/TLS/audit with optional remote access + History;
  reframed health probe as on-demand fetch; renamed `bm server` to
  `bm web`; replaced Sync-as-task with Refresh-as-sync. Documents the
  Clusters list and History pages.
- 2026-05-14 — Initial doc. Covers architecture, data flow, REST
  contract sketch, and the Clusters list page (now superseded).
