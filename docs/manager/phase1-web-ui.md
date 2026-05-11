# Buckit Manager — Phase 1 Web UI Design

## Background

The Buckit Manager (`bm`) is the operational control plane for Buckit
deployments. The full design — single binary with CLI, HTTP API, and embedded
web UI frontends; agentless SSH orchestration; durable task model — is
described in [README.md](./README.md). This document covers the **web UI** for
the first phase of that design.

### Problem and scope

The MinIO ecosystem has no operator-friendly cluster manager. `mc admin`
exposes some cluster-scoped operations (upgrade, restart, decommission) but
is awkward to drive and doesn't help with installation or deployment at all.
The per-cluster Buckit web console — served by the `buckit` binary itself —
covers in-cluster operations like bucket browsing, IAM, and metrics.
Everything else, including bootstrap and deploy, falls to hand-rolled
Ansible or shell scripts.

`bm` fills that gap. The console is the layer **inside** a cluster; `bm`
is the layer **around** it. Phase 1 invests in the two things only `bm`
can do: produce a new cluster, and migrate from MinIO.

## Purpose

This document specifies the Phase 1 web UI for `bm server`. It is scoped to two  
primary user wizards:

1. **Deploy a new Buckit cluster** to a set of fresh hosts.
2. **Migrate an existing MinIO deployment to Buckit** via in-place binary swap.

Once a cluster exists, the UI also provides the surfaces to operate it:
cluster list, cluster detail, node detail, and a tasks center.

This is a wireframe-level specification. It defines screens, layout regions,
primary components, and state transitions. It does not prescribe visual styling
or final copy.

## Audience

Operators deploying Buckit to bare metal or VMs over SSH, and operators
migrating from MinIO. Single local admin account; no multi-user workflows.

## Site Map

```text
/
├── /welcome                 first-run, no clusters exist
├── /clusters                cluster list (default landing after onboarding)
├── /clusters/new            wizard: deploy a new Buckit cluster
├── /clusters/migrate        wizard: migrate from MinIO (in-place)
├── /clusters/:id            cluster detail
│   ├── /overview            health, capacity, version, recent activity
│   ├── /nodes               node table
│   ├── /nodes/:nodeId       per-node detail
│   ├── /services            service controls (start/stop/rolling restart)
│   ├── /tasks               cluster-scoped tasks
│   └── /settings            cluster-scoped settings (SSH creds, version pin)
├── /tasks                   global task center
├── /tasks/:id               task detail + live log
├── /settings                manager settings (admin, TLS, audit)
└── /login                   local admin login
```

Both wizards share `/clusters/new` and `/clusters/migrate` as separate routes
but reuse the same underlying step components where possible (node entry,
discovery, preflight, deploy log).

## Global Chrome

All authenticated pages outside the wizards share the same shell.

```text
┌────────────────────────────────────────────────────────────────────────────┐
│  [Buckit Manager]      Cluster: [prod-east ▾]      [⟳ 2 tasks]   [admin ▾]│
├──────────┬─────────────────────────────────────────────────────────────────┤
│          │                                                                 │
│ Clusters │                                                                 │
│ Tasks    │                          {page content}                        │
│ Settings │                                                                 │
│          │                                                                 │
│ ─────    │                                                                 │
│ Docs ↗   │                                                                 │
│ v0.1.0   │                                                                 │
└──────────┴─────────────────────────────────────────────────────────────────┘
```

- **Top bar**: product name, active cluster selector (also acts as a
breadcrumb when inside a cluster), running-task badge (click → task center),
user menu.
- **Left sidebar**: top-level navigation. Collapsible on small viewports.
- **Task badge**: animated when ≥1 task is `running`. Hover shows a 3-item
popover with the most recent active tasks.

The wizards (`/clusters/new`, `/clusters/migrate`) replace the global chrome
with a wizard-specific chrome (see below) to keep focus on the linear flow.

## Wizard Chrome

```text
┌────────────────────────────────────────────────────────────────────────────┐
│  Deploy a new Buckit cluster                              [✕ Save & exit] │
├────────────────────────────────────────────────────────────────────────────┤
│  ① Basics — ② Nodes — ③ Discover — ④ Topology — ⑤ Preflight — …          │
├────────────────────────────────────────────────────────────────────────────┤
│                                                                            │
│                          {step content area}                              │
│                                                                            │
├────────────────────────────────────────────────────────────────────────────┤
│  [← Back]                                              [Save draft] [Next →]│
└────────────────────────────────────────────────────────────────────────────┘
```

- **Stepper**: current step is bold; completed steps are clickable to revisit;
future steps are disabled until prerequisites are met.
- **Save & exit**: persists the draft cluster row and returns to `/clusters`.
Drafts are visible in the cluster list with a `Draft` badge.
- **Save draft**: explicit save without exit (auto-save also fires on every
step transition).
- The wizard never blocks navigation away mid-step — leaving stores partial
state.

## Screen Catalog

### Screen 0 — Login

```text
┌─────────────────────────────────────┐
│         Buckit Manager              │
│                                     │
│   Username  [____________________]  │
│   Password  [____________________]  │
│                                     │
│              [   Sign in   ]        │
│                                     │
│   First time? See setup guide ↗     │
└─────────────────────────────────────┘
```

- Local admin only in Phase 1.
- Failed login: inline error under the form, no enumeration of which field
was wrong.
- After login, redirect target depends on state:
  - 0 clusters → `/welcome`
  - ≥1 cluster → `/clusters`

### Screen 1 — Welcome / First Run

Shown when the manager has no clusters and no drafts. This is the entry point
for both journeys.

```text
┌─────────────────────────────────────────────────────────────────────┐
│  Welcome to Buckit Manager                                          │
│                                                                     │
│  Let's get your first cluster running.                              │
│                                                                     │
│  ┌──────────────────────────┐    ┌──────────────────────────┐       │
│  │  🟦                       │    │  🟧                       │       │
│  │  Deploy a new cluster     │    │  Migrate from MinIO       │       │
│  │                           │    │                           │       │
│  │  Install Buckit on fresh  │    │  Replace an existing      │       │
│  │  hosts and form a new     │    │  MinIO deployment with    │       │
│  │  cluster over SSH.        │    │  Buckit on the same       │       │
│  │                           │    │  disks (in-place swap).   │       │
│  │  [ Get started → ]        │    │  [ Get started → ]        │       │
│  └──────────────────────────┘    └──────────────────────────┘       │
│                                                                     │
│  Need help? See the install guide ↗                                  │
└─────────────────────────────────────────────────────────────────────┘
```

- Two equally-weighted entry cards.
- "Migrate from MinIO" card includes a small `In-place` chip to set
expectations on the strategy.

### Screen 2 — Clusters List

Default landing page after at least one cluster (or draft) exists.

```text
┌────────────────────────────────────────────────────────────────────────────┐
│  Clusters                                          [+ New ▾]              │
│                                                    ├ New cluster          │
│                                                    └ Migrate from MinIO   │
├────────────────────────────────────────────────────────────────────────────┤
│  NAME           NODES   VERSION    HEALTH        LAST ACTIVITY     STATUS │
│  prod-east        6     v1.0.0     ● Healthy     12m ago          Active  │
│  staging          4     v1.0.0     ● Healthy     2h ago           Active  │
│  prod-west-new    —     —          ○ —           5m ago           Draft   │
│  legacy-migrate   8     v1.0.0     ⚠ Degraded    just now         Migrating│
└────────────────────────────────────────────────────────────────────────────┘
```

- Row click → cluster detail.
- Draft rows: click resumes the wizard at the last completed step.
- Migrating rows: link goes to the running migration wizard.
- `+ New` is a split button so both flows are one click from this page.
- Filter chips above table: `All / Active / Draft / Migrating / Failed`.

## New Cluster Wizard

Route: `/clusters/new`. Eight steps.

### N1 — Basics

```text
Cluster name   [prod-east_____________________]
Description    [Customer-facing production_____]
Intended use   ◉ Production    ○ Staging    ○ Dev/Test
Buckit version [v1.0.0 (latest stable) ▾]
```

- Name validation: DNS-safe, unique within the manager.
- Version selector lists known release tags from the manager's release index.
An advanced toggle reveals "Custom URL" for air-gapped installs.

### N2 — Add Nodes

```text
Add hosts                                                  [Paste list]
┌───────────────────────────────────────────────────────────┐
│ HOSTNAME / IP            SSH PORT    LABEL (optional)     │
│ [node1.example.com_____] [22___]     [_________________] ✕│
│ [node2.example.com_____] [22___]     [_________________] ✕│
│ [+ Add row]                                               │
└───────────────────────────────────────────────────────────┘

SSH credentials
○ Use SSH agent (recommended)
◉ Upload private key      [ Choose file ] (no key uploaded)
○ Password                 [____________________]

SSH user                  [buckit____________________]
Privilege escalation      ☑ Use sudo (passwordless)
```

- "Paste list" opens a textarea accepting one host per line; parses into rows.
- Credentials are entered once per wizard; per-node overrides available via
a row expander.
- "Next" runs an SSH reachability probe in parallel and shows per-row
status pills (`✓ Reachable` / `✗ Auth failed` / `✗ Timeout`). Cannot proceed
with any failing rows except by removing them.

### N3 — Discovery

```text
Discovering nodes…  ▓▓▓▓▓▓▓▓▓░░░  6 / 8 complete

NODE                OS              CORES   RAM    DISKS              STATUS
node1.example.com   Ubuntu 24.04    16      64Gi   12 × 16Ti          ✓ Done
node2.example.com   Ubuntu 24.04    16      64Gi   12 × 16Ti          ✓ Done
node3.example.com   Ubuntu 24.04    16      64Gi   12 × 16Ti          ⟳ …
...
node8.example.com   —               —       —      —                  ✗ Timeout
                                                                       [Retry]
```

- Per-node row expands to show: kernel version, NIC + advertised speed,
free space per disk, time skew vs manager, listening ports, whether a
prior `buckit` or `minio` binary/service was detected.
- A node with detected existing services shows a `⚠ Existing service` chip
and the user must explicitly acknowledge "stop and replace" or remove the
node.
- Discovery is a backend task — the page is a live view of that task. Closing
the tab does not stop it.

### N4 — Topology

The most opinionated step. The UI computes a default layout and lets the
operator adjust.

```text
Proposed topology

Pool 1
  Nodes       8
  Drives/node 12     (□ select drives)
  Set size    16     (recommended for 8×12 layout)
  Parity      EC:4   (◉ default  ○ EC:2  ○ EC:3  ○ EC:6  ○ EC:8)
  Usable     ~864 Ti out of 1.15 Pi raw

[+ Add another pool]

Disk selection (click "select drives" above to override)
┌──────────────────────────────────────────────────────────┐
│ NODE             DRIVES                                   │
│ node1   ☑ /dev/sda  ☑ /dev/sdb  …  ☐ /dev/sdm (boot)     │
│ node2   ☑ /dev/sda  ☑ /dev/sdb  …                         │
└──────────────────────────────────────────────────────────┘

⚠ Boot drives are excluded by default.
ℹ Parity EC:4 tolerates loss of up to 4 drives per set.
```

- Real-time recompute: changing parity updates usable capacity and tolerance.
- Validation: drive count per node must be uniform within a pool; set size
must divide total drive count; mount paths must be writable (verified by
discovery).
- Advanced disclosure for: storage class config, custom mount path template,
per-pool networking interface.

### N5 — Preflight

```text
Running preflight checks…

CHECK                                NODES         RESULT
SSH reachability                     8 / 8         ✓ Pass
Sudo (passwordless)                  8 / 8         ✓ Pass
Time sync (skew < 1s)                8 / 8         ✓ Pass
Free space on selected drives        8 / 8         ✓ Pass
Inter-node port reachability         8 / 8         ✓ Pass
   ↳ Verified ports: 9000, 9001
DNS / hostname resolution            7 / 8         ⚠ Warning
   ↳ node5 cannot resolve node8.example.com
Package manager available (dnf)      8 / 8         ✓ Pass
buckit-1.0.0.rpm reachable           ✓ Pass
   ↳ https://github.com/buckit-io/buckit/releases/...
Existing buckit package              0 / 8         ✓ Pass
Existing minio service               1 / 8         ⚠ Warning
   ↳ node3 has minio installed but not running
Conflicting listeners on 9000/9001   8 / 8         ✓ Pass
Kernel ulimit (nofile ≥ 65536)       8 / 8         ✓ Pass

[Re-run]                                       [Continue with warnings]
```

- Failures block; warnings require acknowledgment to continue.
- Per-row expansion shows raw output captured from the check command.

### N6 — Review

```text
Review your plan
                                                       [Download plan ⤓]

Cluster: prod-east   (v1.0.0)
8 nodes · 1 pool · 96 drives · EC:4 · ~864 Ti usable

Install method: dnf install   (RHEL 9 detected on all 8 nodes)
                              [Override for individual nodes…]

Systemd unit (provided by buckit-1.0.0.rpm)
┌──────────────────────────────────────────────────────────┐
│ [Unit]                                                   │
│ Description=Buckit Object Storage                        │
│ After=network-online.target                              │
│ ...                                                      │
│ EnvironmentFile=/etc/default/minio                       │
│ ExecStart=/usr/local/bin/buckit server $MINIO_OPTS \     │
│   $MINIO_VOLUMES                                         │
│ User=buckit                                              │
│ Group=buckit                                             │
└──────────────────────────────────────────────────────────┘

Environment file written by manager (/etc/default/minio)
┌──────────────────────────────────────────────────────────┐
│ MINIO_ROOT_USER=…    (auto-generated, shown post-deploy) │
│ MINIO_ROOT_PASSWORD=…                                    │
│ MINIO_VOLUMES="https://node{1...8}.example.com/data/...  │
│ MINIO_OPTS="--console-address :9001"                     │
└──────────────────────────────────────────────────────────┘

ℹ The unit, binary, and buckit user/group come from the package.
  The manager writes only the env file at /etc/default/minio (same path
  used by migrated nodes — they remain byte-identical at the config
  layer). The path and MINIO_* var names are kept MinIO-compatible.

What will happen
  1. Manager fetches buckit-1.0.0.rpm from GitHub Release    (one-time, cached)
  2. scp the rpm to each node                                (~3s/node parallel)
  3. ssh node 'dnf install -y /tmp/buckit-1.0.0.rpm'
       Package provides: /usr/local/bin/buckit,
                         /lib/systemd/system/buckit.service,
                         buckit user and group
  4. Manager writes /etc/default/minio with cluster values
  5. systemctl daemon-reload && enable --now buckit
  6. Wait for cluster health-ready (timeout 5m)

☑ I have backed up any existing data on selected drives.
```

- "Download plan" exports the full plan as a YAML file for offline review.
- Root credentials are generated server-side and revealed only on the Done
screen.

### N7 — Deploy

```text
Deploying prod-east                          ● Running · started 1m23s ago

Overall progress  ▓▓▓▓▓▓▓░░░░░  62 %

NODE             STATE                  ELAPSED   LAST EVENT
node1            ✓ Service healthy      1m05s     Started buckit.service
node2            ✓ Service healthy      1m08s     Started buckit.service
node3            ⟳ Starting service     58s       systemctl start buckit
node4            ⟳ Writing config       42s       Wrote /etc/buckit/buckit.env
node5            ⟳ Installing binary    21s       Extracted to /usr/local/bin
node6            ⟳ Installing binary    20s       Extracted to /usr/local/bin
node7            ⟳ Downloading          15s       Fetching v1.0.0 (45 MiB)
node8            ⟳ Downloading          15s       Fetching v1.0.0 (45 MiB)

Live log [Filter: all ▾]                                        [⏸ Pause]
┌──────────────────────────────────────────────────────────────────┐
│ 12:03:18 node1  ✓ systemctl start buckit                         │
│ 12:03:19 node1  ✓ health probe http://localhost:9000/minio/health│
│ 12:03:20 node2  ✓ systemctl start buckit                         │
│ 12:03:21 node3  $ systemctl daemon-reload                        │
│ ...                                                              │
└──────────────────────────────────────────────────────────────────┘

[Cancel deploy]
```

- Log stream via SSE/WebSocket from the task engine.
- Per-node row click filters log to that node.
- "Cancel deploy" triggers a controlled abort that stops in-flight steps but
does not roll back completed nodes (manual cleanup required; surfaced in a
banner with a follow-up "Tear down what was installed" task).

### N8 — Done

```text
✓ prod-east is up

Console URL          https://node1.example.com:9000
Root username        admin                              [Copy]
Root password        s3cr3t-generated-value-here        [Copy]  [Reveal]
Recommended next step
  bm alias set prod-east https://node1.example.com:9000 admin <password>
                                                                  [Copy]

Quick checks
  ● 8 / 8 nodes healthy
  ● 1 / 1 pool online
  ● Read/write smoke test passed

[Go to cluster overview]
```

- Root credentials shown exactly once; subsequent visits replace with
"Credentials previously revealed at  · [Rotate]".
- "Quick checks" runs three lightweight probes; failures here surface a
banner with a "Run remediation" task.

## MinIO Migration Wizard

Route: `/clusters/migrate`. Nine steps. Strategy is **in-place binary swap
only**.

### M1 — Basics

Same as N1, with the addition of a banner explaining the in-place model and
its tradeoffs:

```text
ℹ In-place migration

Buckit will stop minio.service on each node, install the buckit binary
alongside the existing minio binary, and start a new buckit.service that
reads the same /etc/default/minio env file and the same data drives. The
on-disk format (xl.meta, .minio.sys/) is unchanged and no data is copied.

Your existing MinIO config is preserved as-is:
  · /etc/default/minio  — not modified
  · MINIO_* env vars     — read directly by Buckit
  · .minio.sys/ on disks — read directly by Buckit
  · TLS certs, KMS, IAM  — picked up unchanged

Expect a brief write-unavailable window during cutover on each node. A
rollback option is available until you click "Finalize" at the end of the
wizard; rollback simply re-enables minio.service.
```

### M2 — Add Nodes (or Import)

Same component as N2 with one extra affordance at the top:

```text
[Import nodes from a running MinIO alias]
  Alias        [prod ▾]    or    mc alias  [https://_______]  [AK] [SK]
  [Probe]
```

- Probing `mc admin info <alias>` reveals the existing pool layout and node
hostnames; the user confirms before populating the host table.
- After import, SSH credentials are still required (the manager talks to
hosts directly).

### M3 — Discovery + MinIO Detection

Identical to N3, but each row's detail panel additionally reports:

```text
node1.example.com
  MinIO binary    /usr/local/bin/minio  v2024-12-01
  MinIO service   minio.service (active, enabled)
  MinIO env       /etc/default/minio
                  MINIO_VOLUMES="https://node{1...8}:9000/data/disk{1...12}"
                  MINIO_ROOT_USER=…
                  MINIO_OPTS="--console-address :9001"
  Detected pools  1 pool, 8×12 drives, EC:4
```

- All eight nodes must report a consistent MinIO topology, or the wizard
blocks with an explanatory diff view.

### M4 — Snapshot

```text
Snapshot of current MinIO state

Buckets                   142            [View list ▾]
  Largest                 logs-archive (412 Ti)
  With versioning         38
  With lifecycle rules    21
  With object lock        4
IAM
  Users                   17
  Groups                  3
  Policies (custom)       11
  Service accounts        42
  STS sessions            (skipped — short-lived)
Bucket-level config
  Bucket policies         57
  Notification configs    9
  Lifecycle rules         21
  Replication targets     3   ⚠ Review

⚠ Replication targets detected
   This cluster replicates to external targets. Buckit will preserve the
   configuration; the targets must remain reachable post-migration.

[Re-run snapshot]                              [Download snapshot ⤓]
```

- Snapshot is captured via the MinIO admin API using the existing root
credentials (collected in M2 import or M3 detection if not already known).
- The snapshot is stored as a versioned artifact in the manager DB and
referenced by the migration task. It is also used to validate post-cutover.

### M5 — Plan

```text
Migration plan

Install method: dnf install   (RHEL 9 detected on all nodes;
                               minio package installed via rpm)

For each node, in sequence:
  1. Wait for cluster quorum (other nodes must be healthy)
  2. systemctl stop minio
  3. scp buckit-1.0.0.rpm to /tmp/ on the node
  4. dnf install -y /tmp/buckit-1.0.0.rpm
       (minio package is NOT removed — kept installed for rollback)
  5. systemctl disable minio
  6. systemctl enable --now buckit
       Buckit's unit reads /etc/default/minio (same env file as MinIO)
  7. Wait for node-healthy probe (timeout 2m)
  8. Wait for cluster-healthy probe (timeout 5m) before next node

What is NOT touched on any node:
  · /etc/default/minio  — env file kept as-is, read by buckit.service
                          (the buckit package does not ship this file)
  · /etc/minio/         — TLS certs, KMS config kept as-is
  · .minio.sys/         — on-disk cluster state kept as-is
  · /data/disk*         — data drives untouched
  · minio package        — installed and disabled; removed only on Finalize

Rolling order
  ◉ Sequential (safest)
  ○ Two at a time (faster, requires EC parity ≥ 2)

Estimated downtime per node      ~30–90 s
Estimated total migration time   ~12 min  (8 nodes × ~90 s)

Rollback
  Until you click "Finalize" on step 9, a [Rollback] button remains
  available. Rollback per node is symmetric: stop buckit, disable
  buckit.service, re-enable minio.service. The env file, certs, and
  on-disk state are unchanged throughout, so rollback is fast and safe.

☑ I understand each node will briefly stop serving writes during cutover.
```

### M6 — Preflight

Same component as N5, with MinIO-specific checks added:

```text
MinIO admin API reachable on all nodes        8 / 8    ✓ Pass
Current MinIO cluster healthy                 ✓ Pass
xl.meta format version compatible             ✓ Pass
.minio.sys/ readable on all drives            8 / 8    ✓ Pass
/etc/default/minio present and readable       8 / 8    ✓ Pass
Package manager available (dnf)               8 / 8    ✓ Pass
buckit-1.0.0.rpm reachable                    ✓ Pass
minio installed via package manager           8 / 8    ✓ Pass
   ↳ Required for clean "dnf remove minio" on Finalize.
     Tarball-installed minio falls back to manual cleanup.
No package conflicts (minio ↔ buckit)         8 / 8    ✓ Pass
   ↳ Verified buckit package does not claim /etc/default/minio
Root credentials valid                        ✓ Pass
No in-flight admin operations                 ✓ Pass
   ↳ No active healing, decommission, or rebalance jobs
```

- A failing "current cluster healthy" check is a hard block — migrating an
already-degraded cluster is not supported in Phase 1.

### M7 — Cutover

```text
Migrating legacy-east → Buckit                    ● Running · 4m12s elapsed

Overall progress     ▓▓▓▓▓░░░░░░░  3 / 8 nodes

NODE      STATE                     CUTOVER START   DURATION   RESULT
node1     ✓ Buckit healthy          12:00:01        58s        ✓
node2     ✓ Buckit healthy          12:01:02        61s        ✓
node3     ⟳ Waiting cluster-healthy 12:02:04        —          —
node4     · Pending                 —               —          —
...

Cluster health (live)
  Quorum: ✓ maintained throughout cutover
  Read availability:  100 %
  Write availability: 87 %    ↑ recovers when node3 finishes

Live log                                                  [⏸ Pause]
┌──────────────────────────────────────────────────────────┐
│ 12:02:04 node3  $ systemctl stop minio                   │
│ 12:02:05 node3  ✓ minio stopped                          │
│ 12:02:05 node3  $ scp buckit-1.0.0.rpm node3:/tmp/       │
│ 12:02:06 node3  ✓ uploaded (45 MiB)                      │
│ 12:02:06 node3  $ dnf install -y /tmp/buckit-1.0.0.rpm   │
│ 12:02:07 node3  ✓ installed buckit-1.0.0                 │
│                   provides /usr/local/bin/buckit,        │
│                   buckit.service, buckit user/group      │
│ 12:02:07 node3  $ systemctl disable minio                │
│ 12:02:07 node3  $ systemctl enable --now buckit          │
│ 12:02:08 node3  ✓ buckit.service active                  │
│ 12:02:08 node3  i reading EnvironmentFile=               │
│                   /etc/default/minio (unchanged)         │
│ 12:02:09 node3  ✓ health probe /minio/health/live        │
│ ...                                                      │
└──────────────────────────────────────────────────────────┘

[Pause after current node]    [Rollback all completed]
```

- "Pause after current node" lets the operator stop the rolling cutover
cleanly at a node boundary — useful if the cluster shows distress.
- "Rollback all completed" is always available during cutover. It reverses
switched nodes in reverse order and restores `minio.service`.

### M8 — Verify

```text
Post-migration verification

Cluster health                          ● Healthy
Node count                              8 / 8 reporting
Bucket count                            142 / 142
Object count (sampled, 1000 objects)    1000 / 1000 readable
IAM
  Users                                 17 / 17 present
  Groups                                3 / 3 present
  Policies                              11 / 11 present
  Service accounts                      42 / 42 present
Bucket configs
  Policies                              57 / 57 match snapshot
  Lifecycle rules                       21 / 21 match snapshot
  Notification configs                  9 / 9 match snapshot
Smoke test
  PUT 1 KiB to __buckit_migration_probe ✓
  GET back, content match               ✓
  DELETE                                ✓

All checks passed.

[Re-run verification]                           [Download report ⤓]
```

- Any failed verification check leaves the wizard on this screen with a
prominent `[Rollback]` action.
- The verification report is archived against the cluster record for audit.

### M9 — Finalize

```text
You're about to finalize the migration of legacy-east.

After finalizing:
  · dnf remove minio  (or apt remove minio) on each node, removing the
    minio binary, minio.service unit, and minio user/group cleanly via
    the package manager
  · Buckit retains the minio .rpm/.deb in the manager DB for 30 days
    for emergency rollback (manual reinstall)
  · The wizard's [Rollback] option is removed
  · Cluster status moves from "Migrating" to "Active"

If MinIO was originally installed via tarball rather than a package,
finalize falls back to removing /usr/local/bin/minio and the
minio.service unit file directly. The wizard surfaces which mode
applies per node.

What is preserved (not modified by finalize):
  · /etc/default/minio  — buckit.service continues to read it
  · /etc/minio/         — TLS certs, KMS config
  · .minio.sys/, xl.meta — on-disk cluster state and data
  · MINIO_* env var names — read directly by the buckit binary

These paths and names are intentionally kept MinIO-compatible. Fresh
Buckit deployments use the same layout, so post-migration nodes are
byte-identical to a fresh install at every layer except the binary
and the unit name.

[← Back to verify]                              [Finalize migration]
```

- `Finalize migration` triggers a final task that performs the irreversible
cleanup steps and flips cluster state.
- Before finalize, the cluster appears in the cluster list as `Migrating`
with a yellow chip. After finalize, it appears as `Active` with a
`migrated_from: minio@v2024-12-01` chip on the cluster overview.

## Cluster Detail

Route: `/clusters/:id`. Top-level tabs.

### Overview

```text
prod-east                       [↗ Open Buckit console]   [⟳ Actions ▾]
v1.0.0 · 8 nodes · 1 pool · EC:4 · Migrated from MinIO 2024-12-01

┌─────────────────────┬─────────────────────┬─────────────────────┐
│ Health              │ Capacity            │ Activity            │
│ ● Healthy           │ 412 Ti / 864 Ti     │ Last task           │
│ 8/8 nodes online    │ ▓▓▓▓▓░░░░░  47%    │ Rolling restart     │
│ 96/96 drives ready  │                     │ 2h ago · ✓ Success  │
└─────────────────────┴─────────────────────┴─────────────────────┘

Recent tasks                                      [View all →]
  ✓ Rolling restart        2h ago     · 4m12s
  ✓ Health probe           4h ago     · 2s
  ✓ Deploy v1.0.0          3d ago     · 9m34s
```

- `**Open Buckit console**` opens the cluster's built-in web console (served
by the `buckit` binary on its console port) in a new tab. This is the entry
point for bucket browsing, IAM, metrics, and other data-plane operations
that `bm` deliberately does not duplicate.
- `Actions ▾` menu: Rolling restart, Stop cluster, Start cluster, Upgrade…,
Rotate root credentials, Tear down cluster (destructive, double-confirm).

### Nodes

```text
NODE              STATE     VERSION    DRIVES        FREE       UPTIME
node1.example.com ● Online  v1.0.0     12/12 ready   55%        3d
node2.example.com ● Online  v1.0.0     12/12 ready   54%        3d
...
node5.example.com ⚠ Online  v1.0.0     11/12 ready   53%        3d
   ↳ /dev/sdh degraded — healing
node8.example.com ● Online  v1.0.0     12/12 ready   56%        3d
```

- Row click → node detail.

### Node Detail (`/clusters/:id/nodes/:nodeId`)

```text
node5.example.com                                 [Restart] [⟳ Actions ▾]
Online · Buckit v1.0.0 · uptime 3d

System
  OS              Ubuntu 24.04 LTS
  Kernel          6.8.0-31-generic
  CPU / RAM       16 cores · 64 GiB
  Network         eno1 (10 GbE)

Service
  Unit            buckit.service  (active, enabled)
  Listen          :9000, :9001
  Last restart    3 days ago

Drives
  MOUNT          DEVICE     SIZE    USED    STATUS
  /data/disk1    /dev/sda   16 Ti   55%     ● Ready
  /data/disk2    /dev/sdb   16 Ti   54%     ● Ready
  ...
  /data/disk8    /dev/sdh   16 Ti   53%     ⚠ Healing (37 %)
  ...

Recent log lines (last 50)                                  [Tail →]
```

### Services Tab

```text
Cluster service control

State     ● Running on all 8 nodes
Version   v1.0.0

[Rolling restart]   [Stop all]   [Start all]
[Upgrade…]   [Rotate root credentials]
```

- `Rolling restart` opens a small confirmation modal with `Concurrency: Sequential / Two at a time` and an estimated duration.
- `Upgrade…` opens an upgrade wizard (out of scope to detail in this doc but
reuses the deploy log component).

### Cluster Tasks Tab

Same component as the global task center, scoped to this cluster.

### Cluster Settings Tab

```text
SSH credentials      [Rotate] [View]
Buckit version pin   v1.0.0     [Change…]
Health probe         Every 30 s     [Edit]
Audit log retention  30 days        [Edit]
[Tear down cluster…]   (destructive)
```

## Tasks Center

Route: `/tasks`. Global queue across all clusters.

```text
Tasks                                          [Filter: all ▾] [Cluster: all ▾]

STATE       NAME                          CLUSTER       STARTED        DURATION
⟳ Running   Migrate legacy-east           legacy-east   3m ago         3m12s
⟳ Running   Health probe                  prod-east     20s ago        20s
✓ Success   Rolling restart               prod-east     2h ago         4m12s
✗ Failed    Deploy v1.0.0                 staging       1d ago         12m
            ↳ node3: SSH timeout
○ Canceled  Discover nodes                prod-west-new 1d ago         45s
```

- Row click → task detail.
- Failed tasks expose a `Retry` button on the detail page when the task
type supports it; not all do (e.g., a one-shot finalize is not retryable).

### Task Detail (`/tasks/:id`)

```text
Migrate legacy-east                                       ⟳ Running
Started 12:00:01 · running 3m12s
Cluster legacy-east · Triggered by admin

Steps
  ✓ Snapshot MinIO state                  · 18s
  ✓ Preflight                              · 22s
  ⟳ Cutover (3/8 nodes)                   · 2m32s
    ✓ node1                                · 58s
    ✓ node2                                · 61s
    ⟳ node3                                · 33s
    · node4 pending
    ...
  · Verify (pending)
  · Finalize (pending)

Live log                                            [Filter: all ▾] [⏸]
┌──────────────────────────────────────────────────────────────┐
│ ...                                                          │
└──────────────────────────────────────────────────────────────┘

[Pause after current step]   [Cancel]   [Download log ⤓]
```

- Tasks are first-class resources. Every long-running action in the UI lands
here, and every wizard "deploy" / "cutover" screen is just an inline view
of the corresponding task.

## Manager Settings

Route: `/settings`. Minimal in Phase 1.

```text
Admin
  Username           admin
  Password           ●●●●●●●●     [Change]
  Session timeout    8 hours      [Edit]

TLS
  Listener           :9443
  Certificate        /etc/bm/cert.pem (expires 2026-08-12)
  Private key        /etc/bm/key.pem
  [Replace certificate…]

Storage
  Database           SQLite at /var/lib/bm/bm.db
  Backups            Daily at 03:00 UTC      [Edit]
  [Download backup now]

Audit log
  Retention          90 days
  [View log →]   [Export ⤓]
```

## Cross-Cutting States

### Empty states

- **Cluster list, no clusters and no drafts** → redirect to `/welcome`.
- **Cluster list, only drafts** → cluster list with prominent banner:
"You have 1 draft cluster. [Resume] [Discard]".
- **Node detail, drives empty** → "No drives detected. Re-run discovery."
- **Tasks center, no tasks** → "No tasks yet. Tasks created by wizards and
cluster actions will appear here."

### Loading states

- Skeleton rows for tables, with shimmer animation.
- For wizard steps that run a backend task before content is available
(discovery, preflight, snapshot, verify), show a per-row progress view
rather than a single global spinner.

### Error states

- **SSH failure mid-wizard** → inline row error with `[Retry]`, plus a link
to the full task log.
- **Lost connection to manager** → global banner: "Connection to manager
lost. Retrying in 5 s…" Auto-resumes when connection returns.
- **Task failed** → cluster status changes to a yellow `Action needed`
chip; clicking opens the failed task with remediation suggestions.

### Confirmation modals (always double-confirm)

- Tear down cluster
- Rollback migration
- Stop all nodes
- Rotate root credentials

The modal pattern: title, what will happen, what won't happen, a typed
confirmation (e.g., "Type the cluster name to confirm: ____"), and the
destructive button is red.

## Component Inventory

Components reused across screens. A frontend engineer should build these
once and compose them throughout.


| Component                                    | Used in                           |
| -------------------------------------------- | --------------------------------- |
| `WizardShell`                                | All wizards                       |
| `Stepper`                                    | All wizards                       |
| `NodeTable`                                  | N2, N3, M2, M3, cluster nodes tab |
| `SSHCredentialsForm`                         | N2, M2, cluster settings          |
| `DiscoveryRow`                               | N3, M3                            |
| `PreflightTable`                             | N5, M6                            |
| `TopologyBuilder`                            | N4                                |
| `PlanReview`                                 | N6, M5                            |
| `TaskLogStream`                              | N7, M7, task detail               |
| `TaskStepsTimeline`                          | Task detail                       |
| `RollingProgress`                            | N7, M7, rolling restart, upgrade  |
| `ConfirmModal`                               | All destructive actions           |
| `CapacityCard`, `HealthCard`, `ActivityCard` | Cluster overview                  |


## API Contract Sketch

The wireframes assume a backend that exposes:

- `POST /api/v1/clusters` — create draft cluster.
- `PATCH /api/v1/clusters/:id` — update draft (basics, nodes, topology).
- `POST /api/v1/clusters/:id/discover` — start discovery task.
- `POST /api/v1/clusters/:id/preflight` — start preflight task.
- `POST /api/v1/clusters/:id/deploy` — start deploy task.
- `POST /api/v1/clusters/:id/migration/snapshot` — start MinIO snapshot.
- `POST /api/v1/clusters/:id/migration/cutover` — start cutover task.
- `POST /api/v1/clusters/:id/migration/verify` — start verify task.
- `POST /api/v1/clusters/:id/migration/rollback` — start rollback task.
- `POST /api/v1/clusters/:id/migration/finalize` — finalize migration.
- `GET  /api/v1/tasks` / `GET /api/v1/tasks/:id` — list/inspect tasks.
- `GET  /api/v1/tasks/:id/events` — SSE stream of task events.

Every long-running action returns a task ID. The UI never blocks on these
endpoints; it always renders against the task stream.

## Accessibility & Responsiveness

- Keyboard navigation through wizard steps (Tab/Shift-Tab, Enter to advance,
Esc to cancel modals).
- Color is never the only indicator of state — all status pills carry an
icon and a text label.
- Live regions for task log updates (with a pause control for screen-reader
users).
- Minimum supported viewport: 1280 × 800. The wizard collapses gracefully
to 1024 × 768. Mobile is not a target for Phase 1.

## Out of Scope for Phase 1

The following are intentionally excluded and should not appear in the `bm` UI.

**Already provided by the per-cluster Buckit web console** (served by the
`buckit` binary itself; `bm` should link out to it from the cluster overview,
not reimplement it):

- Bucket browser, object upload/download
- IAM user/group/policy editor (a read-only view in M8 verify is fine)
- Metrics and Prometheus-style dashboards
- Bucket-level configuration (lifecycle, replication, notifications)

**Deferred to a later phase of `bm` itself:**

- Kubernetes as a deployment target (a separate operator-oriented path, per
the phased plan in `README.md`)

## What Comes After Phase 1

Subsequent phases build on the same shared core (task engine, cluster store,
SSH layer) introduced by Phase 1. See `README.md` for the full plan.

- **Phase 2 — Manager CLI.** A terminal frontend over the same API: `bm cluster deploy`, `bm cluster status`, remote manager targeting via
`--manager`. Makes `bm` usable without the UI.
- **Phase 3 — `mc` admin replacement.** Move Buckit-specific admin
operations (admin info, user/policy/alias, profile management) into `bm`.
- **Phase 4 — Broader `mc` replacement.** Selected data-path commands
(`ls`, `cp`, `mirror`, bucket/object helpers) once the internal command
model is stable.

Kubernetes as a deployment target is a separate, later track and is not
gated on Phases 2–4.

## Open UI Questions

- Should the wizards support **resuming on a different browser/session**, or
is "same browser" acceptable for Phase 1? (Draft state lives server-side,
so either is technically feasible.)
- For the in-place migration, do we expose the **"two at a time" rolling
concurrency** in v1, or hide it behind an advanced flag until we have
field data on its safety?
- How long should the **rollback window** stay open after verify passes
but before finalize? Current draft assumes "until the user clicks
Finalize" with no timeout — should we add a soft 24-hour reminder?
- Should the **task log** offer client-side text search across the live
stream, or only on the archived log after the task ends?

## Appendix A — MinIO compatibility surface

The wizards rely on a fixed split between operator-facing names (Buckit-branded)
and internal names that remain MinIO-compatible so fresh and migrated nodes are
byte-identical at the storage and config layer.


| Surface             | Name                                 | Owner                                             |
| ------------------- | ------------------------------------ | ------------------------------------------------- |
| Binary              | `/usr/local/bin/buckit`              | Buckit (package)                                  |
| Systemd unit        | `/lib/systemd/system/buckit.service` | Buckit (package)                                  |
| `buckit` user/group | Created by package postinstall       | Buckit (package)                                  |
| Env file            | `/etc/default/minio`                 | Manager (writes on fresh; preserves on migration) |
| Env var names       | `MINIO_`*                            | Read directly by the buckit binary                |
| On-disk state       | `.minio.sys/`, `xl.meta`             | MinIO format, unchanged                           |


The env file path stays MinIO-named so a fresh-deployed node and a
migrated-from-MinIO node have identical layouts, and so the migration wizard
never has to translate env vars. A future release may add `BUCKIT_`* aliases
with `MINIO_*` as a deprecated fallback; that's out of scope here.

## Appendix B — Package install path

Packages are produced by the existing release pipeline (see
`packaging/nfpm.yaml`) and contain the binary, the unit file, and the
postinstall/preremove/postremove scripts that create the `buckit` user.

The manager installs them by **local file**: fetch the artifact once from
the GitHub Release, `scp` it to each node, then run
`dnf install -y /tmp/buckit.rpm` (or the `apt`/`apk` equivalent) on the
node. No yum/apt repository infrastructure is required.


| Target distro family         | Install command on node                                               |
| ---------------------------- | --------------------------------------------------------------------- |
| RHEL / Rocky / Alma / Fedora | `dnf install -y /tmp/buckit.rpm`                                      |
| Debian / Ubuntu              | `apt install -y /tmp/buckit.deb`                                      |
| Alpine                       | `apk add --allow-untrusted /tmp/...`                                  |
| Other / detection failed     | `scp` raw binary; manager writes the unit and creates the user itself |


The package owns the binary, unit, and user; the manager only ever writes
`/etc/default/minio` with cluster-specific values (and, rarely, a systemd
drop-in for a cluster-level override).