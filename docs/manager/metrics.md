# Metrics Page (`/tool/metrics`)

The Metrics page provides server monitoring through 4 tabs: **Info**, **Usage**, **Traffic**, and **Resources**.

Currently, the Usage, Traffic, and Resources tabs are disabled unless Prometheus is configured. Our goal is to build these out natively without requiring Prometheus, sourcing data directly from BuckIt server internals.

---

## Tab 1: Info

Shows real-time server information. Data source: `GET /admin/info` — a single API call returning a point-in-time snapshot. No Prometheus, no time range. The "Sync" button re-fetches this endpoint.

### Wireframe

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ [Info] [Usage] [Traffic] [Resources]                                        │
├─────────────────────────────────────────────────────────────────────────────┤
│  Server Information                                              [Sync]     │
│                                                                             │
│ ┌──────────────┐  ┌──────────────┐  ┌─────────────────────────────────────┐│
│ │ Buckets      │  │ Objects      │  │ Reported Usage          [bar graph] ││
│ │              │  │              │  │ 1.2 TiB                             ││
│ │  156         │  │  1,203,456   │  │                                     ││
│ │   [Browse >] │  │              │  │ Time since last Heal: 2h 15m        ││
│ ├──────────────┤  ├──────────────┤  │ Time since last Scan: 45m           ││
│ │ Servers      │  │ Drives       │  │ Uptime: 14d 3h                      ││
│ │ 🟢 3 Online  │  │ 🟢 12 Online │  │                                     ││
│ │ 🔴 0 Offline │  │ 🔴 0 Offline │  │                                     ││
│ └──────────────┘  └──────────────┘  └─────────────────────────────────────┘│
│                                                                             │
│ ┌─────────────────────┐ ┌─────────────────────┐ ┌────────────────────────┐ │
│ │ Backend type        │ │ Standard SC parity  │ │ RRS parity             │ │
│ │ Erasure             │ │ 4                   │ │ 2                      │ │
│ └─────────────────────┘ └─────────────────────┘ └────────────────────────┘ │
│                                                                             │
│  Servers (3)                                                                │
│ ┌───────────────────────────────────────────────────────────────────────┐   │
│ │ ▶ server1:9000 🟢    Drives: 4/4 🟢   Network: 2/2 🟢   Up: 14d 3h  │   │
│ │                                                        Version: v1.2  │   │
│ ├───────────────────────────────────────────────────────────────────────┤   │
│ │ ▼ server2:9000 🟢    Drives: 4/4 🟢   Network: 2/2 🟢   Up: 14d 3h  │   │
│ │                                                        Version: v1.2  │   │
│ │ ┌─────────────────────────────────────────────────────────────────┐   │   │
│ │ │ Drives (4)                                                      │   │   │
│ │ ├─────────────────────────────────────────────────────────────────┤   │   │
│ │ │ ┌─────────┐                                                     │   │   │
│ │ │ │ Used    │  Drive Name: /data1                                 │   │   │
│ │ │ │ Capacity│  Drive Status: 🟢 Online                            │   │   │
│ │ │ │ [donut] │                                                     │   │   │
│ │ │ │  45%    │  Used Capacity:    450 GiB  (45.00% of 1 TiB)      │   │   │
│ │ │ └─────────┘  Available Capacity: 550 GiB  (55.00% of 1 TiB)    │   │   │
│ │ ├─────────────────────────────────────────────────────────────────┤   │   │
│ │ │ ┌─────────┐                                                     │   │   │
│ │ │ │ Used    │  Drive Name: /data2                                 │   │   │
│ │ │ │ Capacity│  Drive Status: 🟢 Online                            │   │   │
│ │ │ │ [donut] │                                                     │   │   │
│ │ │ │  62%    │  Used Capacity:    620 GiB  (62.00% of 1 TiB)      │   │   │
│ │ │ └─────────┘  Available Capacity: 380 GiB  (38.00% of 1 TiB)    │   │   │
│ │ ├─────────────────────────────────────────────────────────────────┤   │   │
│ │ │  ... (more drives)                                              │   │   │
│ │ └─────────────────────────────────────────────────────────────────┘   │   │
│ ├───────────────────────────────────────────────────────────────────────┤   │
│ │ ▶ server3:9000 🟢    Drives: 4/4 🟢   Network: 2/2 🟢   Up: 14d 3h  │   │
│ │                                                        Version: v1.2  │   │
│ └───────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
```

#### Server Row Details
Each server accordion header shows:
- **Endpoint** — server address (e.g., `server1:9000`)
- **State** — online/offline indicator (colored dot)
- **Drives** — active/total count with status color

### API Response Fields

| Field | Type | Displayed | Description |
|-------|------|-----------|-------------|
| `buckets` | number | ✅ | Total bucket count |
| `objects` | number | ✅ | Total object count |
| `usage` | number | ✅ | Total bytes used |
| `advancedMetricsStatus` | string | ✅ | `"not configured"` / `"available"` / `"unavailable"` — controls whether Usage/Traffic/Resources tabs are enabled; shows Prometheus warning banner |
| `widgets` | Widget[] | ❌ | Vestigial field — not consumed by frontend |
| **`servers[]`** | | | |
| `servers[].endpoint` | string | ✅ | Server address (e.g., `server1:9000`) |
| `servers[].state` | string | ✅ | `"online"` / `"offline"` |
| `servers[].uptime` | number | ✅ | Uptime in seconds |
| `servers[].version` | string | ✅ | Server software version |
| `servers[].commitID` | string | ❌ | Git commit hash of the server build |
| `servers[].poolNumber` | number | ❌ | Which erasure pool the server belongs to |
| `servers[].network` | Record<string, string> | ✅ | Map of network interface → `"online"`/`"offline"`; displayed as active/total count |
| `servers[].drives[]` | ServerDrives[] | ✅ | Array of drives for this server |
| **`servers[].drives[]`** | | | |
| `drives[].uuid` | string | ❌ | Drive unique identifier |
| `drives[].state` | string | ✅ | `"ok"` / `"offline"` |
| `drives[].endpoint` | string | ✅ | Shown as "Drive Name" (mount path) |
| `drives[].drivePath` | string | ❌ | Drive path (separate from endpoint) |
| `drives[].rootDisk` | boolean | ❌ | Whether this is the OS/root disk |
| `drives[].healing` | boolean | ❌ | Whether the drive is currently in a healing operation |
| `drives[].model` | string | ❌ | Drive hardware model |
| `drives[].totalSpace` | number | ✅ | Total capacity in bytes |
| `drives[].usedSpace` | number | ✅ | Used capacity in bytes |
| `drives[].availableSpace` | number | ✅ | Available capacity in bytes |
| **`backend`** | | | |
| `backend.backendType` | string | ✅ | `"Erasure"` / `"FS"` |
| `backend.standardSCParity` | number | ✅ | Standard storage class parity |
| `backend.rrSCParity` | number | ✅ | Reduced redundancy storage class parity |
| `backend.onlineDrives` | number | ✅ | Online drive count (used as fallback when per-server data unavailable) |
| `backend.offlineDrives` | number | ✅ | Offline drive count (used as fallback) |
- **Network** — active/total network interfaces with status color
- **Uptime** — server uptime duration
- **Version** — server software version

#### Drive Details (expanded)
Each drive card shows:
- **Used Capacity donut chart** — visual percentage ring
- **Drive Name** — mount path (e.g., `/data1`)
- **Drive Status** — Online/Offline with colored indicator
- **Used Capacity** — bytes used, percentage of total
- **Available Capacity** — bytes free, percentage of total

---

## Tab 2: Usage

Shows storage usage and object distribution metrics over a selected time range.

### Data Displayed

| Widget | Type | Description |
|--------|------|-------------|
| Buckets | Single rep | Total bucket count (large number + sparkline background) |
| Objects | Single rep | Total object count (large number + sparkline background) |
| Servers (Online/Offline) | Merged dual stat | Server count with 🟢 Online / 🔴 Offline indicators |
| Drives (Online/Offline) | Merged dual stat | Drive count with 🟢 Online / 🔴 Offline indicators |
| Capacity | Donut chart | Used Space vs Usable Free; center shows "X% Free"; right side shows "Used: [value] [unit]" and "Of: [total]" |
| Network (Upload/Download) | Merged value pair | Upload (PUT) and Download (GET) throughput in bytes; speedtest icon |
| Time Since Last Heal | Simple widget | Duration since last heal activity (icon + label + value) |
| Time Since Last Scan | Simple widget | Duration since last scan activity (icon + label + value) |
| Uptime | Simple widget | Server uptime duration (icon + label + value) |
| Data Usage Growth | Area graph | Storage usage over time (Y: bytes, X: time); filled area under curve |
| Object Size Distribution | Bar chart | Object count by size range: < 1024B, 1KB–1MB, 1MB–10MB, 10MB–64MB, 64MB–128MB, 128MB–512MB, > 512MB |
| API Data Received Rate | Line graph | Inbound data rate per node (Y: bytes/s, X: time); one line per node |
| API Data Sent Rate | Line graph | Outbound data rate per node (Y: bytes/s, X: time); one line per node |

### Widget Common Features (all line/area graphs)
- **Title** at top-left
- **Legend** — scrollable list of series names with colored dots (hidden on small screens)
- **Tooltip** on hover — shows values for all series at that time point
- **Expand/Zoom button** — opens the chart in a full-screen modal with longer date format
- **Download data button** — exports chart data as CSV
- **X-axis** — formatted as time (HH:mm)
- **Y-axis** — formatted per widget (bytes, count, etc.)

### Note: Time Range Applies to All Widgets

All widgets on Usage/Traffic/Resources tabs query data within the selected time range. This includes the "simple" widgets (Uptime, Time Since Last Heal, Time Since Last Scan). These display the **last data point** within the selected window — meaning if you select a historical range (e.g., last Tuesday to last Wednesday), you see the value as it was at the end of that window, not the current value.

### Wireframe

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ [Info] [Usage] [Traffic] [Resources]                                        │
├─────────────────────────────────────────────────────────────────────────────┤
│ ┌─ Date Range Selector ───────────────────────────────────────── [Sync] ─┐  │
│ │  Start: [___________]   End: [___________]                             │  │
│ └────────────────────────────────────────────────────────────────────────┘  │
│                                                                             │
│ ┌──────────────────┐  ┌──────────────────┐  ┌───────────────┐  ┌────────┐  │
│ │ Buckets          │  │ Objects          │  │ Servers       │  │ Drives │  │
│ │ ┌──────────────┐ │  │ ┌──────────────┐ │  │ 🟢 3 Online   │  │ 🟢 12  │  │
│ │ │  156         │ │  │ │  1,203,456   │ │  │ 🔴 0 Offline  │  │ 🔴 0   │  │
│ │ │ ~sparkline~  │ │  │ │ ~sparkline~  │ │  │               │  │        │  │
│ │ └──────────────┘ │  │ └──────────────┘ │  │               │  │        │  │
│ └──────────────────┘  └──────────────────┘  └───────────────┘  └────────┘  │
│                                                                             │
│ ┌──────────────────────────────────┐  ┌──────────────────────────────────┐  │
│ │ Capacity                         │  │ Network                    🏎️    │  │
│ │                                  │  │                                  │  │
│ │          ┌────────┐              │  │  ┌──────────┐                    │  │
│ │          │ ░░░░░░ │  Used:       │  │  │ Download │  Upload:           │  │
│ │          │ ░ 45% ░│  450 GiB     │  │  │  1.2 GiB │  2.3 GiB          │  │
│ │          │ ░Free░ │  Of: 1 TiB   │  │  └──────────┘                    │  │
│ │          └────────┘              │  │                                  │  │
│ └──────────────────────────────────┘  └──────────────────────────────────┘  │
│                                                                             │
│ ┌────────────────────┐ ┌────────────────────┐ ┌──────────────────────────┐ │
│ │ 🩹 Time Since      │ │ 🔍 Time Since Last │ │ ⏱️ Uptime                │ │
│ │    Last Heal       │ │    Scan Activity   │ │                          │ │
│ │    2h 15m          │ │    45m             │ │    14d 3h                │ │
│ └────────────────────┘ └────────────────────┘ └──────────────────────────┘ │
│                                                                             │
│ ┌──────────────────────────────────┐  ┌──────────────────────────────────┐  │
│ │ Data Usage Growth         [⤢][⬇] │  │ Object Size Distribution  [⤢][⬇] │  │
│ │ (Area Graph)                     │  │ (Bar Chart)                      │  │
│ │                                  │  │                                  │  │
│ │   ╱──╲    ╱╲                     │  │  < 1024B       ████              │  │
│ │  ╱░░░░╲──╱░░╲──                 │  │  1KB–1MB       ████████          │  │
│ │ ╱░░░░░░░░░░░░░╲                 │  │  1MB–10MB      ██████████████    │  │
│ │ ──────────────────               │  │  10MB–64MB     ████████          │  │
│ │ X: time   Y: bytes               │  │  64MB–128MB    ████              │  │
│ │                                  │  │  128MB–512MB   ██                │  │
│ │ ● series-1                       │  │  > 512MB       █                 │  │
│ └──────────────────────────────────┘  └──────────────────────────────────┘  │
│                                                                             │
│ ┌──────────────────────────────────┐  ┌──────────────────────────────────┐  │
│ │ API Data Received Rate    [⤢][⬇] │  │ API Data Sent Rate        [⤢][⬇] │  │
│ │ (Line Graph, per node)           │  │ (Line Graph, per node)           │  │
│ │                                  │  │                                  │  │
│ │   ╱──╲    ╱╲                     │  │      ╱╲                          │  │
│ │  ╱    ╲──╱  ╲──                 │  │  ───╱  ╲───                      │  │
│ │ ──────────────────               │  │ ──────────────────               │  │
│ │ X: time   Y: bytes/s             │  │ X: time   Y: bytes/s             │  │
│ │                                  │  │                                  │  │
│ │ ● node1  ● node2  ● node3       │  │ ● node1  ● node2  ● node3       │  │
│ └──────────────────────────────────┘  └──────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Tab 3: Traffic

Shows API request and network traffic metrics over a selected time range. All graphs are per-node (one line per node with distinct colors and legend).

### Data Displayed

| Widget | Type | Description |
|--------|------|-------------|
| API Request Rate | Line graph (full width) | Requests/second per node (Y: count, X: time) |
| API Request Error Rate | Line graph (half width) | Failed requests/second per node (Y: count, X: time) |
| Internode Data Transfer | Line graph (half width) | Data transferred between nodes (Y: bytes/s, X: time) |
| Node IO | Line graph (full width) | Disk read/write I/O per node (Y: bytes/s, X: time) |

### Wireframe

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ [Info] [Usage] [Traffic] [Resources]                                        │
├─────────────────────────────────────────────────────────────────────────────┤
│ ┌─ Date Range Selector ───────────────────────────────────────── [Sync] ─┐  │
│ │  Start: [___________]   End: [___________]                             │  │
│ └────────────────────────────────────────────────────────────────────────┘  │
│                                                                             │
│ ┌────────────────────────────────────────────────────────────────────────┐  │
│ │ API Request Rate (full width)                                   [⤢][⬇] │  │
│ │                                                                        │  │
│ │      ╱╲       ╱╲                                                       │  │
│ │   ──╱  ╲──╱╲─╱  ╲───                                                  │  │
│ │  ╱              ╲                                                      │  │
│ │ ────────────────────────                                               │  │
│ │ X: time   Y: requests/s                                                │  │
│ │                                                                        │  │
│ │ ● node1  ● node2  ● node3  ...                                        │  │
│ └────────────────────────────────────────────────────────────────────────┘  │
│                                                                             │
│ ┌──────────────────────────────────┐  ┌──────────────────────────────────┐  │
│ │ API Request Error Rate    [⤢][⬇] │  │ Internode Data Transfer   [⤢][⬇] │  │
│ │ (Line Graph, per node)           │  │ (Line Graph, per node)           │  │
│ │                                  │  │                                  │  │
│ │   ╱╲                             │  │      ╱╲                          │  │
│ │  ╱  ╲──╱╲──                     │  │  ───╱  ╲───                      │  │
│ │ ──────────────────               │  │ ──────────────────               │  │
│ │ X: time   Y: errors/s            │  │ X: time   Y: bytes/s             │  │
│ │                                  │  │                                  │  │
│ │ ● node1  ● node2  ● node3       │  │ ● node1  ● node2  ● node3       │  │
│ └──────────────────────────────────┘  └──────────────────────────────────┘  │
│                                                                             │
│ ┌────────────────────────────────────────────────────────────────────────┐  │
│ │ Node IO (full width)                                            [⤢][⬇] │  │
│ │                                                                        │  │
│ │      ╱╲       ╱╲                                                       │  │
│ │   ──╱  ╲──╱╲─╱  ╲───                                                  │  │
│ │  ╱              ╲                                                      │  │
│ │ ────────────────────────                                               │  │
│ │ X: time   Y: bytes/s                                                   │  │
│ │                                                                        │  │
│ │ ● node1  ● node2  ● node3  ...                                        │  │
│ └────────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Tab 4: Resources

Shows system resource utilization over a selected time range. All graphs are per-node (one line per node). Split into standard and advanced sections.

### Data Displayed

| Widget | Type | Description |
|--------|------|-------------|
| Node Memory Usage | Line graph (half width) | Memory consumption per node (Y: bytes, X: time) |
| Node CPU Usage | Line graph (half width) | CPU utilization per node (Y: integer %, X: time) |
| Drives Free Inodes | Line graph (half width) | Available inodes per drive (X: time; Y-axis hidden) |
| Drive Used Capacity | Line graph (half width) | Disk space used per drive (Y: bytes, X: time) |
| **Advanced** | | |
| Node Syscalls | Line graph (half width) | System calls per node (Y: integer count, X: time) |
| Node File Descriptors | Line graph (half width) | Open file descriptors per node (Y: integer count, X: time) |

### Wireframe

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ [Info] [Usage] [Traffic] [Resources]                                        │
├─────────────────────────────────────────────────────────────────────────────┤
│ ┌─ Date Range Selector ───────────────────────────────────────── [Sync] ─┐  │
│ │  Start: [___________]   End: [___________]                             │  │
│ └────────────────────────────────────────────────────────────────────────┘  │
│                                                                             │
│ ┌──────────────────────────────────┐  ┌──────────────────────────────────┐  │
│ │ Node Memory Usage         [⤢][⬇] │  │ Node CPU Usage            [⤢][⬇] │  │
│ │ (Line Graph, per node)           │  │ (Line Graph, per node)           │  │
│ │                                  │  │                                  │  │
│ │   ╱──╲    ╱╲                     │  │      ╱╲                          │  │
│ │  ╱    ╲──╱  ╲──                 │  │  ───╱  ╲───                      │  │
│ │ ──────────────────               │  │ ──────────────────               │  │
│ │ X: time   Y: bytes               │  │ X: time   Y: %                   │  │
│ │                                  │  │                                  │  │
│ │ ● node1  ● node2  ● node3       │  │ ● node1  ● node2  ● node3       │  │
│ └──────────────────────────────────┘  └──────────────────────────────────┘  │
│                                                                             │
│ ┌──────────────────────────────────┐  ┌──────────────────────────────────┐  │
│ │ Drives Free Inodes        [⤢][⬇] │  │ Drive Used Capacity       [⤢][⬇] │  │
│ │ (Line Graph, per drive)          │  │ (Line Graph, per drive)          │  │
│ │                                  │  │                                  │  │
│ │   ╱──╲    ╱╲                     │  │      ╱╲                          │  │
│ │  ╱    ╲──╱  ╲──                 │  │  ───╱  ╲───                      │  │
│ │ ──────────────────               │  │ ──────────────────               │  │
│ │ X: time   (Y-axis hidden)        │  │ X: time   Y: bytes               │  │
│ │                                  │  │                                  │  │
│ │ ● drive1  ● drive2  ● drive3    │  │ ● drive1  ● drive2  ● drive3    │  │
│ └──────────────────────────────────┘  └──────────────────────────────────┘  │
│                                                                             │
│ ─────────────────── Advanced ────────────────────────────────────────────── │
│                                                                             │
│ ┌──────────────────────────────────┐  ┌──────────────────────────────────┐  │
│ │ Node Syscalls              [⤢][⬇] │  │ Node File Descriptors     [⤢][⬇] │  │
│ │ (Line Graph, per node)           │  │ (Line Graph, per node)           │  │
│ │                                  │  │                                  │  │
│ │   ╱──╲    ╱╲                     │  │      ╱╲                          │  │
│ │  ╱    ╲──╱  ╲──                 │  │  ───╱  ╲───                      │  │
│ │ ──────────────────               │  │ ──────────────────               │  │
│ │ X: time   Y: count               │  │ X: time   Y: count               │  │
│ │                                  │  │                                  │  │
│ │ ● node1  ● node2  ● node3       │  │ ● node1  ● node2  ● node3       │  │
│ └──────────────────────────────────┘  └──────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Implementation Notes

### Current State
- **Info tab**: Works out of the box — data comes from the `AdminInfo` API.
- **Usage / Traffic / Resources tabs**: Disabled when `advancedMetricsStatus === "not configured"` (i.e., Prometheus is not set up).

### Goal
Build native metrics collection within BuckIt so these tabs work without Prometheus. The server should collect and expose time-series data for all widgets listed above through internal APIs.

### Data Sources to Implement

| Category | Metrics Needed |
|----------|---------------|
| Storage | Capacity (used/total), data usage growth over time, object size distribution |
| Objects | Bucket count, object count over time |
| Infrastructure | Server online/offline, drive online/offline, drive used capacity, free inodes |
| Network | API data received rate, API data sent rate, internode data transfer |
| API | Request rate, error rate |
| System | Node memory usage, node CPU usage, node syscalls, node file descriptors, node IO |
| Health | Time since last heal, time since last scan, uptime |

### Multi-Node Display

The original console renders one line per node on each "Node *" chart (CPU, Memory, IO, Syscalls, File Descriptors). With large clusters (50–100+ nodes), this produces an unreadable mess of overlapping lines with no filtering or aggregation.

For our implementation:
- **Default to aggregate view** — show total or average across all nodes
- **Top-N filtering** — e.g., show top 5 nodes by CPU usage, collapse the rest into an "others" band
- **Node selector** — dropdown to pick specific nodes to compare side-by-side

This applies to all per-node widgets:
- Node CPU Usage
- Node Memory Usage
- Node IO
- Node Syscalls
- Node File Descriptors
- Drive Used Capacity
- Drives Free Inodes

### Time Range
All data on Usage, Traffic, and Resources tabs is filtered by the **Date Range Selector** (start/end time). The backend API must support time-range queries for all metrics.
