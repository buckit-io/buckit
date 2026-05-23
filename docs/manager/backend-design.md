# Buckit Manager (`bm`) — Backend design

Companion to [`phase1-web-ui.md`](./phase1-web-ui.md) and
[`ui-architecture.md`](./ui-architecture.md). The UI prototype is fully
clickable on top of an in-memory mock layer; this document describes
the backend that replaces that mock, the REST surface it exposes, and
how `bm` reuses MinIO's admin client and (selectively) `mc` source to
keep parity with the `mc` CLI.

## Goals

1. **One binary, two surfaces.** `bm` is a single static binary that
   serves both a CLI (`bm cluster ls`, `bm cluster restart …`) and a
   local web UI (`bm web` → `127.0.0.1:9443`). Both call into the same
   internal packages.
2. **Personal-tool framing.** Default listener is loopback-only. No
   multi-tenant auth, no RBAC, no audit retention plumbing. Optional
   remote-access mode adds a passcode and TLS — see
   [ui-architecture.md § Optional remote access](./ui-architecture.md).
3. **Parity with `mc` for cluster operations.** Every cluster Action,
   bulk-host action, and per-node Action the UI exposes maps to an
   admin API call (or SSH per-host loop) that `mc` operators would
   recognise. The CLI should accept the same `mc admin service …`
   verbs where it makes sense.
4. **The UI contract drives the API.** Domain types defined in
   `web/src/mock/data.ts` are the wire format. The backend doesn't get
   to invent new shapes.

## Non-goals

- Becoming a fork of MinIO or a re-implementation of `mc`. `bm` calls
  out to running clusters via the admin API; it doesn't reimplement
  erasure coding or object storage primitives.
- Background polling. The on-demand cache loop refreshes a cluster's
  facts when a UI consumer asks. No timers fanning out per-cluster
  probes every N seconds (see [`ui-architecture.md`](./ui-architecture.md)).
- Multi-host coordination. The manager runs on one operator's machine
  at a time. If two operators each run `bm`, they have two independent
  databases.

## Big picture

```
+-------------------------------------------------------------+
|                          bm binary                          |
+-------------------------------------------------------------+
|  cmd/bm/                                                    |
|  ├── bm-native verbs        (urfave/cli registration)       |
|  │     web, cluster, manager, migrate, rolling, node, …     |
|  └── github.com/buckit-io/bm-cli  (vendored mc fork)        |
|        cp, ls, mb, admin *, alias, share, event, ilm, …     |
+-------------------------------------------------------------+
|  internal/                                                  |
|  ├── app/      Process lifecycle, lockfile, signal handling |
|  ├── api/      chi router, REST handlers, SSE plumbing      |
|  ├── store/    bbolt wrapper + KEK-encrypted secrets bucket |
|  ├── tasks/    Operation orchestration + history finalize   |
|  ├── ssh/      Per-cluster SSH client cache + run helpers   |
|  ├── admin/    madmin-go wrapper exposing the calls we use  |
|  ├── deploy/   New-cluster install + MinIO→Buckit cutover   |
|  ├── cluster/  Cluster repo (load, save, refresh, health)   |
|  ├── alias/    Bridge: store → ~/.config/bm/config.json     |
|  ├── auth/     Optional remote-access passcode + TLS        |
|  ├── config/   $XDG_CONFIG_HOME/bm/, KEK material, perms    |
|  └── health/   On-demand cache loop, admin-info probes      |
+-------------------------------------------------------------+
|  forked deps under buckit-io/* (own repos, go.mod-pinned)   |
|  ├── buckit-io/bm-cli       (forked mc — Buckit CLI)        |
|  ├── buckit-io/madmin-go    (forked admin API SDK)          |
|  ├── buckit-io/minio-go     (forked S3 client SDK)          |
|  ├── buckit-io/pkg          (forked shared utilities)       |
|  ├── buckit-io/cli          (forked urfave/cli)             |
|  └── buckit-io/selfupdate   (forked, points at Buckit rel.) |
+-------------------------------------------------------------+
|  web/dist/   (embed.FS in the release binary)               |
+-------------------------------------------------------------+
```

Two CLI surfaces share one binary:

- **bm-native commands** (`web`, `cluster ls`, `cluster import`,
  `cluster migrate`, `cluster deploy`, `manager *`) are written fresh
  in `cmd/bm/` using the same `urfave/cli` framework the vendored
  CLI uses.
- **Buckit CLI commands** (cp, ls, mb, admin service restart, admin
  trace, alias, share, event, ilm, replicate, …) come from
  `buckit-io/bm-cli` — a fork of `minio/mc` we own outright. Full
  feature parity with mc from day one.

The CLI calls straight into internal packages — no HTTP hop. `bm web`
runs the same internal packages behind an HTTP router. The two
surfaces share state via bbolt; concurrent access is serialised by
bbolt's short-lived write locks.

The HTTP server's admin-API path (`POST /operations` for
restart/stop/freeze/heal) deliberately does **not** go through the
forked CLI commands. CLI commands call `os.Exit`, write to
`os.Stdout` via a CLI printer, and look up clusters via the alias
config — none of which fits an HTTP handler. The handlers call
`internal/admin/` (the `madmin-go` wrapper) directly. CLI and HTTP
share the same Go SDK underneath; they're separate consumers of it.

## Persistence layout

bbolt file at `${XDG_CONFIG_HOME:-~/.config}/bm/bm.db`, mode 0600. One
file per operator. Buckets:

| Bucket | Key | Value | Notes |
|---|---|---|---|
| `clusters` | `<clusterId>` | `Cluster` JSON | Mirrors `mock/data.ts` `Cluster` |
| `nodes` | `<clusterId>:<nodeId>` | `Node` JSON | Includes drives, NIC, RAM, kernel |
| `node_facts` | `<clusterId>:<nodeId>` | last admin-info + SSH fact blob | Cache for refresh |
| `cluster_ssh` | `<clusterId>` | `ClusterSshConfig` JSON, AES-GCM encrypted | Per-host overrides included |
| `cluster_admin` | `<clusterId>` | `{ user, password }`, AES-GCM encrypted | Root creds for admin API |
| `history` | `<ULID>` | `HistoryEntry` JSON incl. `result` | Newest-first reads |
| `settings` | `app` | manager settings | Remote access state, version pin |

Encryption: AES-GCM with a 32-byte data key. The key itself sits in
`${XDG_CONFIG_HOME}/bm/data.key`, mode 0600. First-launch generates it.
Real backend later may swap to OS keychain (macOS Keychain, Windows
DPAPI) but the in-tree default is the file. Pulling a row decrypts in
the `store/` package; callers never see ciphertext.

History bucket is bounded — `tasks.Finalize` triggers a sweep that
deletes the oldest entries past 1000 rows (configurable). No timers;
sweep happens on next write.

## REST API

Base path `/api/v1`. Content-type `application/json`. Errors follow
[ui-architecture.md § Errors](./ui-architecture.md).

### Sessions (optional remote access)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/sessions/login` | Body `{ passcode }`. Sets the session cookie. |
| `POST` | `/sessions/logout` | Drops the cookie. |
| `GET` | `/sessions/me` | Returns current user (always `admin` in default mode). |

Loopback default skips auth entirely; the handlers no-op when
`auth.RemoteEnabled == false`.

### Clusters

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/clusters` | List clusters with `Cluster.healthSummary` populated. |
| `GET` | `/clusters/:id` | One cluster (same shape as the list row). |
| `POST` | `/clusters/refresh` | Synchronous re-fetch admin info + cluster-healthy probe across every cluster. Returns the updated list. Powers the **Refresh** button. |
| `POST` | `/clusters/:id/refresh` | Same, scoped to one cluster. |
| `DELETE` | `/clusters/:id` | Drops the cluster definition. Equivalent to the **Remove cluster definition** operation; data on hosts is not touched. |

### Cluster import

Two-step flow — discover (read-only) then commit (persist).

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/clusters/import/discover` | Body `{ url, username, password }`. Calls `/minio/admin/v3/info`, returns `ImportCandidate`. Streams progress lines via SSE on the same response (`Content-Type: text/event-stream`). |
| `POST` | `/clusters/import/commit` | Body `{ candidate, chosenName }`. Persists, returns `{ clusterId }`. |

### Cluster deployment (new-cluster wizard)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/clusters/new/preflight` | Body `NewClusterDraft`. Runs SSH probes, drive uniformity check, hostname pattern check. Returns `PreflightResult[]`. |
| `POST` | `/clusters/new/deploy` | Body `NewClusterDraft`. Returns `{ taskId }`. Progress via SSE on `/operations/:taskId/events`. |

### Migration (MinIO → Buckit)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/clusters/:id/migrate/snapshot` | Captures MinIO state (buckets, users, lifecycle, …) into `MinioSnapshot`. |
| `POST` | `/clusters/:id/migrate/preflight` | SSH probe + minio detection + drive prep checks. |
| `POST` | `/clusters/:id/migrate/cutover` | Returns `{ taskId }`. Install Buckit, swap systemd unit, verify per host. |
| `POST` | `/clusters/:id/migrate/rollback` | Rollback completed nodes to MinIO. |

### Nodes

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/clusters/:id/nodes` | List nodes for one cluster. |
| `GET` | `/clusters/:id/nodes/:nodeId` | One node detail (drives, NIC, kernel, …). |

### Operations (the unified dispatch path)

Every cluster Action, bulk-host action, and per-node Action goes
through this surface. The frontend's `dispatchOperation()` mock maps
to a single endpoint.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/operations` | Body `{ clusterId, kind, params?, targetHostIds? }`. Returns `{ taskId }`. |
| `GET` | `/operations/:taskId` | Snapshot of current `OperationProgress`. |
| `GET` | `/operations/:taskId/events` | SSE stream of progress updates until terminal. |
| `POST` | `/operations/:taskId/cancel` | Cancel an in-flight op (orchestrated only). |

`kind` is the `OpKind` union from the UI catalog (`restart_cluster`,
`rolling_restart`, `systemctl_restart`, `redeploy_software`,
`reboot_host`, …). The orchestrator selects the executor by `kind`.

### Streaming

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/clusters/:id/nodes/:nodeId/logs` | SSE proxying admin journalctl/logs API to the browser. |
| `GET` | `/clusters/:id/nodes/:nodeId/trace` | SSE proxying admin trace API. |

### History

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/history` | List history rows. Query: `?status=&clusterId=&since=&until=`. |
| `DELETE` | `/history` | Clear all (optional `?before=<ts>`). |

### Settings

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/settings` | Manager preferences (remote access state, version, etc.). |
| `PATCH` | `/settings` | Update remote access on/off, passcode, TLS paths. |
| `GET` | `/clusters/:id/ssh` | `ClusterSshConfig`. |
| `PUT` | `/clusters/:id/ssh` | Update SSH config. Returns 204 on success. |

## Operation orchestration

The dispatch path is the heart of the backend. One pipeline handles all
21 ops the UI knows about. Pseudo-flow:

```
POST /operations
   ↓
1. Validate kind + params + targetHostIds.
2. Resolve cluster + admin creds + SSH creds.
3. Write a `running` history row (returns historyId).
4. Allocate a taskId.
5. Pick the executor by kind:
     - Signal (freeze/unfreeze/remove_cluster): one admin API call.
     - Admin w/ progression (restart/stop/heal): admin call + poll back
       to terminal; emit per-host or summary updates as we go.
     - Orchestrated (rolling_restart, redeploy, reboot, systemctl_*):
       per-host SSH loop with health-wait between hosts.
6. Stream OperationProgress updates to subscribers (in-memory pub/sub;
   SSE consumers attach via /operations/:taskId/events).
7. On terminal:
     a. Snapshot the OperationProgress into an OperationResult.
        Drop live-only fields (events stream, progress counters).
     b. Update the history row: status, durationSec, failureNote,
        result.
     c. Close the SSE channel and free in-memory progress state after
        a short grace period.
```

Cancellation: an in-flight orchestrated op checks `ctx.Done()` between
hosts. The in-flight host finishes naturally; the loop halts before the
next host. The history row is finalized as `canceled`.

Concurrency rules:
- At most one mutating operation per cluster at a time. The dispatcher
  takes a per-cluster lock from `store.AcquireOpLock(clusterId)`.
- Refresh and read-only fetches run in parallel with operations.
- Two operations on different clusters run in parallel.

In-memory state for in-flight ops: a `map[taskId]*OperationProgress`
guarded by a mutex. Lost on restart — any in-flight op at restart is
marked `failed` with a "process restarted mid-op" note when the history
sweep runs.

## Forking the MinIO Go ecosystem

`bm` aims for full `mc` parity at the CLI — every verb an operator
already knows (`cp`, `ls`, `mb`, `admin service restart`, `admin
trace`, `admin heal`, `event add`, `replicate`, `ilm`, `tag`, `share`,
…) should work in `bm`. Rewriting ~150 mc command files in cobra is
not the path; we **fork mc wholesale and own it from there**.

This is a hard fork — no upstream rebase obligation. From the fork
point on, Buckit's CLI evolves independently from `mc`. That same
posture extends to the MinIO Go libraries `mc` depends on: if we want
the freedom to change an admin API request shape or extend the S3
client, we need to own those too.

### Repos to fork

License is uniformly AGPLv3 — Buckit, `mc`, and all of MinIO's Go
libraries — so the fork itself raises no licensing concerns.

#### Tier 1 — fork in one coordinated pass

These are mc's load-bearing dependencies. Forking partially leaves
you in `go.mod replace` hell (one fork still imports an upstream
sibling that hasn't been forked), so do them together:

| Fork target | Upstream | Why |
|---|---|---|
| `buckit-io/bm-cli` | `minio/mc` | The CLI itself. Vendored at the top level of `bm/cmd/bm/`. |
| `buckit-io/madmin-go` | `minio/madmin-go/v3` | Admin API client SDK. Every `admin *` verb uses it; the HTTP backend uses it too. Already in `buckit/go.mod` at `v3.0.109` against upstream. Must fork to evolve the Buckit admin protocol. |
| `buckit-io/minio-go` | `minio/minio-go/v7` | S3 client SDK. Powers `cp`, `ls`, `mirror`, `cat`, … and anything in `bm` that needs to talk S3 to a Buckit cluster. |
| `buckit-io/pkg` | `minio/pkg` | Shared utilities: `console`, `ellipses`, `env`, `words`, `sync`, `wildcard`, … mc imports dozens of subpackages. The other Tier-1 forks also import this; forking it last would break their builds. |
| `buckit-io/cli` | `minio/cli` | MinIO's fork of urfave/cli v1.x. mc uses it for command registration. Small surface; fork now to control flag-parsing behaviour. |

#### Tier 2 — fork at the same time for cohesion

Lower change frequency, but coupled enough that owning them avoids
surprises:

| Fork target | Upstream | Why |
|---|---|---|
| `buckit-io/selfupdate` | `minio/selfupdate` | Powers `bm update`. Hard-coded to MinIO release URLs upstream — must repoint to Buckit's release feed. |
| `buckit-io/colorjson` | `minio/colorjson` | `--json` output formatter. Small. |
| `buckit-io/kms-go` | `minio/kms-go` | Used by `admin kms *` commands. Only matters if you ship those verbs. |

#### Tier 3 — leave as upstream deps

Small specialized libraries that almost never need to change. Keeping
them upstream saves maintenance bandwidth:

`minio/highwayhash`, `minio/sha256-simd`, `minio/crc64nvme`,
`minio/filepath`, `minio/csvparser`, `minio/dnscache`, `minio/dperf`.

Promote individual repos to Tier 2 if a specific need arises.

### Repo layout decision: `bm-cli` is standalone

Two structural options were considered for the mc fork:

1. Vendor `mc` directly under `buckit-io/bm/cmd/bm/mc/`. Simpler
   dependency graph; only one binary ever ships the CLI.
2. **(chosen)** Standalone `buckit-io/bm-cli` repo, imported by `bm`
   via go.mod as a **library** (the upstream `package main` was
   removed at fork time; `bm` is the only binary). Keeps the option
   open of a standalone `buckit-cli` build in the future without
   forcing the repo split later.

The CLI has independent value outside `bm`. Standalone forces a
clean public API boundary and keeps the manager-specific code
(`web`, `cluster import`, `manager *`) out of the CLI fork.

### bm-native verbs vs. forked mc verbs

Both live in `cmd/bm/main.go`'s command registration. No top-level
naming conflicts — mc's verbs (`cp`, `ls`, `mb`, `rb`, `admin`,
`alias`, `share`, `event`, `replicate`, `mirror`, `cat`, `stat`,
`legalhold`, `retention`, `encrypt`, `tag`, `ilm`, `quota`, `tier`,
`license`, `support`, `batch`, `idp`, …) don't overlap with
bm-native verbs (`web`, `cluster`, `manager`, `migrate`, `rolling`,
`node`, `history`, `settings`).

bm-native commands use the same `buckit-io/cli` (urfave/cli fork)
framework that the vendored mc commands use, so registration is
uniform.

### Cluster references — the alias bridge

mc commands take a cluster reference as a named **alias**
(`bm admin service restart prod-east` expects `prod-east` to be
listed in mc's config). bm's source of truth for clusters is bbolt,
not the alias file. Bridge approach:

- `internal/alias/` watches the cluster store. Every cluster create
  / update / delete also writes to `${XDG_CONFIG_HOME}/bm/config.json`
  (same filename and JSON shape as `~/.mc/config.json`, just a different
  directory).
- Forked mc commands are patched to read aliases from the bm path
  instead of `~/.mc/`. Single-line change in the forked
  `buckit-io/bm-cli` (the alias-resolution helper).
- If an operator has an existing `~/.mc/config.json`, `bm` doesn't
  touch it. The two configs stay independent. Operators who want
  unified aliases can `cp ~/.mc/config.json ~/.config/bm/config.json`
  once.

The alias file contains admin URL + access/secret pairs per cluster.
On disk it's the operator's responsibility to mode it 0600; the bm
writer enforces 0600 on every save.

### The HTTP path still uses `madmin-go` directly

CLI commands call `os.Exit`, write to `os.Stdout` via a CLI printer,
and surface errors as user prompts — none of which fits an HTTP
handler. `internal/admin/` is a thin Go wrapper around
`buckit-io/madmin-go` that the HTTP backend calls directly. It:

1. Keeps a per-cluster `madmin.AdminClient` cached (created lazily on
   first call, evicted on credential rotation).
2. Exposes only the methods we use: `ServiceRestart`, `ServiceStop`,
   `ServiceFreeze`, `ServiceUnfreeze`, `Heal`, `ServerInfo`,
   `AccountInfo`, `LogStream`, `Trace`.
3. Applies a short default timeout (5s for one-shot calls, no timeout
   for streams) and surfaces errors as typed `OpError` values the
   orchestrator can map to history `failureNote`.

Both surfaces — CLI and HTTP — share the same forked `madmin-go`
underneath. They're independent consumers of one Go SDK, not callers
of each other.

## CLI command tree

Every mc verb works in `bm` from day one — they come from the vendored
`buckit-io/bm-cli` fork. bm-native verbs sit alongside them under the
same top-level dispatch.

### bm-native verbs

Things mc doesn't do — manager state, deployment, migration:

```
bm web                              Start the local web UI + API.
bm version

bm cluster ls                       List clusters this manager knows.
bm cluster info <c>                 Show health, pools, node count.
bm cluster import <url> --name <n>  Two-step import (discover + commit).
bm cluster deploy <draft.yaml>      Drive the new-cluster wizard headless.
bm cluster migrate <c>              Run the MinIO → Buckit cutover.
bm cluster rm <c>                   Drop the definition (no data touched).

bm rolling restart <c>              SSH-orchestrated rolling restart.
bm rolling upgrade <c> --version v  SSH-orchestrated rolling upgrade.
bm node restart <c> <hostnames>     systemctl restart on selected hosts.
bm node reboot <c> <hostnames>      systemctl reboot, sequential.

bm history                          List recent ops.
bm settings                         Show / edit manager settings.
```

These are written fresh in `cmd/bm/` against the same urfave/cli
framework the vendored CLI uses.

### Inherited from `buckit-io/bm-cli` (full mc parity)

The full mc verb set, identical flag surface and output:

```
bm cp / mv / ls / rm / mb / rb / cat / head / tail / pipe / find / du / stat / tree / mirror / diff / sql / version / share

bm admin service restart / stop / freeze / unfreeze
bm admin heal
bm admin trace
bm admin info
bm admin config / cluster / decommission / logs / prometheus / replicate / kms / idp / accesskey / policy / user / group / bucket / tier / scanner / speedtest / lock / rebalance / top / inspect

bm alias set / list / ls / remove / rm
bm event add / list / remove
bm replicate / ilm / tag / legalhold / retention / encrypt / quota / anonymous / version / undo
bm batch / license / support / idp / tier
```

These come unchanged from the fork; nothing for us to maintain on the
command-implementation side unless we deliberately diverge.

Only operations dispatched through the HTTP `POST /operations` path
(i.e. the web UI's unified operation modal) write rows into the
`history` bucket via `internal/tasks/`. Inherited bm-cli verbs (`cp`,
`ls`, `admin service restart`, ...) behave exactly like `mc` —
they print to stdout, return an exit code, and do **not** touch the
history bucket. Operators who run a verb from a shell and want a
record of it can rely on their shell history, the same way they do
with `mc`. `--json` and `--quiet` flags inherit their mc semantics.

## Security model

- **Default mode.** Listener `127.0.0.1:9443`. No authentication on the
  HTTP API — the listener restricts access to the loopback interface
  on the operator's machine. The CLI uses local Unix domain socket
  (`${XDG_CONFIG_HOME}/bm/bm.sock`) or short-lived bbolt locks when
  no `bm web` is running.
- **Remote-access mode.** Operator toggles it in Settings → Remote
  access. The listener moves to `0.0.0.0:9443`, the handlers require a
  passcode-signed session cookie, and TLS is mandatory (self-signed or
  operator-supplied cert).
- **Credentials at rest.** SSH creds and admin root creds are AES-GCM
  encrypted using a 32-byte data key stored beside the database. Real
  follow-up: integrate OS keychain.
- **No CLI history capture.** History rows only come from UI dispatch
  and the bm CLI's own write-side commands. We don't intercept the
  user's shell history or `mc` invocations.

## SSH layer

`internal/ssh/`:

- Per-cluster `*ssh.Client` cache, keyed by `clusterId`. Idle timeout
  5 min. Re-dialed automatically on disconnect.
- Per-host override semantics: when a `HostRow` carries `sshOverride`,
  the client uses those credentials for that host only (different user,
  different key, different password).
- A `Run(ctx, host, command) (stdout, stderr, exitCode, error)` helper
  used by every orchestrated op's executor.
- `RunStream(ctx, host, command, lineCh)` streams stdout/stderr lines
  to the caller — used by the long-running rolling-upgrade install
  step so logs appear live in the modal.
- No SSH agent forwarding. No pubkey distribution. `bm` is a personal
  tool; the operator owns the keys.

## Health & refresh

- **No background polling.** Cluster `health`, `healthSummary`,
  `lastFetchedAt`, `unreachableSince` come from the **last completed
  refresh**.
- **On-demand cache loop.** The Clusters page calls `POST /clusters/refresh`
  on first load and when the operator clicks Refresh. Cluster detail
  calls `POST /clusters/:id/refresh`. Both run admin-info + cluster
  health probe per cluster in parallel; the response is the updated
  records.
- **Staleness display.** The UI shows "Fetched Ns ago" using
  `lastFetchedAt` — see [ui-architecture.md § Staleness display](./ui-architecture.md).

## Phasing — mapping to existing milestones

[phase1-implementation.md](./phase1-implementation.md) already
defines M0–M9. This doc fills in the design for those, plus adds a
prerequisite fork pass before M1.

| Milestone | Scope informed by this doc |
|---|---|
| M0 — Module bootstrap | Done. |
| M0.5 — Fork the MinIO Go ecosystem | Done. All seven forks live under `buckit-io/*` with rebranded module paths, all CI workflows green: `cli`, `selfupdate`, `minio-go`, `pkg`, `madmin-go`, `colorjson`, `bm-cli`. `bm-cli` is library-only (no root `package main`); user-facing `mc`/`MinIO` strings rebranded; upstream copyright headers preserved for AGPL attribution. `bm/go.mod` is currently bare — pins materialise automatically once M1 imports the forks. See [§ Forking the MinIO Go ecosystem](#forking-the-minio-go-ecosystem). |
| M1 — Storage + server shell | bbolt setup; `internal/{app,store,api,config,alias}/`; `bm web` starts a chi server with shape-correct empty handlers for the UI's read paths. **Plus**: import `buckit-io/bm-cli` and wire bm-native verbs alongside; alias bridge writes `${XDG_CONFIG_HOME}/bm/config.json` on cluster save. See [§ M1 — Storage + server shell](#m1--storage--server-shell) for the punch list. |
| M2 — Task engine + SSE | `internal/tasks/`; the orchestrator pipeline; pub/sub for `OperationProgress`; SSE endpoint. |
| M3 — SSH layer + node CRUD | `internal/ssh/`; node bucket; per-cluster client cache. |
| M4 — Discovery | `/clusters/import/discover` + commit. `madmin-go` AccountInfo + ServerInfo. |
| M5 — Topology + preflight | New-cluster + migrate preflight checks. |
| M6 — New-cluster deploy | `internal/deploy/` install loop; SCP + dnf + daemon-reload + systemctl. |
| M7 — Cluster operations | The operation catalog under `internal/operations/`: 3 signal ops (freeze/unfreeze/stop), 2 admin-with-progression (restart_cluster, start_heal), 4 SSH-orchestrated (start_cluster parallel, rolling_restart sequential, rolling_upgrade + redeploy_software both Buckit-only), 5 host-scoped (3 systemctl verbs + reboot + shutdown). Most ops support both Buckit and MinIO clusters; `rolling_upgrade` and `redeploy_software` are Buckit-only and reject MinIO at dispatch with `engine_mismatch`. `start_cluster` is NOT a madmin call (no `ServiceStart` exists) — it's parallel SSH `systemctl start` followed by a cluster-wide health-wait. `rotate_root_creds` and `add_pool` deferred to M7.5; `remove_cluster` dropped (use `DELETE /clusters/:id`). |
| M8 — MinIO migration | Done. Snapshot writer captures buckets / users / groups / canned policies / service accounts / lifecycle / notifications / per-bucket versioning + object-lock + tags into `~/.config/bm/snapshots/<clusterId>-<ts>.json` mode 0600 (wire-stable across versions, soft per-field failures recorded in `Warnings`). Cutover executor runs the install pipeline sequentially (`stopping_minio` → `uploading_pkg` → `installing` → `switching_unit` → `waiting_health` → `waiting_cluster` → `done`), backing up `/etc/default/minio` to `/etc/default/minio.bm-bak` per host and waiting for cluster-wide health between hosts. Rollback reverses the unit swap on hosts where buckit.service is currently active; hosts already on MinIO are skipped cleanly. Engine flips at commit time on success and back on rollback; `MigratedFrom` is stamped/cleared accordingly. Post-cutover verify pass populates the wizard's audit table; failures surface as a warning on the history row, not an auto-rollback. See [§ M8 — MinIO migration](#m8--minio-migration) for the punch list. |
| M9 — Packaging + installers + embed | nfpm, install.sh, install.ps1; embed `web/dist/`. |

Three adjustments to call out vs. the original M-plan:

- **New M0.5: the fork pass.** Done as of 2026-05-18. Five Tier-1 repos
  (`bm-cli` ex `mc`, `madmin-go`, `minio-go`, `pkg`, `cli`) and two
  Tier-2 (`selfupdate`, `colorjson`) live under `buckit-io/*`. Every
  cross-import in the fork tree points at `buckit-io/*` paths — no
  `go.mod replace` directives, no upstream sync work. `bm-cli` is
  library-only (the root `main.go` was removed; `bm` is the binary
  entry point and will consume the package in M1).
- **Drop the separate `Tasks` page work.** The UI consolidates onto
  the History page (every op writes a `result` snapshot; History's
  View modal renders it). No `/tasks/:id` page; no Task records
  distinct from history. `internal/tasks/` still exists — it's the
  in-flight orchestrator — but it doesn't persist a task table.
- **`mc` is vendored, not selectively ported.** Original plan was
  silent on this. This doc lands on "fork the whole CLI tree and own
  it" — see the "Forking the MinIO Go ecosystem" section above.

## M1 — Storage + server shell

Goal: `bm web` boots, opens bbolt, serves a chi router with shape-correct
empty responses for the read paths the UI calls, and the urfave/cli
dispatch mounts both bm-native verbs and the forked `bm-cli` verbs. No
real cluster operations yet — every mutating endpoint stays 501 with a
milestone tag.

Each bullet is `work item → acceptance check`.

### Dependencies

- Import `buckit-io/bm-cli`, `buckit-io/madmin-go/v3`, `buckit-io/cli`, `go-chi/chi/v5`, `go.etcd.io/bbolt` into `go.mod` → `go build ./...` succeeds and the binary still fits the ~10–12 MB target.

### `internal/config/`

- Resolve XDG paths (`~/.config/bm/` on unix, `%APPDATA%\bm\` on Windows) → `config.Dir()` returns the right path per-OS.
- KEK bootstrap chain: `BM_DATA_KEY` env → `data.key` file → auto-generate 32-byte key at `~/.config/bm/data.key` mode 0600 → `config.DataKey()` returns 32 bytes; auto-generation logs the path once on first launch.
- Settings struct (remote access off, version pin nil) loaded from the `settings/app` bucket → defaults applied on first launch; `PATCH /settings` persists across restarts.

### `internal/store/`

- bbolt opened at `~/.config/bm/bm.db` mode 0600 with 5s lock timeout → second `bm web` fails fast with a wrapped, friendly "another bm process is using ~/.config/bm/bm.db" error (bbolt's `flock` is kernel-held and released on crash, so there is no stale-lock case).
- `View(fn)` / `Update(fn)` helpers wrap `db.View` / `db.Update` with a 5s per-txn timeout → both helpers covered by unit tests.
- Buckets auto-created on first open: `clusters`, `nodes`, `node_facts`, `cluster_ssh`, `cluster_admin`, `history`, `settings` → bucket-list assertion test passes.
- `PutEncrypted` / `GetEncrypted` AES-GCM helpers for `cluster_ssh` and `cluster_admin` buckets → round-trip test on a random 1 KiB payload passes; callers never see ciphertext.
- History bucket sweep deferred — a code comment notes M2's `tasks.Finalize` owns it.

### `internal/app/`

- SIGINT / SIGTERM handler triggers graceful shutdown of the chi server with a 5s deadline → Ctrl-C on a running `bm web` exits 0 within 5s.
- Single-instance enforcement piggy-backs on bbolt's file lock (above). No separate `bm.lock`.

### `internal/api/`

- chi router mounted at `/api/v1`, listener default `127.0.0.1:9443`, refuses non-loopback bind in M1 → `bm web --addr 0.0.0.0:9443` errors out pointing at the (future) remote-access milestone.
- Middleware: recoverer, request logger, JSON content-type → a panic in a handler returns 500 + structured JSON, not a stack trace.
- `GET /api/v1/healthz` returns `{"status":"ok","version":...}` → `curl` returns 200.
- Shape-correct read stubs so the UI loads against the real backend with empty state:
  - `GET /clusters` → `[]`
  - `GET /clusters/:id` → 404
  - `GET /history` → `[]`
  - `GET /settings` → the settings struct from bbolt
  - `GET /sessions/me` → `{"username":"admin"}` (no auth in loopback default)
- All other endpoints listed in [§ REST API](#rest-api) → 501 with `{"error":"not implemented","milestone":"Mn"}` and the owning milestone tag.
- Static-asset handler: if `web/dist/` exists on disk, serve it; else return 404 with a hint to run `npm run build` → manual `npm run build` + `bm web` renders the Clusters page in a browser.

### `internal/alias/`

- Write-through helper `alias.Sync(store)` that snapshots all clusters into `~/.config/bm/config.json` mode 0600 in mc-compatible JSON shape → on-demand call produces a file `bm-cli` verbs can read; no clusters yet so the output is the empty mc config skeleton.
- Patch bm-cli at startup to read its config from `~/.config/bm/` (the fork already has `setMcConfigDir` at `cmd/config.go:40` — promote to exported if needed) → `bm alias list` reads from the bm path, not `~/.mc/`.

### `cmd/bm/`

- Replace the hand-rolled switch in `cmd/bm/main.go` with `buckit-io/cli` (urfave/cli fork) dispatch → existing `bm version` and `bm help` keep working; `bm` with no args prints the unified help.
- Register only `web` and `version` as bm-native commands in M1 — the other bm-native verbs (`cluster`, `manager`, `migrate`, `rolling`, `node`, `history`, `settings`) land in their owning milestones, not as M1 stubs.
- Mount bm-cli's `appCmds` slice alongside the bm-native commands → `bm alias list`, `bm admin info --help`, and at least one `cp`/`ls` smoke run dispatch correctly.
- New `cmd/bm/web.go` with the `web` action: flags `--addr` (default `127.0.0.1:9443`), `--no-browser`, `--data-dir` → starting `bm web` opens the default browser (unless `--no-browser`) and binds the listener.

### Milestone exit criteria

1. `bm web` starts, serves `/api/v1/healthz`, exits cleanly on SIGINT.
2. `~/.config/bm/{bm.db,data.key,config.json}` all exist mode 0600 after a first run.
3. The Clusters page in the UI loads against the real backend and shows the empty state.
4. A second `bm web` started while the first is running exits non-zero with the wrapped bbolt-timeout message.
5. `bm version`, `bm help`, and `bm alias list` all dispatch under the unified urfave/cli tree.
6. `make build` produces a binary in the ~10–12 MB range; the cross-compile sanity loop in CLAUDE.md still passes.

### M1-local open questions

- **`appCmds` export.** Is bm-cli's `appCmds` slice exported today, or does it need a one-line fork patch (`Cmds` or `Commands()`)? Decide before the urfave/cli wiring lands.
- **Dev fallback for the static handler.** When `web/dist/` is missing, should `bm web` 404 strictly, or redirect to `http://localhost:5173` so `vite dev` is the natural inner loop? Strict 404 is simpler; redirect is friendlier.
- **Browser auto-open on headless hosts.** `open` / `xdg-open` fails silently on macOS without a GUI session and on bare Linux. Log a warning vs. surface an error?
- **Default listener port.** The doc commits to `127.0.0.1:9443`. 9443 is IANA-registered as `tungsten-https` and is **Portainer's default HTTPS port** — a real conflict for operators running both tools on one machine. Decide before M1 merge: keep 9443 and rely on `--addr` overrides, or move to a less-crowded port (`9543` and `9445` are the cleanest nearby options; `8443` collides with Tomcat / K8s).

## M8 — MinIO migration

Goal: an operator on the Migrate wizard can capture a MinIO cluster's
state, run a sequential per-host cutover that swaps `minio.service` for
`buckit.service`, and roll the change back if needed. All of it goes
through the M2 orchestrator, so progress streams over SSE and history
rows record `Result` snapshots like every other op.

The cutover is **roll-forward only inside one task** — verify failures
surface as warnings on the history row, not an auto-rollback. The
operator triggers rollback explicitly. That keeps the failure mode
visible instead of silently masking real bugs.

Each bullet is `work item → acceptance check`.

### Snapshot capture (`internal/migration/`, `internal/admin/`)

- Extend `internal/admin/Client` with `ListUsers`, `ListGroups` (+ per-group `GetGroupDescription`), `ListCannedPolicies`, `ListServiceAccounts(users)` → admin client returns the typed slices the snapshot writer consumes.
- Add `internal/admin/S3Client` (minio-go wrapper) for bucket-level reads: `ListBuckets`, `EnrichBucket` (versioning + object-lock + tags), `BucketLifecycle`, `BucketNotifications` → 404/NotImplemented on older MinIO versions becomes a snapshot warning, not a fetch failure.
- `migration.Snapshot(ctx, dir, clusterID, creds)` populates the full `domain.MinioSnapshot` and writes it to `~/.config/bm/snapshots/<clusterId>-<ts>.json` mode 0600 → file written, in-memory snapshot returned, soft per-field errors collected in `snap.Warnings`.
- `migration.Summarize(snap)` derives `domain.MinioSnapshotSummary` (counts + `largestBucket`) for the wizard's Review step → counts match what the wizard's `MinioSnapshot` interface in `state.ts` expects.
- `migration.ReadSnapshot(path)` reloads a snapshot for the cutover/rollback executors → wire-stable round trip; new optional fields don't break older files.

### Cutover executor (`internal/migration/`)

- `CutoverParams` + `MigrationBody` (wire shape) + `Stage` enum (mirrors UI's `CutoverNodeState.state` byte-for-byte) → `Validate()` rejects empty hosts, missing snapshot, unsupported version.
- `Installer.Install(ctx, host, params, emit)` per-host pipeline: backup `/etc/default/minio` → stop minio → curl rpm → dnf/yum/apt install → disable minio.service + enable --now buckit.service → curl `/minio/health/live` → done. Reuses `deploy.PickInstallCmd`, `deploy.SudoWrap`, `deploy.ShellEscape`, `deploy.RunStep` (exported from `internal/deploy/install.go`) → emits a `StepEvent` per stage.
- `CutoverExecutor` (sequential, no parallel knob): per-host loop with `waitClusterHealthy` between hosts via `admin.Pool` ServerInfo (default 120s timeout) → halts on first host failure with `FailureNote` listing the failed host; remaining hosts stay on MinIO.
- After all hosts done: `commitEngineFlip` updates the persisted `domain.Cluster`: `Engine: minio→buckit`, sets `Version`, stamps `MigratedFrom{Product:"minio", Version: snap.Version, FinalizedAt: now}` → cluster row reflects the new engine; UI banner shows the migration source.
- Cancellation: `markCanceled` records the in-flight host's stage in `FailureNote` ("cutover canceled at <host> (stage: <stage>)"), keeps earlier hosts as `HostSucceeded`, leaves later hosts as `HostPending` → operator reads exactly which hosts are on Buckit and which are still on MinIO from the History result modal.

### Verify pass (`internal/migration/verify.go`)

- After commit, `Verify(ctx, pool, params)` reads back the migrated cluster: `ServerInfo` for `clusterHealthy + nodesReporting`, `AccountInfo` for bucket count + smoke check (every snapshot bucket still exists), `ListUsers/ListGroups/ListCannedPolicies/ListServiceAccounts` for IAM counts → result populates the wizard's `VerifyResult` shape.
- Verify failures land in `OperationResult.FailureNote` and surface as a warning on the history row. **No auto-rollback** → operator decides; bm doesn't second-guess.
- Time-boxed at 60s so a wedged cluster doesn't push the cutover history row past the operator's expectation.

### Rollback executor (`internal/migration/rollback.go`)

- `RollbackExecutor` validates, then per host: `systemctl is-active buckit.service` → if inactive, mark `HostSucceeded` with detail "Already on MinIO" and skip; otherwise run `Installer.Rollback`: stop buckit.service → restore env-file backup → enable --now minio.service → wait `/minio/health/live` → if at least one host actually rolled back, flip `Cluster.Engine` back to `EngineMinio` and clear `MigratedFrom`.
- Pure no-op rollback (every host already on MinIO) leaves the cluster row alone → idempotent; safe to call twice.

### Preflight check

- `bak_writable` (blocking, per host) — `sudo touch + rm /etc/default/.bm-bak-probe` → catches sudo-required hosts where `/etc/default` isn't writable before the cutover hits the same step on host #2.

### REST surface

- `POST /clusters/:id/migrate/snapshot` — returns `{snapshot, summary, path}`. Already mounted in M5; M8 fills in the body.
- `POST /clusters/:id/migrate/preflight` — already mounted in M5; M8 adds `bak_writable`.
- `POST /clusters/:id/migrate/cutover` — body is the wizard's MigrationBody, dispatches `migrate_cutover`. 404 on missing cluster, 404 on missing admin creds, 400 on validation, 409 cluster_busy.
- `POST /clusters/:id/migrate/rollback` — same body shape minus the snapshot requirement, dispatches `migrate_rollback`. 404/400/409 as above.
- The wire stays compatible with the wizard's existing `MigrationDraft` shape — `FromMigrationBody` picks the executor-relevant fields.

### Wiring (`cmd/bm/web.go`)

- `migration.Register(deps)` wires `CutoverExecutor` + `RollbackExecutor` into the tasks registry. Added alongside `operations.RegisterAll(...)` after the existing `deploy.Register(...)` block. New `OpKind` constants `OpMigrateCutover` / `OpMigrateRollback` live in `internal/tasks/types.go`.

### Tests

- `internal/migration/snapshot_test.go` — write/read round-trip + 0600 mode + Summarize counts + end-to-end against fake httptest admin/S3 endpoints.
- `internal/migration/cutover_test.go` — 1-host happy path, snapshot-missing rejection, 2-host with cluster-healthy wait between hosts. Uses the in-memory `internal/sshtest` server.
- `internal/migration/rollback_test.go` — full rollback (engine flips back) + no-op when buckit isn't active (uses a new `sshtest.Server.CmdOverride` hook to simulate the inactive probe).
- `internal/api/m8_integration_test.go` — full HTTP path: dispatch cutover → poll terminal → assert engine, then dispatch rollback → assert engine flips back. Includes 404 (missing cluster) and 400 (validation) cases.

### Milestone exit criteria

1. `make build` + `make test` clean (`-race -count=1 ./...`).
2. The wizard's Migrate step renders against the real backend: snapshot endpoint returns counts the Review step renders, cutover dispatch streams stage events over SSE.
3. Cluster row's `Engine` flips deterministically on success and on rollback. `MigratedFrom` is stamped on cutover, cleared on rollback.
4. The 5-platform cross-compile sanity loop in `CLAUDE.md` still passes.

### M8-local open questions

- **Snapshot file format versioning.** The on-disk JSON has no schema version field today — adding fields with `omitempty` keeps older files decodable, but a future breaking change would need an explicit `schemaVersion` int. Defer until a real schema break is needed.
- **Per-bucket smoke read.** `Verify` doesn't yet HEAD the largest bucket to confirm reads work post-cutover — `ObjectsSampled` is `0/0` today. Cheap to add when minio-go's anonymous read is already wired in for `bm cp` parity.
- **Parallel cutover.** Sequential is the M8 contract because the UI's per-node state machine assumes one host at a time. If operators want a parallel knob (à la `BM_DEPLOY_CONCURRENCY`), it lands as a future M8.5 alongside the corresponding wizard change.

## Open questions

1. **Where does Buckit console live for the deep-link button?** Today
   the UI assumes `cluster.url`/`cluster.consoleUrl`. Backend needs to
   capture and persist whatever the admin-info reports as the console
   address (and handle the case where it's empty).
2. **TLS at the admin API layer.** Many real clusters terminate TLS at
   a load balancer. `bm` needs to handle both `http://` and `https://`
   endpoints; for the latter, decide whether to require cert
   verification or allow `--insecure` per cluster.
3. **Cluster import discovery latency.** Discovery streams progress
   lines today (per-host detail). For clusters with many hosts the
   total time can be tens of seconds. Confirm SSE is the right
   transport vs. WebSocket vs. one-shot poll-and-return.
4. **`bm` ↔ `bm` coordination.** Two `bm` processes on the same machine
   contending for bbolt is fine for short-lived CLI calls, but a
   `bm web` server plus a long-running CLI op (e.g. `bm cluster
   migrate`) probably wants explicit coordination through a manager
   socket — not just bbolt locks. To revisit when M3 lands.
5. **Alias-file coexistence with an existing `mc` install.** If the
   operator already runs `mc` and has `~/.mc/config.json` populated,
   does `~/.config/bm/config.json` shadow it, supplement it, or stay
   isolated? Current design says isolated — `bm` reads its own file
   only. Revisit if user feedback says otherwise.
6. **Rebrand depth in the forked CLI.** Forked mc still says "mc" in
   error messages, help text, and the binary name. Decide once how
   aggressively to rebrand (all strings → "bm", or leave as-is with
   only the binary entry renamed). Affects upstream-pull friction —
   which we no longer pay anyway — and operator clarity.
