# FastOpen Local Performance Test Plan

Audience: Codex or an engineer starting a new session. This document captures
the immediate next step after FastOpen Phase 1 implementation: local performance
testing with controlled object reuse/cache levels before moving back to the
two-node warp/HDD-style rig.

The feature under test is **FastGet Phase 1 FastOpen**, described in:

```text
docs/single-trip-get-phase1-fastopen-plan.md
```

FastOpen is the replacement for the earlier single-trip `current/part.1` shadow
prototype. Instead of maintaining a separate shadow object next to each stored
object, FastOpen asks each storage disk for a single framed stream:

```text
FastOpenPart -> CoalescedMetadataFrame -> optional encoded shard/inline body
```

The landing node uses those compact frames to reconstruct the metadata needed for
GET quorum selection and, when eligible, decodes the already-open shard streams
without first running the normal `xl.meta` fan-out. The implementation is scoped to
plain full-object GETs for single-part objects; unsupported requests fall back to
canonical GET before response bytes are committed. FastOpen also includes lazy
replacement readers for mid-stream shard failures and metrics for hit/fallback,
replacement, stream-open/cancel, selected-set failure, and final-error categories.

## 1. Current Code Context

Branch:

```sh
feature/single-trip-get-phase1
```

Relevant recent commits:

```text
d52d9d586 chore: remove FastGet prototype path
b09d64fe3 feat: add FastOpen observability
91526a978 test: expand FastOpen GET golden coverage
6344ff128 feat: add FastOpen lazy replacement
```

FastOpen Phase 1 is implemented and the old `current/part.1` single-trip shadow
prototype has been removed from the request, write, and delete paths. The active
read path is:

```text
GetObjectNInfo -> tryFastOpenGET -> canonical GET fallback
```

When reviewing behavior or expected scope, use the feature plan above as the
source of truth, especially:

```text
Section 1: Target Shape
Section 4: Frame Protocol
Section 5: Metadata Conversion Contract
Section 6: Landing-Node Read Algorithm
Section 11: Metrics
```

There should be no remaining prototype symbols under `cmd`:

```sh
rg -n "singleTrip|SingleTrip|single-trip|tryFastGet|openSingleTrip|writeSingleTrip|invalidateSingleTrip|installSingleTrip|singleTripCurrentDir|fastGetRequestEligible|fastGetHeaderValid|envBuckitFastGetEager|envBuckitFastGetHedge|selectSingleTrip|newSingleTrip" cmd
rg --files cmd | rg 'singletrip'
```

Both commands should return no matches.

Do not run the full `./cmd` package test unless the user asks. The user prefers
running that themselves. Focused tests are fine.

## 2. Why This Local Test Exists

The previous load-test handoff is:

```text
docs/single-trip-warp-load-handoff.md
```

That handoff used `warp get` for concurrent load testing. For the next step, warp
is not enough as the measurement driver because we need to control exactly how
many times each object is requested. OS cache effects are a major part of the
result:

- Requesting every object once approximates cold/no-reuse behavior.
- Requesting every object twice adds a controlled warm-cache component.
- Requesting only 10%, 20%, or 50% of objects more than once models partial cache
  warmth.
- Hot-set runs, where a small set is requested 5x or 10x, model cache-heavy
  workloads.

Warp may still be useful for seeding object sets, but the actual measured GETs
should use a small custom runner/script that controls object ordering and reuse.

Start locally with concurrency 1. The first question is whether FastOpen improves
time-to-first-byte for a single request pattern before introducing stress,
parallelism, remote storage transport, or HDD contention.

## 3. What To Measure

For every run, collect statistics for both:

- TTFB: time from request start until first response byte is read.
- Total latency: time from request start until full object body is read.

Report these fields:

```text
count
min TTFB
max TTFB
mean TTFB
p50 TTFB
p90 TTFB
p99 TTFB
min total latency
max total latency
mean total latency
p50 total latency
p90 total latency
p99 total latency
bytes read
errors
FastOpen hits
FastOpen fallbacks
FastOpen final errors
```

Also split latency stats by access class:

```text
first access / cold-ish
repeated access / warm-ish
all requests combined
```

The min/max values are required. They catch outliers and pathological first-hit
behavior that percentile-only summaries can hide.

## 4. A/B Matrix

Run each matrix row for both paths:

```text
OFF: BUCKIT_FAST_GET=0
ON:  BUCKIT_FAST_GET=1
```

Use the same binary, same local erasure directories, same object set, and same
request plan for both arms.

### Object Sizes

Start with:

```text
640KiB
2MiB
```

Optional sanity case:

```text
64KiB or 128KiB inline/small object
```

The inline/small object case is not expected to be the main FastOpen win. It is
useful to confirm fallback/metadata behavior and metrics.

### Concurrency

Start with:

```text
1
```

Only after concurrency 1 looks correct, scale to:

```text
2
4
8
```

Keep the same cache/reuse profiles at each concurrency level.

### Cache / Reuse Profiles

The cache level is a first-class test axis.

Profiles:

```text
1x all       every object requested exactly once
2x all       every object requested exactly twice
10% hot      10% of objects requested twice, rest once
20% hot      20% of objects requested twice, rest once
50% hot      50% of objects requested twice, rest once
hotset 5x    small hot set requested five times
hotset 10x   small hot set requested ten times
```

Ordering variants:

```text
immediate repeat   obj1,obj1,obj2,obj2,...
pass repeat        all objects once, then repeated/hot objects again
shuffled           randomized mixed warm/cold order
```

The same reuse profile and order must be replayed for OFF and ON.

### Arm Ordering

Alternate path order to reduce drift:

```text
Round 1: OFF, ON
Round 2: ON, OFF
```

For a quick first pass, use:

```text
OFF -> ON -> OFF -> ON
```

## 5. Local Server Setup

Build current branch:

```sh
CGO_ENABLED=0 go build -tags kqueue -trimpath -o /tmp/buckit-fastopen .
```

Create local erasure directories:

```sh
rm -rf /tmp/buckit-fastopen-{1..8}
mkdir -p /tmp/buckit-fastopen-{1..8}
```

Run OFF:

```sh
BUCKIT_FAST_GET=0 \
MINIO_ROOT_USER=buckitadmin \
MINIO_ROOT_PASSWORD=buckitadmin \
/tmp/buckit-fastopen server /tmp/buckit-fastopen-{1..8}
```

Run ON:

```sh
BUCKIT_FAST_GET=1 \
MINIO_ROOT_USER=buckitadmin \
MINIO_ROOT_PASSWORD=buckitadmin \
/tmp/buckit-fastopen server /tmp/buckit-fastopen-{1..8}
```

Keep all other settings identical between arms.

If a run uses the same object set across OFF and ON, do not clear the data
directories between arms. If a run needs a new object set or object size, seed it
explicitly and record the object list.

## 6. Object Seeding

Use deterministic object names:

```text
obj-000001
obj-000002
...
obj-N
```

Seed enough objects for the target size and cache profile. For local smoke, a
small set is acceptable. For more meaningful cache behavior, use a larger working
set.

Warp can be used to seed, but it should not be the measured GET driver for these
controlled reuse tests.

Example seeding with warp, if convenient:

```sh
warp put --host=127.0.0.1:9000 \
  --access-key=buckitadmin --secret-key=buckitadmin \
  --obj.size=640KiB --concurrent=16 --duration=2m --noclear --no-color
```

If warp-generated names are hard to replay exactly, prefer a small custom seeding
script that writes deterministic keys and emits the key list.

## 7. GET Runner Requirements

Create or use a small runner that can:

1. Read a key list.
2. Generate a request order from a cache/reuse profile.
3. Run GETs with configurable concurrency, starting at 1.
4. Measure TTFB and total latency per request.
5. Verify status code and bytes read.
6. Emit CSV or JSON with one row per request.
7. Emit summary stats grouped by access class.

Per-request output should include at least:

```text
run_id
path_arm          OFF or ON
object_size
cache_profile
ordering
concurrency
request_index
key
access_number     1 for first access to this key, 2+ for repeated access
ttfb_ms
total_ms
bytes
status
error
```

Summary output should include the fields listed in Section 3.

The runner should read the full response body after measuring first byte. Do not
measure TTFB by issuing `HEAD`; FastOpen is in the GET body path.

## 8. Metrics To Check During ON Runs

Use the metrics endpoint or existing metric scraping method to confirm:

```text
fast_get_hits
fast_get_fallbacks
fast_open_attempted_total
fast_open_hits_total
fast_open_unsupported_total
fast_open_replacement_path_total
fast_open_streams_opened_total
fast_open_replacement_opens_total
fast_open_selected_set_failures_total
fast_open_stream_cancellations_total
fast_open_final_errors_total
```

Expected for supported non-inline full-object GETs:

```text
FastOpen hits increase
fallbacks near zero
final errors zero
unexpected selected-set failures zero
```

For small/inline or unsupported cases, fallbacks may be expected. Record them
separately and do not mix them with the main non-inline object results.

## 9. First Local Run Checklist

Use this as the first concrete task list in a new session:

1. Build `/tmp/buckit-fastopen`.
2. Start local server OFF with 8 local dirs.
3. Seed deterministic `640KiB` objects.
4. Run GET runner with:
   ```text
   concurrency=1
   cache_profile=1x all
   ordering=key order
   ```
5. Restart server ON against the same dirs.
6. Run the exact same GET plan.
7. Repeat OFF and ON once more:
   ```text
   OFF -> ON -> OFF -> ON
   ```
8. Compare first-access TTFB and total latency.
9. Repeat for:
   ```text
   2x all
   10% hot
   20% hot
   ```
10. Only then move to higher concurrency or remote/two-node testing.

## 10. Interpretation Rules

- Local testing is a sanity and latency-shape test, not the final HDD result.
- Local cache warmth can dominate quickly; always report the cache/reuse profile.
- Do not compare one arm's warm run with another arm's cold-ish run.
- Do not trust an ON result unless FastOpen hit metrics confirm engagement.
- Keep object-size classes separate.
- Keep inline/small-object sanity results separate from supported non-inline
  object results.

## 11. Next Stage After Local

After local concurrency-1 and low-concurrency runs are understood, return to the
two-node approach in:

```text
docs/single-trip-warp-load-handoff.md
```

For remote/HDD tests, keep the same cache/reuse concepts, but expect additional
effects from:

- storage REST transport
- remote stream cancellation
- HDD seek and queueing
- cold-cache reboot protocol
- request distribution across nodes

## 12. Current Local Rig And Findings

This section records the concrete local rig and the latest measured results from
the current investigation. Treat the numbers as local Mac/OS-cache latency-shape
data, not final HDD/distributed results.

### Rig Configuration

Current local run configuration:

```text
address: 127.0.0.1:9000
console: 127.0.0.1:9001
drives: 16 local directories under /private/tmp/buckit-fastopen-{1..16}
storage class: MINIO_STORAGE_CLASS_STANDARD=EC:4
root user: buckitadmin
root password: buckitadmin
measured concurrency: 1
cache profile: 1x-all
ordering: key-order
object count per run: 200
```

The local helper files are:

```text
testing/fastopen-local/rig.sh
testing/fastopen-local/fastopen_local.py
testing/fastopen-local/README.md
```

The active ON arm uses:

```sh
BUCKIT_FAST_GET=1
BUCKIT_FASTOPEN_PROFILE=0
```

The OFF arm uses:

```sh
BUCKIT_FAST_GET=0
BUCKIT_FASTOPEN_PROFILE=0
```

FastOpen now skips the synchronous block-0 decode before returning the
`GetObjectReader` and validates body data during reader consumption, matching
canonical GET timing. This improves time-to-first-byte, but means block-0 decode
or bitrot errors can happen after the response reader has been returned. If the
first read fails before any bytes are written, the S3 handler can still emit an
error response via `WriteOnClose`; after bytes are written, failures are
streaming failures, as in canonical GET.

### Profile Finding: Block-0 Preflight Was The Main TTFB Cost

With detailed profiling enabled on 2MiB objects, the original FastOpen path spent
most of pre-return time in:

```text
try_open_info_first_done          mean 2.191ms
try_prefix_first_done             mean 1.939ms
try_done                          mean 4.229ms
```

For 2MiB objects with a 1MiB erasure block, the original preflight decoded the
first 1MiB before first byte. Disabling block-0 preflight changed the profile
shape to:

```text
try_open_info_first_done          mean 2.060ms
try_done                          mean 2.153ms
```

The body decode still happens, but it happens behind the response reader:

```text
try_no_block0_body_goroutine_decode_done   mean 3.724ms
```

The profile log artifacts from this investigation were:

```text
/private/tmp/buckit-fastopen-logs/server-fastget-1-profile-2m-20260610-1302.log
/private/tmp/fastopen-profile-2m-on.profile-summary.json
/private/tmp/buckit-fastopen-logs/server-fastget-1-profile-2m-noblock0-20260610-1315.log
/private/tmp/fastopen-profile-2m-on-noblock0.profile-summary.json
```

Do not use profiled latency numbers for A/B conclusions. The per-phase logging
adds substantial overhead. Use profiled runs only to locate where time is spent.

### 640KiB Clean Rerun

Run conditions:

```text
object size: 640KiB
keys file: /private/tmp/fastopen-640k-rerun.keys
objects: 200
profile logging: disabled
ON mode: FastOpen enabled, no pre-return block-0 preflight
```

Summary:

```text
Metric       OFF       ON FastOpen  Improvement
total min    1.593 ms  1.129 ms      -0.464 ms (-29.1%)
total mean   2.196 ms  1.383 ms      -0.813 ms (-37.0%)
total p50    2.044 ms  1.297 ms      -0.747 ms (-36.6%)
total p90    2.847 ms  1.503 ms      -1.344 ms (-47.2%)
total p99    4.862 ms  4.390 ms      -0.472 ms (-9.7%)
total max    6.250 ms  5.270 ms      -0.980 ms (-15.7%)

TTFB min     1.273 ms  0.866 ms      -0.408 ms (-32.0%)
TTFB mean    1.863 ms  1.110 ms      -0.753 ms (-40.4%)
TTFB p50     1.742 ms  1.024 ms      -0.718 ms (-41.2%)
TTFB p90     2.432 ms  1.243 ms      -1.189 ms (-48.9%)
TTFB p99     3.837 ms  3.846 ms      +0.008 ms (+0.2%)
TTFB max     4.613 ms  5.053 ms      +0.440 ms (+9.5%)
```

ON metrics:

```text
requests                 200
FastOpen hits            200
fallbacks                0
final errors             0
replacement opens        0
stream cancellations     0
streams opened           2400
bytes read               131,072,000
```

Artifacts:

```text
/private/tmp/fastopen-640k-off-noprofile-noblock0-rerun.json
/private/tmp/fastopen-640k-off-noprofile-noblock0-rerun.csv
/private/tmp/fastopen-640k-on-noprofile-noblock0-rerun.json
/private/tmp/fastopen-640k-on-noprofile-noblock0-rerun.csv
```

### 2MiB Clean Rerun

Run conditions:

```text
object size: 2MiB
keys file: /private/tmp/fastopen-2m.keys
objects: 200
profile logging: disabled
ON mode: FastOpen enabled, no pre-return block-0 preflight
```

Summary:

```text
Metric       OFF       ON FastOpen  Improvement
total min    3.085 ms  3.041 ms      -0.044 ms (-1.4%)
total mean   4.244 ms  3.919 ms      -0.325 ms (-7.7%)
total p50    4.184 ms  3.763 ms      -0.421 ms (-10.1%)
total p90    4.982 ms  4.575 ms      -0.408 ms (-8.2%)
total p99    6.424 ms  7.293 ms      +0.869 ms (+13.5%)
total max    7.683 ms  7.888 ms      +0.205 ms (+2.7%)

TTFB min     1.999 ms  1.892 ms      -0.107 ms (-5.4%)
TTFB mean    2.890 ms  2.550 ms      -0.340 ms (-11.8%)
TTFB p50     2.850 ms  2.430 ms      -0.420 ms (-14.7%)
TTFB p90     3.516 ms  2.955 ms      -0.561 ms (-16.0%)
TTFB p99     4.189 ms  5.542 ms      +1.353 ms (+32.3%)
TTFB max     6.255 ms  5.892 ms      -0.363 ms (-5.8%)
```

ON metrics:

```text
requests                 200
FastOpen hits            200
fallbacks                0
final errors             0
replacement opens        0
stream cancellations     0
streams opened           2400
bytes read               419,430,400
```

Artifacts:

```text
/private/tmp/fastopen-2m-off-noprofile-rerun.json
/private/tmp/fastopen-2m-off-noprofile-rerun.csv
/private/tmp/fastopen-2m-on-noblock0-noprofile-rerun.json
/private/tmp/fastopen-2m-on-noblock0-noprofile-rerun.csv
```

### Integrity Verification

The benchmark runner verifies HTTP success and bytes read. For the FastOpen ON
path without pre-return block-0 preflight, a separate byte-for-byte verification
was run against all 200 2MiB keys.
Each response body was compared with the deterministic seeded payload generated
from the key name.

Result:

```text
objects checked         200
object size             2,097,152 bytes
total bytes read        419,430,400
mismatches              0
```

Artifact:

```text
/private/tmp/fastopen-2m-on-noblock0-integrity.json
```

This confirms the FastOpen ON path returned complete and intact objects for the
tested dataset. It does not remove the streaming-error caveat: without pre-return
block-0 preflight, some classes of decode/bitrot errors may be observed after
response commit.

### Linux Host HDD-Simulated Docker Results

A separate run was executed on the Linux host `ubuntudell` to get closer to a
distributed HDD-style topology. This used the Docker rig rather than the local
single-host process rig.

Run conditions:

```text
host: ubuntudell
topology: 4 nodes x 4 drives
objects: 200
cache profile: 1x-all
ordering: key-order
concurrency: 1
fresh seed: yes, separate OFF and ON prefixes
FastOpen OFF credentials: buckitadmin / buckitadmin
FastOpen ON credentials:  buckitadmin / buckitadmin
simulated disk latency: dm-delay 8ms per drive
cpu limit: 1 vCPU per node
```

Validation:

```text
OFF node1 dmsetup: 0 2097152 delay 7:15 0 8
ON  node1 dmsetup: 0 2097152 delay 7:21 0 8
OFF node1 cpu.max: 100000 100000
ON  node1 cpu.max: 100000 100000
```

#### Linux 2MiB

Summary:

```text
Metric       OFF         ON FastOpen  Improvement
total min    128.993 ms  108.807 ms   -20.187 ms (-15.6%)
total mean   229.246 ms  174.463 ms   -54.784 ms (-23.9%)
total p50    219.875 ms  176.061 ms   -43.815 ms (-19.9%)
total p90    302.243 ms  212.199 ms   -90.044 ms (-29.8%)
total p99    338.413 ms  281.245 ms   -57.168 ms (-16.9%)
total max    369.686 ms  298.750 ms   -70.936 ms (-19.2%)

TTFB min     109.175 ms   81.369 ms   -27.806 ms (-25.5%)
TTFB mean    189.542 ms  141.047 ms   -48.495 ms (-25.6%)
TTFB p50     184.937 ms  135.406 ms   -49.531 ms (-26.8%)
TTFB p90     251.949 ms  179.043 ms   -72.906 ms (-28.9%)
TTFB p99     295.721 ms  204.766 ms   -90.955 ms (-30.8%)
TTFB max     311.193 ms  240.950 ms   -70.243 ms (-22.6%)
```

Artifacts on `ubuntudell`:

```text
/tmp/buckit-linux-hdd-2m-4x4-off.csv
/tmp/buckit-linux-hdd-2m-4x4-off.json
/tmp/buckit-linux-hdd-2m-4x4-on.csv
/tmp/buckit-linux-hdd-2m-4x4-on.json
/tmp/buckit-linux-hdd-2m-4x4-compare.json
```

#### Linux 640KiB

Summary:

```text
Metric       OFF         ON FastOpen  Improvement
total min     38.723 ms   30.598 ms    -8.125 ms (-21.0%)
total mean    74.774 ms   71.240 ms    -3.534 ms (-4.7%)
total p50     71.389 ms   66.837 ms    -4.552 ms (-6.4%)
total p90    100.235 ms  101.639 ms    +1.404 ms (+1.4%)
total p99    126.827 ms  127.830 ms    +1.004 ms (+0.8%)
total max    155.683 ms  142.872 ms   -12.811 ms (-8.2%)

TTFB min      34.618 ms   28.693 ms    -5.925 ms (-17.1%)
TTFB mean     70.955 ms   67.714 ms    -3.241 ms (-4.6%)
TTFB p50      67.777 ms   64.173 ms    -3.604 ms (-5.3%)
TTFB p90      96.764 ms  100.535 ms    +3.771 ms (+3.9%)
TTFB p99     126.106 ms  124.369 ms    -1.737 ms (-1.4%)
TTFB max     148.647 ms  131.069 ms   -17.578 ms (-11.8%)
```

Artifacts on `ubuntudell`:

```text
/tmp/buckit-linux-hdd-640k-4x4-off.csv
/tmp/buckit-linux-hdd-640k-4x4-off.json
/tmp/buckit-linux-hdd-640k-4x4-on.csv
/tmp/buckit-linux-hdd-640k-4x4-on.json
/tmp/buckit-linux-hdd-640k-4x4-compare.json
```

### Current Interpretation

For this local 16-drive EC:4 setup:

- Original FastOpen with block-0 preflight was slightly worse for 2MiB objects,
  mostly because it decoded a full 1MiB erasure block before first byte.
- Disabling block-0 preflight made FastOpen faster on mean and p50 for both
  640KiB and 2MiB.
- 640KiB improved substantially across mean, p50, and p90.
- 2MiB improved on mean, p50, and p90, but p99 worsened in the latest rerun.
- All measured FastOpen ON runs had 200/200 FastOpen hits, zero fallbacks, zero
  final errors, and zero replacement opens.

For the Linux-host distributed HDD-simulated Docker setup:

- 2MiB improved strongly across every measured TTFB and total metric.
- 640KiB still improved on min, mean, p50, p99, and max, but p90 was roughly
  flat to slightly worse.
- The HDD-simulated runs show the benefit is materially larger once storage
  latency dominates, especially for the 2MiB path.

### Linux Host Rig Setup

The Linux-host runs in this session used:

```text
host: ubuntudell
ssh:  rooseveltlai@ubuntudell
docker: installed on host
sudo: passwordless
go: installed on host
```

Buckit was not built on every run from source on the host. A Linux `amd64`
binary was built locally once and copied to the host at:

```text
/home/rooseveltlai/buckit-linux-run/testing/cluster/buckit
```

The remote working tree used for all Linux-host runs was:

```text
/home/rooseveltlai/buckit-linux-run
```

Linux-host benchmark results were written to:

```text
/home/rooseveltlai/buckit-linux-results
```

### Linux Host Credentials

The Docker cluster harness on `ubuntudell` starts Buckit with:

```text
MINIO_ROOT_USER=buckitadmin
MINIO_ROOT_PASSWORD=buckitadmin
```

This is different from the local single-process rig, which often used
`minioadmin/minioadmin`.
Any remote benchmark helper or manual validation must use:

```text
access key: buckitadmin
secret key: buckitadmin
```

### Linux Host Helper Scripts

The following helper exists in the repository and should be reused in future
sessions for `640KiB` cold/warm HDD-style runs:

```text
testing/fastopen-docker/run_hdd_arm.sh
```

It does the following:

```text
- loads dm_delay with sudo modprobe dm_delay
- verifies the delay target exists in dmsetup targets
- creates the 4x4 Docker rig
- seeds objects with retries
- runs the benchmark
- captures node1 dmsetup/cpu.max validation
- destroys the rig after the arm completes
```

For `2MiB`, a session-local remote helper was created directly on `ubuntudell`:

```text
/home/rooseveltlai/buckit-linux-run/testing/fastopen-docker/run_hdd_arm_2m.sh
```

That script is not committed in this repository. If another session needs to
repeat the cold `2MiB` methodology, either recreate that helper or generalize
`testing/fastopen-docker/run_hdd_arm.sh` to accept object size as a parameter.

### Important HDD Delay Behavior After Reboot

On `ubuntudell`, rebooting the host unloads or otherwise leaves unavailable the
`dm-delay` target until it is explicitly reloaded.

Observed behavior:

```text
before modprobe after reboot:
  dmsetup targets -> striped, linear, error

after:
  sudo modprobe dm_delay

dmsetup targets -> delay, striped, linear, error
```

Because of this, any post-reboot HDD-simulated run must do:

```text
sudo modprobe dm_delay
sudo dmsetup targets | grep '^delay'
```

before starting the Docker rig.

This was the reason the first cold reboot attempts failed with:

```text
HDD_DELAY_MS=8 requested, but the Linux kernel in this container does not
expose the device-mapper delay target.
```

That is now handled by `testing/fastopen-docker/run_hdd_arm.sh`.

### Cold Reboot Methodology

Cold `640KiB` and `2MiB` Linux-host runs used the following method:

```text
1. Force reboot host with sudo reboot -f
2. Wait for SSH to drop
3. Wait for SSH to return
4. Verify uptime -s changed
5. Reload dm_delay with modprobe
6. Run one arm only
7. Repeat reboot cycle for the second arm
```

The order used in the completed cold runs was:

```text
ON first, then OFF
```

Cold run characteristics:

```text
- unique key prefixes per run tag
- fresh seeded objects per arm
- concurrency 1
- cache profile 1x-all
- ordering key-order
- 4 nodes x 4 drives
- 1 vCPU per node
- 8ms dm-delay per drive
```

### Cold Reboot Results

#### Cold 640KiB, 300 objects, ON first

Run tag:

```text
20260611T002329Z
```

Summary:

```text
Metric       ON         OFF        ON vs OFF
total min    30.601 ms  33.651 ms  -3.050 ms (+9.1%)
total mean   69.889 ms  72.094 ms  -2.205 ms (+3.1%)
total p50    66.525 ms  68.555 ms  -2.030 ms (+3.0%)
total p90    98.727 ms  97.309 ms  +1.418 ms (-1.5%)
total p99   120.971 ms 146.405 ms -25.434 ms (+17.4%)
total max   178.650 ms 194.865 ms -16.215 ms (+8.3%)

TTFB min     29.259 ms  30.596 ms  -1.337 ms (+4.4%)
TTFB mean    65.942 ms  67.911 ms  -1.968 ms (+2.9%)
TTFB p50     63.916 ms  63.607 ms  +0.309 ms (-0.5%)
TTFB p90     94.794 ms  94.247 ms  +0.547 ms (-0.6%)
TTFB p99    120.151 ms 135.278 ms -15.127 ms (+11.2%)
TTFB max    176.764 ms 193.999 ms -17.235 ms (+8.9%)
```

Interpretation:

```text
At 640KiB under cold rebooted HDD-simulated conditions, FastOpen ON was
roughly flat to slightly better overall. Mean/min/max and total p50 improved,
while TTFB p50/p90 stayed effectively flat.
```

Artifacts:

```text
/home/rooseveltlai/buckit-linux-results/buckit-linux-hdd-640k-cold-on-20260611T002329Z.json
/home/rooseveltlai/buckit-linux-results/buckit-linux-hdd-640k-cold-off-20260611T002329Z.json
/home/rooseveltlai/buckit-linux-results/buckit-linux-hdd-640k-cold-compare-20260611T002329Z.json
```

#### Cold 2MiB, 300 objects, ON first

Run tag:

```text
20260611T003704Z
```

Summary:

```text
Metric       ON          OFF         ON vs OFF
total min     99.272 ms   93.917 ms   +5.354 ms (-5.7%)
total mean   177.847 ms  162.622 ms  +15.225 ms (-9.4%)
total p50    178.117 ms  162.024 ms  +16.093 ms (-9.9%)
total p90    216.456 ms  201.248 ms  +15.207 ms (-7.6%)
total p99    288.241 ms  261.506 ms  +26.735 ms (-10.2%)
total max    371.922 ms  318.920 ms  +53.002 ms (-16.6%)

TTFB min      73.951 ms   76.725 ms   -2.774 ms (+3.6%)
TTFB mean    144.288 ms  135.226 ms   +9.062 ms (-6.7%)
TTFB p50     139.642 ms  132.780 ms   +6.861 ms (-5.2%)
TTFB p90     187.153 ms  175.996 ms  +11.158 ms (-6.3%)
TTFB p99     217.248 ms  209.498 ms   +7.750 ms (-3.7%)
TTFB max     271.150 ms  240.558 ms  +30.592 ms (-12.7%)
```

Interpretation:

```text
At 2MiB under cold rebooted HDD-simulated conditions, FastOpen ON was worse
than OFF across most latency metrics. This differs from the earlier non-cold
Linux 2MiB run, where ON was strongly better.
```

Artifacts:

```text
/home/rooseveltlai/buckit-linux-results/buckit-linux-hdd-2m-cold-on-20260611T003704Z.json
/home/rooseveltlai/buckit-linux-results/buckit-linux-hdd-2m-cold-off-20260611T003704Z.json
/home/rooseveltlai/buckit-linux-results/buckit-linux-hdd-2m-cold-compare-20260611T003704Z.json
```

### Session Handoff Notes

State at the end of this session:

```text
- Local Docker rig exists and supports 4x4 / 8x2 style cluster bring-up
- Linux-host HDD-simulated methodology is working
- dm_delay must be reloaded after reboot
- remote helper for 640KiB cold/warm HDD runs is committed
- remote helper for 2MiB cold run exists only on ubuntudell
- warm Linux 2MiB strongly favored ON
- cold rebooted Linux 2MiB favored OFF
- cold rebooted Linux 640KiB was roughly neutral to slightly ON-positive
```

If a future session continues this investigation, the highest value follow-up is
not more setup work, but explaining the divergence between:

```text
warm Linux 2MiB: ON much better
cold Linux 2MiB: ON worse
```

That is the main unresolved result from this session.

Next useful tests:

```text
1. Repeat each clean A/B at least 5 rounds and aggregate.
2. Use fresh object names or reboot/drop cache if cold-cache accuracy matters.
3. Add larger object sizes, such as 16MiB and 64MiB.
4. Add concurrency 2, 4, and 8.
5. Add HTTP-level tests proving first-read failures before response body write
   still produce canonical-compatible error responses.

### Additional Session Handoff: Real Linux Hosts

After the Docker and EC2 work, a non-Docker two-host Linux rig was also
validated in this session.

Real-host rig:

```text
host1: ubuntudell
host2: thinkpad
node1 alias: buckit-node1
node2 alias: buckit-node2
layout: 2 nodes / 8 drives total
root dir on both hosts: ~/buckit-fastopen-ec2
```

Host IPs used:

```text
ubuntudell: 192.168.4.46
thinkpad:   192.168.4.47
```

Key observation:

```text
The checked-in fastopen_local.py seed helper was too slow for 40k real-host
seeding. A threaded boto3 uploader was created and should now be reused from:

testing/fastopen-ec2/threaded_seed.py
```

Latest real-host validation run:

```text
run dir: ~/buckit-fastopen-ec2/results/20260611T131931Z-on-5k-conc40
arm: ON
objects: 5000
object size: 640KiB
concurrency: 40
client: ubuntudell
```

Summary:

```text
elapsed: 253.44s
throughput: 19.73 obj/s
bandwidth: 12.93 MB/s
total p50: 1832.61ms
TTFB p50: 1829.37ms
httptrace reuse: 98.95%
```

Slice interpretation:

```text
No convincing late-run degradation.
The run was uniformly slow from the beginning, with only mild late wobble.
```

Resource summary:

```text
ubuntudell CPU mean: 14.74%
thinkpad CPU mean:    3.74%
ubuntudell heap p50: 465.7 MB
thinkpad heap p50:   322.7 MB
```

### Additional Session Handoff: EC2 m6g.medium Spot Run

An EC2 follow-up run was executed in this session on two `m6g.medium` Spot
instances using the checked-in two-node host rig under:

```text
testing/fastopen-ec2/
```

Rig:

```text
region: us-east-1
instance type: m6g.medium
purchase model: Spot
topology: 2 nodes / 8 drives total
drives per node: 4 directories under /mnt
storage class: EC:2
object size: 640KiB
object count: 40000
concurrency: 16
cache profile: 1x-all
ordering: key-order
client/ingress host: node1
```

Important caveat:

```text
This was not a fully symmetric cold A/B.
The OFF arm ran after seeding on the same boot.
The ON arm ran only after both hosts were rebooted.
So the run is useful, but it is not a strict cold-cache OFF-vs-ON comparison.
```

Measured result:

```text
elapsed_seconds  OFF=336.569  ON=280.436  delta=-56.134s (-16.7%)

TTFB mean        OFF=129.434ms  ON=109.081ms  delta=-20.353ms (-15.7%)
TTFB p50         OFF=121.091ms  ON=103.692ms  delta=-17.399ms (-14.4%)
TTFB p90         OFF=174.091ms  ON=144.136ms  delta=-29.955ms (-17.2%)
TTFB p99         OFF=263.517ms  ON=191.035ms  delta=-72.483ms (-27.5%)

total mean       OFF=134.520ms  ON=112.084ms  delta=-22.436ms (-16.7%)
total p50        OFF=126.409ms  ON=106.768ms  delta=-19.641ms (-15.5%)
total p90        OFF=179.711ms  ON=147.538ms  delta=-32.173ms (-17.9%)
total p99        OFF=272.814ms  ON=196.118ms  delta=-76.696ms (-28.1%)
```

Throughput:

```text
OFF throughput: 118.85 obj/s, 74.28 MiB/s
ON  throughput: 142.64 obj/s, 89.15 MiB/s
```

FastOpen ON metrics:

```text
fast_get_hits                         40000
fast_get_fallbacks                        0
fast_open_attempted_total             40000
fast_open_hits_total                  40000
fast_open_final_errors_total              0
fast_open_replacement_opens_total         0
fast_open_streams_opened_total       240000
fast_open_httptrace_connections_total 120000
fast_open_httptrace_reused_connections_total 119779
fast_open_httptrace_was_idle_connections_total 119339
```

Slice read:

```text
The 40000-request run was summarized in 2000-request slices.
All 20 slices improved ON vs OFF on both mean TTFB and mean total latency.
The first slice had the largest gain, but there was no late-run reversal.
```

Resource highlights:

```text
node1 host CPU mean:   OFF 54.16%  ON 56.08%
node1 mem used mean:   OFF 39.28%  ON 28.87%
node1 GC count delta:  OFF 248     ON 220
node1 heap alloc mean: OFF 496.95 MiB  ON 475.43 MiB

node2 host CPU mean:   OFF 19.47%  ON 21.27%
node2 mem used mean:   OFF 29.82%  ON 20.89%
node2 GC count delta:  OFF 102     ON 144
node2 heap alloc mean: OFF 300.25 MiB  ON 321.61 MiB
```

Socket-state highlights:

```text
node1 mean ESTAB:     OFF 64.67  ON 51.14
node1 mean TIME_WAIT: OFF  2.80  ON  1.60

node2 mean ESTAB:     OFF 64.79  ON 51.14
node2 mean TIME_WAIT: OFF  0.00  ON  5.36

No meaningful SYN-SENT, SYN-RECV, CLOSE-WAIT, or FIN-* accumulation was seen
in either arm.
```

Local artifact bundle from this run:

```text
/tmp/buckit-fastopen-ec2-results-20260611T1400Z/20260611T1400Z-ec2-m6g-medium-40k/
```

Useful files:

```text
off.csv
off.json
on.csv
on.json
off.slices-2000.json
on.slices-2000.json
compare-summary.txt
slice-compare-2000.tsv
node1/off.*.json
node1/on.*.json
node2/off.*.json
node2/on.*.json
```

### Additional Session Handoff: EC2 3-Round Spot Run With Dedicated Loadgen

A stricter EC2 follow-up run was executed in this session using two
`m6g.medium` Spot storage nodes plus one dedicated `t3.micro` Spot loadgen
host. Unlike the earlier single-round EC2 run above, this run rebooted all
hosts before every measured arm.

Rig:

```text
region: us-east-1
storage nodes: 2 x m6g.medium Spot
load generator: 1 x t3.micro Spot
topology: 2 nodes / 8 drives total
drives per node: 4 directories under /mnt
storage class: EC:2
object size: 640KiB
object count: 40000
concurrency: 32
cache profile: 1x-all
ordering: key-order
seed policy: seed once, do not reseed between runs
round order: ON->OFF, OFF->ON, ON->OFF
```

Method:

```text
- all three instances were rebooted before every arm
- a dedicated loadgen host issued the measured GET workload
- the same 40000-key plan was reused for every arm
- per-round raw request CSV/JSON, Buckit metrics before/after, and host/GC/socket
  monitor files were copied locally after each valid round
- an initial round1 OFF attempt had startup 503s and was preserved separately,
  but excluded from comparison
```

Per-round benchmark comparison:

```text
round  arm  elapsed_s  obj_s   MiB_s   TTFB p50  TTFB p90  TTFB p99  total p50  total p90  total p99
1      ON   230.33     173.67  108.54  154.15    208.64    278.29    175.14     240.92     342.69
1      OFF  283.46     141.11   88.19  191.82    253.86    353.49    212.77     289.68     450.93
1      Δ    -18.7%     +23.1%  +23.1%  -19.6%    -17.8%    -21.3%    -17.7%     -16.8%     -24.0%

2      ON   230.52     173.52  108.45  153.27    204.26    283.81    175.75     239.37     349.17
2      OFF  284.47     140.61   87.88  193.12    253.98    352.01    213.21     287.66     437.94
2      Δ    -19.0%     +23.4%  +23.4%  -20.6%    -19.6%    -19.4%    -17.6%     -16.8%     -20.3%

3      ON   231.42     172.85  108.03  153.94    206.31    281.76    175.34     241.65     341.18
3      OFF  282.47     141.61   88.50  191.47    250.77    346.53    211.47     283.99     440.72
3      Δ    -18.1%     +22.1%  +22.1%  -19.6%    -17.7%    -18.7%    -17.1%     -14.9%     -22.6%
```

Median summary across the three valid rounds:

```text
elapsed        ON 230.52s   OFF 283.46s   delta -18.7%
throughput     ON 172.85 obj/s, 108.03 MiB/s
               OFF 141.11 obj/s,  88.19 MiB/s
               delta +22.5%

TTFB p50       ON 153.94ms  OFF 191.82ms  delta -19.7%
TTFB p90       ON 206.31ms  OFF 253.86ms  delta -18.7%
TTFB p99       ON 281.76ms  OFF 352.01ms  delta -20.0%

total p50      ON 175.34ms  OFF 212.77ms  delta -17.6%
total p90      ON 240.92ms  OFF 287.66ms  delta -16.2%
total p99      ON 342.69ms  OFF 440.72ms  delta -22.2%
```

Buckit-specific metrics:

```text
round  arm  attempted  fastopen_hits  fastget_hits  fallbacks  final_errors  replacement_opens  http_conns  fresh  reused   was_idle  streams_opened
1      ON     40000        40000         40000          0            0               0            120000    1909   118091    117143        240000
1      OFF        0            0             0          0            0               0                 0       0        0         0             0
2      ON     40000        40000         40000          0            0               0            120000    1091   118909    118262        240000
2      OFF        0            0             0          0            0               0                 0       0        0         0             0
3      ON     40000        40000         40000          0            0               0            120000     623   119377    118840        240000
3      OFF        0            0             0          0            0               0                 0       0        0         0             0
```

Buckit metrics read:

```text
- FastOpen engaged for every ON request in every round
- no FastOpen fallback, replacement-open, or final-error activity was observed
- connection reuse improved across ON rounds:
  fresh: 1909 -> 1091 -> 623
  reused: 118091 -> 118909 -> 119377
```

Median OS/runtime comparison across the three valid rounds:

```text
host     metric               OFF median  ON median  delta
loadgen  CPU p50 %             15.42       18.36     +19.1%
loadgen  mem used p50 %        47.08       47.36      +0.6%
loadgen  ESTAB sockets p50     31.00       32.00      +3.2%
loadgen  TIME_WAIT p50         93.00      110.00     +18.3%

node1    CPU p50 %             48.04       53.27     +10.9%
node1    mem used p50 %        29.40       28.71      -2.4%
node1    ESTAB sockets p50    117.00       89.50     -23.5%
node1    TIME_WAIT p50         23.00        0.00    -100.0%
node1    GC total             235         208        -11.5%
node1    GC pause ms total     25.04       22.47     -10.3%
node1    heap alloc p50 MiB   555.00      523.79      -5.6%
node1    goroutines p50       870         809         -7.0%

node2    CPU p50 %             23.00       26.92     +17.1%
node2    mem used p50 %        22.12       21.68      -2.0%
node2    ESTAB sockets p50    117.00       90.00     -23.1%
node2    GC total             101         151        +49.5%
node2    GC pause ms total      7.22       10.89     +50.8%
node2    heap alloc p50 MiB   296.47      306.31      +3.3%
node2    goroutines p50       598         570         -4.7%
```

Current read:

```text
- the benchmark result was very stable across all three rebooted rounds
- ON improved elapsed time by about 18-19% and throughput by about 22-23%
- node1, the ingress/storage node, looked healthier on ON:
  lower socket pressure, lower GC totals, lower heap, fewer goroutines
- node2 showed the same socket improvement but somewhat higher GC work on ON
- the Buckit metrics support a clean happy-path interpretation rather than a
  fallback-heavy or retry-heavy win
```

Local artifact bundle from this run:

```text
/tmp/buckit-fastopen-3round-results/20260611T1453Z-3round-conc32/
```

Important artifact note:

```text
The invalid startup-race attempt was preserved separately at:
/tmp/buckit-fastopen-3round-results/20260611T1453Z-3round-conc32/round1-off-invalid-startup503/
Do not include that arm in performance comparison.
```

### EC2 West-1: 4 x c6g.xlarge, 16 Drives, EC:4, 40000 x 640KiB

Because the earlier 8-node Spot topology was too unstable under current Spot
capacity, the next formal EC2 run was revised to:

```text
region / AZ / subnet: us-west-1 / us-west-1c / single subnet
storage nodes: 4 x c6g.xlarge Spot
load generator: 1 x t3.large Spot
data volumes: 4 x gp3 15 GiB attached per storage node
drive count: 16 total
storage class: EC:4
ingress target: one fixed storage node
object count: 40000
object size: 640KiB
concurrency: 32
reboot policy: reboot all storage nodes and the load generator before every arm
seed policy: seed once, no reseed between arms
```

Seed and measured object set:

```text
seed run id: 20260611T1848Z-west1-4node-640k-prep
key prefix: obj-west1-4node-640k-
keys file: /home/ec2-user/buckit-fastopen-loadgen/results/20260611T1848Z-west1-4node-640k-prep/keys-40000.txt
```

Valid rounds:

```text
round 1: ON -> OFF
round 2: OFF -> ON
round 3: ON -> OFF
```

Per-round benchmark comparison:

```text
round  arm   elapsed s  obj/s    MiB/s   TTFB min  TTFB p50  TTFB p90  TTFB p99  TTFB max   total min  total p50  total p90  total p99  total max
1      ON     64.529    619.87   387.42    5.486    39.953    65.198    93.302   1104.361      7.852     48.243     75.339    108.773   1114.518
1      OFF    78.018    512.70   320.44    7.344    50.140    77.703   107.219   1107.899     10.754     59.387     88.624    123.590   1111.646
1      Δ      -17.3%    +20.9%   +20.9%   -25.3%    -20.3%    -16.1%    -13.0%     -0.3%     -27.0%     -18.8%     -15.0%    -12.0%     +0.3%

2      ON     64.185    623.20   389.50    5.407    39.656    63.623    88.392   1094.251      7.683     48.103     74.077    102.646   1102.881
2      OFF    77.744    514.51   321.57    5.932    50.051    77.434   105.513   1123.552     10.716     59.291     88.658    119.984   1144.737
2      Δ      -17.4%    +21.1%   +21.1%    -8.9%    -20.8%    -17.8%    -16.2%     -2.6%     -28.3%     -18.9%     -16.4%    -14.5%     -3.7%

3      ON     64.421    620.92   388.07    6.280    39.473    63.711    90.447   1125.307     10.489     47.827     74.128    105.256   1144.370
3      OFF    78.278    510.99   319.37    5.655    50.014    77.307   107.061   1139.231      7.559     59.814     89.229    122.515   1141.383
3      Δ      -17.7%    +21.5%   +21.5%   +11.1%    -21.1%    -17.6%    -15.5%     -1.2%     +38.8%     -20.0%     -16.9%    -14.1%     +0.3%
```

Median summary across the three valid rounds:

```text
elapsed        ON 64.421s   OFF 78.018s   delta -17.4%
throughput     ON 620.92 obj/s, 388.07 MiB/s
               OFF 512.70 obj/s, 320.44 MiB/s
               delta +21.1%

TTFB p50       ON 39.656ms  OFF 50.051ms  delta -20.8%
TTFB p90       ON 63.711ms  OFF 77.434ms  delta -17.7%
TTFB p99       ON 90.447ms  OFF 107.061ms delta -15.5%

total p50      ON 48.103ms  OFF 59.387ms  delta -19.0%
total p90      ON 74.128ms  OFF 88.658ms  delta -16.4%
total p99      ON 105.256ms OFF 122.515ms delta -14.1%
```

Cross-round ranges for selected aggregate metrics:

```text
elapsed:
- ON  min/median/max = 64.185 / 64.421 / 64.529 s
- OFF min/median/max = 77.744 / 78.018 / 78.278 s

throughput:
- ON  obj/s min/median/max = 619.87 / 620.92 / 623.20
- OFF obj/s min/median/max = 510.99 / 512.70 / 514.51
- ON  MiB/s min/median/max = 387.42 / 388.07 / 389.50
- OFF MiB/s min/median/max = 319.37 / 320.44 / 321.57

TTFB p50:
- ON  min/median/max = 39.473 / 39.656 / 39.953 ms
- OFF min/median/max = 50.014 / 50.051 / 50.140 ms

total p50:
- ON  min/median/max = 47.827 / 48.103 / 48.243 ms
- OFF min/median/max = 59.291 / 59.387 / 59.814 ms
```

Buckit-specific metrics for the ON arms:

```text
round  attempted  fastopen_hits  fastget_hits  fallbacks  final_errors  replacement_opens  http_conns  fresh  reused   was_idle  streams_opened
1        40000        40000         40000          0            0               0            359663     695   358968    356456        480000
2        40000        40000         40000          0            0               0            359663     781   358882    356375        480000
3        40000        40000         40000          0            0               0            359663     785   358878    356366        480000
```

Buckit metrics read:

```text
- FastOpen engaged for every ON request in all three rounds
- no FastOpen fallback, replacement-open, or final-error activity was observed
- connection reuse remained extremely high:
  round 1 reused 358968 / fresh 695
  round 2 reused 358882 / fresh 781
  round 3 reused 358878 / fresh 785
```

Detailed OS/runtime stats by round:

```text
round   host     arm   CPU p50  mem p50 %  ESTAB p50  TIME_WAIT p50  GC total  GC pause ms  heap alloc p50 MiB  goroutines p50
1       loadgen  ON    52.82      6.80       29.00        464.00         -          -               -                 -
1       loadgen  OFF   43.83      6.93       31.00        258.00         -          -               -                 -
1       node1    ON    64.78     13.00       58.00          4.00       179        50.08           423.58           1095.50
1       node1    OFF   62.66     13.63       89.00         19.00       241        62.96           508.08           1288.00
1       node2    ON    20.20     10.26       58.00          8.00        79         9.46           289.09            553.00
1       node2    OFF   18.67     10.48       89.00          4.00        54         6.49           288.47            581.00
1       node3    ON    21.17     10.50       60.00          4.00        79         9.30           287.79            555.00
1       node3    OFF   19.18     10.58       85.00          4.00        54         6.92           288.08            582.00
1       node4    ON    20.16     10.56       61.00          8.00        77         9.19           310.97            565.00
1       node4    OFF   18.56     10.73       79.00          4.00        54         6.15           285.66            576.00

2       loadgen  ON    54.64      6.86       29.00        422.00         -          -               -                 -
2       loadgen  OFF   44.53      6.87       30.00        290.00         -          -               -                 -
2       node1    ON    65.36     13.13       59.00          8.00       179        48.84           477.70           1081.00
2       node1    OFF   63.08     13.59       85.00          5.00       240        63.10           510.89           1296.00
2       node2    ON    22.07     10.26       58.00          4.00        79         8.96           286.89            553.00
2       node2    OFF   16.75     10.51       87.00          8.00        53         6.59           306.13            581.00
2       node3    ON    21.14     10.36       61.00          4.00        79        10.27           307.44            555.50
2       node3    OFF   18.27     10.67       90.00          4.00        54         6.52           281.64            587.00
2       node4    ON    21.06     10.51       63.00          4.00        79        10.11           286.31            561.00
2       node4    OFF   17.72     10.78       89.00          4.00        53         6.82           315.37            584.50

3       loadgen  ON    53.22      7.01       27.50        438.50         -          -               -                 -
3       loadgen  OFF   45.88      6.85       30.00        184.00         -          -               -                 -
3       node1    ON    65.82     12.93       57.00          8.00       183        45.39           485.35           1083.50
3       node1    OFF   63.19     13.96       92.00         13.00       236        63.03           512.28           1263.50
3       node2    ON    20.57     10.24       57.00          4.00        79         9.55           284.76            552.00
3       node2    OFF   17.78     10.47       92.00          7.00        53         6.77           298.30            587.50
3       node3    ON    20.26     10.43       58.50          4.00        79        10.35           306.04            553.00
3       node3    OFF   18.13     10.70       94.50          4.00        53         6.39           308.23            592.00
3       node4    ON    21.71     10.58       60.00          4.00        79         9.35           316.60            556.00
3       node4    OFF   17.52     10.71       79.00          4.00        54         6.30           286.42            577.00
```

Median OS/runtime comparison across the three valid rounds:

```text
host     metric               OFF median  ON median  delta
loadgen  CPU p50 %             44.53       53.22     +19.5%
loadgen  mem used p50 %         6.87        6.86      -0.0%
loadgen  mem used p50 MiB     537.14      537.00      -0.0%
loadgen  ESTAB sockets p50     30.00       29.00      -3.3%
loadgen  TIME_WAIT p50        258.00      438.50     +70.0%

node1    CPU p50 %             63.08       65.36      +3.6%
node1    mem used p50 %        13.63       13.00      -4.6%
node1    mem used p50 MiB    1058.43     1010.11      -4.6%
node1    ESTAB sockets p50     89.00       58.00     -34.8%
node1    TIME_WAIT p50         13.00        8.00     -38.5%
node1    GC total             240         179        -25.4%
node1    GC pause ms total     63.03       48.84     -22.5%
node1    heap alloc p50 MiB   510.89      477.70      -6.5%
node1    goroutines p50      1288.00     1083.50     -15.9%

node2    CPU p50 %             17.78       20.57     +15.6%
node2    mem used p50 %        10.48       10.26      -2.0%
node2    ESTAB sockets p50     89.00       58.00     -34.8%
node2    TIME_WAIT p50          7.00        4.00     -42.9%
node2    GC total              53          79        +49.1%
node2    GC pause ms total      6.59        9.46     +43.5%
node2    heap alloc p50 MiB   298.30      286.89      -3.8%
node2    goroutines p50       581.00      553.00      -4.8%

node3    CPU p50 %             18.27       21.14     +15.7%
node3    mem used p50 %        10.67       10.43      -2.3%
node3    ESTAB sockets p50     90.00       60.00     -33.3%
node3    TIME_WAIT p50          4.00        4.00      +0.0%
node3    GC total              54          79        +46.3%
node3    GC pause ms total      6.52       10.27     +57.4%
node3    heap alloc p50 MiB   288.08      306.04      +6.2%
node3    goroutines p50       587.00      555.00      -5.5%

node4    CPU p50 %             17.72       21.06     +18.9%
node4    mem used p50 %        10.73       10.56      -1.6%
node4    ESTAB sockets p50     79.00       61.00     -22.8%
node4    TIME_WAIT p50          4.00        4.00      +0.0%
node4    GC total              54          79        +46.3%
node4    GC pause ms total      6.30        9.35     +48.5%
node4    heap alloc p50 MiB   286.42      310.97      +8.6%
node4    goroutines p50       577.00      561.00      -2.8%
```

Current read:

```text
- the west-1 4-node result was stable across all three rebooted rounds
- ON improved elapsed time by about 17-18% and throughput by about 21%
- node1, the fixed ingress/storage node, looked healthier on ON:
  lower memory, lower socket pressure, lower GC totals, lower heap, fewer goroutines
- node2-4 showed the same socket improvement on ON, but somewhat higher GC work
- loadgen memory stayed flat; CPU rose on ON but did not look capacity-bound
- the Buckit counters support a clean happy-path interpretation rather than a fallback-heavy win
```

Local artifact bundles for this run:

```text
/tmp/buckit-fastopen-west1-results/20260611T185626Z-4node-640k-conc32/
/tmp/buckit-fastopen-west1-results/20260611T191135Z-4node-640k-conc32/
```

2000-request slice outputs were generated for the first two rounds:

```text
/tmp/buckit-fastopen-west1-results/20260611T185626Z-4node-640k-conc32/round1-on/round1-on.slices-2000.json
/tmp/buckit-fastopen-west1-results/20260611T185626Z-4node-640k-conc32/round1-off/round1-off.slices-2000.json
/tmp/buckit-fastopen-west1-results/20260611T185626Z-4node-640k-conc32/round1.slice-compare-2000.tsv
/tmp/buckit-fastopen-west1-results/20260611T185626Z-4node-640k-conc32/round2-on/round2-on.slices-2000.json
/tmp/buckit-fastopen-west1-results/20260611T185626Z-4node-640k-conc32/round2-off/round2-off.slices-2000.json
/tmp/buckit-fastopen-west1-results/20260611T185626Z-4node-640k-conc32/round2.slice-compare-2000.tsv
```

Slice-level read across all three rounds:

```text
- rounds 1 and 2 have generated 2000-request slice comparison files
- round 3 was checked directly from round3-on.csv and round3-off.csv
- all 20/20 slices improved in every checked round; there was no ON regression

round1:
- mild late softening, but still positive in every slice
- first 5 slices median delta:
  TTFB p50 -20.99%, total p50 -20.09%
- last 5 slices median delta:
  TTFB p50 -17.45%, total p50 -16.84%

round2:
- essentially flat across the run
- first 5 slices median delta:
  TTFB p50 -20.16%, total p50 -19.05%
- last 5 slices median delta:
  TTFB p50 -21.35%, total p50 -18.89%

round3:
- slightly better late than early
- first 5 slices median delta:
  TTFB p50 -19.36%, total p50 -19.97%
- last 5 slices median delta:
  TTFB p50 -21.39%, total p50 -20.26%

overall:
- no slice-level degradation pattern was visible in the 640KiB run
```

### EC2 West-1: 4 x c6g.xlarge, 16 Drives, EC:4, 40000 x 2MiB

The same west-1 Spot cluster was then used for a clean `2MiB` rerun. One
storage Spot instance had been reclaimed after the `640KiB` run, so the node was
replaced with another same-subnet `c6g.xlarge` Spot instance, all storage data
volumes were wiped, and the `2MiB` object set was reseeded from scratch before
the measured arms.

Rig and workload:

```text
region / AZ / subnet: us-west-1 / us-west-1c / single subnet
storage nodes: 4 x c6g.xlarge Spot
load generator: 1 x t3.large Spot
data volumes: 4 x gp3 15 GiB attached per storage node
drive count: 16 total
storage class: EC:4
ingress target: one fixed storage node
object count: 40000
object size: 2MiB
concurrency: 32
reboot policy: reboot all storage nodes and the load generator before every arm
seed policy: clean wipe, reseed once, no reseed between measured arms
```

Seed and measured object set:

```text
seed run id: 20260611T195857Z-west1-4node-2m-prep
key prefix: obj-west1-4node-2m-
keys file: /home/ec2-user/buckit-fastopen-loadgen/results/20260611T195857Z-west1-4node-2m-prep/keys-40000.txt
```

Valid rounds:

```text
round 1: ON -> OFF
round 2: OFF -> ON
```

Per-round benchmark comparison:

```text
round  arm   elapsed s  obj/s    MiB/s   TTFB min  TTFB p50  TTFB p90  TTFB p99   TTFB max   total min  total p50  total p90  total p99   total max
1      ON    138.378    289.06   578.13    6.848    18.852    42.921   1066.528   3120.677     17.168     66.347    153.510   1111.627   3204.313
1      OFF   145.644    274.64   549.29    6.590    44.146    75.246   1062.158   2192.534     17.029     85.975    144.107   1101.940   2292.411
1      Δ      -5.0%     +5.2%    +5.2%    +3.9%    -57.3%    -43.0%     +0.4%     +42.3%      +0.8%     -22.8%     +6.5%     +0.9%     +39.8%

2      ON    138.461    288.89   577.78    6.450    18.602    42.809   1044.660   2256.073     16.894     66.731    159.263   1092.609   3341.055
2      OFF   145.624    274.68   549.36    6.401    43.484    75.416   1071.148   2181.634     16.083     86.140    146.301   1111.974   2224.426
2      Δ      -4.9%     +5.2%    +5.2%    +0.8%    -57.2%    -43.2%     -2.5%      +3.4%      +5.0%     -22.5%     +8.9%     -1.7%     +50.2%
```

Median summary across the two valid rounds:

```text
elapsed        ON 138.419s   OFF 145.634s   delta -5.0%
throughput     ON 288.98 obj/s, 577.95 MiB/s
               OFF 274.66 obj/s, 549.32 MiB/s
               delta +5.2%

TTFB p50       ON 18.727ms   OFF 43.815ms   delta -57.3%
TTFB p90       ON 42.865ms   OFF 75.331ms   delta -43.1%
TTFB p99       ON 1055.594ms OFF 1066.653ms delta -1.0%

total p50      ON 66.539ms   OFF 86.057ms   delta -22.7%
total p90      ON 156.386ms  OFF 145.204ms  delta +7.7%
total p99      ON 1102.118ms OFF 1106.957ms delta -0.4%
```

Read on the `2MiB` result:

```text
- ON improved elapsed time and throughput modestly
- ON improved TTFB p50/p90 strongly and total p50 clearly
- ON did not improve the high tail materially
- total p90 was worse in both rounds
- p99 was roughly flat
```

Buckit-specific metrics for the ON arms:

```text
round  attempted  fastopen_hits  fastget_hits  fallbacks  final_errors  replacement_opens  http_conns  fresh  reused   was_idle  streams_opened
1        40000        40000         40000          0            0               0            360251     262   359989    359839        480000
2        40000        40000         40000          0            0               0            360251     271   359980    359797        480000
```

Buckit metrics read:

```text
- FastOpen engaged for every ON request in both rounds
- no FastOpen fallback, replacement-open, or final-error activity was observed
- connection reuse remained extremely high:
  round 1 reused 359989 / fresh 262
  round 2 reused 359980 / fresh 271
- this was a clean happy-path ON run, not a fallback-heavy win
```

Detailed OS/runtime stats by round:

```text
round  host     arm   CPU p50  mem p50 %  mem p50 MiB  ESTAB p50  TIME_WAIT p50  GC total  GC pause ms  heap alloc p50 MiB  goroutines p50
1      loadgen  ON    78.92      7.08        553.52      23.00       149.00          -          -               -                 -
1      loadgen  OFF   70.15      7.23        565.43      25.00       382.00          -          -               -                 -
1      node1    ON    64.36     14.62       1136.00      66.00        16.00         2         6.05           498.28            967.00
1      node1    OFF   57.53     14.62       1136.00      72.00        15.00         2         5.85           485.15           1073.00
1      node2    ON    13.16     11.93        927.01      66.00         0.00         1         0.16           289.17            558.00
1      node2    OFF   16.80     12.29        954.45      72.00         4.00         1         0.51           295.43            573.00
1      node3    ON    13.95     12.04        935.16      63.00         0.00         1         0.20           298.25            556.00
1      node3    OFF   17.03     12.40        962.96      70.00         0.00         1         0.16           302.95            565.00
1      node4    ON    13.66     12.22        948.92      65.00         0.00         1         0.15           298.97            559.00
1      node4    OFF   16.92     12.53        973.33      67.00         0.00         1         0.21           290.60            563.50

2      loadgen  ON    78.92      7.13        557.98      23.00       127.00          -          -               -                 -
2      loadgen  OFF   69.80      7.06        552.17      25.00       391.00          -          -               -                 -
2      node1    ON    64.01     14.59       1133.13      61.00        29.00         2         6.43           505.02            974.00
2      node1    OFF   57.92     14.66       1138.51      76.00        26.00         2         1.13           483.79           1098.50
2      node2    ON    14.39     11.92        925.67      61.00         0.00         -          -               -                 -
2      node2    OFF   17.86     12.12        941.81      75.00         0.00         -          -               -                 -
2      node3    ON    14.14     12.06        936.98      64.00         0.00         1         0.15           289.71            558.00
2      node3    OFF   17.57     12.30        955.18      76.00         0.00         1         0.50           284.72            571.00
2      node4    ON    13.59     12.18        945.95      64.00         0.00         1         0.15           298.16            558.00
2      node4    OFF   17.26     12.41        964.09      69.00         0.00         1         0.22           283.37            564.00
```

Median OS/runtime comparison across the two valid rounds:

```text
host     metric               OFF median  ON median  delta
loadgen  CPU p50 %             69.98       78.92     +12.8%
loadgen  mem used p50 %         7.14        7.10      -0.5%
loadgen  mem used p50 MiB     558.80      555.75      -0.5%
loadgen  ESTAB sockets p50     25.00       23.00      -8.0%
loadgen  TIME_WAIT p50        386.50      138.00     -64.3%

node1    CPU p50 %             57.73       64.18     +11.2%
node1    mem used p50 %        14.64       14.61      -0.2%
node1    mem used p50 MiB    1137.25     1134.57      -0.2%
node1    ESTAB sockets p50     74.00       63.50     -14.2%
node1    TIME_WAIT p50         20.50       22.50      +9.8%
node1    GC total               2.00        2.00      +0.0%
node1    GC pause ms total      3.49        6.24     +78.6%
node1    heap alloc p50 MiB   484.47      501.65      +3.5%
node1    goroutines p50      1085.75      970.50     -10.6%

node2    CPU p50 %             17.33       13.78     -20.5%
node2    mem used p50 %        12.21       11.93      -2.3%
node2    mem used p50 MiB     948.13      926.34      -2.3%
node2    ESTAB sockets p50     73.50       63.50     -13.6%
node2    TIME_WAIT p50          2.00        0.00    -100.0%
node2    GC caveat: round2 node2 gc.json was not present in the synced artifacts

node3    CPU p50 %             17.30       14.05     -18.8%
node3    mem used p50 %        12.35       12.05      -2.4%
node3    mem used p50 MiB     959.07      936.07      -2.4%
node3    ESTAB sockets p50     73.00       63.50     -13.0%
node3    TIME_WAIT p50          0.00        0.00      +0.0%
node3    GC total               1.00        1.00      +0.0%
node3    GC pause ms total      0.33        0.18     -47.0%
node3    heap alloc p50 MiB   293.83      293.98      +0.1%
node3    goroutines p50       568.00      557.00      -1.9%

node4    CPU p50 %             17.09       13.62     -20.3%
node4    mem used p50 %        12.47       12.20      -2.2%
node4    mem used p50 MiB     968.71      947.44      -2.2%
node4    ESTAB sockets p50     68.00       64.50      -5.1%
node4    TIME_WAIT p50          0.00        0.00      +0.0%
node4    GC total               1.00        1.00      +0.0%
node4    GC pause ms total      0.22        0.15     -32.4%
node4    heap alloc p50 MiB   286.99      298.57      +4.0%
node4    goroutines p50       563.75      558.50      -0.9%
```

Socket-pressure interpretation:

```text
- ON again reduced median ESTAB counts across the load generator and all storage nodes
- loadgen ESTAB dropped 25.0 -> 23.0
- node1 ESTAB dropped 74.0 -> 63.5
- node2 ESTAB dropped 73.5 -> 63.5
- node3 ESTAB dropped 73.0 -> 63.5
- node4 ESTAB dropped 68.0 -> 64.5

- loadgen TIME_WAIT also dropped materially: 386.5 -> 138.0
- this does not prove a kernel-level issue by itself, but it is consistent with less
  connection churn on the client side
- Buckit counters support the same read:
  ON maintained extremely high connection reuse with only 262-271 fresh
  connections out of 360251 total

- in this document, "lower socket pressure" means fewer concurrently established
  and churning TCP connections for the same workload, not packet loss or TCP
  failure symptoms
```

Slice-level read across the two valid rounds:

```text
- slice comparison was derived directly from the raw per-request CSV files in
  2000-request windows
- all 20/20 slices improved in both rounds for both TTFB p50 and total p50

round1:
- TTFB p50 slice deltas: median -56.7%, range -64.9% to -19.2%
- total p50 slice deltas: median -23.0%, range -30.5% to -9.7%
- first 5 slices median delta:
  TTFB p50 -56.1%, total p50 -23.5%
- last 5 slices median delta:
  TTFB p50 -56.9%, total p50 -24.2%

round2:
- TTFB p50 slice deltas: median -57.5%, range -65.0% to -8.9%
- total p50 slice deltas: median -23.2%, range -29.4% to -10.3%
- first 5 slices median delta:
  TTFB p50 -55.8%, total p50 -23.0%
- last 5 slices median delta:
  TTFB p50 -56.3%, total p50 -19.1%

overall:
- there was no slice-level median regression pattern
- the mixed 2MiB result comes from the high tail, not from loss of the median win
```

Current read:

```text
- for 2MiB on this 4-node west-1 topology, FastOpen ON clearly improved the median path
- TTFB p50/p90 improved strongly, total p50 improved clearly, and elapsed/throughput improved modestly
- unlike the 640KiB run, ON did not materially improve the tail
- total p90 was worse in both rounds, and p99 stayed roughly flat
- the Buckit counters still show a clean happy-path ON run with near-total connection reuse
```

Local artifact bundle for this run:

```text
/tmp/buckit-fastopen-west1-results/20260611T195857Z-4node-2m-conc32/
```

### EC2 West-1: 4 x c6g.xlarge, 16 Drives, EC:4, 40000 x 2MiB, Round 3 With 250 MiB/s gp3 Throughput

This round should not be aggregated into the earlier `2MiB` result above. The
earlier two valid rounds used the default gp3 data-volume throughput of
`125 MiB/s` per attached volume. Before this round, all `16` attached gp3 data
volumes were modified to `250 MiB/s`.

Rig delta from the earlier `2MiB` section:

```text
storage-node data volumes: still 4 x gp3 15 GiB per node
throughput change: 125 MiB/s -> 250 MiB/s on all 16 data volumes
everything else unchanged: same 4-node west-1 Spot cluster, same 40000 x 2MiB object set,
same concurrency=32, same reboot-before-each-arm method, same single ingress target
```

Valid round:

```text
round 3: ON -> OFF
```

Per-round benchmark comparison:

```text
round  arm   elapsed s  obj/s    MiB/s   TTFB p50  TTFB p90  TTFB p99  total p50  total p90  total p99
3      ON    138.825    288.14   576.28   18.996    45.645   1052.437    67.628    159.518   1099.943
3      OFF   143.825    278.12   556.25   38.334    72.443   1063.299    83.196    146.885   1106.023
3      Δ      -3.5%     +3.6%    +3.6%   -50.4%    -37.0%     -1.0%     -18.7%     +8.6%     -0.5%
```

Buckit-specific metrics for `round3-on`:

```text
attempted:          40000
fastopen_hits:      40000
fastget_hits:       40000
fallbacks:              0
final_errors:           0
replacement_opens:      0
http_conns:        360251
fresh:                255
reused:            359996
was_idle:          359865
streams_opened:    480000
```

Round-3 read:

```text
- raising gp3 throughput from 125 MiB/s to 250 MiB/s did not materially improve Buckit throughput
- ON MiB/s in the earlier 125 MiB/s rounds was 578.13 and 577.78
- ON MiB/s in this 250 MiB/s round was 576.28
- the result shape stayed the same:
  strong TTFB p50/p90 improvement,
  clear total p50 improvement,
  modest elapsed/throughput improvement,
  and worse total p90 on ON
- this suggests EBS throughput was not the active bottleneck for this 2MiB workload
```

Local artifact bundle for this round:

```text
/tmp/buckit-fastopen-west1-results/20260611T211417Z-4node-2m-conc32-round3/
```

### EC2 West-1: 4 x c6g.xlarge, 16 Drives, EC:4, 40000 x 2MiB, 4-Target Fanout

This run should also stay separate from the earlier single-ingress `2MiB`
results. It used the same west-1 Spot cluster and the same `250 MiB/s` gp3
throughput configuration, but changed the client shape so the load generator
targeted all 4 Buckit nodes at the same time.

Rig delta relative to the earlier `250 MiB/s` single-ingress round:

```text
storage nodes: same 4 x c6g.xlarge Spot
load generator: same t3.large Spot
data volumes: same 16 x gp3 at 250 MiB/s
object set: same 40000 x 2MiB set
ingress pattern: 4 targets at once instead of 1 target
per-target concurrency: 8
total concurrency: 32
```

Method:

```text
round 1: ON  -> OFF
round 2: OFF -> ON
reboot before every arm
same seeded 2MiB set, no reseed between arms
```

Per-round benchmark comparison:

| Round | Arm | Elapsed s | Obj/s | MiB/s | TTFB p50 ms | TTFB p90 ms | TTFB p99 ms | Total p50 ms | Total p90 ms | Total p99 ms |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | ON | 136.164 | 293.76 | 587.53 | 12.846 | 19.853 | 1043.562 | 59.268 | 166.525 | 1095.773 |
| 1 | OFF | 136.428 | 293.20 | 586.39 | 12.212 | 19.968 | 1043.571 | 58.194 | 168.409 | 1098.593 |
| 1 | Delta | -0.2% | +0.2% | +0.2% | +5.2% | -0.6% | -0.0% | +1.8% | -1.1% | -0.3% |
| 2 | ON | 136.504 | 293.03 | 586.06 | 12.791 | 19.491 | 1043.754 | 59.021 | 166.276 | 1093.119 |
| 2 | OFF | 136.569 | 292.89 | 585.78 | 12.183 | 19.216 | 1047.246 | 58.143 | 168.811 | 1103.389 |
| 2 | Delta | -0.0% | +0.0% | +0.0% | +5.0% | +1.4% | -0.3% | +1.5% | -1.5% | -0.9% |

Median across the 2 rounds:

```text
elapsed:   ON 136.334s vs OFF 136.498s  (-0.1%)
throughput: ON 293.39 obj/s vs OFF 293.04 obj/s (+0.1%)
data rate:  ON 586.80 MiB/s vs OFF 586.08 MiB/s (+0.1%)

TTFB p50:  ON 12.819 ms vs OFF 12.198 ms (+5.1%)
TTFB p90:  ON 19.672 ms vs OFF 19.592 ms (+0.4%)
TTFB p99:  ON 1043.658 ms vs OFF 1045.409 ms (-0.2%)

total p50: ON 59.144 ms vs OFF 58.169 ms (+1.7%)
total p90: ON 166.400 ms vs OFF 168.610 ms (-1.3%)
total p99: ON 1094.446 ms vs OFF 1100.991 ms (-0.6%)
```

Buckit-specific counters:

```text
- per-target ON summaries were clean:
  0 fallbacks, 0 final errors, 0 replacement opens
- FastOpen counters were collected per target shard, not pre-summed into one
  cluster-wide aggregate in this run bundle
- representative ON target-shard metrics:
  10000 attempted
  10000 hits
  about 89900 FastOpenPart HTTP connections
  about 89800 reused connections
```

Read on the result:

```text
- spreading the client across all 4 ingress nodes mostly erased the FastOpen
  win seen in the single-ingress 250 MiB/s round
- absolute throughput was slightly higher than the single-ingress run, but the
  ON vs OFF difference was essentially flat
- ON no longer improved the median path; TTFB p50 and total p50 were slightly
  worse on ON in both rounds
- only small p90/p99 improvements remained
```

Comparison to the earlier single-ingress `250 MiB/s` round:

```text
single ingress, 250 MiB/s:
- elapsed delta:   -3.5%
- TTFB p50 delta: -50.4%
- total p50 delta: -18.7%

4-target fanout, 250 MiB/s:
- elapsed delta:   -0.1%
- TTFB p50 delta: +5.1%
- total p50 delta: +1.7%
```

Current interpretation:

```text
- increasing gp3 throughput alone still did not unlock additional FastOpen
  benefit for 2MiB
- the single-ingress FastOpen gain appears to depend strongly on that ingress
  node and its connection-reuse pattern
- once requests are spread across all 4 ingress nodes, the ON/OFF difference
  becomes negligible for this workload
```

Local artifact bundle for this round:

```text
/tmp/buckit-fastopen-west1-results/20260611T213212Z-4node-2m-fanout4x8/
```

### EC2 West-1: 4 x c6g.xlarge, 16 Drives, EC:4, 40000 x 2MiB, 4-Target Fanout at 16 Concurrency Per Target

This run keeps the same 4-node west-1 Spot cluster and `40000 x 2MiB` object
set, but uses the replacement `c6g.xlarge` load generator and increases the
multi-ingress fanout concurrency from `8` to `16` per target.

Rig delta relative to the earlier `2MiB` 4-target fanout run:

```text
storage nodes: same 4 x c6g.xlarge Spot
load generator: c6g.xlarge Spot instead of t3.large Spot
data volumes: same 16 x gp3 at 250 MiB/s
object set: same 40000 x 2MiB set
ingress pattern: 4 targets at once
per-target concurrency: 16
total concurrency: 64
```

Method:

```text
round 1: ON  -> OFF
round 2: OFF -> ON
reboot before every arm
same seeded 2MiB set, no reseed between arms
```

Per-round benchmark comparison:

| Round | Arm | Elapsed s | Obj/s | MiB/s | TTFB min ms | TTFB p50 ms | TTFB p90 ms | TTFB p99 ms | TTFB max ms | Total min ms | Total p50 ms | Total p90 ms | Total p99 ms | Total max ms |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | ON | 90.729 | 440.87 | 881.75 | 6.250 | 95.330 | 178.148 | 333.572 | 3138.580 | 11.219 | 115.221 | 248.263 | 456.883 | 3141.337 |
| 1 | OFF | 98.991 | 404.08 | 808.15 | 7.457 | 115.796 | 202.185 | 363.122 | 2130.093 | 12.196 | 131.457 | 259.926 | 486.442 | 2182.484 |
| 1 | Delta | -8.3% | +9.1% | +9.1% | -16.2% | -17.7% | -11.9% | -8.1% | +47.3% | -8.0% | -12.4% | -4.5% | -6.1% | +43.9% |
| 2 | ON | 89.457 | 447.14 | 894.28 | 5.934 | 91.581 | 180.381 | 363.853 | 2132.211 | 12.852 | 111.216 | 251.618 | 523.312 | 2187.036 |
| 2 | OFF | 102.883 | 388.79 | 777.58 | 7.567 | 118.859 | 202.142 | 321.060 | 2173.857 | 11.909 | 134.945 | 258.065 | 427.490 | 2299.859 |
| 2 | Delta | -13.0% | +15.0% | +15.0% | -21.6% | -22.9% | -10.8% | +13.3% | -1.9% | +7.9% | -17.6% | -2.5% | +22.4% | -4.9% |

Median across the 2 rounds:

```text
elapsed:    ON 90.093s vs OFF 100.937s  (-10.7%)
throughput: ON 444.01 obj/s vs OFF 396.43 obj/s (+12.0%)
data rate:  ON 888.02 MiB/s vs OFF 792.87 MiB/s (+12.0%)

TTFB p50:   ON 93.456 ms vs OFF 117.327 ms (-20.3%)
TTFB p90:   ON 179.265 ms vs OFF 202.164 ms (-11.3%)
TTFB p99:   ON 348.713 ms vs OFF 342.091 ms (+1.9%)

total p50:  ON 113.219 ms vs OFF 133.201 ms (-15.0%)
total p90:  ON 249.940 ms vs OFF 258.996 ms (-3.5%)
total p99:  ON 490.097 ms vs OFF 456.966 ms (+7.3%)
```

Buckit-specific counters:

```text
- all ON target shards were clean in both rounds:
  10000 attempted, 10000 hits, 0 fallbacks, 0 final errors per shard
- replacement opens and replacement-path activity stayed at 0
- per-shard FastOpenPart HTTP connections were about 89600-90300
- fresh connections per shard were about 160-230
- reused connections per shard were about 89400-90100
- streams opened were 120000 per shard
```

Median OS/runtime comparison across the 2 rounds:

| Host | CPU p50 OFF | CPU p50 ON | Mem p50 OFF | Mem p50 ON |
| --- | ---: | ---: | ---: | ---: |
| loadgen | 32.31% | 38.30% | 559.98 MiB | 558.35 MiB |
| node1 | 42.57% | 41.25% | 1070.06 MiB | 1183.57 MiB |
| node2 | 33.96% | 40.00% | 1081.32 MiB | 1067.00 MiB |
| node3 | 38.80% | 39.40% | 1101.88 MiB | 1133.47 MiB |
| node4 | 42.54% | 34.60% | 1111.05 MiB | 1093.31 MiB |

Runtime metrics on storage nodes:

| Host | GC count OFF | GC count ON | GC pause OFF | GC pause ON | Heap p50 OFF | Heap p50 ON | Goroutines OFF | Goroutines ON |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| node1 | 65.5 | 60.5 | 16.58 ms | 13.60 ms | 405.55 MiB | 410.82 MiB | 1161.5 | 1082.0 |
| node2 | 67.5 | 68.0 | 16.03 ms | 15.07 ms | 388.49 MiB | 365.29 MiB | 1157.3 | 1087.5 |
| node3 | 67.0 | 64.0 | 17.35 ms | 15.07 ms | 395.18 MiB | 407.97 MiB | 1151.8 | 1080.0 |
| node4 | 68.0 | 67.0 | 15.92 ms | 14.49 ms | 381.81 MiB | 404.16 MiB | 1146.5 | 1087.3 |

Socket metrics:

| Host | ESTAB OFF | ESTAB ON | TIME_WAIT OFF | TIME_WAIT ON | Total OFF | Total ON |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| node1 | 125.5 | 114.0 | 7.0 | 4.0 | 132.5 | 118.0 |
| node2 | 125.5 | 114.0 | 6.5 | 5.0 | 133.0 | 119.0 |
| node3 | 127.5 | 111.0 | 6.0 | 5.0 | 134.5 | 116.0 |
| node4 | 126.5 | 115.0 | 4.0 | 5.5 | 130.5 | 119.0 |

Loadgen sockets by target:

| Target | ESTAB OFF | ESTAB ON | TIME_WAIT OFF | TIME_WAIT ON | Total OFF | Total ON |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| target1 | 16.0 | 15.0 | 541.25 | 466.5 | 557.25 | 482.5 |
| target2 | 15.0 | 15.0 | 623.0 | 565.5 | 630.5 | 566.5 |
| target3 | 15.5 | 15.0 | 574.0 | 492.25 | 582.0 | 508.25 |
| target4 | 15.5 | 15.0 | 565.25 | 493.5 | 573.5 | 493.5 |

Read on the result:

```text
- increasing fanout concurrency to 16 per target made the 2MiB fanout result
  materially better than the earlier 4x8 run
- median TTFB and total latency improved on ON in both rounds
- elapsed/throughput improved by about 11-12% at the median across rounds
- total p90 improved modestly, but p99 remained mixed and regressed in round 2
- the c6g.xlarge loadgen still had headroom: CPU p50 was 38.30% on ON
- ON reduced storage-node established sockets and goroutine counts
- unlike the 640KiB 80000-object run, loadgen TIME_WAIT was lower on ON here
```

Local artifact bundle for this round:

```text
/tmp/buckit-fastopen-west1-results/20260611T234038Z-4node-2m-fanout4x16/
```

### EC2 West-1: 4 x c6g.xlarge, 16 Drives, EC:4, 40000 x 640KiB, 4-Target Fanout With c6g.xlarge Loadgen

This run should stay separate from the earlier `640KiB` results above. It used
the same 4-node west-1 Spot storage cluster and the same `250 MiB/s` gp3
throughput configuration, but changed both the load generator size and the
client shape. The earlier `t3.large` loadgen showed very high CPU usage, so it
was replaced with a `c6g.xlarge`, and per-target concurrency was increased.

Rig delta relative to the earlier `640KiB` 4-target fanout run:

```text
storage nodes: same 4 x c6g.xlarge Spot
load generator: c6g.xlarge Spot instead of t3.large Spot
data volumes: same 16 x gp3 at 250 MiB/s
object set: 40000 x 640KiB seeded on top of the existing 2MiB set
ingress pattern: 4 targets at once
per-target concurrency: 12
total concurrency: 48
```

Valid round:

```text
round 1: ON -> OFF
reboot before each arm
```

Per-round benchmark comparison:

| Arm | Elapsed s | Obj/s | MiB/s | TTFB min ms | TTFB p50 ms | TTFB p90 ms | TTFB p99 ms | TTFB max ms | Total min ms | Total p50 ms | Total p90 ms | Total p99 ms | Total max ms |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| ON | 43.196 | 926.01 | 578.75 | 13.652 | 42.176 | 72.400 | 111.166 | 182.651 | 17.597 | 45.899 | 78.917 | 119.535 | 198.928 |
| OFF | 51.653 | 774.40 | 484.00 | 15.418 | 51.377 | 85.890 | 128.363 | 197.574 | 19.906 | 55.269 | 93.644 | 139.402 | 238.299 |
| Delta | -16.4% | +19.6% | +19.6% | -11.4% | -17.9% | -15.7% | -13.4% | -7.6% | -11.6% | -17.0% | -15.7% | -14.3% | -16.5% |

Buckit-specific counters for the ON arm:

```text
- all 4 target shards were clean:
  10000 attempted, 10000 hits, 0 fallbacks, 0 final errors per shard
- per-shard FastOpenPart HTTP connections were about 90000
- fresh connections per shard were about 2478 to 3483
- reused connections per shard were about 86708 to 87528
- streams opened were 120000 per shard
```

OS and runtime highlights:

```text
loadgen:
- CPU p50:  OFF 32.34% vs ON 36.53%
- CPU p90:  OFF 37.07% vs ON 44.35%
- mem p50:  OFF 537.78 MiB vs ON 543.71 MiB

storage nodes:
- ESTAB p50 dropped from about 98 on OFF to about 62-68 on ON
- TIME_WAIT p50 stayed flat and low on the nodes:
  node1 8->8, node2 4->4, node3 4->4, node4 4->4
- node memory p50 was lower on ON for all 4 storage nodes
- node goroutine p50 was lower on ON for all 4 storage nodes
```

Read on the result:

```text
- replacing the load generator fixed the earlier client CPU bottleneck
- compared with the earlier 4-target 640KiB run using the t3.large loadgen,
  the FastOpen win became much stronger:
  elapsed delta improved from -7.0% to -16.4%
  throughput delta improved from +7.6% to +19.6%
- ON still reduced established inter-node socket counts materially
- unlike the earlier 4x8 run, loadgen TIME_WAIT increased on ON even though
  node-side TIME_WAIT stayed flat
```

Local artifact bundle for this round:

```text
/tmp/buckit-fastopen-west1-results/20260611T2230Z-4node-640k-fanout4x12-over2m/
```

### EC2 West-1: 4 x c6g.xlarge, 16 Drives, EC:4, 80000 x 640KiB, 4-Target Fanout With c6g.xlarge Loadgen

This run extends the previous `40000 x 640KiB` 4-target fanout run by seeding
another `40000` `640KiB` objects on top of the existing dataset and replaying
the combined `80000`-object keyset. It uses the same west-1 Spot cluster, the
same `c6g.xlarge` load generator, the same `250 MiB/s` gp3 data-volume
throughput, and the same fanout shape.

Rig:

```text
storage nodes: same 4 x c6g.xlarge Spot
load generator: same c6g.xlarge Spot
data volumes: same 16 x gp3 at 250 MiB/s
object set: combined 80000 x 640KiB
ingress pattern: 4 targets at once
per-target concurrency: 12
total concurrency: 48
```

Valid round:

```text
round 1: ON -> OFF
reboot before each arm
```

Per-round benchmark comparison:

| Arm | Elapsed s | Obj/s | MiB/s | TTFB min ms | TTFB p50 ms | TTFB p90 ms | TTFB p99 ms | TTFB max ms | Total min ms | Total p50 ms | Total p90 ms | Total p99 ms | Total max ms |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| ON | 86.502 | 924.84 | 578.02 | 5.167 | 43.730 | 73.425 | 107.677 | 210.550 | 6.990 | 47.128 | 79.658 | 116.306 | 229.549 |
| OFF | 104.286 | 767.12 | 479.45 | 7.494 | 52.279 | 87.203 | 125.119 | 275.585 | 8.759 | 55.963 | 94.754 | 135.352 | 341.793 |
| Delta | -17.1% | +20.6% | +20.6% | -31.0% | -16.4% | -15.8% | -13.9% | -23.6% | -20.2% | -15.8% | -15.9% | -14.1% | -32.8% |

Run validity notes:

```text
- the first attempt accidentally reused stale 40000-key shard files and was
  discarded
- the clean rerun regenerated 4 shard files with 20000 keys each
- total request count was 80000 for both ON and OFF
- both arms completed with 0 errors
```

Comparison to the previous `40000 x 640KiB` 4-target `4x12` run:

| Metric | 40000-object run | 80000-object run | Change |
| --- | ---: | ---: | ---: |
| ON elapsed | 43.196s | 86.502s | about 2.00x |
| OFF elapsed | 51.653s | 104.286s | about 2.02x |
| ON obj/s | 926.01 | 924.84 | -0.1% |
| OFF obj/s | 774.40 | 767.12 | -0.9% |
| ON MiB/s | 578.75 | 578.02 | -0.1% |
| OFF MiB/s | 484.00 | 479.45 | -0.9% |
| elapsed delta | -16.4% | -17.1% | slightly stronger |
| throughput delta | +19.6% | +20.6% | slightly stronger |
| TTFB p50 delta | -17.9% | -16.4% | slightly weaker |
| total p50 delta | -17.0% | -15.8% | slightly weaker |

Slice analysis using `2000`-request slices:

```text
slice count: 40
TTFB p50 improved in 37/40 slices
total p50 improved in 37/40 slices

median slice delta:
- TTFB p50:  -16.31%
- total p50: -15.90%

best slice:
- slice 17, requests 32001-34000
- TTFB p50:  -32.87%
- total p50: -33.50%

regressing slices:
- slice 25, requests 48001-50000: TTFB p50 +3.43%, total p50 +7.86%
- slice 31, requests 60001-62000: TTFB p50 +2.22%, total p50 +6.22%
- slice 39, requests 76001-78000: TTFB p50 +9.32%, total p50 +11.60%
```

Early-vs-late slice medians:

```text
TTFB p50:
- ON  first 10 slices median: 42.936 ms
- ON  last 10 slices median:  46.269 ms
- OFF first 10 slices median: 51.715 ms
- OFF last 10 slices median:  51.843 ms

total p50:
- ON  first 10 slices median: 46.629 ms
- ON  last 10 slices median:  49.611 ms
- OFF first 10 slices median: 55.859 ms
- OFF last 10 slices median:  55.150 ms
```

GC and socket interpretation:

```text
- node1 GC count did not show a steady increase over the run; it oscillated in
  a narrow band of about 3-8 GC events per slice-aligned window
- node1 GC pause medians were mostly about 0.18-0.25 ms and did not explain
  the late-run TTFB softening
- established TCP connections were stable:
  loadgen target1 ESTAB first10 median 12.0, last10 median 12.0
  node1 ESTAB ON first10 median 69.0, last10 median 67.75
  node1 ESTAB OFF first10 median 98.0, last10 median 98.5
- node-side TIME_WAIT stayed low and stable
- loadgen TIME_WAIT accumulated over time, especially on ON:
  target1 ON first10 median 186.5, last10 median 1292.5
  target1 OFF first10 median 112.75, last10 median 538.0
- because TIME_WAIT accumulation is loadgen-side while node-side socket counts
  remain stable, this looks like client-side connection lifecycle/churn rather
  than a Buckit server-side socket leak
```

Read on the result:

```text
- the larger 80000-object keyset did not materially reduce throughput
- elapsed time scaled almost exactly with request count
- the full-run FastOpen win stayed strong, but late slices were noisier
- ON TTFB/total p50 drifted upward late in the run, while OFF stayed mostly flat
- the drift did not correlate with steadily increasing GC or established socket
  counts on the storage nodes
```

Local artifact bundle for this round:

```text
/tmp/buckit-fastopen-west1-results/20260611T230803Z-4node-640k-fanout4x12-80k-over2m-clean/
```

### EC2 West-1: 4 x c6g.xlarge, 16 Drives, EC:4, Mixed 100000-Object Final Run

This final run used the same 4-node west-1 Spot cluster after the gp3 data
volumes were raised to `250 MiB/s`. The workload mixed the full `80000 x
640KiB` keyset with the first `20000 x 2MiB` keys, randomized the combined key
order once, and replayed the same shuffled plan for every ON and OFF arm.

Rig:

```text
storage nodes: 4 x c6g.xlarge Spot
load generator: 1 x c6g.xlarge Spot
data volumes: 16 x gp3, 15 GiB each, 250 MiB/s throughput
storage class: EC:4
object set: 80000 x 640KiB + 20000 x 2MiB
total logical bytes per arm: 94,371,840,000 bytes = 90,000 MiB
key order: deterministic shuffle, seed 20260611
ingress pattern: 4 targets at once
per-target concurrency: 20
total concurrency: 80
round order: round1 ON -> OFF, round2 OFF -> ON, round3 ON -> OFF
reboot before every arm: yes
reseed between arms: no
errors: 0 in every arm
```

The final orchestration helper was saved locally as:

```text
testing/fastopen-ec2/run_mixed_fanout_rounds.sh
```

Per-round benchmark comparison:

| Round | Arm | Elapsed s | Obj/s | MiB/s | TTFB min | TTFB p50 | TTFB p90 | TTFB p99 | TTFB max | Total min | Total p50 | Total p90 | Total p99 | Total max |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | ON | 132.653 | 753.85 | 678.46 | 7.525 | 85.703 | 146.493 | 238.574 | 1200.961 | 8.702 | 93.459 | 166.081 | 271.604 | 1210.461 |
| 1 | OFF | 151.235 | 661.22 | 595.10 | 7.643 | 98.745 | 170.834 | 265.451 | 2207.835 | 9.371 | 105.237 | 190.771 | 311.045 | 2210.465 |
| 1 | Delta | -12.3% | +14.0% | +14.0% | -1.5% | -13.2% | -14.2% | -10.1% | -45.6% | -7.1% | -11.2% | -12.9% | -12.7% | -45.2% |
| 2 | ON | 127.961 | 781.49 | 703.34 | 6.193 | 81.564 | 139.213 | 219.358 | 1329.289 | 7.481 | 89.728 | 159.343 | 262.156 | 1337.539 |
| 2 | OFF | 153.938 | 649.61 | 584.65 | 6.579 | 99.927 | 174.791 | 279.981 | 1318.471 | 9.015 | 106.269 | 194.949 | 319.136 | 1332.089 |
| 2 | Delta | -16.9% | +20.3% | +20.3% | -5.9% | -18.4% | -20.4% | -21.7% | +0.8% | -17.0% | -15.6% | -18.3% | -17.9% | +0.4% |
| 3 | ON | 132.080 | 757.12 | 681.41 | 6.600 | 84.595 | 146.043 | 239.817 | 1355.133 | 8.770 | 92.462 | 165.259 | 276.546 | 1383.826 |
| 3 | OFF | 155.248 | 644.13 | 579.72 | 7.825 | 100.694 | 174.906 | 279.308 | 1193.204 | 9.027 | 106.746 | 194.722 | 321.718 | 1193.878 |
| 3 | Delta | -14.9% | +17.5% | +17.5% | -15.7% | -16.0% | -16.5% | -14.1% | +13.6% | -2.8% | -13.4% | -15.1% | -14.0% | +15.9% |

Median across rounds:

| Metric | ON median | OFF median | Delta |
| --- | ---: | ---: | ---: |
| Elapsed s | 132.080 | 153.938 | -14.2% |
| Obj/s | 757.119 | 649.613 | +16.5% |
| MiB/s | 681.407 | 584.652 | +16.5% |
| TTFB p50 | 84.595 | 99.927 | -15.3% |
| TTFB p90 | 146.043 | 174.791 | -16.4% |
| TTFB p99 | 238.574 | 279.308 | -14.6% |
| Total p50 | 92.462 | 106.269 | -13.0% |
| Total p90 | 165.259 | 194.722 | -15.1% |
| Total p99 | 271.604 | 319.136 | -14.9% |

Buckit FastOpen counters:

```text
ON attempted: 100000 per arm
ON hits:      100000 per arm
ON fallbacks: 0 per arm
final errors: 0 per arm
HTTP connections per ON arm: about 899k
reused HTTP connections per ON arm: about 879k
fresh HTTP connections per ON arm: about 20k
streams opened per ON arm: 1,200,000
```

OS host metrics, median of per-round p50/p90 summaries:

| Host | CPU p50 OFF % | CPU p50 ON % | CPU p90 OFF % | CPU p90 ON % | Mem p50 OFF MiB | Mem p50 ON MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| loadgen | 31.01 | 38.05 | 34.01 | 41.32 | 763.27 | 768.78 |
| node1 | 44.38 | 46.29 | 58.92 | 56.73 | 1508.95 | 1468.75 |
| node2 | 45.29 | 45.41 | 57.73 | 56.87 | 1520.05 | 1469.75 |
| node3 | 44.22 | 46.68 | 59.28 | 58.68 | 1513.05 | 1474.04 |
| node4 | 45.79 | 45.99 | 57.33 | 56.79 | 1534.82 | 1472.03 |

Go runtime metrics, median across rounds:

| Node | GC count OFF | GC count ON | GC pause OFF ms | GC pause ON ms | Heap p50 OFF MiB | Heap p50 ON MiB | Goroutines p50 OFF | Goroutines p50 ON |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| node1 | 202 | 201 | 59.73 | 51.98 | 490.42 | 471.08 | 1367.5 | 1156.0 |
| node2 | 229 | 228 | 63.56 | 56.57 | 422.25 | 408.70 | 1344.0 | 1146.0 |
| node3 | 229 | 226 | 71.08 | 58.72 | 427.10 | 402.98 | 1358.0 | 1146.0 |
| node4 | 231 | 229 | 63.71 | 56.00 | 433.34 | 412.04 | 1348.5 | 1140.0 |

Storage-node socket metrics, median across rounds:

| Node | ESTAB p50 OFF | ESTAB p50 ON | TIME_WAIT p50 OFF | TIME_WAIT p50 ON | Total p50 OFF | Total p50 ON |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| node1 | 162.0 | 115.0 | 4.0 | 0.0 | 166.0 | 116.5 |
| node2 | 162.0 | 114.5 | 3.0 | 0.0 | 164.0 | 116.0 |
| node3 | 162.0 | 114.0 | 2.0 | 0.0 | 163.0 | 115.0 |
| node4 | 161.5 | 116.0 | 3.0 | 0.0 | 164.0 | 117.5 |

Loadgen socket metrics, median across rounds:

| Target | ESTAB p50 OFF | ESTAB p50 ON | TIME_WAIT p50 OFF | TIME_WAIT p50 ON | Total p50 OFF | Total p50 ON |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| target1 | 20.0 | 20.0 | 527.0 | 876.0 | 547.0 | 891.0 |
| target2 | 20.0 | 20.0 | 639.5 | 1126.0 | 659.0 | 1131.0 |
| target3 | 20.0 | 20.0 | 572.0 | 1036.0 | 586.0 | 1051.0 |
| target4 | 20.0 | 20.0 | 576.5 | 943.0 | 596.5 | 947.5 |

Slice analysis used `5000`-request request-index windows. Because the request
CSV does not include per-request wall-clock timestamps, these are plan-order
slices, not time slices. The last one or two slices should not be interpreted
as clean steady state: the global slice still has balanced target counts and
the expected object mix, but per-target local tail slices show uneven latency,
consistent with some targets draining earlier and reducing pressure near the
end.

Round 1 slice actual median latency:

| Slice | Requests | ON TTFB p50 ms | OFF TTFB p50 ms | ON Total p50 ms | OFF Total p50 ms | 2MiB objects | Target counts |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 1-5000 | 135.333 | 113.785 | 140.308 | 122.197 | 1065 | 1250/1250/1250/1250 |
| 2 | 5001-10000 | 96.353 | 112.061 | 102.583 | 118.343 | 1008 | 1250/1250/1250/1250 |
| 3 | 10001-15000 | 90.166 | 97.675 | 96.753 | 101.927 | 1002 | 1250/1250/1250/1250 |
| 4 | 15001-20000 | 86.156 | 101.666 | 95.790 | 108.000 | 1033 | 1250/1250/1250/1250 |
| 5 | 20001-25000 | 83.139 | 106.550 | 91.400 | 114.202 | 1035 | 1250/1250/1250/1250 |
| 6 | 25001-30000 | 80.236 | 97.231 | 86.491 | 102.763 | 966 | 1250/1250/1250/1250 |
| 7 | 30001-35000 | 84.720 | 102.118 | 92.835 | 108.034 | 1027 | 1250/1250/1250/1250 |
| 8 | 35001-40000 | 83.847 | 105.897 | 91.291 | 114.984 | 1008 | 1250/1250/1250/1250 |
| 9 | 40001-45000 | 81.049 | 97.566 | 88.775 | 103.651 | 966 | 1250/1250/1250/1250 |
| 10 | 45001-50000 | 84.438 | 100.453 | 92.241 | 107.994 | 936 | 1250/1250/1250/1250 |
| 11 | 50001-55000 | 88.082 | 93.275 | 96.942 | 100.575 | 1012 | 1250/1250/1250/1250 |
| 12 | 55001-60000 | 82.386 | 74.339 | 89.368 | 84.623 | 980 | 1250/1250/1250/1250 |
| 13 | 60001-65000 | 81.473 | 99.474 | 90.993 | 106.197 | 959 | 1250/1250/1250/1250 |
| 14 | 65001-70000 | 87.561 | 101.847 | 95.688 | 108.181 | 960 | 1250/1250/1250/1250 |
| 15 | 70001-75000 | 85.386 | 102.594 | 93.474 | 108.362 | 975 | 1250/1250/1250/1250 |
| 16 | 75001-80000 | 86.585 | 104.275 | 95.351 | 110.597 | 1008 | 1250/1250/1250/1250 |
| 17 | 80001-85000 | 84.377 | 102.451 | 90.950 | 108.290 | 993 | 1250/1250/1250/1250 |
| 18 | 85001-90000 | 84.862 | 107.238 | 92.157 | 114.837 | 999 | 1250/1250/1250/1250 |
| 19 | 90001-95000 | 86.416 | 90.968 | 95.285 | 96.258 | 1063 | 1250/1250/1250/1250 |
| 20 | 95001-100000 | 62.587 | 71.306 | 68.138 | 77.219 | 1005 | 1250/1250/1250/1250 |

Round 2 slice actual median latency:

| Slice | Requests | ON TTFB p50 ms | OFF TTFB p50 ms | ON Total p50 ms | OFF Total p50 ms | 2MiB objects | Target counts |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 1-5000 | 40.740 | 158.662 | 57.829 | 164.423 | 1065 | 1250/1250/1250/1250 |
| 2 | 5001-10000 | 65.436 | 108.386 | 73.934 | 113.142 | 1008 | 1250/1250/1250/1250 |
| 3 | 10001-15000 | 89.200 | 102.798 | 96.377 | 108.421 | 1002 | 1250/1250/1250/1250 |
| 4 | 15001-20000 | 85.390 | 100.950 | 92.786 | 107.155 | 1033 | 1250/1250/1250/1250 |
| 5 | 20001-25000 | 86.609 | 105.823 | 95.072 | 113.849 | 1035 | 1250/1250/1250/1250 |
| 6 | 25001-30000 | 85.745 | 102.912 | 94.516 | 110.259 | 966 | 1250/1250/1250/1250 |
| 7 | 30001-35000 | 83.272 | 99.261 | 91.934 | 105.137 | 1027 | 1250/1250/1250/1250 |
| 8 | 35001-40000 | 84.638 | 97.936 | 92.615 | 103.154 | 1008 | 1250/1250/1250/1250 |
| 9 | 40001-45000 | 86.395 | 101.542 | 95.439 | 108.015 | 966 | 1250/1250/1250/1250 |
| 10 | 45001-50000 | 84.994 | 101.457 | 94.704 | 106.954 | 936 | 1250/1250/1250/1250 |
| 11 | 50001-55000 | 81.714 | 98.135 | 88.137 | 103.666 | 1012 | 1250/1250/1250/1250 |
| 12 | 55001-60000 | 86.424 | 102.373 | 95.959 | 109.515 | 980 | 1250/1250/1250/1250 |
| 13 | 60001-65000 | 80.225 | 107.983 | 86.999 | 117.646 | 959 | 1250/1250/1250/1250 |
| 14 | 65001-70000 | 86.002 | 94.877 | 95.183 | 100.682 | 960 | 1250/1250/1250/1250 |
| 15 | 70001-75000 | 83.240 | 100.458 | 91.478 | 108.386 | 975 | 1250/1250/1250/1250 |
| 16 | 75001-80000 | 88.812 | 96.537 | 97.433 | 102.367 | 1008 | 1250/1250/1250/1250 |
| 17 | 80001-85000 | 79.899 | 96.644 | 88.115 | 102.424 | 993 | 1250/1250/1250/1250 |
| 18 | 85001-90000 | 84.701 | 92.912 | 92.268 | 98.895 | 999 | 1250/1250/1250/1250 |
| 19 | 90001-95000 | 83.523 | 76.167 | 91.857 | 83.645 | 1063 | 1250/1250/1250/1250 |
| 20 | 95001-100000 | 60.355 | 70.778 | 67.256 | 77.429 | 1005 | 1250/1250/1250/1250 |

Round 3 slice actual median latency:

| Slice | Requests | ON TTFB p50 ms | OFF TTFB p50 ms | ON Total p50 ms | OFF Total p50 ms | 2MiB objects | Target counts |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 1-5000 | 131.492 | 154.182 | 135.709 | 157.202 | 1065 | 1250/1250/1250/1250 |
| 2 | 5001-10000 | 100.622 | 112.322 | 106.593 | 116.881 | 1008 | 1250/1250/1250/1250 |
| 3 | 10001-15000 | 91.830 | 105.306 | 99.174 | 111.892 | 1002 | 1250/1250/1250/1250 |
| 4 | 15001-20000 | 83.647 | 95.299 | 91.666 | 100.778 | 1033 | 1250/1250/1250/1250 |
| 5 | 20001-25000 | 87.017 | 101.248 | 96.315 | 107.518 | 1035 | 1250/1250/1250/1250 |
| 6 | 25001-30000 | 84.590 | 97.280 | 92.439 | 103.292 | 966 | 1250/1250/1250/1250 |
| 7 | 30001-35000 | 83.947 | 95.659 | 91.310 | 101.453 | 1027 | 1250/1250/1250/1250 |
| 8 | 35001-40000 | 83.096 | 102.976 | 91.201 | 109.746 | 1008 | 1250/1250/1250/1250 |
| 9 | 40001-45000 | 81.036 | 99.245 | 88.055 | 105.138 | 966 | 1250/1250/1250/1250 |
| 10 | 45001-50000 | 83.435 | 106.719 | 91.019 | 114.267 | 936 | 1250/1250/1250/1250 |
| 11 | 50001-55000 | 86.710 | 96.022 | 95.874 | 100.997 | 1012 | 1250/1250/1250/1250 |
| 12 | 55001-60000 | 84.170 | 105.543 | 92.278 | 112.623 | 980 | 1250/1250/1250/1250 |
| 13 | 60001-65000 | 79.976 | 102.162 | 88.322 | 109.208 | 959 | 1250/1250/1250/1250 |
| 14 | 65001-70000 | 72.778 | 104.101 | 80.286 | 111.254 | 960 | 1250/1250/1250/1250 |
| 15 | 70001-75000 | 88.773 | 101.315 | 97.085 | 107.470 | 975 | 1250/1250/1250/1250 |
| 16 | 75001-80000 | 84.406 | 101.002 | 92.119 | 106.870 | 1008 | 1250/1250/1250/1250 |
| 17 | 80001-85000 | 89.632 | 99.480 | 100.139 | 105.084 | 993 | 1250/1250/1250/1250 |
| 18 | 85001-90000 | 77.834 | 94.798 | 84.785 | 100.377 | 999 | 1250/1250/1250/1250 |
| 19 | 90001-95000 | 74.448 | 89.192 | 81.165 | 97.245 | 1063 | 1250/1250/1250/1250 |
| 20 | 95001-100000 | 63.124 | 66.060 | 71.014 | 71.350 | 1005 | 1250/1250/1250/1250 |

Read on the result:

```text
- ON is consistently better at full-run median throughput and latency.
- ON improves median throughput by 16.5% across the three rounds.
- ON reduces storage-node ESTAB socket pressure by about 28-30%.
- ON does not show evidence of upward drift in the steady middle slices.
- The final one or two slices are likely affected by target drain, so they
  should be discounted for steady-state drift analysis.
- For future drift analysis, add per-request wall-clock timestamps and slice
  by elapsed time, not only request index.
```

Local artifact bundle for this run:

```text
/tmp/buckit-fastopen-west1-results/20260612T001638Z-4node-mixed100k-fanout4x20/
```

### Next Planned EC2 Spec: 8 Storage Nodes / 16 Drives

The next EC2 performance run should use an 8-node distributed topology with 16
actual attached data volumes and a stronger load generator.

Rig:

```text
region: us-east-1
storage nodes: 8 x m6g.large Spot
load generator: 1 x m6g.large Spot
storage-node root volume: default AMI/root size
storage-node data volumes: 2 x gp3 20 GiB attached
loadgen root volume: default AMI/root size
topology: 8 nodes / 16 drives
storage class: EC:4
```

Storage layout:

```text
- use the two attached 20 GiB gp3 volumes for Buckit data
- do not place Buckit data on the root volume
- root volume is for OS, logs, tools, and temporary orchestration files only
- create one Buckit drive path per attached data volume
- total drive count should be 16 across the 8 nodes
```

Object set:

```text
object count: 40000
primary object size: 640KiB
follow-up object size: 2MiB
seed policy: seed once per object size, do not reseed between measured arms
```

Capacity note:

```text
40000 x 640KiB  = about 24.4 GiB logical
40000 x 2MiB    = about 78.1 GiB logical

At EC:4, raw footprint is about 2x logical:
- 640KiB set: about 48.8 GiB raw cluster-wide
- 2MiB set:   about 156.3 GiB raw cluster-wide

With 16 attached data volumes at 20 GiB each:
- total attached raw capacity = 320 GiB

This is sufficient for both the 640KiB and 2MiB 40000-object runs with usable
headroom.
```

Measured workload:

```text
ordering: key-order
cache profile: 1x-all
concurrency: 32
```

Measured arms:

```text
round 1: ON  -> OFF
round 2: OFF -> ON
round 3: ON  -> OFF only if rounds 1 and 2 are not sufficiently consistent
```

Default stopping rule:

```text
- run rounds 1 and 2 first
- if rounds 1 and 2 are directionally consistent and close enough on the key
  latency/throughput deltas, stop there
- only run round 3 if the first two rounds disagree materially or if there is
  reason to suspect drift, startup contamination, or a noisy outlier
```

Method constraints:

```text
- all 9 instances must be Spot
- reboot storage nodes and loadgen before every measured arm
- wait for SSH readiness after reboot
- wait for Buckit readiness after process start
- include a post-start stabilization delay before beginning the measured GET run
- use the exact same request plan for ON and OFF
- do not reseed between measured arms
- preserve any invalid arm separately rather than overwriting it
```

Artifacts to collect after every valid arm:

```text
- request CSV
- request summary JSON
- Buckit metrics before JSON
- Buckit metrics after JSON
- plan CSV
- loadgen host CSV/JSON
- loadgen sockets CSV/JSON
- per-storage-node host CSV/JSON
- per-storage-node GC CSV/JSON
- per-storage-node sockets CSV/JSON
```

Result presentation:

```text
- per-round ON vs OFF comparison
- median across the valid rounds that were actually run
- Buckit-specific metric table:
  attempted, hits, fallbacks, final errors, replacement opens,
  HTTP connections, fresh, reused, was-idle, streams opened
- OS/runtime comparison table:
  CPU, memory, GC, heap, goroutines, ESTAB, TIME_WAIT
- preserve raw request data so the run can be sliced afterward
- generate slice summaries in 2000-request windows
```

### Degradation Hypotheses And Current Read

For the late-run FastOpen regression seen in some EC2 `t3.micro` runs, the
current evidence does not support a single confirmed root cause yet, but it
does narrow the explanation substantially.

#### What Is Not Well Supported

The strongest ruled-down theory is HTTP connection burn on the full-read happy
path.

Later EC2 runs added FastOpen HTTP trace counters and socket-state sampling.
Those runs showed:

```text
- very high connection reuse on full-read FastOpen requests
- no meaningful TIME_WAIT explosion
- zero stream cancellations on the measured happy-path runs
```

That weakens the earlier theory that FastOpen was tearing down the transport
connection on every shard read during the normal fully-consumed path. That
mechanism may still matter for partial-read or early-close cases, but it was
not supported by the measured full-object EC2 runs.

#### Leading Theory For The Bad EC2 Runs

The leading explanation remains small-instance noise and sustained resource
pressure on `t3.micro`.

Reasons:

```text
- some EC2 runs showed ON degrading late, while others with the same code did not
- the bad behavior was not stable enough to look like a deterministic logic bug
- ON often carried somewhat higher CPU / allocation / orchestration pressure
- t3.micro has burst-credit behavior, small CPU headroom, small memory headroom,
  and more sensitivity to scheduler and noisy-neighbor effects
```

A plausible interpretation is:

```text
FastOpen adds some steady extra work on the ingress path.
On larger or steadier hosts that overhead is acceptable.
On t3.micro it may push the system over a noisy threshold intermittently, which
then appears as a late-run regression or unstable slice behavior.
```

#### What Docker And Real-Host Follow-Ups Showed

The follow-up environments did not reproduce the same degradation shape:

```text
- local Docker runs, even with mild netem latency and longer object counts,
  did not show a clear progressive late-run collapse
- the first non-Docker real-host run on ubuntudell + thinkpad was uniformly
  slow from the start rather than degrading only late in the run
```

That suggests the original EC2 degradation was not a generic FastOpen behavior
that reproduces automatically on every environment. It appears to depend on the
particular runtime conditions of the earlier EC2 setup.

#### Current Conclusion

The current working conclusion is:

```text
1. Full-read connection-burn is not supported by the later HTTP reuse evidence.
2. T3/small-host instability remains the best explanation for the worst EC2 runs.
3. Docker and the first real-host Linux run did not reproduce the same late-run
   collapse, so the degradation mechanism is still environment-sensitive and
   not fully explained.
```

The next session should preserve this framing and avoid re-investigating the
already-weakened connection-burn theory unless a partial-read or early-close
workload is introduced.

That suggests the first real-host bottleneck is not raw CPU saturation. The
next session should keep the existing two-host workflow, reuse
`threaded_seed.py`, and scale object count upward from `5k` once a cleaner
seed strategy is in place.
```
