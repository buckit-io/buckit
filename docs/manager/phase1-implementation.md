# Buckit Manager (`bm`) — Phase 1 Implementation Plan

## Context

The Buckit ecosystem currently has no operator-friendly cluster
manager. Per [README.md](./README.md), `bm` is being introduced as
the operational control plane for Buckit deployments — a single
binary embedding a CLI, an HTTP API, and a web UI, with agentless
SSH orchestration over a shared application core. Phase 1
([phase1-web-ui.md](./phase1-web-ui.md)) scopes the first release to
two operator wizards driven by the web UI:

1. **Deploy a new Buckit cluster** to fresh hosts over SSH.
2. **Migrate an existing MinIO deployment to Buckit** via in-place
   binary swap on the same disks.

Phase 1 builds the foundation (`bm web` + embedded web UI +
task engine + SSH layer + state store). The richer CLI (Phase 2),
`mc` admin replacement (Phase 3), and broader object-client surface
(Phase 4) all reuse the same shared core and are explicitly out of
scope for this plan.

The work lives in a new, self-contained Go module at the repo-level
`bm/` directory (peer to `buckit/`).

## Stack (locked)

| Layer | Choice | Rationale |
|---|---|---|
| Language | Go (1.25, matching `buckit/`) | Single static binary, SSH client maturity, clean cross-compile |
| HTTP router | `github.com/go-chi/chi/v5` | Idiomatic `net/http`, mature middleware, minimal deps |
| Storage | `go.etcd.io/bbolt` with short-lived locks + flat task log files at `tasks/<id>.log` | Pure Go (~200 KB), transactional, CLI can read RO while server holds RW only during writes |
| Event stream | Server-Sent Events (`text/event-stream`) | One-way fits task logs; trivial in Go; proxy-friendly |
| Frontend | React 18 + Vite + TypeScript | Wizard density + table/topology UI; ecosystem includes `@tanstack/react-table`, `@tanstack/react-query`, `react-router` |
| Embedding | Go `embed.FS` of `web/dist` into the binary | Single artifact for release |
| SSH | `golang.org/x/crypto/ssh` + `github.com/pkg/sftp` | Standard, no shellouts to `ssh`/`scp` |
| Logging | `log/slog` (stdlib) | Structured logs, zero extra deps |
| Target binary size | ~10–12 MB | bbolt + small UI bundle |
| Target platforms | linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 (server mode primarily Linux) | Pure Go for clean cross-compile |

## Storage model

A `store.Store` interface in front of bbolt with the open-per-transaction
pattern:

```go
type Store interface {
    View(fn func(Tx) error) error    // RO open, multi-process safe
    Update(fn func(Tx) error) error  // RW open, server-only writer
    Close() error
}
```

Each call internally does `bbolt.Open(...) → txn → Close()`, with a
5s `Timeout` on `Open()` to avoid hangs under contention. The server
holds no lock when idle, so a `bm` CLI running read-only commands is
always safe alongside `bm web`. Phase 2+ CLI write commands route
through the HTTP API (per README "manager-backed operations") and
never open the bbolt file directly.

Bbolt buckets:
- `clusters` — cluster records (id → JSON-encoded `Cluster`).
- `nodes` — node records keyed by `<clusterID>/<nodeID>`.
- `node_facts` — discovery results keyed by `<clusterID>/<nodeID>`.
- `specs` — topology/plan specs keyed by cluster id.
- `tasks` — task metadata (id → `Task`).
- `task_index` — secondary index `(state, started_at) → taskID`.
- `audit` — append-only audit events keyed by ULID.
- `credentials` — SSH + admin credentials, AES-GCM encrypted with the data key (see below).
- `meta` — schema version, manager UUID, root admin hash.

**Data key bootstrap.** The AES-GCM key for the `credentials` bucket
resolves in this order on startup:

1. `BM_DATA_KEY` env (base64-encoded 32 bytes) — for operators who
   want the key out of the data dir entirely (mount from a secret
   manager, KMS, etc.).
2. `~/.config/bm/data.key` (`%APPDATA%\bm\data.key` on Windows) —
   a 32-byte file, mode `0600`.
3. Neither exists → generate a fresh 32-byte key from `crypto/rand`,
   write it to path #2 with mode `0600`, and log one line on startup
   noting the path so the operator knows to back it up alongside
   `bm.db`.

`bm` refuses to start if the file exists but has the wrong size or
world/group-readable permissions — fail loudly rather than silently
downgrading. Personal-tool framing: zero-friction default via
auto-gen, escape hatch via env for operators who care about
backup-leakage scenarios.

Task **logs** never go into bbolt. They stream to
`~/.config/bm/tasks/<task-id>.log` (rotated by size, capped per-cluster).
The task record stores a path + offset + size. SSE streams tail the
file directly.

## Repository layout (`bm/`)

```
bm/
├── go.mod                              module github.com/buckit-io/bm
├── go.sum
├── Makefile                            build, test, lint, web, package, release
├── README.md
├── cmd/bm/
│   └── main.go                         entry point; dispatches to subcommands
├── internal/
│   ├── app/                            shared application services (the "core")
│   │   ├── app.go                      App struct: wires store, tasks, ssh, deploy
│   │   ├── clusters.go                 cluster CRUD + draft state machine
│   │   ├── deploy.go                   new-cluster deploy workflow
│   │   ├── migrate.go                  MinIO migration workflow
│   │   ├── discovery.go                node discovery
│   │   ├── preflight.go                preflight checks
│   │   ├── topology.go                 topology planner & validator
│   │   └── health.go                   health probing
│   ├── api/                            HTTP layer
│   │   ├── server.go                   chi router + middleware chain
│   │   ├── auth.go                     local admin login, session cookies
│   │   ├── clusters.go                 /api/v1/clusters CRUD handlers
│   │   ├── wizards.go                  discover/preflight/deploy/migration endpoints
│   │   ├── tasks.go                    /api/v1/tasks + /events SSE
│   │   ├── settings.go                 /api/v1/settings
│   │   └── ui.go                       serves embedded React bundle
│   ├── store/                          bbolt persistence
│   │   ├── store.go                    Store interface + bbolt impl
│   │   ├── clusters.go                 typed accessors for cluster bucket
│   │   ├── nodes.go
│   │   ├── tasks.go
│   │   ├── audit.go
│   │   └── credentials.go              AES-GCM at rest
│   ├── tasks/                          task engine
│   │   ├── engine.go                   in-process runner, worker pool
│   │   ├── task.go                     Task model + state machine
│   │   ├── log.go                      log writer (file) + tailer (for SSE)
│   │   └── events.go                   pub/sub for live task events
│   ├── ssh/                            SSH execution
│   │   ├── client.go                   pooled clients, keyed by host+creds
│   │   ├── exec.go                     Run, RunStream, sudo wrapping
│   │   └── sftp.go                     file upload (binaries, env files)
│   ├── deploy/                         deploy workflow primitives
│   │   ├── package.go                  fetch buckit-*.rpm/.deb from GitHub Releases, cache
│   │   ├── install.go                  per-node: scp + dnf/apt/apk install
│   │   ├── envfile.go                  render /etc/default/minio
│   │   ├── systemd.go                  daemon-reload, enable/start, status
│   │   └── rollback.go                 used by migrate; reverses cutover
│   ├── cluster/                        domain models
│   │   ├── cluster.go                  Cluster, Pool, NodeRef, Status
│   │   ├── topology.go                 topology compute (set size, parity, usable)
│   │   └── minio.go                    MinIO admin probe (snapshot, validate)
│   ├── auth/                           local admin: hash, sessions, CSRF
│   │   └── auth.go
│   ├── config/                         server config (listener, TLS, paths)
│   │   └── config.go
│   └── version/                        build-time version metadata
│       └── version.go
├── web/
│   ├── package.json                    vite, react, @tanstack/*, react-router
│   ├── tsconfig.json
│   ├── vite.config.ts
│   ├── index.html
│   └── src/
│       ├── main.tsx
│       ├── routes.tsx                  react-router config
│       ├── api/                        typed fetch wrappers + react-query hooks
│       ├── components/                 see Component Inventory below
│       ├── pages/
│       │   ├── Login.tsx
│       │   ├── Welcome.tsx
│       │   ├── Clusters.tsx
│       │   ├── ClusterDetail.tsx
│       │   ├── NodeDetail.tsx
│       │   ├── Tasks.tsx
│       │   ├── TaskDetail.tsx
│       │   ├── Settings.tsx
│       │   └── wizards/
│       │       ├── NewCluster/         8 step components
│       │       └── MigrateMinio/       9 step components
│       └── styles/
├── web/embed.go                        //go:embed dist/* → fs.FS for api/ui.go
└── packaging/
    ├── nfpm.yaml                       bm rpm/deb (mirrors buckit's pattern)
    ├── bm.service                      systemd unit for bm server
    ├── install.sh                      thin POSIX installer
    └── install.ps1                     Windows installer
```

## Phase 1 deliverable scope

**In scope:**
- `bm web` running on a single host, with the embedded web UI.
- Local admin auth (single user, bcrypt password, cookie session).
- Cluster create + draft persistence.
- Add nodes + SSH credentials, reachability probe.
- Discovery: OS/CPU/RAM/disks/NICs/clock skew/existing services.
- Topology planner: pool, set size, parity (EC:2/3/4/6/8), capacity math.
- Preflight (new + migrate variants).
- Deploy: fetch package from GitHub Release, scp, install via dnf/apt/apk, write `/etc/default/minio`, `systemctl enable --now buckit`, health-wait.
- MinIO migration: snapshot via admin API, in-place sequential cutover, verify, rollback, finalize.
- Tasks Center: list, detail, live log via SSE, cancel, pause.
- Cluster detail: overview, nodes tab, node detail, services tab, settings tab.
- Manager settings: change admin password, view TLS cert metadata.
- Packaging: nfpm rpm/deb, `install.sh`, `install.ps1`.

**Explicitly out of scope (deferred):**
- Phase 2 CLI write commands — only `bm web`, `bm version`, `bm migrate-db`, and read-only `bm cluster ls` / `bm tasks ls` ship in Phase 1.
- Concurrent ("two at a time") migration rolling — sequential only.
- Optional remote-access mode (passcode + TLS) — design noted in `ui-architecture.md`, but the listener stays localhost-only in Phase 1.
- Multi-user / RBAC — `bm` is a personal tool; not on the roadmap.
- Kubernetes.
- `mc` admin replacement.
- Bucket browser, IAM editor, metrics (delegated to per-cluster Buckit console via "Open Buckit console" link).

## Implementation milestones

Each milestone is a vertically sliced, demoable chunk. Build in this
order so the UI always has working endpoints to call.

### M0 — Module bootstrap
- `go mod init github.com/buckit-io/bm`, Makefile (`build`, `test`, `lint`, `web`, `release`), `.golangci.yml`, baseline `cmd/bm/main.go` printing `bm version`.
- `web/` Vite+React+TS scaffold; dev server proxies `/api/*` to `localhost:9443`.
- CI placeholder matrix (linux/darwin/windows × amd64/arm64).

### M1 — Storage + server shell
- `internal/store` bbolt impl with the bucket layout above and short-lived-lock pattern. Data dir at `~/.config/bm/` (`%APPDATA%\bm\` on Windows).
- `internal/api` chi router, middleware (logging, recover), `/api/v1/healthz`. Bind defaults to `127.0.0.1:9443`.
- `internal/config` reads optional `bm.yaml` + env overrides.
- `bm web` subcommand: starts the listener, opens the default browser unless `--no-browser`, runs in foreground; Ctrl-C exits with a confirm prompt if any task is in flight.
- No auth in default localhost mode. Optional remote-access mode (passcode + TLS, opt-in via Settings) deferred to a later milestone.

### M2 — Task engine + SSE
- `internal/tasks`: in-process worker pool, `Task` model with state machine (`pending → running → succeeded|failed|canceled`), step substructure, pause/cancel signals.
- Log writer streams to `tasks/<id>.log`; in-memory ring buffer feeds late subscribers without re-reading the file.
- `/api/v1/tasks` (list/get) + `/api/v1/tasks/:id/events` (SSE).
- Frontend: Tasks page + Task Detail page + `TaskLogStream` component wired against SSE.

### M3 — SSH layer + node CRUD
- `internal/ssh`: client pool, key/agent/password auth, sudo wrapping, `Run`, `RunStream`, `Upload`.
- `internal/api` cluster endpoints (create draft, patch, add nodes).
- `NodeTable` + `SSHCredentialsForm` UI components.
- N2/M2 wizard steps: add hosts, parse paste-list, SSH reachability probe.

### M4 — Discovery
- `internal/app/discovery`: parallel SSH fact collection (OS, kernel, CPU, RAM, NICs, disks via `lsblk -J`, time via `date +%s`, existing services via `systemctl is-active minio buckit`).
- Discovery runs as a task; results write to `node_facts`.
- N3/M3 wizard steps: live per-row progress, expandable detail panel.

### M5 — Topology + preflight
- `internal/cluster/topology` compute and validation (uniform drives/node, set size divides total, capacity & tolerance).
- `internal/app/preflight` checks; new-cluster and migration variants.
- N4, N5, M6 wizard steps; `TopologyBuilder`, `PreflightTable` components.

### M6 — New-cluster deploy
- `internal/deploy/package`: GitHub Release fetcher with cache at `/var/lib/bm/cache/`, sha256 verify.
- `internal/deploy/install`: detect package manager (dnf/apt/apk), scp + install, fallback to raw binary + manager-written unit.
- `internal/deploy/envfile`: render `/etc/default/minio` with `MINIO_VOLUMES`, `MINIO_OPTS`, generated `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD`.
- `internal/deploy/systemd`: daemon-reload, enable --now, health probe `http://node:9000/minio/health/live`.
- N6 Review, N7 Deploy (task-backed live view), N8 Done with one-time credential reveal.

### M7 — Cluster operations
- Health probe job (per-cluster, default 30s).
- Cluster detail Overview, Nodes, Node Detail, Services tab (rolling restart reuses task engine).
- "Open Buckit console" deep-link.
- Cluster Settings tab; root credential rotation task.

### M8 — MinIO migration
- `internal/cluster/minio`: MinIO admin API probe — gather snapshot of buckets, IAM, bucket configs, replication targets.
- M1–M7 wizard steps.
- Cutover task (sequential): stop minio → scp+install buckit pkg → disable minio → enable+start buckit → node-healthy probe → cluster-healthy gate → next node.
- Verify task: re-snapshot + diff against pre-snapshot + smoke PUT/GET/DELETE.
- Rollback task: reverse order, re-enable `minio.service`.
- Finalize task: `dnf/apt remove minio`, snapshot archived, status → Active.

### M9 — Packaging + installers + embed
- `web/embed.go` with `//go:embed dist/*`.
- Per-user install: Homebrew tap (`buckit-io/tap` → `bm`), Scoop manifest, direct binary downloads. No system-wide install, no systemd unit, no `bm` system user — `bm` is a personal-tool binary.
- Goreleaser config or Makefile target producing checksummed artifacts for all 5 target platforms.

## Progress

Canonical state for resuming work in a new session. Last updated
**2026-05-15**. The git repo (`git@github.com:buckit-io/bm.git`,
branch `main`) holds everything checked off below.

### ✅ Completed

**Documentation** (in `buckit/docs/manager/`)
- `phase1-implementation.md` — this doc
- `ui-architecture.md` — full spec for the data flow, fetch pipeline,
  REST contract sketch, the `Cluster`/`Node`/`Task`/`HistoryEntry`/
  `HealthSummary` types, the `Extending Buckit` (HostInfo) proposal,
  and per-page details for **Clusters list**, **Cluster detail**,
  **History**, and **Manager Settings**

**M0 — Module bootstrap**
- Go module at `github.com/buckit-io/bm`, Makefile, `.golangci.yml`,
  baseline `cmd/bm/main.go`
- `bm version` and `bm help` land; `bm web` (and the `bm server`
  alias) are stubs printing "not yet implemented (M1)"
- `web/` Vite + React + TypeScript scaffold with `@tanstack/react-query`,
  `@tanstack/react-table`, `react-router-dom`
- CI workflow placeholder: matrix builds for linux/darwin/windows ×
  amd64/arm64, plus web typecheck + build

**Web UI prototype** (clickable end-to-end on top of an in-memory
mock layer at `web/src/mock/{data.ts,api.ts}` + `web/src/api/hooks.ts`)
- **UI-P0** — Routing, layouts (`AppShell`, `WizardShell`), design
  tokens (light + dark via `prefers-color-scheme`), `Pill`,
  `Stepper`, `TaskStateIcon` primitives
- **UI-P1** — Welcome, Clusters list (with Refresh button +
  `Fetched Ns ago` staleness display, multi-pool support, severity-
  coloured numerator on Nodes/Drives ratios), Manager Settings
  (rebuilt as Preferences / Storage / Remote access / About per the
  personal-tool framing — auth + TLS + audit retention sections
  intentionally dropped). Login route exists but isn't in the default
  flow; lands when remote-access mode flips on
- **UI-P2** — Tasks Center, Task Detail, `TaskLogStream`,
  `TaskStepsTimeline` with synthetic SSE-shaped log ticking
- **UI-P3** — Cluster detail. **Major redesign mid-prototype:**
  collapsed from a tabbed layout to a single monitoring + ops page
  centred on the node table. Per-pool health card with severity-
  ordered truncation (worst pools always visible; expand to see all),
  cluster Actions menu (Rolling restart / Rolling upgrade / Stop all /
  Start all / Rotate root creds / Tear down). Node table has 4 probe
  columns (Ping / SSH / S3 API / Console), per-column filters with
  text inputs for Host + Kernel and dropdowns for Pool / State /
  Version, sortable headers (default Pool asc → hostname asc within
  pool, with stable tiebreaker on every other sort key), multi-select
  + always-on bulk action bar (Restart buckit.service / Redeploy /
  Reboot host / Shut down host) disabled when SSH not configured or
  selection empty. Node Detail page has System / Hardware / Service /
  Connectivity cards
- **UI-P4** — New Cluster wizard (8 steps: Basics, Add Nodes,
  Discovery, Topology, Preflight, Review, Deploy, Done) with
  animated mock progress
- **UI-P5** — MinIO Migration wizard (9 steps: Basics, Add Nodes,
  Discovery, Snapshot, Plan, Preflight, Cutover, Verify, Finalize)
  with typed-confirm Finalize modal
- **History** tab — added beyond original P-tasks scope. Records
  literal CLI commands (future) and UI action descriptions (current);
  filter chips + target dropdown + search + Copy on CLI rows + jump-
  to-task on rows with a `taskId`. Wired up so cluster Actions menu
  + per-host bulk actions + Services-style buttons all append rows
- **Mock data computes derived state** — `computeHealthSummary` +
  `computeHealth` + per-pool rollup live in `web/src/mock/data.ts`
  and are the reference implementation that ports to Go in M1+
- **Cross-cutting fixes** — every `padding: 0, overflow: "hidden"`
  table card switched to a shared `.card--table` utility class with
  `overflow-x: auto` so wide tables scroll horizontally instead of
  clipping at narrow viewports

### ⬜ Not yet started

**Backend implementation milestones** (M1–M9 — see [Implementation
milestones](#implementation-milestones) above for full scope):
- M1 — Storage + server shell *(next; bbolt + chi + `bm web` foreground process; localhost-only by default; remote-access mode deferred)*
- M2 — Task engine + SSE
- M3 — SSH layer + node CRUD
- M4 — Node discovery
- M5 — Topology + preflight
- M6 — New-cluster deploy
- M7 — Cluster operations
- M8 — MinIO migration
- M9 — Packaging + installers + embed

**Web UI prototype**
- UI-P6 — Cross-cutting polish: skeleton loaders, lost-connection
  banner, full keyboard nav through wizards, a11y pass on status
  pills/icons. Deferred to the end of the prototype phase
- UI-P7 — Derive backend API contract from the prototype. **Largely
  already captured** in `ui-architecture.md` (REST contract sketch,
  type shapes, per-page details for Clusters list, Cluster detail,
  History, Manager Settings). Per-page docs for the wizards
  (`/clusters/new`, `/clusters/migrate`) and for Tasks Center +
  Task Detail are stubbed in the doc and need to be filled in
- **TODO — Add new pool wizard.** The `Actions ▾ → Add new pool…`
  menu item is wired up on `/clusters/:id` (`ClusterDetail.tsx`
  `CLUSTER_ACTIONS`) but currently only records a History row. A
  proper wizard is still to be built. Sketch: reuses the New Cluster
  wizard's Add Nodes → Discovery → Topology → Preflight steps, then
  a Cutover step that rolling-restarts every node with an expanded
  `MINIO_VOLUMES` envfile. Constraints worth calling out in the doc
  once it's drafted: the new pool's per-set drive layout must match
  existing pools (uniform set size), parity is fixed cluster-wide
  and not re-selectable, and the rolling restart is mandatory because
  `MINIO_VOLUMES` is env-only (see `request-flow.md` § 10.1). Backend
  side belongs in a post-Phase-1 milestone — not part of M1–M9 today.
- **TODO — M5 preflight: detect stale `.minio.sys/format.json`.**
  When a deploy reuses a drive from a prior Buckit/MinIO deployment
  (different deployment ID), the server refuses to start with a
  cryptic error. Preflight should SSH into each host and `test -f
  <mount>/.minio.sys/format.json` for every chosen mountpoint, then
  parse the file's `id` (deployment ID). If any drive's deployment ID
  is set and doesn't match an empty / about-to-be-created cluster,
  surface a warning row: *"`<mount>` on `<host>` already belongs to a
  different deployment. The server will refuse to start. Wipe with
  `rm -rf <mount>/.minio.sys` (destroys data) or pick a different
  drive."* High-frequency support issue in practice — operators who
  retry after a failed deploy hit this. Belongs in M5 alongside the
  existing preflight checks; not a separate milestone.
- **TODO — Auto-save bm alias on successful deploy / import.** The
  New Cluster Done step and the Import flow's success path both
  promise that a `bm` alias has been saved to the operator's
  `~/.bm/config.json` (the prototype's UI copy says so). This is
  `bm`'s own alias file — separate from `mc`'s `~/.mc/config.json` —
  consumed by future `bm alias`/`bm admin …` CLI subcommands. Backend
  behavior `bm` should implement when M6 lands:
  - Resolve config dir: `BM_CONFIG_DIR` env, else `~/.bm/`
    (`%USERPROFILE%\.bm\` on Windows). Create the dir mode `0700` if
    missing. (Distinct from the bbolt data dir at `~/.config/bm/`;
    the alias file is operator-facing, the bbolt is manager-internal.)
  - Read existing `config.json` or initialize the skeleton if absent.
    Schema mirrors mc's v10 shape for familiarity:
    `{ "version": "1", "aliases": { "<alias>": { url, accessKey, secretKey, api: "S3v4", path: "auto" } } }`.
  - **Alias key is derived from the cluster name** — slugified to
    `[a-z0-9-]{1,32}` (lowercase, non-alphanumeric → `-`, collapse
    repeats, trim ends). Cluster display name stays free-form;
    `My Production East!` becomes alias `my-production-east`.
    Implemented at the Done step (`toAliasName` in
    `pages/wizards/new/steps/Done.tsx`) — port the same rule when M6
    writes config.json.
  - Atomic write: write `config.json.tmp` then rename. Mode `0600`.
  - If an alias with the same name already exists pointing at a
    different URL, prompt the operator before overwriting (likely an
    unrelated collision — they probably don't want it clobbered).
  - The Done step's "Run on another machine" disclosure stays as the
    fallback for operators whose `bm` lives on a different host.
- **TODO — Drive preparation wizard (post-Phase-1).** The Topology
  step today bails to *"mount drives consistently and re-run
  discovery"* when no common mountpoints exist (case C). A proper
  flow would let bm format and mount raw drives during deploy with
  passwordless sudo. Out of scope for Phase 1; in scope for a
  follow-up milestone (call it M6.5). Safety requirements before this
  ships:
  - Refuse to prep any drive with an existing filesystem signature
    unless the operator explicitly types the device path to wipe.
    `wipefs` on the wrong drive is unrecoverable.
  - Require passwordless sudo. If absent, the wizard is unavailable.
  - Show every command bm will run, per host, before the operator
    confirms (matches the History tab's literal-CLI treatment).
  - Typed-confirm modal (cluster name or `PREPARE DRIVES`) before
    any destructive command runs.
  - Anchor `/etc/fstab` entries by `UUID=` not device name, so reboot
    + drive-letter shuffles don't break the deployment.
  - Default mountpoint pattern is `/data/disk{1..N}`. Default fs is
    XFS (MinIO recommendation). Operator can override both.
  - Only a single path in the codebase ever touches a raw block
    device. No other M6/M7/M8 operations call `wipefs`/`mkfs`.

### Notes for resuming

- **Next chunk of work** is M1. Suggested first PR: `internal/store`
  (bbolt with the open-per-transaction pattern), `internal/config`
  (load `~/.config/bm/`), and `cmd/bm/web.go` that opens
  `127.0.0.1:9443` with chi serving `/api/v1/healthz` and the
  embedded UI. Auth is intentionally not part of M1 anymore — it's
  optional and only kicks in when remote access is enabled (later
  milestone).
- **The mock layer is the API contract.** When implementing the real
  backend, `web/src/mock/api.ts` enumerates exactly what shape each
  endpoint needs to return. Swapping it for a `fetch`-based real
  client is the planned cutover.
- **Reference implementations in TypeScript** — `computeHealthSummary`,
  `computeHealth`, `summarizePools`, `compareNodes` (sort comparator
  with stable tiebreaker) all live in `web/src/mock/` and need to be
  ported to Go in M1+. Behaviour is well-exercised by the prototype.
- **The prototype does not yet use** the proposed `madmin-go` `HostInfo`
  fork — fields like `cpuModel`, `kernel` are populated directly in
  the mock fixture. When the real backend lands, the merge step in
  `internal/app/refresh.go` reads these from `/minio/admin/v3/info`'s
  `Host` substruct (see `ui-architecture.md` § "Extending Buckit to
  return host info" for the proposal).
- **Doc anchors to keep in sync** when behaviour changes: the
  "Per-page details" sections in `ui-architecture.md` and this doc's
  Progress section.



Nothing is imported from the `buckit/` module — `bm` is a separate
module that talks to `buckit` over SSH and HTTP, never as a library.
What we do **consume** from buckit:

- **Release artifacts** produced by [`buckit/packaging/nfpm.yaml`](../../packaging/nfpm.yaml)
  and [`buckit/packaging/buckit.service`](../../packaging/buckit.service)
  — `bm` fetches these from the GitHub Release for the target version
  and installs them on nodes. The unit file's
  `EnvironmentFile=-/etc/default/minio` contract is what `bm` writes against.
- **Health endpoint** `http://node:9000/minio/health/live` for the
  post-install probe.
- **MinIO admin API** surface (used during migration snapshot/verify).

We deliberately do **not** import any `buckit/internal/*` package —
that would couple the manager to the object server's Go API and
break the "manager around / console inside" separation in the spec.

## Verification

End-to-end test plan once milestones are landed:

1. **Build + binary size check**
   ```sh
   cd bm && make build
   ls -lh bm                # confirm 10-12 MB target
   ./bm version
   ```

2. **Local smoke (no real cluster needed)**
   ```sh
   BM_INITIAL_ADMIN_PASSWORD=admin ./bm server --listen :9443
   ```
   Browse `https://localhost:9443`, log in, see Welcome screen.
   Frontend dev mode: `cd web && npm run dev`; verify Vite proxy works.

3. **Unit tests** — `make test` covers store, topology, envfile rendering, MinIO snapshot diff, task state machine. Integration tests can use `testcontainers-go` to spin a real `buckit` container for health-probe and unit-file contract tests.

4. **New-cluster wizard against local VMs** — 3-VM Vagrant or multipass lab (Ubuntu 24.04, RHEL 9, Alpine — exercises all three package managers). Run the new-cluster wizard end-to-end via the UI. Verify `/etc/default/minio` exists, `buckit.service` is active, `mc admin info` against `node1:9000` succeeds.

5. **MinIO migration against a real MinIO lab** — 4-node MinIO setup with 12 buckets, IAM users, lifecycle rules. Run migration wizard; confirm verify reports parity; finalize. Confirm `dnf list installed minio` returns empty post-finalize. Test rollback on a parallel 4-node cluster: cutover 2 nodes, click Rollback, confirm `minio.service` is back up.

6. **CLI-vs-server coexistence** — with `bm web` running and an active deploy task, run `bm cluster ls` and `bm tasks ls` from another shell. Confirm both return promptly (< 100 ms) and show consistent state — proves the bbolt short-lived-lock model works under load.

7. **Cross-compile sanity** — from a single host:
   ```sh
   GOOS=linux   GOARCH=arm64 go build ./cmd/bm
   GOOS=darwin  GOARCH=arm64 go build ./cmd/bm
   GOOS=windows GOARCH=amd64 go build ./cmd/bm
   ```
   All should succeed without CGO.

## Deferred decisions (from README "Open Questions")

These do not block Phase 1 but should be revisited before Phase 2:

- **SSH credential rotation policy** — Phase 1 stores SSH creds AES-GCM at rest using `BM_DATA_KEY`. Rotation UX lives in Cluster Settings → "Rotate" and re-encrypts in place. KMS-backed key is Phase 3.
- **Multi-user / RBAC** — single local admin in Phase 1. Auth layer is structured (`auth.User`, `auth.Session`) so adding user/role tables later is incremental.
- **Postgres backend** — `store.Store` interface keeps this open; not implemented in Phase 1.
- **"Two-at-a-time" migration concurrency** — sequential only in Phase 1, per the open question in `phase1-web-ui.md`.
