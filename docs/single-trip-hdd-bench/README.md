# Single-Trip HDD Bench Notes

This document captures the ad hoc single-HDD test rig used on June 4-5, 2026 for the phase-1 single-trip GET prototype.

The goal of this run was not to prove the final HDD benefit. The host had only one rotational disk, so this was a control rig to:

- validate the host-side test procedure
- validate basic path dependency assumptions
- get an early latency signal before a real multi-node HDD run

## Host

- Host: `root@192.184.90.116`
- OS: Ubuntu 24.04.1
- Disk layout: one rotational root disk (`/dev/vda1`)
- Buckit paths used as pseudo-drives:
  - `/root/singletrip-bench/data4/d01`
  - `/root/singletrip-bench/data4/d02`
  - `/root/singletrip-bench/data4/d03`
  - `/root/singletrip-bench/data4/d04`

These are four directories on the same HDD, not four independent disks.

## Binaries And Scripts

Host-side paths:

- `buckit`: `/root/singletrip-bench/bin/buckit`
- `warp`: `/root/singletrip-bench/bin/warp`
- server launcher copied from repo: `testing/singletrip-hdd/start-buckit.sh`

Checked-in rig assets live in:

- [testing/singletrip-hdd/README.md](/Users/rooseveltlai/develop/buckit-io/buckit/testing/singletrip-hdd/README.md)

Local helper state used during the runs:

- `mc` config dir: `/private/tmp/buckit-hdd-mc`
- `640 KiB` presigned URL file: `/private/tmp/hdd-640k-obj-urls.jsonl`
- `2 MiB` presigned URL file: `/private/tmp/hdd-obj-urls.jsonl`

## Server Start

Use `MINIO_CI_CD=1` because the test data lives on the root disk.

`FAST_GET=1`:

```sh
ssh root@192.184.90.116 '
  env MINIO_CI_CD=1 FAST_GET=1 \
    MINIO_ROOT_USER=buckitadmin \
    MINIO_ROOT_PASSWORD=buckitadmin \
    /root/singletrip-bench/run/start-buckit.sh \
    /root/singletrip-bench/data4/d01 \
    /root/singletrip-bench/data4/d02 \
    /root/singletrip-bench/data4/d03 \
    /root/singletrip-bench/data4/d04
'
```

`FAST_GET=0`:

```sh
ssh root@192.184.90.116 '
  env MINIO_CI_CD=1 FAST_GET=0 \
    MINIO_ROOT_USER=buckitadmin \
    MINIO_ROOT_PASSWORD=buckitadmin \
    /root/singletrip-bench/run/start-buckit.sh \
    /root/singletrip-bench/data4/d01 \
    /root/singletrip-bench/data4/d02 \
    /root/singletrip-bench/data4/d03 \
    /root/singletrip-bench/data4/d04
'
```

Stop the server:

```sh
ssh root@192.184.90.116 'pkill -f "/root/singletrip-bench/bin/buckit"'
```

## Benchmark Method

The method that gave the cleanest signal was:

1. Use 10 different objects of the same size.
2. Run the GETs from the host itself, not from a remote client.
3. Before each GET:
   - `sync`
   - `echo 3 > /proc/sys/vm/drop_caches`
4. Measure:
   - client-visible TTFB with `curl -w '%{time_starttransfer}'`
   - server-side TTFB with `mc admin trace --verbose`
5. Compare either:
   - last 5 of 10 runs, or
   - lowest 5 of 10 runs when the run is noisy

Example host-local loop:

```sh
ssh root@192.184.90.116 '
  set -e
  for u in "$@"; do
    sync
    echo 3 > /proc/sys/vm/drop_caches
    curl --http1.1 -sS -o /dev/null -w "%{time_starttransfer}\n" "$u"
  done
' sh "<presigned-url-1>" "<presigned-url-2>" ...
```

Server-side trace:

```sh
mc --config-dir /private/tmp/buckit-hdd-mc admin trace --verbose hdd
```

## Datasets

### 2 MiB objects

- Bucket: `singletrip-cold-2m-4d`
- Object count: 20 total, 10 used in the clean comparison
- Object size: `2 MiB`
- Shadow verified: `current/part.1` present on all 4 pseudo-drives

### 640 KiB objects

- Bucket: `singletrip-cold-640k-4d`
- Object count: 10
- Object size: `640 KiB`
- Shadow verified: `current/part.1` present on all 4 pseudo-drives
- `current/part.1` size on disk: `328736` bytes
- `xl.meta` size on disk: `368` bytes

## Functional Validation

These checks passed on the host:

- `FAST_GET=1`: rename `xl.meta` away on all 4 object directories before GET, GET still returns `200`
- `FAST_GET=0`: rename `current/` away on all 4 object directories before GET, GET still returns `200`

This validates the basic dependency split:

- ON path can serve without `xl.meta`
- OFF path can serve without `current/`

## Results

### 2 MiB, 10 different objects, OFF first then ON

Using the last 5 of 10 runs:

- `FAST_GET=0`
  - `curl` median: `17.032 ms`
  - trace median: `15.301 ms`
- `FAST_GET=1`
  - `curl` median: `12.073 ms`
  - trace median: `11.698 ms`

Improvement:

- `curl`: `29.1%`
- trace: `23.5%`

### 2 MiB, 10 different objects, ON first then OFF

Using the last 5 of 10 runs:

- `FAST_GET=1`
  - `curl` median: `30.449 ms`
  - trace median: `30.214 ms`
- `FAST_GET=0`
  - `curl` median: `43.962 ms`
  - trace median: `43.241 ms`

Improvement:

- `curl`: `30.7%`
- trace: `30.1%`

This was the most convincing control result from the single-HDD host.

### 640 KiB, OFF first then ON

Using the last 5 of 10 runs:

- `FAST_GET=0`
  - `curl` median: `20.905 ms`
  - trace median: `19.100 ms`
- `FAST_GET=1`
  - `curl` median: `42.286 ms`
  - trace median: `39.010 ms`

This run inverted and was not trustworthy as a clean fast-path comparison.

Observed counters after the ON arm:

- `fast_get_hits_total=10`
- `fast_get_fallbacks_total=15`

That indicates fallback noise or unrelated contamination during the `640 KiB` run.

### 640 KiB, ON first then OFF

Full 10-run lists:

- `FAST_GET=1` `curl` ms:
  - `157.507`, `112.020`, `50.408`, `70.818`, `267.515`, `39.806`, `53.513`, `35.694`, `109.684`, `140.253`
- `FAST_GET=1` trace ms:
  - `153.795`, `112.089`, `48.038`, `70.438`, `265.378`, `37.215`, `51.709`, `34.376`, `108.788`, `139.557`
- `FAST_GET=0` `curl` ms:
  - `291.295`, `69.004`, `70.546`, `54.955`, `70.527`, `73.920`, `215.505`, `55.148`, `65.311`, `33.009`
- `FAST_GET=0` trace ms:
  - `288.341`, `68.346`, `69.096`, `54.154`, `69.758`, `72.806`, `167.278`, `42.117`, `64.080`, `30.531`

Using the lowest 5 runs in each group:

- `FAST_GET=1`
  - `curl` mean: `50.048 ms`
  - `curl` median: `50.408 ms`
  - trace mean: `48.355 ms`
  - trace median: `48.038 ms`
- `FAST_GET=0`
  - `curl` mean: `55.485 ms`
  - `curl` median: `55.148 ms`
  - trace mean: `51.846 ms`
  - trace median: `54.154 ms`

Difference from lowest-5 slices:

- `curl` median improvement: `8.6%`
- trace median improvement: `11.3%`

This is weaker and much noisier than the `2 MiB` result.

## Interpretation

Current conclusion from the single-HDD host:

- The host rig works and is reusable.
- The `2 MiB` different-object cold test produced a repeatable positive signal around `24-31%` TTFB improvement.
- The `640 KiB` case is noisy and not yet a stable signal.
- Because all four paths are on the same physical disk, none of these numbers should be presented as proof of the final HDD benefit.

## Recommended Reuse For Tomorrow

For the true multi-node HDD run:

- keep the host-local `curl` plus `mc admin trace` method
- keep the "10 different objects" pattern
- keep cache-drop before each GET
- prefer object sizes that cleanly produce `current/part.1`
- record both full 10-run lists and the summary slice used for comparison
- check `fast_get_hits_total` and `fast_get_fallbacks_total` after every ON arm

If the multi-node rig is clean, the `2 MiB` method from this document should be the baseline procedure.
